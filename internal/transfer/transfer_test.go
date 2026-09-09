package transfer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/hash"
)

func newTransfer(t *testing.T) *Transfer {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync não disponível")
	}
	return New("rsync", []string{"--archive", "--partial", "--inplace", "--numeric-ids"}, false, false)
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

	xf := New(fake, nil, false, false)
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

// setuid, setgid e sticky também são permissões, e os.FileMode.Perm() os
// descarta. Um binário que perde o setuid deixa de funcionar.
func TestSyncAttrsPropagatesSpecialBits(t *testing.T) {
	xf := newTransfer(t)
	dir := t.TempDir()

	src := filepath.Join(dir, "origem")
	dst := filepath.Join(dir, "destino")
	for _, p := range []string{src, dst} {
		if err := os.WriteFile(p, []byte("igual"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(src, os.FileMode(0o755)|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, os.FileMode(0o755)|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}

	if err := xf.SyncAttrs(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetgid == 0 {
		t.Errorf("setgid não foi propagado; modo = %v", fi.Mode())
	}
	if fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("setuid antigo permaneceu; modo = %v", fi.Mode())
	}
}

// O mtime de um symlink precisa ser ajustado no próprio link, não no alvo.
func TestSyncAttrsTouchesSymlinkNotTarget(t *testing.T) {
	xf := newTransfer(t)
	dir := t.TempDir()

	alvo := filepath.Join(dir, "alvo.txt")
	if err := os.WriteFile(alvo, []byte("conteudo"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "link-src")
	dst := filepath.Join(dir, "link-dst")
	for _, p := range []string{src, dst} {
		if err := os.Symlink("alvo.txt", p); err != nil {
			t.Fatal(err)
		}
	}

	// mtime do alvo, para conferir depois que não foi tocado.
	alvoAntes, err := os.Stat(alvo)
	if err != nil {
		t.Fatal(err)
	}

	quando := time.Unix(1_600_000_000, 0)
	if err := lchtimes(src, quando); err != nil {
		t.Skipf("lchtimes indisponível neste filesystem: %v", err)
	}

	if err := xf.SyncAttrs(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}

	fiSrc, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	fiDst, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !fiSrc.ModTime().Equal(fiDst.ModTime()) {
		t.Errorf("mtime do link não convergiu: src=%s dst=%s", fiSrc.ModTime(), fiDst.ModTime())
	}

	alvoDepois, err := os.Stat(alvo)
	if err != nil {
		t.Fatal(err)
	}
	if !alvoAntes.ModTime().Equal(alvoDepois.ModTime()) {
		t.Error("o mtime do alvo foi alterado; a operação seguiu o symlink")
	}
}

// secondaryGid devolve um gid do qual o usuário é membro e que não é o
// primário — a única divergência de propriedade que um processo sem privilégio
// consegue criar, e portanto a única forma de exercitar o chown de verdade
// sem ser root.
func secondaryGid(t *testing.T) int {
	t.Helper()
	gids, err := os.Getgroups()
	if err != nil {
		t.Skipf("não foi possível listar grupos: %v", err)
	}
	primary := os.Getgid()
	for _, g := range gids {
		if g != primary {
			return g
		}
	}
	t.Skip("usuário não pertence a nenhum grupo secundário")
	return -1
}

func TestSyncAttrsPropagatesGroup(t *testing.T) {
	xf := New("rsync", []string{"--archive"}, false, true)
	dir := t.TempDir()
	other := secondaryGid(t)

	src := filepath.Join(dir, "origem.txt")
	dst := filepath.Join(dir, "destino.txt")
	for _, p := range []string{src, dst} {
		if err := os.WriteFile(p, []byte("igual"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Divergência real de propriedade: src no grupo secundário, dst no primário.
	if err := os.Lchown(src, -1, other); err != nil {
		t.Skipf("chown para o grupo %d indisponível: %v", other, err)
	}

	if err := xf.SyncAttrs(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}

	st, err := hash.StatOf(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Gid != other {
		t.Errorf("gid do destino = %d, quero %d", st.Gid, other)
	}
}

// Sem syncOwnership, a propriedade não é tocada — nem para melhor.
func TestSyncAttrsSkipsOwnerWhenDisabled(t *testing.T) {
	xf := New("rsync", []string{"--archive"}, false, false)
	dir := t.TempDir()
	other := secondaryGid(t)

	src := filepath.Join(dir, "origem.txt")
	dst := filepath.Join(dir, "destino.txt")
	for _, p := range []string{src, dst} {
		if err := os.WriteFile(p, []byte("igual"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Lchown(src, -1, other); err != nil {
		t.Skipf("chown indisponível: %v", err)
	}

	if err := xf.SyncAttrs(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	st, err := hash.StatOf(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Gid == other {
		t.Error("a propriedade foi alterada com syncOwnership desligado")
	}
}

// O chown de um symlink precisa agir no link, não no alvo.
func TestSyncAttrsChownsSymlinkNotTarget(t *testing.T) {
	xf := New("rsync", []string{"--archive"}, false, true)
	dir := t.TempDir()
	other := secondaryGid(t)

	alvo := filepath.Join(dir, "alvo.txt")
	if err := os.WriteFile(alvo, []byte("conteudo"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "link-src")
	dst := filepath.Join(dir, "link-dst")
	for _, p := range []string{src, dst} {
		if err := os.Symlink("alvo.txt", p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Lchown(src, -1, other); err != nil {
		t.Skipf("lchown indisponível: %v", err)
	}

	alvoAntes, err := hash.StatOf(alvo)
	if err != nil {
		t.Fatal(err)
	}
	if err := xf.SyncAttrs(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}

	linkDepois, err := hash.StatOf(dst)
	if err != nil {
		t.Fatal(err)
	}
	if linkDepois.Gid != other {
		t.Errorf("gid do link = %d, quero %d", linkDepois.Gid, other)
	}
	alvoDepois, err := hash.StatOf(alvo)
	if err != nil {
		t.Fatal(err)
	}
	if alvoDepois.Gid != alvoAntes.Gid {
		t.Error("o grupo do alvo foi alterado: a operação seguiu o symlink")
	}
}
