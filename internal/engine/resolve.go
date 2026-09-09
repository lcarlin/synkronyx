package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/hash"
	"github.com/lcarlin/synkronyx/internal/state"
)

// Resolution é a escolha do operador para um conflito.
type Resolution string

const (
	// ResolveA faz a versão de A vencer. A versão de B fica preservada
	// localmente, com nome que o exclude ignora — recuperável, não propagada.
	ResolveA Resolution = "a"
	// ResolveB é o simétrico.
	ResolveB Resolution = "b"
	// ResolveBoth faz a versão de A vencer o path original e preserva a de B
	// com um nome sincronizável, de modo que as duas passem a existir nos
	// dois lados.
	ResolveBoth Resolution = "both"
)

// ParseResolution valida a escolha vinda da linha de comando.
func ParseResolution(s string) (Resolution, error) {
	switch Resolution(s) {
	case ResolveA, ResolveB, ResolveBoth:
		return Resolution(s), nil
	default:
		return "", fmt.Errorf("resolução inválida: %q (use a, b ou both)", s)
	}
}

// keptSuffix nomeia a versão preservada de forma sincronizável.
//
// Diferente de ".sync-conflict-", que o exclude padrão ignora de propósito,
// este nome é sincronizado: é o que faz "both" significar de fato "as duas
// versões, nos dois lados".
const keptSuffix = ".sync-kept-"

// ApplyResolution resolve um conflito e devolve uma descrição do que foi
// feito.
//
// Chamada tanto pelo daemon, ao consumir um pedido pendente, quanto pelo CLI
// quando o daemon não está rodando. A diferença entre os dois casos é só quem
// segura o guard: a lógica é a mesma, e ter uma implementação só evita que as
// duas divirjam.
func (e *Engine) ApplyResolution(ctx context.Context, rel string, want Resolution) (string, error) {
	winner, loser := event.SideA, event.SideB
	if want == ResolveB {
		winner, loser = event.SideB, event.SideA
	}

	winnerAbs := e.abs(winner, rel)
	loserAbs := e.abs(loser, rel)

	winnerStat, err := hash.StatOf(winnerAbs)
	if err != nil {
		return "", fmt.Errorf("lado vencedor %s não tem %s: %w", winner, rel, err)
	}
	if winnerStat.IsSpecial() {
		return "", fmt.Errorf("%s é um %s e não é sincronizável", rel, winnerStat.Kind)
	}

	loserStat, loserErr := hash.StatOf(loserAbs)
	loserExists := loserErr == nil
	if loserErr != nil && !errors.Is(loserErr, os.ErrNotExist) {
		return "", loserErr
	}

	// A expectativa precisa valer ANTES de qualquer escrita e cobrir a
	// subárvore inteira: tanto o perdedor quanto o vencedor podem ser
	// diretórios, e substituir um pelo outro emite um número indeterminado de
	// eventos.
	//
	// Ela deliberadamente NÃO é esquecida ao fim da função. Um eco atrasado
	// pode chegar depois da última escrita, e esquecer antes disso deixaria a
	// resolução ser desfeita. O preço é uma janela de self_write_ttl em que
	// alterações externas neste path específico são ignoradas — aceitável
	// para uma operação rara e explícita como resolver um conflito, e o Full
	// Resync cobre o caso extremo.
	e.guard.ExpectSubtree(loser, rel)

	var preservedRel string
	if loserExists {
		preservedRel, err = e.preserveLoser(ctx, rel, loser, loserStat, want)
		if err != nil {
			return "", err
		}

		// Quando os tipos diferem — arquivo de um lado, diretório do outro —
		// não basta copiar por cima: o path precisa deixar de ser o que era.
		if loserStat.IsDir || loserStat.Kind != winnerStat.Kind {
			if err := e.xfer.Remove(loserAbs, loserStat.IsDir); err != nil {
				return "", fmt.Errorf("liberando o path para a versão vencedora: %w", err)
			}
		}
	}

	// A cópia é feita direto, sem passar por propagateContent. A diferença
	// importa: propagateContent re-executa a detecção de conflito e delega à
	// política configurada — e com conflict_policy: manual essa política se
	// recusa, por definição, a mexer nos arquivos. A decisão do operador seria
	// silenciosamente ignorada, com o log ainda reportando sucesso.
	//
	// Aqui não há o que decidir: alguém já decidiu.
	if winnerStat.IsDir {
		if err := e.xfer.CopyTree(ctx, winnerAbs, loserAbs); err != nil {
			return "", fmt.Errorf("copiando a árvore de %s: %w", winner, err)
		}
	} else if err := e.xfer.CopyFile(ctx, winnerAbs, loserAbs); err != nil {
		return "", fmt.Errorf("copiando a versão de %s: %w", winner, err)
	}

	if err := e.recordPair(ctx, rel, winnerAbs, loserAbs, winnerStat.IsDir); err != nil {
		return "", err
	}
	if winnerStat.IsDir {
		if err := e.recordSubtree(ctx, rel); err != nil {
			return "", err
		}
	}

	resolution := string(want)
	if preservedRel != "" {
		resolution += ":" + preservedRel
	}
	if err := e.db.ResolveConflict(ctx, rel, resolution); err != nil {
		return "", err
	}
	for _, side := range []event.Side{event.SideA, event.SideB} {
		if err := e.db.MarkStatus(ctx, side, rel, state.StatusSynced); err != nil {
			return "", err
		}
	}

	desc := fmt.Sprintf("%s: versão de %s aplicada", rel, winner)
	if preservedRel != "" {
		desc += fmt.Sprintf("; versão de %s preservada em %s", loser, preservedRel)
	}
	return desc, nil
}

