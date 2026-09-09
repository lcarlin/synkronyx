package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/engine"
	"github.com/lcarlin/synkronyx/internal/hash"
	"github.com/lcarlin/synkronyx/internal/state"
)

// daemonAliveFactor define quantos intervalos de heartbeat sem notícia bastam
// para considerar o daemon parado.
const daemonAliveFactor = 3

// printConflicts lista os conflitos abertos com contexto suficiente para
// decidir qual versão manter.
//
// Sem isso, "há 3 conflitos" no -status é informação sem ação possível: o
// operador precisa ver o que difere entre os dois lados.
func printConflicts(ctx context.Context, w io.Writer, cfg config.Config) error {
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		return err
	}
	defer db.Close()

	details, err := db.ConflictDetails(ctx, 0)
	if err != nil {
		return err
	}
	if len(details) == 0 {
		fmt.Fprintln(w, "nenhum conflito aberto")
		return nil
	}

	for i, d := range details {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\n", d.Path)
		fmt.Fprintf(w, "  detectado em %s\n", d.DetectedAt.Format(time.RFC3339))
		if d.Want != "" {
			fmt.Fprintf(w, "  resolução pendente: %s (será aplicada pelo daemon)\n", d.Want)
		}

		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  lado\tdisco\testado")
		fmt.Fprintf(tw, "  A\t%s\t%s\n", describeOnDisk(cfg.A, d.Path), describeEntry(d.EntryA))
		fmt.Fprintf(tw, "  B\t%s\t%s\n", describeOnDisk(cfg.B, d.Path), describeEntry(d.EntryB))
		tw.Flush()
	}

	fmt.Fprintf(w, "\nresolver com:\n")
	fmt.Fprintf(w, "  synkronyx -resolve <path> -with a      # versão de A vence; a de B fica preservada localmente\n")
	fmt.Fprintf(w, "  synkronyx -resolve <path> -with b      # o simétrico\n")
	fmt.Fprintf(w, "  synkronyx -resolve <path> -with both   # A vence o path e a versão de B é mantida, sincronizada\n")
	return nil
}

// describeOnDisk resume o que existe agora no disco, que é a informação que
// de fato decide — o estado gravado pode estar desatualizado.
func describeOnDisk(root, rel string) string {
	abs := root + string(os.PathSeparator) + rel
	st, err := hash.StatOf(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "ausente"
		}
		return "erro: " + err.Error()
	}
	if st.IsDir {
		return "diretório"
	}
	return fmt.Sprintf("%s, %s, %s", st.Kind, humanBytes(st.Size),
		st.MTime.Format("2006-01-02 15:04:05"))
}

func describeEntry(e *state.Entry) string {
	if e == nil {
		return "nunca sincronizado"
	}
	return fmt.Sprintf("%s, sync em %s", e.Digest.Short(),
		e.SyncedAt.Format("2006-01-02 15:04:05"))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// resolveConflictCmd registra ou aplica a resolução de um conflito.
//
// Com o daemon rodando, o pedido é gravado no banco e aplicado por ele. Isso
// não é preciosismo: resolver escrevendo direto nas árvores faria o daemon ver
// as escritas como alteração externa e propagá-las de volta, desfazendo
// exatamente o que se acabou de decidir. Só quem tem o guard em mãos pode
// aplicar com segurança.
//
// Com o daemon parado, não há ninguém observando e o CLI aplica na hora.
func resolveConflictCmd(ctx context.Context, w io.Writer, cfg config.Config, path, with string) error {
	want, err := engine.ParseResolution(with)
	if err != nil {
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

	if daemonLooksAlive(sum, cfg.HeartbeatInterval) {
		if err := db.RequestResolution(ctx, path, string(want)); err != nil {
			return err
		}
		fmt.Fprintf(w, "pedido registrado: %s -> %s\n", path, want)
		fmt.Fprintf(w, "o daemon aplica em até %s; acompanhe com -conflicts\n",
			cfg.HeartbeatInterval)
		return nil
	}

	log := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
	eng := engine.New(cfg, db, log)

	desc, err := eng.ApplyResolution(ctx, path, want)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, desc)
	return nil
}

// daemonLooksAlive decide pelo heartbeat se há um daemon rodando.
func daemonLooksAlive(sum state.Summary, interval time.Duration) bool {
	if sum.Heartbeat == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, sum.Heartbeat)
	if err != nil {
		return false
	}
	return time.Since(at) <= time.Duration(daemonAliveFactor)*interval
}
