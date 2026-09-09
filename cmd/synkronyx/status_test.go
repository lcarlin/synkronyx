package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/hash"
	"github.com/lcarlin/synkronyx/internal/state"
)

func statusConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.A = filepath.Join(dir, "A")
	cfg.B = filepath.Join(dir, "B")
	cfg.StatePath = filepath.Join(dir, "state.db")
	return cfg
}

func TestStatusWithoutStateFileExplainsItself(t *testing.T) {
	cfg := statusConfig(t)
	var buf bytes.Buffer

	if err := printStatus(context.Background(), &buf, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "nunca rodou") {
		t.Errorf("saída não explica a ausência do banco:\n%s", buf.String())
	}
}

func TestStatusReportsCountsAndConflicts(t *testing.T) {
	cfg := statusConfig(t)
	ctx := context.Background()

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put(ctx, state.Entry{Path: "f", Side: event.SideA}); err != nil {
		t.Fatal(err)
	}
	if err := db.Put(ctx, state.Entry{Path: "f", Side: event.SideB}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConflict(ctx, "disputado.txt", hash.Digest{}, hash.Digest{}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, state.MetaHeartbeat, time.Now().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, state.MetaWatches, "17"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var buf bytes.Buffer
	if err := printStatus(ctx, &buf, cfg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{"A=1 B=1", "conflitos abertos", "disputado.txt", "17"} {
		if !strings.Contains(out, want) {
			t.Errorf("saída não contém %q:\n%s", want, out)
		}
	}
}

// Heartbeat velho significa que o daemon provavelmente morreu; o relatório
// não pode apresentar dados antigos como se fossem atuais.
func TestStatusFlagsStaleHeartbeat(t *testing.T) {
	cfg := statusConfig(t)
	cfg.HeartbeatInterval = time.Second
	ctx := context.Background()

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, state.MetaHeartbeat,
		time.Now().Add(-time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var buf bytes.Buffer
	if err := printStatus(ctx, &buf, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "vencido") {
		t.Errorf("heartbeat velho não foi sinalizado:\n%s", buf.String())
	}
}

// O aviso de estado perdido só deve sair quando há dados a ressuscitar: banco
// vazio, first sync já rodado e árvores com conteúdo.
func TestStatusWarnsAboutLostState(t *testing.T) {
	cfg := statusConfig(t)
	ctx := context.Background()

	if err := os.MkdirAll(cfg.A, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.A, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, state.MetaFirstSync, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var buf bytes.Buffer
	if err := printStatus(ctx, &buf, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "não tem entradas") {
		t.Errorf("estado perdido não foi sinalizado:\n%s", buf.String())
	}
}

// Estado vazio com árvores vazias é o estado correto, não um alerta: avisar
// aqui seria ruído em toda instalação nova.
func TestStatusStaysQuietWhenTreesAreEmpty(t *testing.T) {
	cfg := statusConfig(t)
	ctx := context.Background()
	for _, root := range []string{cfg.A, cfg.B} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, state.MetaFirstSync, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var buf bytes.Buffer
	if err := printStatus(ctx, &buf, cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "não tem entradas") {
		t.Errorf("aviso desnecessário com árvores vazias:\n%s", buf.String())
	}
}

func TestStatusIsStableAcrossRuns(t *testing.T) {
	cfg := statusConfig(t)
	ctx := context.Background()

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{state.StatusSynced, state.StatusError, state.StatusPending} {
		if err := db.Put(ctx, state.Entry{Path: "p" + s, Side: event.SideA, Status: s}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	// Ordem fixa: um relatório que embaralha campos entre execuções não serve
	// para comparar nem para diff.
	var first bytes.Buffer
	if err := printStatus(ctx, &first, cfg); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		var again bytes.Buffer
		if err := printStatus(ctx, &again, cfg); err != nil {
			t.Fatal(err)
		}
		if again.String() != first.String() {
			t.Fatalf("saída instável:\n%s\n---\n%s", first.String(), again.String())
		}
	}
}
