// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/event"
)

// Estes testes exercitam o inotify de verdade, contra o filesystem real. É a
// única forma honesta de validar a tradução de eventos: um fake reproduziria
// as suposições do código em vez do comportamento do kernel.

func startWatcher(t *testing.T, root string) *Watcher {
	t.Helper()

	w, err := New(Options{Side: event.SideA, Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = w.Stop()
	})
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return w
}

// waitFor consome eventos até encontrar um que satisfaça pred.
func waitFor(t *testing.T, w *Watcher, what string, pred func(event.Event) bool) event.Event {
	t.Helper()

	deadline := time.After(3 * time.Second)
	var seen []event.Event
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				t.Fatalf("canal fechado esperando %s; vistos: %v", what, seen)
			}
			seen = append(seen, ev)
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout esperando %s; vistos: %v", what, seen)
		}
	}
}

func TestWatcherDetectsCreateAndWrite(t *testing.T) {
	root := t.TempDir()
	w := startWatcher(t, root)

	if err := os.WriteFile(filepath.Join(root, "novo.txt"), []byte("olá"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, w, "CREATE novo.txt", func(ev event.Event) bool {
		return ev.Kind == event.KindCreate && ev.Path == "novo.txt"
	})
	// O sinal de conteúdo pronto é o CLOSE_WRITE, traduzido para MODIFY.
	waitFor(t, w, "MODIFY novo.txt", func(ev event.Event) bool {
		return ev.Kind == event.KindModify && ev.Path == "novo.txt"
	})
}

func TestWatcherPairsRenameIntoSingleMove(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "velho.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := startWatcher(t, root)

	if err := os.Rename(filepath.Join(root, "velho.txt"), filepath.Join(root, "novo.txt")); err != nil {
		t.Fatal(err)
	}

	ev := waitFor(t, w, "MOVE", func(ev event.Event) bool { return ev.Kind == event.KindMove })
	if ev.From != "velho.txt" || ev.Path != "novo.txt" {
		t.Errorf("MOVE = %s -> %s, quero velho.txt -> novo.txt", ev.From, ev.Path)
	}
}

func TestWatcherTreatsMoveOutOfTreeAsDelete(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "some.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := startWatcher(t, root)

	// Sem o par MOVED_TO dentro da árvore, o MOVED_FROM órfão só pode ser
	// lido como remoção.
	if err := os.Rename(filepath.Join(root, "some.txt"), filepath.Join(outside, "some.txt")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, w, "DELETE some.txt", func(ev event.Event) bool {
		return ev.Kind == event.KindDelete && ev.Path == "some.txt"
	})
}

func TestWatcherFollowsNewSubdirectories(t *testing.T) {
	root := t.TempDir()
	w := startWatcher(t, root)

	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Escreve dentro de um diretório criado depois de o watcher subir: só
	// funciona se o watch recursivo tiver acompanhado a criação.
	if err := os.WriteFile(filepath.Join(sub, "fundo.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, w, "evento em a/b/fundo.txt", func(ev event.Event) bool {
		return ev.Path == filepath.Join("a", "b", "fundo.txt")
	})
}

func TestWatcherHonorsExclude(t *testing.T) {
	root := t.TempDir()
	w, err := New(Options{Side: event.SideA, Root: root, Exclude: excludeFunc(func(rel string, isDir bool) bool {
		return filepath.Base(rel) == "ignorado.txt"
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer w.Stop()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "ignorado.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "visivel.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Chegar ao evento de visivel.txt sem ter passado por ignorado.txt prova
	// a exclusão, já que a ordem dos dois writes é determinística.
	waitFor(t, w, "visivel.txt", func(ev event.Event) bool {
		if ev.Path == "ignorado.txt" {
			t.Error("evento de path excluído foi emitido")
		}
		return ev.Path == "visivel.txt"
	})
}

type excludeFunc func(rel string, isDir bool) bool

func (f excludeFunc) Excluded(rel string, isDir bool) bool { return f(rel, isDir) }

func TestTreeRenameSubtreeReassignsDescendants(t *testing.T) {
	tr := newTree()
	tr.add(1, "a")
	tr.add(2, filepath.Join("a", "b"))
	tr.add(3, "outro")

	tr.renameSubtree("a", "z")

	if p, _ := tr.pathOf(1); p != "z" {
		t.Errorf("wd 1 = %q, quero \"z\"", p)
	}
	if p, _ := tr.pathOf(2); p != filepath.Join("z", "b") {
		t.Errorf("wd 2 = %q, quero \"z/b\"", p)
	}
	if p, _ := tr.pathOf(3); p != "outro" {
		t.Errorf("wd 3 = %q, quero \"outro\" intocado", p)
	}
}

func TestTreeRemoveSubtree(t *testing.T) {
	tr := newTree()
	tr.add(1, "a")
	tr.add(2, filepath.Join("a", "b"))
	tr.add(3, "ab") // prefixo textual, mas não descendente

	got := tr.removeSubtree("a")
	if len(got) != 2 {
		t.Fatalf("removeSubtree devolveu %d wds, quero 2: %v", len(got), got)
	}
	if _, ok := tr.pathOf(3); !ok {
		t.Error("\"ab\" não é descendente de \"a\" e não deveria ter sido removido")
	}
}

// A raiz deixar de existir é terminal: o lado fica cego, e seguir rodando
// faria o engine propagar só uma direção sem perceber.
func TestWatcherReportsRootRemovalAsFatal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "raiz")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	w := startWatcher(t, root)

	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-w.Fatal():
		if err == nil {
			t.Fatal("Fatal devolveu erro nulo")
		}
		if !strings.Contains(err.Error(), "removida") {
			t.Errorf("erro = %q, quero menção à remoção da raiz", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("remoção da raiz não gerou falha terminal")
	}
}

func TestWatcherReportsRootRenameAsFatal(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "raiz")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	w := startWatcher(t, root)

	if err := os.Rename(root, filepath.Join(dir, "outro-nome")); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-w.Fatal():
		if !strings.Contains(err.Error(), "movida") {
			t.Errorf("erro = %q, quero menção à raiz movida", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rename da raiz não gerou falha terminal")
	}
}

func TestWatchCountTracksDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := startWatcher(t, root)

	// raiz + a + a/b
	if got := w.WatchCount(); got != 3 {
		t.Errorf("WatchCount() = %d, quero 3", got)
	}
}
