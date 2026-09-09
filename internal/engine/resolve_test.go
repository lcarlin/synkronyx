package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/hash"
)

// setupConflict deixa os dois lados com conteúdos diferentes no mesmo path e
// registra o conflito, como o engine faria.
func setupConflict(t *testing.T, eng *Engine, cfg config.Config, rel, a, b string) {
	t.Helper()
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(cfg.A, rel), []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.B, rel), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eng.db.RecordConflict(ctx, rel, hash.Digest{}, hash.Digest{}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveWithAOverwritesBAndPreservesLocally(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, nil)
	ctx := context.Background()
	setupConflict(t, eng, cfg, "disputado.txt", "versão de A", "versão de B")

	desc, err := eng.ApplyResolution(ctx, "disputado.txt", ResolveA)
	if err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}
	if !strings.Contains(desc, "A") {
		t.Errorf("descrição = %q, quero menção ao lado vencedor", desc)
	}

	got, err := os.ReadFile(filepath.Join(cfg.B, "disputado.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "versão de A" {
		t.Errorf("B = %q, quero a versão de A", got)
	}

	// A versão perdedora fica com nome que o exclude ignora: recuperável, mas
	// fora do fluxo de sincronização.
	preserved := findByFragment(t, cfg.B, ".sync-conflict-")
	if preserved == "" {
		t.Fatal("versão de B não foi preservada")
	}
	kept, err := os.ReadFile(preserved)
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != "versão de B" {
		t.Errorf("preservado = %q, quero a versão de B", kept)
	}

	if excl := config.NewExcluder(cfg.Exclude); !excl.Excluded(filepath.Base(preserved), false) {
		t.Error("o nome preservado deveria ser ignorado pelo exclude")
	}

	open, err := db.UnresolvedConflicts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("conflito continua aberto após resolução: %v", open)
	}
}

func TestResolveWithBIsSymmetric(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	setupConflict(t, eng, cfg, "disputado.txt", "versão de A", "versão de B")

	if _, err := eng.ApplyResolution(context.Background(), "disputado.txt", ResolveB); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(cfg.A, "disputado.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "versão de B" {
		t.Errorf("A = %q, quero a versão de B", got)
	}
	if findByFragment(t, cfg.A, ".sync-conflict-") == "" {
		t.Error("versão de A não foi preservada")
	}
}

