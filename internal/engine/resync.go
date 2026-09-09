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
	"github.com/lcarlin/synkronyx/internal/scan"
	"github.com/lcarlin/synkronyx/internal/state"
)

// firstSync estabelece o estado inicial (seção 10).
//
// Roda depois de os watchers subirem, não antes: eventos ocorridos durante o
// scan ficam enfileirados no debouncer e são aplicados na sequência. A ordem
// inversa deixaria uma janela cega.
func (e *Engine) firstSync(ctx context.Context) error {
	done, err := e.db.Meta(ctx, state.MetaFirstSync)
	if err != nil {
		return err
	}
	empty, err := e.db.IsEmpty(ctx)
	if err != nil {
		return err
	}

	switch {
	case done == "":
		e.log.Info("iniciando first sync", "policy", string(e.cfg.FirstSyncPolicy))
	case empty && e.rootsHaveContent():
		// O First Sync já rodou, o banco não tem entradas e ainda há dados no
		// disco: o arquivo de estado foi apagado, recriado ou perdido junto
		// com o disco. Com as duas árvores vazias não haveria o que
		// ressuscitar, e o aviso seria só ruído.
		e.warnEmptyState(done)
	default:
		// Reconciliar mesmo assim: o serviço pode ter ficado parado enquanto
		// a árvore mudava, e nada disso gerou evento.
		e.log.Info("reconciliando árvores após restart", "first_sync_em", done)
	}

	if err := e.reconcile(ctx, "first-sync", "."); err != nil {
		return err
	}
	return e.db.SetMeta(ctx, state.MetaFirstSync, time.Now().Format(time.RFC3339))
}

// warnEmptyState avisa sobre a consequência de reconciliar sem estado.
//
// Com o banco vazio, `union` não tem como distinguir "arquivo novo neste
// lado" de "arquivo apagado do outro lado": as duas situações têm exatamente
// a mesma aparência no disco. O resultado é que remoções feitas enquanto o
// serviço estava parado são desfeitas — o arquivo volta, copiado do lado onde
// ainda existe.
//
// Isso é comportamento esperado, não defeito: sem histórico, ressuscitar um
// arquivo é o erro recuperável (basta apagar de novo) e apagá-lo é o erro
// irrecuperável. Fail Safe manda escolher o primeiro. O que não é aceitável é
// fazer isso em silêncio, então o aviso é explícito.
func (e *Engine) warnEmptyState(firstSyncAt string) {
	e.log.Warn("banco de estado está vazio, mas o first sync já havia rodado antes — "+
		"remoções feitas com o serviço parado serão desfeitas, porque não há histórico "+
		"para distingui-las de arquivos novos",
		"first_sync_anterior", firstSyncAt,
		"state_path", e.cfg.StatePath,
		"policy", string(e.cfg.FirstSyncPolicy))
}

// rootsHaveContent informa se alguma das raízes tem algo dentro.
func (e *Engine) rootsHaveContent() bool {
	for _, root := range e.roots {
		empty, err := isEmptyDir(root)
		if err != nil {
			// Não conseguir ler a raiz é problema de outra ordem, e vai
			// aparecer no scan logo em seguida; aqui basta não engolir o aviso.
			return true
		}
		if !empty {
			return true
		}
	}
	return false
}

// FullResync reconstrói o estado a partir do disco (seção 11).
//
// Diferença em relação ao First Sync: aqui o estado gravado é tratado como
// suspeito, não como verdade. Só o que está no disco conta.
func (e *Engine) FullResync(ctx context.Context) error {
	start := time.Now()
	if err := e.reconcile(ctx, "full-resync", "."); err != nil {
		return err
	}
	if err := e.db.SetMeta(ctx, state.MetaLastResync, time.Now().Format(time.RFC3339)); err != nil {
		return err
	}
	e.log.Info("full resync concluído", "duracao", time.Since(start).String())
	return nil
}

