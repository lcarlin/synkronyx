package engine

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lcarlin/synkronyx/internal/config"
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
