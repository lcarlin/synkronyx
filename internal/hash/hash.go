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
	"syscall"
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

	// Uid e Gid valem UnknownOwner quando não foi possível obtê-los.
	Uid int
	Gid int
}

// UnknownOwner marca dono ou grupo indeterminado — em um Stat que não pôde
// lê-los, ou em uma entrada de estado gravada antes de o Synkronyx passar a
// registrá-los.
//
// A distinção importa: 0 é o root, e tratar "não sei" como "root" faria a
// reconciliação enxergar divergência onde não há.
const UnknownOwner = -1

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
	st := Stat{
		Size:  fi.Size(),
		MTime: fi.ModTime(),
		Mode:  fi.Mode(),
		IsDir: fi.IsDir(),
		Kind:  KindOf(fi.Mode()),
		Uid:   UnknownOwner,
		Gid:   UnknownOwner,
	}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.Uid, st.Gid = int(sys.Uid), int(sys.Gid)
	}
	return st, nil
}

// OwnerKnown informa se dono e grupo foram determinados.
func (s Stat) OwnerKnown() bool { return s.Uid != UnknownOwner && s.Gid != UnknownOwner }

// SameOwner compara dono e grupo, tratando "desconhecido" de qualquer um dos
// lados como ausência de evidência de diferença.
func (s Stat) SameOwner(other Stat) bool {
	if !s.OwnerKnown() || !other.OwnerKnown() {
		return true
	}
	return s.Uid == other.Uid && s.Gid == other.Gid
}

// PermMask são os bits de permissão que o Synkronyx sincroniza: os nove
// habituais mais setuid, setgid e sticky.
//
// os.FileMode.Perm() sozinho não serve, porque mascara para 0777 e descarta
// justamente os três que mais importam quando mudam. Um binário que perde o
// setuid deixa de funcionar; um que ganha um setuid indevido é problema de
// outra ordem. Nenhum dos dois pode passar despercebido.
const PermMask = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// Perms extrai de um modo os bits que são sincronizados.
func Perms(m os.FileMode) os.FileMode { return m & PermMask }

// Perms devolve os bits de permissão sincronizados desta entrada.
func (s Stat) Perms() os.FileMode { return Perms(s.Mode) }

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
