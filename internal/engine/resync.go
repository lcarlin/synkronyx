package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/hash"
	"github.com/lcarlin/synkronyx/internal/scan"
)

// firstSync estabelece o estado inicial (seção 10).
//
// Roda depois de os watchers subirem, não antes: eventos ocorridos durante o
// scan ficam enfileirados no debouncer e são aplicados na sequência. A ordem
// inversa deixaria uma janela cega.
func (e *Engine) firstSync(ctx context.Context) error {
	done, err := e.db.Meta(ctx, metaFirstSync)
	if err != nil {
		return err
	}
	if done != "" {
		e.log.Info("first sync já concluído anteriormente", "em", done)
		// Ainda assim reconciliamos: o serviço pode ter ficado parado
		// enquanto a árvore mudava, e nada disso gerou evento.
		e.log.Info("reconciliando árvores após restart")
	} else {
		e.log.Info("iniciando first sync", "policy", string(e.cfg.FirstSyncPolicy))
	}

	if err := e.reconcile(ctx, "first-sync"); err != nil {
		return err
	}
	return e.db.SetMeta(ctx, metaFirstSync, time.Now().Format(time.RFC3339))
}

// FullResync reconstrói o estado a partir do disco (seção 11).
//
// Diferença em relação ao First Sync: aqui o estado gravado é tratado como
// suspeito, não como verdade. Só o que está no disco conta.
func (e *Engine) FullResync(ctx context.Context) error {
	start := time.Now()
	if err := e.reconcile(ctx, "full-resync"); err != nil {
		return err
	}
	e.log.Info("full resync concluído", "duracao", time.Since(start).String())
	return nil
}

// reconcile compara as duas árvores e aplica a política configurada.
func (e *Engine) reconcile(ctx context.Context, phase string) error {
	excluder := config.NewExcluder(e.cfg.Exclude)

	invA, err := scan.Walk(ctx, e.cfg.A, excluder)
	if err != nil {
		return fmt.Errorf("scan de A: %w", err)
	}
	invB, err := scan.Walk(ctx, e.cfg.B, excluder)
	if err != nil {
		return fmt.Errorf("scan de B: %w", err)
	}
	e.log.Info("scan concluído", "phase", phase, "entradas_a", len(invA), "entradas_b", len(invB))

	var applied, skipped, conflicts int
	for _, rel := range scan.Union(invA, invB) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		stA, inA := invA[rel]
		stB, inB := invB[rel]

		switch {
		case inA && !inB:
			if err := e.reconcileOneSided(ctx, rel, event.SideA, stA); err != nil {
				e.log.Error("falha reconciliando", "path", rel, "err", err)
				continue
			}
			applied++

		case inB && !inA:
			if err := e.reconcileOneSided(ctx, rel, event.SideB, stB); err != nil {
				e.log.Error("falha reconciliando", "path", rel, "err", err)
				continue
			}
			applied++

		case inA && inB:
			n, err := e.reconcileBothSides(ctx, rel, stA, stB)
			if err != nil {
				e.log.Error("falha reconciliando", "path", rel, "err", err)
				continue
			}
			switch n {
			case reconcileNoop:
				skipped++
			case reconcileConflict:
				conflicts++
			default:
				applied++
			}
		}
	}

	e.log.Info("reconciliação concluída", "phase", phase,
		"aplicadas", applied, "sem_acao", skipped, "conflitos", conflicts)
	return nil
}

type reconcileResult int

const (
	reconcileApplied reconcileResult = iota
	reconcileNoop
	reconcileConflict
)

