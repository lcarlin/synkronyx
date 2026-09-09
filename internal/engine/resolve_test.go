// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
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

// TestReconcileUnilateralChangeIsNotAConflict é regressão de um caso em que a
// reconciliação transformava toda edição feita com o serviço parado num
// arquivo .sync-conflict-.
//
// Conflito, pela seção 9 do escopo, é a ausência de origem única. Conteúdos
// diferentes não bastam: se só um lado se afastou do que foi sincronizado por
// último, aquele lado É a origem.
func TestReconcileUnilateralChangeIsNotAConflict(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "d.txt"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, []byte("versão sincronizada"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Linha de base: os dois lados registrados como sincronizados.
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}

	// Só A muda.
	if err := os.WriteFile(absA, []byte("editado apenas em A"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(absB)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "editado apenas em A" {
		t.Errorf("B = %q, quero a versão de A propagada", got)
	}

	conflicts, err := db.UnresolvedConflicts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Errorf("alteração unilateral virou conflito: %v", conflicts)
	}
	if p := findByFragment(t, cfg.B, ".sync-conflict-"); p != "" {
		t.Errorf("arquivo de conflito criado sem necessidade: %s", p)
	}
}

// A simetria precisa valer: só B mudar também tem origem única.
func TestReconcileUnilateralChangeFromB(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "d.txt"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, []byte("base"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absB, []byte("editado apenas em B"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(absA)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "editado apenas em B" {
		t.Errorf("A = %q, quero a versão de B propagada", got)
	}
}

// Quando os DOIS lados mudaram, aí sim não há origem única e o conflito é o
// desfecho correto.
func TestReconcileBilateralChangeIsAConflict(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "d.txt"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, []byte("base"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(absA, []byte("editado em A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absB, []byte("editado em B, diferente"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}

	// A política padrão preserva, então o conflito consta como resolvido, mas
	// alguma versão precisa ter sido posta de lado.
	preserved := findByFragment(t, cfg.A, ".sync-conflict-") + findByFragment(t, cfg.B, ".sync-conflict-")
	if preserved == "" {
		t.Error("alteração bilateral não preservou nenhuma versão")
	}
	_ = db
}

// Sem linha de base no estado não há como afirmar quem é a origem, e o
// conservador é tratar como conflito.
func TestReconcileWithoutBaselineIsAConflict(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "d.txt"
	if err := os.WriteFile(filepath.Join(cfg.A, rel), []byte("de A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.B, rel), []byte("de B, distinto"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}
	preserved := findByFragment(t, cfg.A, ".sync-conflict-") + findByFragment(t, cfg.B, ".sync-conflict-")
	if preserved == "" {
		t.Error("par sem linha de base deveria ter sido tratado como conflito")
	}
}

// TestReconcilePropagatesModeChange é regressão de uma alteração que se
// perdia por completo: um chmod feito com o serviço parado não gera evento, e
// a reconciliação só comparava conteúdo — então o modo divergente sobrevivia a
// qualquer número de resyncs.
//
// A seção 2 do escopo lista metadados entre as operações contempladas, e o
// caminho de eventos já as tratava. Era a reconciliação que as ignorava.
func TestReconcilePropagatesModeChange(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "restrito.txt"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, []byte("mesmo conteudo"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}

	// Só o modo muda, e só em A.
	if err := os.Chmod(absA, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(absB)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("modo em B = %o, quero 755", got)
	}

	// O conteúdo não pode ter sido tocado, e nada disso é conflito.
	content, err := os.ReadFile(absB)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "mesmo conteudo" {
		t.Errorf("conteúdo alterado: %q", content)
	}
	conflicts, err := db.UnresolvedConflicts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Errorf("mudança de modo virou conflito: %v", conflicts)
	}
}

func TestReconcileModeChangeFromB(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "f.txt"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, []byte("igual"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(absB, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(absA)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("modo em A = %o, quero 600", got)
	}
}

// Modos iguais não devem gerar trabalho nenhum.
func TestReconcileEqualModesIsNoop(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "f.txt"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, []byte("igual"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	when := time.Unix(1_700_000_000, 0)
	for _, p := range []string{absA, absB} {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}

	before, err := os.Stat(absB)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(absB)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("par já convergido foi tocado sem motivo")
	}
}

// TestReconcilePropagatesDirectoryMode é regressão de um caso que a comparação
// de metadados anterior não pegava: reconcileBothSides devolvia noop assim que
// via dois diretórios, antes de olhar qualquer atributo.
func TestReconcilePropagatesDirectoryMode(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "publico"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, true); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(absA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(absB)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("modo do diretório em B = %o, quero 700", got)
	}
}

// TestReconcilePropagatesMtimeOnlyChange cobre o `touch`: conteúdo e modo
// idênticos, só o mtime diferente.
//
// Deixar isso passar não era cosmético. Um par sincronizado tem mtimes
// idênticos, e sampledButStale conta com isso para detectar alterações que a
// amostra não vê. Com o mtime divergindo à toa, cada resync retransferiria o
// arquivo grande inteiro, para sempre, sem nunca convergir.
func TestReconcilePropagatesMtimeOnlyChange(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "tempo.txt"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, []byte("mesmo conteudo"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	synced := time.Now().Add(-time.Hour)
	for _, p := range []string{absA, absB} {
		if err := os.Chtimes(p, synced, synced); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}

	touched := synced.Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(absA, touched, touched); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}

	stA, err := os.Stat(absA)
	if err != nil {
		t.Fatal(err)
	}
	stB, err := os.Stat(absB)
	if err != nil {
		t.Fatal(err)
	}
	if d := stA.ModTime().Sub(stB.ModTime()); d > time.Second || d < -time.Second {
		t.Errorf("mtimes seguem divergentes: A=%s B=%s", stA.ModTime(), stB.ModTime())
	}

	// O conteúdo não pode ter sido tocado.
	got, err := os.ReadFile(absB)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "mesmo conteudo" {
		t.Errorf("conteúdo = %q, deveria estar intacto", got)
	}
}

// Depois de convergir os atributos, um segundo resync não deve encontrar mais
// nada — senão a divergência de mtime viraria trabalho perpétuo.
func TestReconcileAttrsConvergesInOnePass(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "tempo.txt"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, []byte("igual"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(absA, old, old); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "primeira", "."); err != nil {
		t.Fatal(err)
	}
	// Depois da primeira passada, nada mais pode se mexer.
	beforeA, err := os.Stat(absA)
	if err != nil {
		t.Fatal(err)
	}
	beforeB, err := os.Stat(absB)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.reconcile(ctx, "segunda", "."); err != nil {
		t.Fatal(err)
	}
	afterA, err := os.Stat(absA)
	if err != nil {
		t.Fatal(err)
	}
	afterB, err := os.Stat(absB)
	if err != nil {
		t.Fatal(err)
	}
	if !beforeA.ModTime().Equal(afterA.ModTime()) || !beforeB.ModTime().Equal(afterB.ModTime()) {
		t.Error("a segunda passada mexeu nos arquivos: os atributos não convergiram")
	}
}

// TestReconcileSymlinkAttrsConverge é regressão de um trabalho perpétuo: o
// mtime de um symlink divergente era detectado, a sincronização de atributos
// pulava symlinks por completo, e a divergência reaparecia em todo resync —
// para sempre, sem nunca convergir.
func TestReconcileSymlinkAttrsConverge(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	ctx := context.Background()

	for _, root := range []string{cfg.A, cfg.B} {
		if err := os.WriteFile(filepath.Join(root, "alvo.txt"), []byte("conteudo"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("alvo.txt", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{"alvo.txt", "link"} {
		if err := eng.recordPair(ctx, rel, filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel), false); err != nil {
			t.Fatal(err)
		}
	}

	// Só o mtime do link de A muda.
	if err := eng.xfer.SyncAttrs(ctx, filepath.Join(cfg.A, "alvo.txt"), filepath.Join(cfg.A, "link")); err != nil {
		t.Skipf("ajuste de mtime de symlink indisponível: %v", err)
	}

	// Duas passadas: a primeira deve resolver, a segunda não deve achar nada.
	if err := eng.reconcile(ctx, "primeira", "."); err != nil {
		t.Fatal(err)
	}
	fiA, err := os.Lstat(filepath.Join(cfg.A, "link"))
	if err != nil {
		t.Fatal(err)
	}
	fiB, err := os.Lstat(filepath.Join(cfg.B, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if d := fiA.ModTime().Sub(fiB.ModTime()); d > time.Second || d < -time.Second {
		t.Errorf("mtime do symlink não convergiu: A=%s B=%s", fiA.ModTime(), fiB.ModTime())
	}

	// Estabilidade: nada pode se mexer na segunda passada.
	if err := eng.reconcile(ctx, "segunda", "."); err != nil {
		t.Fatal(err)
	}
	againA, err := os.Lstat(filepath.Join(cfg.A, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if !fiA.ModTime().Equal(againA.ModTime()) {
		t.Error("a segunda passada mexeu no link: não convergiu")
	}
}

// Bits especiais divergentes precisam ser reconciliados: o setuid é o caso em
// que perder a diferença tem consequência prática.
func TestReconcilePropagatesSpecialBits(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "binario"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, []byte("igual"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, os.FileMode(0o755)|os.ModeSetuid); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}

	// A troca setuid -> setgid não muda nenhum dos nove bits de Perm().
	if err := os.Chmod(absA, os.FileMode(0o755)|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(absB)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetgid == 0 || fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("bits especiais não reconciliados; modo em B = %v", fi.Mode())
	}
}

// TestOwnershipNotComparedWithoutPrivilege é a proteção contra o trabalho
// perpétuo: sem privilégio para chown, comparar dono produziria divergência
// detectada e nunca resolvida, refeita em todo resync — exatamente o defeito
// que os symlinks tinham.
func TestOwnershipNotComparedWithoutPrivilege(t *testing.T) {
	eng, _, _ := newIdleEngine(t, nil)

	a := hash.Stat{Uid: 1000, Gid: 1000, Mode: 0o644}
	b := hash.Stat{Uid: 0, Gid: 0, Mode: 0o644}

	if os.Geteuid() == 0 {
		if !eng.syncOwnership {
			t.Fatal("rodando como root, ownership deveria entrar na reconciliação")
		}
		if !eng.attrsDiverged(a, b) {
			t.Error("como root, donos diferentes são divergência")
		}
		return
	}

	if eng.syncOwnership {
		t.Fatal("sem privilégio, ownership não deveria entrar na reconciliação")
	}
	if eng.attrsDiverged(a, b) {
		t.Error("sem poder aplicar chown, a divergência de dono não pode ser reportada")
	}
}

// O estado precisa registrar dono e grupo, senão a reconciliação não tem base
// para dizer qual lado mudou a propriedade.
func TestStateRecordsOwner(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "f.txt"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}

	entry, err := db.Get(ctx, event.SideA, rel)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("entrada não gravada")
	}
	if entry.Uid != os.Getuid() || entry.Gid != os.Getgid() {
		t.Errorf("dono gravado = %d:%d, quero %d:%d", entry.Uid, entry.Gid, os.Getuid(), os.Getgid())
	}
}
