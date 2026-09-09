// Package transfer executa as operações de escrita no lado destino.
//
// A cópia de conteúdo é delegada ao rsync (seção 5 do escopo): ele evita
// transferir o que não mudou, lida bem com arquivos grandes e preserva
// permissões, timestamps e links por conta própria. As demais operações
// (mkdir, rename, remoção) são chamadas de sistema diretas — invocar rsync
// para elas seria custo sem benefício.
package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Transfer aplica operações no filesystem destino.
type Transfer struct {
	rsyncPath         string
	rsyncArgs         []string
	preserveHardlinks bool
}

// New cria um Transfer. args são as opções base passadas ao rsync em toda
// invocação (ver config.Default).
func New(rsyncPath string, args []string, preserveHardlinks bool) *Transfer {
	if rsyncPath == "" {
		rsyncPath = "rsync"
	}
	return &Transfer{
		rsyncPath:         rsyncPath,
		rsyncArgs:         append([]string(nil), args...),
		preserveHardlinks: preserveHardlinks,
	}
}

// Check valida que o rsync existe e é executável. Falhar cedo, na subida do
// serviço, é melhor que falhar na primeira sincronização.
func (t *Transfer) Check(ctx context.Context) error {
	bin, err := exec.LookPath(t.rsyncPath)
	if err != nil {
		return fmt.Errorf("rsync não encontrado (%s): %w", t.rsyncPath, err)
	}
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return fmt.Errorf("executando %s --version: %w", bin, err)
	}
	if !bytes.Contains(out, []byte("rsync")) {
		return fmt.Errorf("%s não parece ser o rsync", bin)
	}
	return nil
}

// CopyFile copia um arquivo de src para dst, ambos absolutos, criando os
// diretórios intermediários.
func (t *Transfer) CopyFile(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("criando diretório de destino: %w", err)
	}
	return t.rsync(ctx, src, dst)
}

// CopyTree copia recursivamente o diretório src para dst.
//
// A barra final em src é significativa para o rsync: com ela, copia-se o
// *conteúdo* de src para dentro de dst; sem ela, copia-se o diretório src
// como filho de dst. Aqui queremos sempre a primeira forma.
//
// # Hardlinks
//
// --hard-links só preserva ligações que o rsync consegue ver dentro de uma
// mesma invocação. Por isso a opção é aplicada aqui, na cópia de árvore
// (First Sync, Full Resync, propagação de diretório novo), e não em CopyFile:
// dois arquivos ligados que chegam por eventos separados são copiados por
// invocações separadas, e nesse caminho a ligação se perde — viram dois
// arquivos independentes com o mesmo conteúdo.
//
// Ou seja: a preservação é de melhor esforço, garantida no scan e não na
// propagação incremental. Quem depende de hardlinks deve contar com o Full
// Resync para restabelecê-los.
func (t *Transfer) CopyTree(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("criando diretório de destino: %w", err)
	}

	args := append([]string(nil), t.rsyncArgs...)
	if t.preserveHardlinks {
		args = append(args, "--hard-links")
	}
	args = append(args, strings.TrimSuffix(src, "/")+"/", dst)
	return t.run(ctx, args)
}

// Mkdir cria um diretório no destino, copiando o modo da origem.
func (t *Transfer) Mkdir(src, dst string) error {
	mode := os.FileMode(0o755)
	if fi, err := os.Stat(src); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(dst, mode); err != nil {
		return fmt.Errorf("criando %s: %w", dst, err)
	}
	return nil
}

// Move renomeia from para to no destino.
//
// Um rename só funciona dentro do mesmo filesystem; se cruzar limites, o
// chamador deve degradar para copiar e apagar. Aqui o caso comum — os dois
// paths sob a mesma raiz — é sempre o mesmo filesystem.
func (t *Transfer) Move(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Errorf("criando diretório de destino: %w", err)
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("movendo %s -> %s: %w", from, to, err)
	}
	return nil
}

// Remove apaga um arquivo ou diretório (recursivamente). Remover o que já
// não existe não é erro: o estado desejado já foi alcançado.
func (t *Transfer) Remove(path string, isDir bool) error {
	var err error
	if isDir {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removendo %s: %w", path, err)
	}
	return nil
}

// SyncAttrs propaga apenas modo e timestamps, sem tocar no conteúdo.
func (t *Transfer) SyncAttrs(ctx context.Context, src, dst string) error {
	args := []string{"--archive", "--no-recursive", "--existing", "--times", "--perms", src, dst}
	return t.run(ctx, args)
}

func (t *Transfer) rsync(ctx context.Context, src, dst string) error {
	args := append(append([]string(nil), t.rsyncArgs...), src, dst)
	return t.run(ctx, args)
}

func (t *Transfer) run(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, t.rsyncPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("rsync %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}