// reconcileSubtree reconcilia apenas rel e o que está abaixo dele.
//
// É a resposta certa quando uma operação localizada falha de forma que
// deixa a subárvore em estado desconhecido — um rename cuja origem não existe
// no destino, por exemplo. Varrer as duas árvores inteiras resolveria também,
// mas cobrar o preço de um Full Resync por uma divergência de um diretório é
// desproporcional.
func (e *Engine) reconcileSubtree(ctx context.Context, rel string) error {
	return e.reconcile(ctx, "subtree", rel)
}

// reconcile compara as duas árvores sob scope e aplica a política configurada.
// scope "." abrange a árvore inteira.
func (e *Engine) reconcile(ctx context.Context, phase, scope string) error {
	excluder := config.NewExcluder(e.cfg.Exclude)

	invA, err := scan.WalkSubtree(ctx, e.cfg.A, scope, excluder)
	if err != nil {
		return fmt.Errorf("scan de A: %w", err)
	}
	invB, err := scan.WalkSubtree(ctx, e.cfg.B, scope, excluder)
	if err != nil {
		return fmt.Errorf("scan de B: %w", err)
	}
	e.log.Info("scan concluído", "phase", phase, "scope", scope,
		"entradas_a", len(invA), "entradas_b", len(invB))

	var applied, skipped, conflicts, failed int

	// Diretórios já resolvidos por inteiro (copiados ou removidos como
	// árvore). Seus descendentes precisam ser saltados, e não por economia:
	// o inventário foi tirado antes da operação, então para um filho ele
	// ainda diz "existe só de um lado" — enquanto o estado, já atualizado
	// pela cópia da árvore, diz que o path existe do outro lado. Processar
	// nessas condições faria reconcileOneSided ler a situação como "foi
	// apagado lá" e remover o arquivo que acabou de ser sincronizado.
	//
	// Union ordena por profundidade, então o diretório sempre aparece antes
	// do seu conteúdo e a lista está completa quando os filhos chegam.
	var handled []string

	for _, rel := range scan.Union(invA, invB) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if underAny(rel, handled) {
			skipped++
			continue
		}

		stA, inA := invA[rel]
		stB, inB := invB[rel]

		var res reconcileResult
		var err error
		switch {
		case inA && !inB:
			err = e.reconcileOneSided(ctx, rel, event.SideA, stA)
			if stA.IsDir {
				handled = append(handled, rel)
			}
		case inB && !inA:
			err = e.reconcileOneSided(ctx, rel, event.SideB, stB)
			if stB.IsDir {
				handled = append(handled, rel)
			}
		case inA && inB:
			res, err = e.reconcileBothSides(ctx, rel, stA, stB)
		}

		if err != nil {
			// Falhar em um path não invalida a reconciliação dos outros; o
			// retry e o próximo resync cobrem o que não passou agora.
			e.log.Error("falha reconciliando", "path", rel, "err", err)
			failed++
			continue
		}
		switch res {
		case reconcileNoop:
			skipped++
		case reconcileConflict:
			conflicts++
		default:
			applied++
		}
	}

	e.log.Info("reconciliação concluída", "phase", phase, "scope", scope,
		"aplicadas", applied, "sem_acao", skipped, "conflitos", conflicts, "falhas", failed)
	return nil
}

// underAny informa se rel está sob algum dos prefixos dados.
func underAny(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		if rel == p {
			continue
		}
		if strings.HasPrefix(rel, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
// anterior não há como distinguir — daí FirstSyncPolicy, e daí o aviso de
// warnEmptyState quando o estado se perdeu.
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

	if st.IsDir {
		e.guard.ExpectSubtree(present, rel)
		defer e.guard.ForgetSubtree(present, rel)
		if err := e.removeDir(ctx, present, rel, e.abs(present, rel)); err != nil {
			return err
		}
		return e.db.DeleteSubtree(ctx, rel)
	}

	e.guard.Expect(present, rel, 1)
	if err := e.xfer.Remove(e.abs(present, rel), false); err != nil {
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
		if err := e.db.RecordConflict(ctx, rel, hash.Digest{}, hash.Digest{}); err != nil {
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