// reconcileOneSided trata um path que existe em apenas um lado.
//
// A ambiguidade aqui é real e o escopo pede que a política seja decidida
// antes de rodar: um arquivo presente só em A pode ser novo (deve ser
// copiado) ou pode ter sido apagado em B (deve ser apagado em A). Sem estado
// anterior não há como distinguir — daí FirstSyncPolicy.
func (e *Engine) reconcileOneSided(ctx context.Context, rel string, present event.Side, st hash.Stat) error {
	absent := present.Opposite()

	// Se o estado registra que o path já existiu no lado ausente, a leitura
	// correta é "foi apagado lá", não "é novo aqui".
	prev, err := e.db.Get(ctx, absent, rel)
	if err != nil {
		return err
	}
	deletedRemotely := prev != nil

	switch e.cfg.FirstSyncPolicy {
	case config.FirstSyncAWins:
		if present == event.SideA {
			return e.copyOneSided(ctx, rel, present, st)
		}
		return e.deleteOneSided(ctx, rel, present, st)

	case config.FirstSyncBWins:
		if present == event.SideB {
			return e.copyOneSided(ctx, rel, present, st)
		}
		return e.deleteOneSided(ctx, rel, present, st)

	default: // FirstSyncUnion
		if deletedRemotely {
			return e.deleteOneSided(ctx, rel, present, st)
		}
		return e.copyOneSided(ctx, rel, present, st)
	}
}

func (e *Engine) copyOneSided(ctx context.Context, rel string, src event.Side, st hash.Stat) error {
	ev := event.Event{Side: src, Kind: event.KindCreate, Path: rel, IsDir: st.IsDir, At: time.Now()}
	return e.propagateContent(ctx, ev, e.abs(src, rel), e.abs(src.Opposite(), rel))
}

func (e *Engine) deleteOneSided(ctx context.Context, rel string, present event.Side, st hash.Stat) error {
	e.log.Info("removendo por política de reconciliação", "path", rel, "side", present.String())
	e.guard.Expect(present, rel, 1)
	if err := e.xfer.Remove(e.abs(present, rel), st.IsDir); err != nil {
		e.guard.Forget(present, rel)
		return err
	}
	return e.db.DeleteSubtree(ctx, rel)
}

// reconcileBothSides trata um path presente nos dois lados.
func (e *Engine) reconcileBothSides(ctx context.Context, rel string, stA, stB hash.Stat) (reconcileResult, error) {
	if stA.IsDir && stB.IsDir {
		return reconcileNoop, nil // diretórios existem dos dois lados; nada a fazer
	}
	if stA.IsDir != stB.IsDir {
		// Um é diretório e o outro é arquivo. É conflito por definição, e
		// nenhuma política automática deveria escolher — sobrescrever
		// apagaria uma árvore inteira.
		e.log.Warn("tipo divergente entre os lados", "path", rel,
			"a_dir", stA.IsDir, "b_dir", stB.IsDir)
		if err := e.db.RecordConflict(ctx, rel, "", ""); err != nil {
			return reconcileConflict, err
		}
		return reconcileConflict, nil
	}

	absA, absB := e.abs(event.SideA, rel), e.abs(event.SideB, rel)
	differs, err := e.contentDiffers(absA, absB)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return reconcileNoop, nil
		}
		return reconcileNoop, err
	}
	if !differs {
		// Conteúdos iguais: basta registrar o estado, sem transferir nada.
		return reconcileNoop, e.recordPair(ctx, rel, absA, absB, false)
	}

	switch e.cfg.FirstSyncPolicy {
	case config.FirstSyncAWins:
		ev := event.Event{Side: event.SideA, Kind: event.KindModify, Path: rel, At: time.Now()}
		return reconcileApplied, e.propagateContent(ctx, ev, absA, absB)

	case config.FirstSyncBWins:
		ev := event.Event{Side: event.SideB, Kind: event.KindModify, Path: rel, At: time.Now()}
		return reconcileApplied, e.propagateContent(ctx, ev, absB, absA)

	default:
		// União com conteúdos divergentes é exatamente o caso da seção 9:
		// não há origem única. Delega para a política de conflito.
		src := event.SideA
		if stB.MTime.After(stA.MTime) {
			src = event.SideB
		}
		ev := event.Event{Side: src, Kind: event.KindModify, Path: rel, At: time.Now()}
		if err := e.resolveConflict(ctx, ev, e.abs(src, rel), e.abs(src.Opposite(), rel)); err != nil {
			return reconcileConflict, err
		}
		return reconcileConflict, nil
	}
}