// "both" difere de "a" num ponto só, e é o ponto que importa: o nome da
// versão preservada é sincronizável, então ela passa a existir nos dois lados.
func TestResolveBothKeepsLoserUnderSyncableName(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	setupConflict(t, eng, cfg, "disputado.txt", "versão de A", "versão de B")

	if _, err := eng.ApplyResolution(context.Background(), "disputado.txt", ResolveBoth); err != nil {
		t.Fatal(err)
	}

	kept := findByFragment(t, cfg.B, keptSuffix)
	if kept == "" {
		t.Fatal("versão de B não foi preservada com nome sincronizável")
	}
	if excl := config.NewExcluder(cfg.Exclude); excl.Excluded(filepath.Base(kept), false) {
		t.Error("o nome preservado em \"both\" precisa ser sincronizável, e o exclude o ignora")
	}

	got, err := os.ReadFile(filepath.Join(cfg.B, "disputado.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "versão de A" {
		t.Errorf("path original = %q, quero a versão de A", got)
	}
}

// Conflito de tipo — arquivo de um lado, diretório do outro — é o caso que
// nenhuma política automática resolve, e o motivo de existir resolução
// assistida.
func TestResolveTypeConflictFileWinsOverDirectory(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "ambiguo"
	if err := os.WriteFile(filepath.Join(cfg.A, rel), []byte("sou um arquivo"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.B, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dentro.txt"), []byte("conteúdo do diretório"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eng.db.RecordConflict(ctx, rel, hash.Digest{}, hash.Digest{}); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ApplyResolution(ctx, rel, ResolveA); err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}

	fi, err := os.Lstat(filepath.Join(cfg.B, rel))
	if err != nil {
		t.Fatal(err)
	}
	if fi.IsDir() {
		t.Error("o path deveria ter virado arquivo, seguindo o lado vencedor")
	}

	// O diretório inteiro precisa ter sobrevivido em algum lugar.
	preserved := findByFragment(t, cfg.B, ".sync-conflict-")
	if preserved == "" {
		t.Fatal("o diretório perdedor não foi preservado")
	}
	kept, err := os.ReadFile(filepath.Join(preserved, "dentro.txt"))
	if err != nil {
		t.Fatalf("conteúdo do diretório preservado sumiu: %v", err)
	}
	if string(kept) != "conteúdo do diretório" {
		t.Errorf("preservado = %q", kept)
	}
}

func TestResolveFailsWhenWinnerMissing(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)

	if err := os.WriteFile(filepath.Join(cfg.B, "so-em-b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ApplyResolution(context.Background(), "so-em-b.txt", ResolveA); err == nil {
		t.Error("resolver por um lado que não tem o arquivo deveria falhar")
	}
}

func TestParseResolution(t *testing.T) {
	for _, s := range []string{"a", "b", "both"} {
		if _, err := ParseResolution(s); err != nil {
			t.Errorf("ParseResolution(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "A", "ambos", "keep"} {
		if _, err := ParseResolution(s); err == nil {
			t.Errorf("ParseResolution(%q) aceitou entrada inválida", s)
		}
	}
}

// O daemon consome os pedidos gravados pelo CLI.
func TestApplyPendingResolutionsConsumesQueue(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, nil)
	ctx := context.Background()
	setupConflict(t, eng, cfg, "disputado.txt", "versão de A", "versão de B")

	if err := db.RequestResolution(ctx, "disputado.txt", "a"); err != nil {
		t.Fatal(err)
	}
	eng.applyPendingResolutions(ctx)

	pending, err := db.PendingResolutions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("pedidos pendentes = %d, quero 0", len(pending))
	}
	got, err := os.ReadFile(filepath.Join(cfg.B, "disputado.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "versão de A" {
		t.Errorf("B = %q, quero a versão de A", got)
	}
}

// Um pedido que falha precisa registrar o motivo, não sumir da fila em
// silêncio nem ficar preso repetindo para sempre.
func TestApplyPendingResolutionsRecordsFailure(t *testing.T) {
	eng, _, db := newIdleEngine(t, nil)
	ctx := context.Background()

	if err := db.RequestResolution(ctx, "nao-existe.txt", "a"); err != nil {
		t.Fatal(err)
	}
	eng.applyPendingResolutions(ctx)

	pending, err := db.PendingResolutions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("pedido inaplicável deveria sair da fila, restaram %d", len(pending))
	}
}

func TestKeptNameCarriesTimestampAndSide(t *testing.T) {
	name := "f.txt" + keptSuffix + "B-" + time.Now().Format(conflictTimeLayout)
	if !strings.Contains(name, "sync-kept-B") {
		t.Errorf("nome = %q", name)
	}
}

// findByFragment localiza a primeira entrada cujo nome contém o fragmento.
func findByFragment(t *testing.T, root, fragment string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), fragment) {
			return filepath.Join(root, e.Name())
		}
	}
	return ""
}

// TestResolveWorksUnderManualPolicy é regressão de um caso em que a resolução
// dizia ter funcionado e não tinha: ApplyResolution delegava a cópia a
// propagateContent, que re-executa a detecção de conflito e obedece à
// conflict_policy — e "manual" existe justamente para não tocar em nada.
//
// A decisão explícita do operador precisa passar por cima da política
// automática; é o motivo de a resolução assistida existir.
func TestResolveWorksUnderManualPolicy(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, func(c *config.Config) {
		c.ConflictPolicy = config.ConflictManual
	})
	setupConflict(t, eng, cfg, "disputado.txt", "versão de A", "versão de B")

	if _, err := eng.ApplyResolution(context.Background(), "disputado.txt", ResolveB); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(cfg.A, "disputado.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "versão de B" {
		t.Errorf("A = %q, quero a versão de B — a política manual não pode vetar a decisão do operador", got)
	}
}

// Conteúdos de mesmo tamanho e mesmo mtime são o desfecho comum de um
// conflito, e eram exatamente o caso que o quick check do rsync pulava.
func TestResolveTransfersWhenSizeAndMtimeMatch(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	setupConflict(t, eng, cfg, "d.txt", "AAA", "BBB")

	when := time.Unix(1_700_000_000, 0)
	for _, root := range []string{cfg.A, cfg.B} {
		if err := os.Chtimes(filepath.Join(root, "d.txt"), when, when); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := eng.ApplyResolution(context.Background(), "d.txt", ResolveB); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(cfg.A, "d.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "BBB" {
		t.Errorf("A = %q, quero \"BBB\"", got)
	}
}
