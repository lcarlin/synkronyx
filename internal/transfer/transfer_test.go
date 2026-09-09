package transfer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func newTransfer(t *testing.T) *Transfer {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync não disponível")
	}
	return New("rsync", []string{"--archive", "--partial", "--inplace", "--numeric-ids"}, false)
}

// Regressão do bug mais custoso do projeto: dois arquivos de mesmo tamanho e
// mesmo mtime passam no quick check do rsync e a transferência é pulada em
// silêncio — mesmo com conteúdos diferentes. É o desfecho comum de um
// conflito, em que os dois lados foram editados quase ao mesmo tempo.
func TestCopyFileTransfersDespiteMatchingSizeAndMtime(t *testing.T) {
	xf := newTransfer(t)
	dir := t.TempDir()

	src := filepath.Join(dir, "origem.txt")
	dst := filepath.Join(dir, "destino.txt")
	if err := os.WriteFile(src, []byte("AAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("BBB"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mesmo tamanho, mesmo mtime, conteúdo diferente.
	when := time.Unix(1_700_000_000, 0)
	for _, p := range []string{src, dst} {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}

	if err := xf.CopyFile(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "AAA" {
		t.Errorf("destino = %q, quero %q — a transferência foi pulada pelo quick check", got, "AAA")
	}
}

func TestCopyFileCreatesParentDirectories(t *testing.T) {
	xf := newTransfer(t)
	dir := t.TempDir()

	src := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "a", "b", "c", "f.txt")

	if err := xf.CopyFile(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("destino não foi criado: %v", err)
	}
}

// A barra final em src é significativa para o rsync: sem ela, o diretório
// entraria como filho do destino em vez de ter o conteúdo copiado para dentro.
func TestCopyTreeCopiesContentsNotTheDirectoryItself(t *testing.T) {
	xf := newTransfer(t)
	dir := t.TempDir()

	src := filepath.Join(dir, "origem")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "f.txt"), []byte("fundo"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "destino")

	if err := xf.CopyTree(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "sub", "f.txt"))
	if err != nil {
		t.Fatalf("conteúdo não foi copiado para dentro do destino: %v", err)
	}
	if string(got) != "fundo" {
		t.Errorf("conteúdo = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "origem")); err == nil {
		t.Error("o diretório de origem virou filho do destino")
	}
}

func TestCopyTreePreservesSymlinks(t *testing.T) {
	xf := newTransfer(t)
	dir := t.TempDir()

	src := filepath.Join(dir, "origem")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "alvo.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alvo.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "destino")

	if err := xf.CopyTree(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("o symlink foi copiado como arquivo comum")
	}
}

// Remover o que já não existe não é erro: o estado desejado já foi alcançado.
func TestRemoveIsIdempotent(t *testing.T) {
	xf := newTransfer(t)
	dir := t.TempDir()

	if err := xf.Remove(filepath.Join(dir, "nunca-existiu.txt"), false); err != nil {
		t.Errorf("remover arquivo inexistente = %v, quero nil", err)
	}
	if err := xf.Remove(filepath.Join(dir, "nunca-existiu"), true); err != nil {
		t.Errorf("remover diretório inexistente = %v, quero nil", err)
	}
}

func TestCheckRejectsNonRsync(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "falso")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho outra coisa\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	xf := New(fake, nil, false)
	if err := xf.Check(context.Background()); err == nil {
		t.Error("Check aceitou um binário que não é o rsync")
	}
}

// SyncAttrs precisa propagar permissões sem tocar no conteúdo.
func TestSyncAttrsPropagatesModeWithoutTouchingContent(t *testing.T) {
	xf := newTransfer(t)
	dir := t.TempDir()

	src := filepath.Join(dir, "origem.txt")
	dst := filepath.Join(dir, "destino.txt")
	if err := os.WriteFile(src, []byte("mesmo conteudo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("mesmo conteudo"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := xf.SyncAttrs(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("modo = %o, quero 755", got)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "mesmo conteudo" {
		t.Errorf("conteúdo = %q, deveria estar intacto", got)
	}
}

// --existing impede que SyncAttrs crie o arquivo: ele só ajusta atributos de
// algo que já existe.
func TestSyncAttrsDoesNotCreateMissingFile(t *testing.T) {
	xf := newTransfer(t)
	dir := t.TempDir()

	src := filepath.Join(dir, "origem.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "inexistente.txt")

	if err := xf.SyncAttrs(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("SyncAttrs criou um arquivo que não existia")
	}
}
