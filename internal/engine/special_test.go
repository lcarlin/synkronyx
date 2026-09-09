package engine

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/hash"
)

// Arquivos especiais e symlinks são as duas categorias que o rsync trata de
// forma diferente de um arquivo comum, e que o digest precisava aprender a
// distinguir.

func TestSpecialFilesAreNotPropagated(t *testing.T) {
	h := newHarness(t, nil)

	fifo := filepath.Join(h.A, "cano")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo indisponível: %v", err)
	}

	sock := filepath.Join(h.A, "socket")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("socket unix indisponível: %v", err)
	}
	defer l.Close()

	// Um arquivo comum depois deles serve de marco: quando ele chega, os
	// especiais já passaram pelo engine.
	if err := os.WriteFile(filepath.Join(h.A, "marco.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitContent(t, filepath.Join(h.B, "marco.txt"), "ok")

	for _, name := range []string{"cano", "socket"} {
		if _, err := os.Lstat(filepath.Join(h.B, name)); !os.IsNotExist(err) {
			t.Errorf("%s foi propagado; arquivos especiais não são sincronizáveis", name)
		}
	}
}

func TestSymlinksArePropagatedAsLinks(t *testing.T) {
	h := newHarness(t, nil)

	if err := os.WriteFile(filepath.Join(h.A, "alvo.txt"), []byte("conteúdo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alvo.txt", filepath.Join(h.A, "link")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fi, err := os.Lstat(filepath.Join(h.B, "link"))
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filepath.Join(h.B, "link"))
			if err != nil {
				t.Fatal(err)
			}
			if target != "alvo.txt" {
				t.Errorf("alvo do link = %q, quero \"alvo.txt\"", target)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("o symlink não chegou ao outro lado como symlink")
}

// Um link quebrado é um link válido: o alvo pode passar a existir depois. O
// digest não pode falhar por causa disso.
func TestDigestOfBrokenSymlink(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)

	link := filepath.Join(cfg.A, "quebrado")
	if err := os.Symlink("nao-existe.txt", link); err != nil {
		t.Fatal(err)
	}

	d, err := eng.digestOf(link)
	if err != nil {
		t.Fatalf("digest de link quebrado falhou: %v", err)
	}
	if d.Kind != hash.KindLink {
		t.Errorf("Kind = %q, quero %q", d.Kind, hash.KindLink)
	}
}

// A identidade de um symlink é o alvo, não o conteúdo apontado: dois links
// para alvos diferentes divergem mesmo que os alvos tenham o mesmo conteúdo.
func TestSymlinkDigestFollowsTargetPathNotContent(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, nil)

	for _, name := range []string{"um.txt", "dois.txt"} {
		if err := os.WriteFile(filepath.Join(cfg.A, name), []byte("idêntico"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("um.txt", filepath.Join(cfg.A, "link1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dois.txt", filepath.Join(cfg.A, "link2")); err != nil {
		t.Fatal(err)
	}

	d1, err := eng.digestOf(filepath.Join(cfg.A, "link1"))
	if err != nil {
		t.Fatal(err)
	}
	d2, err := eng.digestOf(filepath.Join(cfg.A, "link2"))
	if err != nil {
		t.Fatal(err)
	}
	if hash.Compare(d1, d2) != hash.Different {
		t.Error("links para alvos diferentes deveriam divergir, mesmo com conteúdo igual")
	}
}

// Com a política error, o arquivo especial vira registro no log em vez de
// desaparecer em silêncio.
func TestSpecialFilesErrorPolicyLogs(t *testing.T) {
	eng, _, _ := newIdleEngine(t, func(c *config.Config) {
		c.SpecialFiles = config.SpecialFilesError
	})

	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if !eng.skipSpecial("cano", hash.KindSpecial, log) {
		t.Error("arquivo especial deveria ser ignorado de qualquer forma")
	}
	if !strings.Contains(buf.String(), "cano") {
		t.Errorf("a política error deveria registrar o path:\n%s", buf.String())
	}
	if eng.skipSpecial("normal.txt", hash.KindRegular, log) {
		t.Error("arquivo comum não deveria ser ignorado")
	}
}

// Com a amostragem ligada, o engine precisa continuar propagando alterações
// que a amostra enxerga. O limite aqui é baixo de propósito, para o teste
// rodar rápido; o mecanismo é o mesmo do padrão de 100 MiB.
func TestPropagatesLargeFilesUnderSampledDigest(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.HashMaxBytes = 8 << 10    // 8 KiB
		c.HashSampleBytes = 1 << 10 // 1 KiB de cada ponta
	})

	big := make([]byte, 64<<10)
	for i := range big {
		big[i] = byte(i)
	}
	src := filepath.Join(h.A, "grande.bin")
	dst := filepath.Join(h.B, "grande.bin")

	if err := os.WriteFile(src, big, 0o644); err != nil {
		t.Fatal(err)
	}
	waitSameContent(t, src, dst)

	// Append: a amostra cobre a cauda, então precisa ser detectado.
	f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("dados novos no fim")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	waitSameContent(t, src, dst)
}

// waitSameContent espera os dois arquivos ficarem byte a byte iguais.
//
// Comparar com a origem, em vez de com um tamanho esperado, evita depender de
// quando exatamente cada etapa termina: a invariante do sistema é justamente
// que os dois lados convergem.
func waitSameContent(t *testing.T, src, dst string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var lastSrc, lastDst int
	for time.Now().Before(deadline) {
		a, errA := os.ReadFile(src)
		b, errB := os.ReadFile(dst)
		if errA == nil && errB == nil {
			lastSrc, lastDst = len(a), len(b)
			if bytes.Equal(a, b) {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("os lados não convergiram: origem %d bytes, destino %d", lastSrc, lastDst)
}

// O digest gravado no estado precisa refletir o tipo amostrado, senão uma
// comparação futura confrontaria tipos diferentes sem perceber.
func TestStateRecordsPartialDigestKind(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, func(c *config.Config) {
		c.HashMaxBytes = 4 << 10
		c.HashSampleBytes = 512
	})

	abs := filepath.Join(cfg.A, "grande.bin")
	if err := os.WriteFile(abs, make([]byte, 32<<10), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := eng.digestOf(abs)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != hash.KindPartial {
		t.Errorf("Kind = %q, quero %q acima do limite", d.Kind, hash.KindPartial)
	}
	if d.Size != 32<<10 {
		t.Errorf("Size = %d, quero %d", d.Size, 32<<10)
	}
}

// TestBlindSpotOnlyAffectsReconciliation fixa o alcance real da limitação da
// amostragem, que é mais estreito do que "alterações no meio não são vistas".
//
// Na reconciliação o digest decide sozinho, e ali a limitação vale. No fluxo
// de eventos não: alreadySynced compara mtime antes, e escrever sempre altera
// o mtime.
func TestBlindSpotOnlyAffectsReconciliation(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, func(c *config.Config) {
		c.HashMaxBytes = 8 << 10
		c.HashSampleBytes = 1 << 10
	})

	buf := make([]byte, 64<<10)
	a := filepath.Join(cfg.A, "f.bin")
	b := filepath.Join(cfg.B, "f.bin")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Divergência só no meio, tamanhos idênticos.
	f, err := os.OpenFile(b, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("DIFERENTE"), 32<<10); err != nil {
		t.Fatal(err)
	}
	f.Close()

	when := time.Unix(1_700_000_000, 0)
	for _, p := range []string{a, b} {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}

	differs, err := eng.contentDiffers(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if differs {
		t.Skip("a amostra alcançou o meio do arquivo; ótimo, mas não é garantido")
	}

	// Confirmado o ponto cego na reconciliação. Agora a outra metade: com
	// mtime diferente, o caminho de eventos age sem consultar o digest.
	if err := os.Chtimes(a, when.Add(time.Hour), when.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	entry, err := eng.entryFor(event.SideA, "f.bin", a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if entry.MTime.Equal(when) {
		t.Error("o estado deveria registrar o mtime novo, que é o que dispara a propagação")
	}
}

// TestFullResyncCatchesMiddleChangeInLargeFile é regressão de uma divergência
// que sobrevivia ao full resync.
//
// A documentação prometia que o full resync era a rede que pegava o que a
// amostra não via. Não era: ele usa o mesmo digest amostrado, então um arquivo
// grande alterado no meio, com o tamanho preservado, atravessava a
// reconciliação inteira sendo declarado idêntico.
//
// O mtime estava disponível o tempo todo e era ignorado.
func TestFullResyncCatchesMiddleChangeInLargeFile(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, func(c *config.Config) {
		c.HashMaxBytes = 8 << 10
		c.HashSampleBytes = 1 << 10
	})
	if testing.Verbose() {
		eng.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	ctx := context.Background()

	rel := "grande.bin"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	buf := make([]byte, 64<<10)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Par sincronizado tem o mesmo mtime, porque o rsync preserva o da origem.
	synced := time.Now().Add(-time.Hour)
	for _, p := range []string{absA, absB} {
		if err := os.Chtimes(p, synced, synced); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.recordPair(ctx, rel, absA, absB, false); err != nil {
		t.Fatal(err)
	}

	// Alteração no meio, tamanho preservado: fora do alcance da amostra.
	f, err := os.OpenFile(absA, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("ALTERADO"), 32<<10); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// A escrita atualiza o mtime de A; o de B fica onde estava. É essa
	// diferença que denuncia a alteração que a amostra não vê.
	edited := synced.Add(30 * time.Minute)
	if err := os.Chtimes(absA, edited, edited); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(absB)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(absA)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("a alteração no meio do arquivo sobreviveu ao resync: os lados seguem divergentes")
	}
}

// A contrapartida: mtimes iguais e digest amostrado igual continuam sendo
// tratados como convergidos, senão todo resync retransferiria os arquivos
// grandes sem motivo.
func TestSampledDigestWithEqualMtimeIsNoop(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, func(c *config.Config) {
		c.HashMaxBytes = 8 << 10
		c.HashSampleBytes = 1 << 10
	})
	ctx := context.Background()

	rel := "grande.bin"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	buf := make([]byte, 64<<10)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, buf, 0o644); err != nil {
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
		t.Error("arquivo convergido foi retransferido sem motivo")
	}
}

// Alterar o meio de um arquivo grande em UM lado só é alteração unilateral,
// não conflito. Sem o mtime como segunda evidência, originOf não conseguia
// atribuir origem — os digests amostrados eram iguais aos gravados dos dois
// lados — e o caso caía na política de conflito sem necessidade.
func TestMiddleChangeInLargeFileIsNotAConflict(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, func(c *config.Config) {
		c.HashMaxBytes = 8 << 10
		c.HashSampleBytes = 1 << 10
	})
	ctx := context.Background()

	rel := "grande.bin"
	absA, absB := filepath.Join(cfg.A, rel), filepath.Join(cfg.B, rel)
	buf := make([]byte, 64<<10)
	for _, p := range []string{absA, absB} {
		if err := os.WriteFile(p, buf, 0o644); err != nil {
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

	f, err := os.OpenFile(absA, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("ALTERADO"), 32<<10); err != nil {
		t.Fatal(err)
	}
	f.Close()
	edited := synced.Add(30 * time.Minute)
	if err := os.Chtimes(absA, edited, edited); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcile(ctx, "teste", "."); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(absB)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(absA)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("a alteração não foi propagada")
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
