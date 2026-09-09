package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/state"
)

// Teste de integração do engine: dois diretórios reais, inotify real e rsync
// real. O que se está validando não é uma unidade isolada, e sim a
// propriedade central do sistema — propagar nos dois sentidos sem entrar em
// loop —, que só emerge da combinação das peças.

type harness struct {
	A, B string
	db   *state.DB
	eng  *Engine
}

func newHarness(t *testing.T, tune func(*config.Config)) *harness {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync não disponível")
	}

	dir := t.TempDir()
	cfg := config.Default()
	cfg.A = filepath.Join(dir, "A")
	cfg.B = filepath.Join(dir, "B")
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.Debounce = 50 * time.Millisecond
	if tune != nil {
		tune(&cfg)
	}
	for _, root := range []string{cfg.A, cfg.B} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config inválida: %v", err)
	}

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	h := &harness{A: cfg.A, B: cfg.B, db: db, eng: New(cfg, db, log)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.eng.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine não encerrou em 5s")
		}
	})

	// Dá tempo de os watchers subirem e o first sync concluir.
	time.Sleep(300 * time.Millisecond)
	return h
}

// waitContent espera até que abs tenha exatamente o conteúdo esperado.
func waitContent(t *testing.T, abs, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(abs)
		if err == nil {
			last = string(b)
			if last == want {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s: conteúdo = %q, quero %q", abs, last, want)
}

func waitGone(t *testing.T, abs string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(abs); os.IsNotExist(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s ainda existe; deveria ter sido removido", abs)
}

func TestPropagatesBothDirections(t *testing.T) {
	h := newHarness(t, nil)

	if err := os.WriteFile(filepath.Join(h.A, "de-a.txt"), []byte("vindo de A"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitContent(t, filepath.Join(h.B, "de-a.txt"), "vindo de A")

	if err := os.WriteFile(filepath.Join(h.B, "de-b.txt"), []byte("vindo de B"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitContent(t, filepath.Join(h.A, "de-b.txt"), "vindo de B")
}

// TestNoSyncLoop é o teste do requisito da seção 6. Uma escrita em A deve
// produzir exatamente uma escrita em B — e parar aí.
func TestNoSyncLoop(t *testing.T) {
	h := newHarness(t, nil)

	src := filepath.Join(h.A, "loop.txt")
	dst := filepath.Join(h.B, "loop.txt")
	if err := os.WriteFile(src, []byte("uma vez"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitContent(t, dst, "uma vez")

	// Depois de estabilizar, nada mais pode se mexer sozinho.
	time.Sleep(time.Second)
	stA, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	stB, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)

	afterA, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	afterB, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !afterA.ModTime().Equal(stA.ModTime()) {
		t.Errorf("A foi reescrito sozinho: %v -> %v", stA.ModTime(), afterA.ModTime())
	}
	if !afterB.ModTime().Equal(stB.ModTime()) {
		t.Errorf("B foi reescrito sozinho: %v -> %v", stB.ModTime(), afterB.ModTime())
	}

	// Um loop se manifestaria como conflitos e arquivos preservados.
	assertNoConflictFiles(t, h.A)
	assertNoConflictFiles(t, h.B)
}

func TestPropagatesDelete(t *testing.T) {
	h := newHarness(t, nil)

	if err := os.WriteFile(filepath.Join(h.A, "efemero.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitContent(t, filepath.Join(h.B, "efemero.txt"), "x")

	if err := os.Remove(filepath.Join(h.A, "efemero.txt")); err != nil {
		t.Fatal(err)
	}
	waitGone(t, filepath.Join(h.B, "efemero.txt"))
}

func TestPropagatesRename(t *testing.T) {
	h := newHarness(t, nil)

	if err := os.WriteFile(filepath.Join(h.A, "antes.txt"), []byte("conteúdo"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitContent(t, filepath.Join(h.B, "antes.txt"), "conteúdo")

	if err := os.Rename(filepath.Join(h.A, "antes.txt"), filepath.Join(h.A, "depois.txt")); err != nil {
		t.Fatal(err)
	}
	waitContent(t, filepath.Join(h.B, "depois.txt"), "conteúdo")
	waitGone(t, filepath.Join(h.B, "antes.txt"))
}

func TestPropagatesNestedDirectories(t *testing.T) {
	h := newHarness(t, nil)

	deep := filepath.Join(h.A, "um", "dois", "tres")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "fundo.txt"), []byte("fundo"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitContent(t, filepath.Join(h.B, "um", "dois", "tres", "fundo.txt"), "fundo")
}

// TestFirstSyncReconcilesPreexistingTrees cobre a seção 10: o que já existia
// antes de o serviço subir precisa ser reconciliado, sem depender de evento.
func TestFirstSyncReconcilesPreexistingTrees(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync não disponível")
	}

	dir := t.TempDir()
	cfg := config.Default()
	cfg.A = filepath.Join(dir, "A")
	cfg.B = filepath.Join(dir, "B")
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.Debounce = 50 * time.Millisecond

	if err := os.MkdirAll(filepath.Join(cfg.A, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.B, 0o755); err != nil {
		t.Fatal(err)
	}
	// Estes arquivos existem antes de qualquer watcher: só o scan os vê.
	if err := os.WriteFile(filepath.Join(cfg.A, "sub", "so-em-a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.B, "so-em-b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	eng := New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()

	waitContent(t, filepath.Join(cfg.B, "sub", "so-em-a.txt"), "A")
	waitContent(t, filepath.Join(cfg.A, "so-em-b.txt"), "B")

	// O estado precisa refletir a reconciliação, senão o próximo restart
	// refaria tudo do zero. A gravação acontece logo depois de o arquivo
	// aparecer, então vale esperar por ela em vez de ler uma vez só.
	entry := waitEntry(t, db, event.SideB, filepath.Join("sub", "so-em-a.txt"))
	if entry.SHA256 == "" {
		t.Error("entrada gravada sem hash")
	}
	if entry.Status != state.StatusSynced {
		t.Errorf("status = %q, quero %q", entry.Status, state.StatusSynced)
	}
}

// waitEntry espera a entrada de estado de (side, path) aparecer.
func waitEntry(t *testing.T, db *state.DB, side event.Side, path string) state.Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e, err := db.Get(context.Background(), side, path)
		if err != nil {
			t.Fatal(err)
		}
		if e != nil {
			return *e
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("estado de %s/%s nunca foi gravado", side, path)
	return state.Entry{}
}

func assertNoConflictFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(d.Name(), ".sync-conflict-") {
			t.Errorf("arquivo de conflito inesperado: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