// preserveLoser põe a versão perdedora de lado e devolve seu novo path
// relativo.
//
// O nome escolhido decide se ela vai ou não ser sincronizada: em "a"/"b" usa o
// padrão que o exclude ignora, mantendo a cópia local e fora do caminho; em
// "both" usa um nome comum, que o daemon propaga como qualquer outro arquivo.
func (e *Engine) preserveLoser(ctx context.Context, rel string, loser event.Side,
	st hash.Stat, want Resolution) (string, error) {

	now := time.Now()

	var preservedRel string
	switch {
	case want == ResolveBoth:
		preservedRel = fmt.Sprintf("%s%s%s-%s", rel, keptSuffix, loser, now.Format(conflictTimeLayout))
	case st.IsDir:
		preservedRel = preservedDirName(rel, loser, now)
	default:
		preservedRel = conflictName(rel, loser, now)
	}

	e.guard.ExpectSubtree(loser, preservedRel)

	// Copiar, não renomear — pelo mesmo motivo documentado em
	// resolveConflict: um rename deixa para trás um MOVED_FROM órfão no path
	// original, que é lido como remoção e propagado contra o vencedor.
	src, dst := e.abs(loser, rel), e.abs(loser, preservedRel)
	var err error
	if st.IsDir {
		err = e.xfer.CopyTree(ctx, src, dst)
	} else {
		err = e.xfer.CopyFile(ctx, src, dst)
	}
	if err != nil {
		e.guard.ForgetSubtree(loser, preservedRel)
		return "", fmt.Errorf("preservando a versão de %s: %w", loser, err)
	}
	return preservedRel, nil
}

// applyPendingResolutions consome os pedidos gravados pelo CLI.
//
// É chamado no ritmo do heartbeat, e não por sinal: um pedido de resolução não
// tem urgência, e evitar mais um canal de comunicação mantém a superfície do
// serviço pequena.
func (e *Engine) applyPendingResolutions(ctx context.Context) {
	pending, err := e.db.PendingResolutions(ctx)
	if err != nil {
		e.log.Error("falha lendo pedidos de resolução", "err", err)
		return
	}

	for _, req := range pending {
		want, err := ParseResolution(req.Want)
		if err != nil {
			e.log.Error("pedido de resolução inválido", "path", req.Path, "err", err)
			_ = e.db.MarkResolutionApplied(ctx, req.Path, err.Error())
			continue
		}

		desc, err := e.ApplyResolution(ctx, req.Path, want)
		if err != nil {
			e.log.Error("falha resolvendo conflito", "path", req.Path, "want", req.Want, "err", err)
			_ = e.db.MarkResolutionApplied(ctx, req.Path, err.Error())
			continue
		}
		e.log.Info("conflito resolvido", "detalhe", desc, "want", req.Want)
		if err := e.db.MarkResolutionApplied(ctx, req.Path, ""); err != nil {
			e.log.Error("falha marcando resolução aplicada", "path", req.Path, "err", err)
		}
	}
}
