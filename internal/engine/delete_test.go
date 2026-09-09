package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/state"
)

// newIdleEngine monta um engine sem watchers nem loop, para exercitar as
// operações diretamente.
func newIdleEngine(t *testing.T, tune func(*config.Config)) (*Engine, config.Config, *state.DB) {
	t.Helper()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.A = filepath.Join(dir, "A")
	cfg.B = filepath.Join(dir, "B")
	cfg.StatePath = filepath.Join(dir, "state.db")
	if tune != nil {
		tune(&cfg)
	}
	for _, root := range []string{cfg.A, cfg.B} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	return New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil))), cfg, db
}

// TestRemoveDirPreservesUnsyncedFiles cobre o item Fail Safe: propagar a
// remoção de um diretório não pode destruir o que só existe no destino.
func TestRemoveDirPreservesUnsyncedFiles(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, nil)
	ctx := context.Background()

	dirRel := "docs"
	dirAbs := filepath.Join(cfg.B, dirRel)
	if err := os.MkdirAll(dirAbs, 0o755); err != nil {
		t.Fatal(err)
	}

	syncedRel := filepath.Join(dirRel, "sincronizado.txt")
	unknownRel := filepath.Join(dirRel, "so-em-b.txt")
	for _, rel := range []string{syncedRel, unknownRel} {
		if err := os.WriteFile(filepath.Join(cfg.B, rel), []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Só um dos dois consta do estado; o outro nasceu em B e nunca foi
	// propagado, então não existe em A para ser recuperado de lá.
	entry, err := eng.entryFor(event.SideB, syncedRel, filepath.Join(cfg.B, syncedRel), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}

	if err := eng.removeDir(ctx, event.SideB, dirRel, dirAbs); err != nil {
		t.Fatalf("removeDir: %v", err)
	}

	if _, err := os.Stat(dirAbs); !os.IsNotExist(err) {
		t.Error("o diretório deveria ter sido removido")
	}

	preserved := findPreserved(t, cfg.B)
	if preserved == "" {
		t.Fatal("nenhum diretório de resguardo foi criado")
	}
	if _, err := os.Stat(filepath.Join(preserved, "so-em-b.txt")); err != nil {
		t.Errorf("arquivo não sincronizado não foi preservado: %v", err)
	}
	if _, err := os.Stat(filepath.Join(preserved, "sincronizado.txt")); !os.IsNotExist(err) {
		t.Error("arquivo já sincronizado não precisava ser preservado")
	}

	// O caso precisa ficar registrado, senão o operador não descobre.
	conflicts, err := db.UnresolvedConflicts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Errorf("conflito deveria constar como resolvido, obtive %d abertos", len(conflicts))
	}
}

// Com todos os arquivos já sincronizados não há nada a preservar, e o
// diretório deve sair inteiro, sem resíduo.
func TestRemoveDirWithEverythingSyncedLeavesNothingBehind(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, nil)
	ctx := context.Background()

	dirRel := "docs"
	dirAbs := filepath.Join(cfg.B, dirRel)
	if err := os.MkdirAll(dirAbs, 0o755); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(dirRel, "a.txt")
	if err := os.WriteFile(filepath.Join(cfg.B, rel), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := eng.entryFor(event.SideB, rel, filepath.Join(cfg.B, rel), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}

	if err := eng.removeDir(ctx, event.SideB, dirRel, dirAbs); err != nil {
		t.Fatal(err)
	}
	if p := findPreserved(t, cfg.B); p != "" {
		t.Errorf("nada precisava ser preservado, mas surgiu %s", p)
	}
}

// Um arquivo que consta do estado mas foi alterado depois também nunca chegou
// ao outro lado, e portanto também precisa ser preservado.
func TestRemoveDirPreservesLocallyModifiedFiles(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, nil)
	ctx := context.Background()

	dirRel := "docs"
	dirAbs := filepath.Join(cfg.B, dirRel)
	if err := os.MkdirAll(dirAbs, 0o755); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(dirRel, "editado.txt")
	abs := filepath.Join(cfg.B, rel)
	if err := os.WriteFile(abs, []byte("versao sincronizada"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := eng.entryFor(event.SideB, rel, abs, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}

	// Alteração local posterior ao registro de estado.
	if err := os.WriteFile(abs, []byte("versao editada depois, mais longa"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := eng.removeDir(ctx, event.SideB, dirRel, dirAbs); err != nil {
		t.Fatal(err)
	}
	preserved := findPreserved(t, cfg.B)
	if preserved == "" {
		t.Fatal("alteração local não preservada")
	}
	got, err := os.ReadFile(filepath.Join(preserved, "editado.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "versao editada depois, mais longa" {
		t.Errorf("conteúdo preservado = %q", got)
	}
}

// Com a política force, o diretório sai inteiro sem inspeção.
func TestRemoveDirForcePolicySkipsInspection(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, func(c *config.Config) {
		c.DirDeletePolicy = config.DirDeleteForce
	})

	dirAbs := filepath.Join(cfg.B, "docs")
	if err := os.MkdirAll(dirAbs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirAbs, "desconhecido.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := eng.removeDir(context.Background(), event.SideB, "docs", dirAbs); err != nil {
		t.Fatal(err)
	}
	if p := findPreserved(t, cfg.B); p != "" {
		t.Errorf("política force não deveria preservar nada, mas surgiu %s", p)
	}
}

// findPreserved localiza o diretório de resguardo, se existir.
func findPreserved(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.Contains(e.Name(), ".sync-conflict-") {
			return filepath.Join(root, e.Name())
		}
	}
	return ""
}
