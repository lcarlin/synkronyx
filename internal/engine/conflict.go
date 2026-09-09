package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/hash"
	"github.com/lcarlin/synkronyx/internal/state"
)

// conflictTimeLayout é o sufixo temporal do arquivo preservado.
const conflictTimeLayout = "20060102-150405"

// detectConflict responde à pergunta da seção 9: houve alteração concorrente?
//
// A definição operacional é precisa, e vale registrar por que ela é essa:
// há conflito quando o lado destino também mudou desde a última
// sincronização registrada E seu conteúdo atual difere do da origem.
//
// Só "os dois arquivos são diferentes" não basta — é exatamente o caso normal
// de uma alteração unilateral, que deve ser propagada. O que caracteriza
// conflito é o destino ter saído por conta própria do estado que nós
// gravamos: aí não existe origem única e sobrescrever perderia dados.
func (e *Engine) detectConflict(ctx context.Context, ev event.Event, srcAbs, dstAbs string) (bool, error) {
	dstSide := ev.Side.Opposite()

	dstStat, err := hash.StatOf(dstAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil // destino não existe: criação simples
		}
		return false, err
	}
	if dstStat.IsDir {
		return false, nil
	}

	dstEntry, err := e.db.Get(ctx, dstSide, ev.Path)
	if err != nil {
		return false, err
	}
	if dstEntry == nil {
		// O destino existe mas nunca passou por nós. Conservadoramente, é um
		// conflito: não temos base para afirmar que a origem é mais recente.
		return e.contentDiffers(srcAbs, dstAbs)
	}
	if dstStat.Unchanged(dstEntry.Stat()) {
		return false, nil // destino intocado desde a última sync
	}
	return e.contentDiffers(srcAbs, dstAbs)
}

func (e *Engine) contentDiffers(srcAbs, dstAbs string) (bool, error) {
	srcDigest, err := e.digestOf(srcAbs)
	if err != nil {
		return false, err
	}
	dstDigest, err := e.digestOf(dstAbs)
	if err != nil {
		return false, err
	}

	switch hash.Compare(srcDigest, dstDigest) {
	case hash.Same:
		return false, nil
	case hash.Different:
		return true, nil
	default:
		// Sem digest de algum dos lados (diretório, ou falha de leitura já
		// tratada acima). Tamanho é o que resta, e diferença de tamanho
		// prova diferença de conteúdo.
		srcStat, err := hash.StatOf(srcAbs)
		if err != nil {
			return false, err
		}
		dstStat, err := hash.StatOf(dstAbs)
		if err != nil {
			return false, err
		}
		return srcStat.Size != dstStat.Size, nil
	}
}

// resolveConflict aplica a política configurada. O princípio Fail Safe da
// seção 16 é o que decide o padrão: preservar, nunca sobrescrever em silêncio.
func (e *Engine) resolveConflict(ctx context.Context, ev event.Event, srcAbs, dstAbs string) error {
	dstSide := ev.Side.Opposite()

	srcDigest, err := e.digestOf(srcAbs)
	if err != nil {
		return err
	}
	dstDigest, err := e.digestOf(dstAbs)
	if err != nil {
		return err
	}

	digestA, digestB := srcDigest, dstDigest
	if ev.Side == event.SideB {
		digestA, digestB = dstDigest, srcDigest
	}
	if err := e.db.RecordConflict(ctx, ev.Path, digestA, digestB); err != nil {
		return err
	}

	e.log.Warn("conflito detectado",
		"path", ev.Path, "policy", string(e.cfg.ConflictPolicy),
		"digest_"+ev.Side.String(), srcDigest.Short(),
		"digest_"+dstSide.String(), dstDigest.Short())

	switch e.cfg.ConflictPolicy {
	case config.ConflictManual:
		// Não tocar em nada; deixar marcado para intervenção humana.
		if err := e.db.MarkStatus(ctx, ev.Side, ev.Path, state.StatusConflict); err != nil {
			return err
		}
		return e.db.MarkStatus(ctx, dstSide, ev.Path, state.StatusConflict)

	case config.ConflictNewerWins:
		srcStat, err := hash.StatOf(srcAbs)
		if err != nil {
			return err
		}
		dstStat, err := hash.StatOf(dstAbs)
		if err != nil {
			return err
		}
		if dstStat.MTime.After(srcStat.MTime) {
			// O destino é mais novo: a propagação correta é na direção
			// oposta à do evento.
			e.guard.Expect(ev.Side, ev.Path, 3)
			if err := e.xfer.CopyFile(ctx, dstAbs, srcAbs); err != nil {
				e.guard.Forget(ev.Side, ev.Path)
				return err
			}
		} else {
			e.guard.Expect(dstSide, ev.Path, 3)
			if err := e.xfer.CopyFile(ctx, srcAbs, dstAbs); err != nil {
				e.guard.Forget(dstSide, ev.Path)
				return err
			}
		}
		if err := e.db.ResolveConflict(ctx, ev.Path, "newer-wins"); err != nil {
			return err
		}
		return e.recordPair(ctx, ev.Path, srcAbs, dstAbs, false)

	default: // ConflictPreserve
		// A versão do destino é renomeada para um nome de conflito e a da
		// origem é copiada por cima. Nada é perdido: as duas continuam no
		// disco, e o rename é local, sem transferência.
		preserved := conflictName(ev.Path, dstSide, time.Now())
		preservedAbs := e.abs(dstSide, preserved)

		e.guard.Expect(dstSide, preserved, 2)
		if err := e.xfer.Move(dstAbs, preservedAbs); err != nil {
			e.guard.Forget(dstSide, preserved)
			return fmt.Errorf("preservando versão em conflito: %w", err)
		}
		e.log.Warn("versão preservada", "original", ev.Path, "preservada", preserved)

		e.guard.Expect(dstSide, ev.Path, 3)
		if err := e.xfer.CopyFile(ctx, srcAbs, dstAbs); err != nil {
			e.guard.Forget(dstSide, ev.Path)
			return err
		}
		if err := e.db.ResolveConflict(ctx, ev.Path, "preserve:"+preserved); err != nil {
			return err
		}
		return e.recordPair(ctx, ev.Path, srcAbs, dstAbs, false)
	}
}

// conflictName produz "dir/file.sync-conflict-B-20260909-143012.txt".
//
// O sufixo vai antes da extensão de propósito: mantém o arquivo abrível pelo
// mesmo programa que abriria o original, o que importa para quem vai
// inspecionar o conflito. O padrão casa com o exclude default, então esses
// arquivos não são eles próprios sincronizados.
func conflictName(rel string, side event.Side, at time.Time) string {
	dir := filepath.Dir(rel)
	base := filepath.Base(rel)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	name := fmt.Sprintf("%s.sync-conflict-%s-%s%s", stem, side, at.Format(conflictTimeLayout), ext)
	if dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}
