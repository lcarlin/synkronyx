package hash

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Kind distingue como um digest foi calculado. A distinção não é cosmética:
// comparar um digest completo com um amostrado daria uma resposta sem
// significado, e o tipo existe para impedir que isso aconteça em silêncio.
type Kind string

const (
	// KindFull é o SHA-256 de todo o conteúdo.
	KindFull Kind = "sha256"
	// KindPartial é o SHA-256 de uma amostra: tamanho + início + fim do
	// arquivo. Usado acima de hash_max_bytes.
	KindPartial Kind = "sha256p"
)

// Digest identifica o conteúdo de um arquivo.
type Digest struct {
	Kind Kind
	Hex  string
	Size int64
}

// IsZero informa se o digest não foi calculado.
func (d Digest) IsZero() bool { return d.Hex == "" }

// String serializa o digest para persistência. O formato carrega o tipo
// justamente para que um digest amostrado nunca seja lido de volta como se
// fosse completo.
//
//	sha256:<hex>
//	sha256p:<hex>:<tamanho>
func (d Digest) String() string {
	if d.IsZero() {
		return ""
	}
	if d.Kind == KindPartial {
		return fmt.Sprintf("%s:%s:%d", d.Kind, d.Hex, d.Size)
	}
	return fmt.Sprintf("%s:%s", KindFull, d.Hex)
}

// Short devolve um prefixo legível para log.
func (d Digest) Short() string {
	if d.IsZero() {
		return "-"
	}
	h := d.Hex
	if len(h) > 12 {
		h = h[:12]
	}
	if d.Kind == KindPartial {
		return h + "~" // o til marca que é amostra, não conteúdo completo
	}
	return h
}

// ParseDigest lê a forma serializada.
//
// Aceita hex puro sem prefixo e o interpreta como KindFull: é o formato que
// as primeiras versões gravavam, e recusá-lo invalidaria bancos de estado
// existentes sem ganho nenhum.
func ParseDigest(s string) (Digest, error) {
	if s == "" {
		return Digest{}, nil
	}

	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		return Digest{Kind: KindFull, Hex: parts[0]}, nil
	case 2:
		if Kind(parts[0]) != KindFull {
			return Digest{}, fmt.Errorf("digest %q: tipo %q exige tamanho", s, parts[0])
		}
		return Digest{Kind: KindFull, Hex: parts[1]}, nil
	case 3:
		if Kind(parts[0]) != KindPartial {
			return Digest{}, fmt.Errorf("digest %q: tipo %q inesperado", s, parts[0])
		}
		size, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return Digest{}, fmt.Errorf("digest %q: tamanho inválido: %w", s, err)
		}
		return Digest{Kind: KindPartial, Hex: parts[1], Size: size}, nil
	default:
		return Digest{}, fmt.Errorf("digest %q: formato desconhecido", s)
	}
}

// Comparison é o resultado de comparar dois digests.
type Comparison int

const (
	// Same — os conteúdos são equivalentes.
	Same Comparison = iota
	// Different — os conteúdos divergem.
	Different
	// Indeterminate — não há base para decidir (algum digest ausente).
	Indeterminate
)

// Compare confronta dois digests.
//
// Tipos diferentes implicam tamanhos em lados opostos do limite de
// amostragem, e tamanhos diferentes já provam conteúdos diferentes — então o
// caso é decidível mesmo sem digests comparáveis.
func Compare(a, b Digest) Comparison {
	if a.IsZero() || b.IsZero() {
		return Indeterminate
	}
	if a.Kind != b.Kind {
		return Different
	}
	if a.Kind == KindPartial && a.Size != b.Size {
		return Different
	}
	if a.Hex == b.Hex {
		return Same
	}
	return Different
}

// DefaultSampleBytes é quanto se lê de cada extremidade no digest amostrado.
const DefaultSampleBytes int64 = 2 << 20 // 2 MiB

// Compute calcula o digest de abs.
//
// Arquivos até maxBytes recebem digest completo. Acima disso, o digest é
// amostrado: tamanho, os primeiros sampleBytes e os últimos sampleBytes.
// maxBytes <= 0 significa sempre completo.
//
// A amostra é uma escolha consciente entre custo e certeza. Ela não prova
// igualdade — dois arquivos grandes podem diferir só no meio —, mas detecta
// a esmagadora maioria das alterações reais (append, reescrita, truncamento,
// mudança de cabeçalho) por um custo fixo, independente do tamanho do
// arquivo. É estritamente melhor que a alternativa anterior, que era
// comparar apenas tamanho e mtime.
func Compute(abs string, maxBytes, sampleBytes int64) (Digest, error) {
	fi, err := os.Stat(abs)
	if err != nil {
		return Digest{}, err
	}
	if fi.IsDir() {
		return Digest{}, nil
	}

	if maxBytes <= 0 || fi.Size() <= maxBytes {
		sum, err := File(abs)
		if err != nil {
			return Digest{}, err
		}
		return Digest{Kind: KindFull, Hex: sum, Size: fi.Size()}, nil
	}

	if sampleBytes <= 0 {
		sampleBytes = DefaultSampleBytes
	}
	return partial(abs, fi.Size(), sampleBytes)
}

func partial(abs string, size, sampleBytes int64) (Digest, error) {
	f, err := os.Open(abs)
	if err != nil {
		return Digest{}, err
	}
	defer f.Close()

	h := sha256.New()

	// O tamanho entra no hash para que truncamento e append sejam sempre
	// detectados, mesmo quando as duas amostras coincidem.
	var sizeBuf [8]byte
	binary.BigEndian.PutUint64(sizeBuf[:], uint64(size))
	h.Write(sizeBuf[:])

	if _, err := io.CopyN(h, f, sampleBytes); err != nil && !errors.Is(err, io.EOF) {
		return Digest{}, fmt.Errorf("amostrando início de %s: %w", abs, err)
	}

	// Se as duas amostras se sobrepõem, o arquivo é pequeno o bastante para
	// a primeira já cobrir tudo; ler a cauda de novo só adicionaria custo.
	if size > 2*sampleBytes {
		if _, err := f.Seek(size-sampleBytes, io.SeekStart); err != nil {
			return Digest{}, fmt.Errorf("posicionando na cauda de %s: %w", abs, err)
		}
		if _, err := io.CopyN(h, f, sampleBytes); err != nil && !errors.Is(err, io.EOF) {
			return Digest{}, fmt.Errorf("amostrando fim de %s: %w", abs, err)
		}
	}

	return Digest{Kind: KindPartial, Hex: hex.EncodeToString(h.Sum(nil)), Size: size}, nil
}
