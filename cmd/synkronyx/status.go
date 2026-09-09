package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/state"
)

// maxConflictsListed limita a listagem de conflitos no relatório; o número
// total continua sendo reportado.
const maxConflictsListed = 15

// staleAfter é a partir de quanto tempo sem heartbeat o daemon é reportado
// como possivelmente morto. Três vezes o intervalo de heartbeat dá margem
// para um scan longo sem gerar alarme falso.
const staleFactor = 3

// printStatus reporta o estado operacional lendo o próprio banco.
//
// Não há IPC com o daemon: ele publica um heartbeat na tabela meta e este
// comando lê de lá. É a opção KISS da seção 16 — o banco já existe, e um
// socket de controle acrescentaria superfície e um protocolo para manter.
//
// A consequência a ter em mente é que o relatório é do último heartbeat, não
// do instante exato da consulta; daí o aviso de heartbeat vencido.
func printStatus(ctx context.Context, w io.Writer, cfg config.Config) error {
	if _, err := os.Stat(cfg.StatePath); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(w, "synkronyx: sem banco de estado em %s — o serviço nunca rodou com esta configuração\n",
				cfg.StatePath)
			return nil
		}
		return err
	}

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		return err
	}
	defer db.Close()

	sum, err := db.Summarize(ctx)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "raiz A:\t%s\n", cfg.A)
	fmt.Fprintf(tw, "raiz B:\t%s\n", cfg.B)
	fmt.Fprintf(tw, "estado:\t%s (schema v%d)\n", cfg.StatePath, sum.SchemaVer)
	fmt.Fprintf(tw, "processo:\t%s\n", describeProcess(sum, cfg.HeartbeatInterval))
	fmt.Fprintf(tw, "watches:\t%s\n", orDash(sum.Watches))
	fmt.Fprintf(tw, "fila de retry:\t%s\n", orDash(sum.RetryQueue))
	fmt.Fprintf(tw, "first sync:\t%s\n", orDash(sum.FirstSync))
	fmt.Fprintf(tw, "último resync:\t%s\n", orDash(sum.LastResync))
	fmt.Fprintf(tw, "entradas:\tA=%d B=%d\n", sum.EntriesA, sum.EntriesB)

	if len(sum.ByStatus) > 0 {
		fmt.Fprintf(tw, "por status:\t%s\n", formatStatuses(sum.ByStatus))
	}
	fmt.Fprintf(tw, "conflitos abertos:\t%d\n", sum.Conflicts)
	tw.Flush()

	// O aviso só faz sentido se houver dados no disco para serem
	// ressuscitados: com as árvores vazias, estado vazio é o estado correto.
	if sum.StateIsEmpty && sum.FirstSync != "" && anyRootHasContent(cfg) {
		fmt.Fprintln(w, "\naviso: o banco de estado não tem entradas, mas o first sync já rodou antes")
		fmt.Fprintln(w, "e as raízes têm conteúdo. A próxima reconciliação vai desfazer remoções")
		fmt.Fprintln(w, "feitas com o serviço parado — sem histórico não há como distingui-las")
		fmt.Fprintln(w, "de arquivos novos.")
	}

	if sum.Conflicts == 0 {
		return nil
	}

	conflicts, err := db.UnresolvedConflicts(ctx, maxConflictsListed)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "\nconflitos abertos:")
	for _, c := range conflicts {
		fmt.Fprintf(w, "  %s  (detectado em %s)\n",
			c.Path, c.DetectedAt.Format(time.RFC3339))
	}
	if sum.Conflicts > len(conflicts) {
		fmt.Fprintf(w, "  ... e %d outros\n", sum.Conflicts-len(conflicts))
	}
	return nil
}

// describeProcess interpreta o heartbeat: rodando, vencido ou nunca visto.
func describeProcess(sum state.Summary, interval time.Duration) string {
	if sum.Heartbeat == "" {
		return "sem heartbeat registrado"
	}
	at, err := time.Parse(time.RFC3339, sum.Heartbeat)
	if err != nil {
		return "heartbeat ilegível: " + sum.Heartbeat
	}

	age := time.Since(at).Round(time.Second)
	desc := fmt.Sprintf("último heartbeat há %s", age)
	if sum.PID != "" {
		desc += " (pid " + sum.PID + ")"
	}
	if age > time.Duration(staleFactor)*interval {
		desc += " — vencido; o daemon provavelmente não está rodando"
	}
	return desc
}

// anyRootHasContent informa se alguma das raízes tem algo dentro. É um
// readdir de uma entrada, não uma varredura.
func anyRootHasContent(cfg config.Config) bool {
	for _, root := range []string{cfg.A, cfg.B} {
		f, err := os.Open(root)
		if err != nil {
			continue
		}
		names, err := f.Readdirnames(1)
		f.Close()
		if err == nil && len(names) > 0 {
			return true
		}
	}
	return false
}

func formatStatuses(byStatus map[string]int) string {
	// Ordem fixa: um relatório que muda de layout entre execuções é difícil
	// de comparar e impossível de usar em diff.
	order := []string{state.StatusSynced, state.StatusPending, state.StatusConflict, state.StatusError}
	var parts []string
	for _, s := range order {
		if n, ok := byStatus[s]; ok {
			parts = append(parts, s+"="+strconv.Itoa(n))
		}
	}
	for s, n := range byStatus {
		if !contains(order, s) {
			parts = append(parts, s+"="+strconv.Itoa(n))
		}
	}
	return strings.Join(parts, " ")
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
