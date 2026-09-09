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

// Stat é a identidade barata de um arquivo: o que dá para saber sem ler o
// conteúdo.
type Stat struct {
	Size  int64
	MTime time.Time
	Mode  os.FileMode
	IsDir bool
}

// StatOf coleta a identidade barata de um path absoluto.
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
	}, nil
}

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
