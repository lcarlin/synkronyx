// Package hash identifica conteúdo de arquivos (seção 8 do escopo).
//
// SHA-256 é a identidade definitiva, mas calculá-lo a cada evento é caro. A
// estratégia aqui é a que o escopo pede: usar metadados baratos do
// filesystem como filtro, e só recorrer ao hash quando eles não bastam.
package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"
)

// FileKind classifica uma entrada do filesystem.
//
// A distinção existe porque nem tudo que aparece numa árvore é sincronizável
// da mesma forma — e algumas coisas não são sincronizáveis de jeito nenhum.
type FileKind uint8

const (
	// KindRegular é um arquivo comum.
	KindRegular FileKind = iota
	// KindDir é um diretório.
	KindDir
	// KindSymlink é um link simbólico. O que se sincroniza é o alvo do link,
	// como texto, e não o conteúdo apontado.
	KindSymlink
	// KindSpecial é socket, FIFO ou device node.
	KindSpecial
)

func (k FileKind) String() string {
	switch k {
	case KindRegular:
		return "arquivo"
	case KindDir:
		return "diretório"
	case KindSymlink:
		return "symlink"
	case KindSpecial:
		return "arquivo especial"
	default:
		return "desconhecido"
	}
}

// Stat é a identidade barata de um arquivo: o que dá para saber sem ler o
// conteúdo.
type Stat struct {
	Size  int64
	MTime time.Time
	Mode  os.FileMode
	IsDir bool
	Kind  FileKind
}

// StatOf coleta a identidade barata de um path absoluto.
//
// Usa Lstat, nunca Stat: um symlink precisa ser identificado como symlink,
// não como aquilo que ele aponta. Seguir o link faria um link quebrado virar
// erro e um link para fora da árvore virar cópia do alvo.
func StatOf(abs string) (Stat, error) {
	fi, err := os.Lstat(abs)
	if err != nil {
		return Stat{}, err
	}
	return Stat{
		Size:  fi.Size(),
		MTime: fi.ModTime(),
		Mode:  fi.Mode(),
		IsDir: fi.IsDir(),
		Kind:  KindOf(fi.Mode()),
	}, nil
}

// KindOf classifica a partir do modo devolvido por Lstat.
func KindOf(mode os.FileMode) FileKind {
	switch {
	case mode.IsDir():
		return KindDir
	case mode&os.ModeSymlink != 0:
		return KindSymlink
	case mode.IsRegular():
		return KindRegular
	default:
		// Socket, FIFO, device de bloco ou de caractere, ou irregular.
		return KindSpecial
	}
}

// IsSpecial informa se a entrada é socket, FIFO ou device node.
func (s Stat) IsSpecial() bool { return s.Kind == KindSpecial }

// Unchanged informa se dois Stats são compatíveis o suficiente para presumir
// conteúdo idêntico sem hashear.
//
// A comparação de mtime usa tolerância de um segundo: nem todo filesystem
// guarda sub-segundo, e rsync pode preservar o mtime com granularidade
// diferente da origem.
func (s Stat) Unchanged(other Stat) bool {
	if s.Size != other.Size || s.IsDir != other.IsDir {
		return false
	}
	d := s.MTime.Sub(other.MTime)
	if d < 0 {
		d = -d
	}
	return d < time.Second
}

// File calcula o SHA-256 do conteúdo de abs e devolve o digest em hex.
func File(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hasheando %s: %w", abs, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
