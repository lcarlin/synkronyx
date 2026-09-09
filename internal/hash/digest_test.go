package hash

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name string, size int, fill byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = fill
	}
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestComputeFullBelowLimit(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "pequeno.bin", 1024, 'x')

	d, err := Compute(p, 1<<20, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != KindFull {
		t.Errorf("Kind = %q, quero %q", d.Kind, KindFull)
	}
}

func TestComputePartialAboveLimit(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "grande.bin", 100_000, 'x')

	d, err := Compute(p, 10_000, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != KindPartial {
		t.Errorf("Kind = %q, quero %q", d.Kind, KindPartial)
	}
	if d.Size != 100_000 {
		t.Errorf("Size = %d, quero 100000", d.Size)
	}
}

// O digest amostrado precisa detectar as alterações que de fato acontecem em
// arquivos grandes: append, truncamento e reescrita de cabeçalho.
func TestPartialDetectsRealisticChanges(t *testing.T) {
	dir := t.TempDir()
	const (
		size   = 100_000
		limit  = 10_000
		sample = 1024
	)

	base := write(t, dir, "base.bin", size, 'a')
	original, err := Compute(base, limit, sample)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("append", func(t *testing.T) {
		p := write(t, dir, "append.bin", size, 'a')
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte("mais dados"))
		f.Close()

		d, err := Compute(p, limit, sample)
		if err != nil {
			t.Fatal(err)
		}
		if Compare(original, d) != Different {
			t.Error("append não foi detectado")
		}
	})

	t.Run("truncamento", func(t *testing.T) {
		p := write(t, dir, "trunc.bin", size, 'a')
		if err := os.Truncate(p, size-1); err != nil {
			t.Fatal(err)
		}
		d, err := Compute(p, limit, sample)
		if err != nil {
			t.Fatal(err)
		}
		if Compare(original, d) != Different {
			t.Error("truncamento não foi detectado")
		}
	})

	t.Run("cabecalho", func(t *testing.T) {
		p := write(t, dir, "hdr.bin", size, 'a')
		f, err := os.OpenFile(p, os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteAt([]byte("CABECALHO"), 0)
		f.Close()

		d, err := Compute(p, limit, sample)
		if err != nil {
			t.Fatal(err)
		}
		if Compare(original, d) != Different {
			t.Error("alteração de cabeçalho não foi detectada")
		}
	})

	t.Run("cauda", func(t *testing.T) {
		p := write(t, dir, "tail.bin", size, 'a')
		f, err := os.OpenFile(p, os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteAt([]byte("FIM"), size-3)
		f.Close()

		d, err := Compute(p, limit, sample)
		if err != nil {
			t.Fatal(err)
		}
		if Compare(original, d) != Different {
			t.Error("alteração na cauda não foi detectada")
		}
	})

	// O limite conhecido e aceito da amostragem: alteração no meio, sem mudar
	// o tamanho, passa despercebida. Está documentado em Compute, e o teste
	// existe para que a limitação seja explícita em vez de surpresa.
	t.Run("meio_nao_detectado", func(t *testing.T) {
		p := write(t, dir, "meio.bin", size, 'a')
		f, err := os.OpenFile(p, os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteAt([]byte("MEIO"), size/2)
		f.Close()

		d, err := Compute(p, limit, sample)
		if err != nil {
			t.Fatal(err)
		}
		if Compare(original, d) != Same {
			t.Skip("amostragem detectou alteração no meio; ótimo, mas não é garantido")
		}
	})
}

func TestCompareKindMismatchMeansDifferent(t *testing.T) {
	// Tipos diferentes só ocorrem quando os tamanhos estão em lados opostos do
	// limite — e tamanhos diferentes já provam conteúdos diferentes.
	full := Digest{Kind: KindFull, Hex: "abc"}
	partial := Digest{Kind: KindPartial, Hex: "abc", Size: 999}

	if got := Compare(full, partial); got != Different {
		t.Errorf("Compare = %v, quero Different", got)
	}
}

func TestComparePartialSameHexDifferentSize(t *testing.T) {
	a := Digest{Kind: KindPartial, Hex: "abc", Size: 1}
	b := Digest{Kind: KindPartial, Hex: "abc", Size: 2}

	if got := Compare(a, b); got != Different {
		t.Errorf("Compare = %v, quero Different", got)
	}
}

func TestCompareMissingDigestIsIndeterminate(t *testing.T) {
	if got := Compare(Digest{}, Digest{Kind: KindFull, Hex: "abc"}); got != Indeterminate {
		t.Errorf("Compare = %v, quero Indeterminate", got)
	}
}

func TestDigestRoundTrip(t *testing.T) {
	for _, want := range []Digest{
		{Kind: KindFull, Hex: "deadbeef"},
		{Kind: KindPartial, Hex: "deadbeef", Size: 123456},
	} {
		got, err := ParseDigest(want.String())
		if err != nil {
			t.Fatalf("ParseDigest(%q): %v", want.String(), err)
		}
		if got.Kind != want.Kind || got.Hex != want.Hex {
			t.Errorf("round-trip = %+v, quero %+v", got, want)
		}
		if want.Kind == KindPartial && got.Size != want.Size {
			t.Errorf("Size = %d, quero %d", got.Size, want.Size)
		}
	}
}

// Bancos gravados pela primeira versão guardam hex puro. Recusá-los
// invalidaria o estado existente sem ganho nenhum.
func TestParseDigestAcceptsLegacyBareHex(t *testing.T) {
	d, err := ParseDigest("aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != KindFull || d.Hex != "aabbccdd" {
		t.Errorf("ParseDigest legado = %+v", d)
	}
}

func TestParseDigestRejectsGarbage(t *testing.T) {
	for _, s := range []string{"sha256p:abc", "sha256:abc:1", "a:b:c:d"} {
		if _, err := ParseDigest(s); err == nil {
			t.Errorf("ParseDigest(%q) aceitou entrada inválida", s)
		}
	}
}

func TestParseDigestEmptyIsZero(t *testing.T) {
	d, err := ParseDigest("")
	if err != nil {
		t.Fatal(err)
	}
	if !d.IsZero() {
		t.Error("string vazia deveria produzir digest zero")
	}
}

func TestStatOfReadsOwner(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "f.txt", 10, 'x')

	st, err := StatOf(p)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OwnerKnown() {
		t.Fatal("dono e grupo não foram lidos")
	}
	if st.Uid != os.Getuid() || st.Gid != os.Getgid() {
		t.Errorf("dono = %d:%d, quero %d:%d", st.Uid, st.Gid, os.Getuid(), os.Getgid())
	}
}

// Desconhecido não é igual a root. Uma entrada de estado gravada antes de o
// Synkronyx registrar dono e grupo tem -1, e tratá-la como uid 0 faria a
// reconciliação enxergar divergência onde não há.
func TestSameOwnerTreatsUnknownAsNoEvidence(t *testing.T) {
	conhecido := Stat{Uid: 1000, Gid: 1000}
	desconhecido := Stat{Uid: UnknownOwner, Gid: UnknownOwner}
	root := Stat{Uid: 0, Gid: 0}

	if !conhecido.SameOwner(desconhecido) {
		t.Error("dono desconhecido não deveria contar como divergência")
	}
	if !desconhecido.SameOwner(conhecido) {
		t.Error("a comparação deveria ser simétrica")
	}
	if conhecido.SameOwner(root) {
		t.Error("1000:1000 e 0:0 são donos diferentes")
	}
	if !conhecido.SameOwner(Stat{Uid: 1000, Gid: 1000}) {
		t.Error("donos iguais deveriam comparar iguais")
	}
}

func TestOwnerKnownRequiresBoth(t *testing.T) {
	if (Stat{Uid: 1000, Gid: UnknownOwner}).OwnerKnown() {
		t.Error("gid desconhecido deveria tornar o dono indeterminado")
	}
	if (Stat{Uid: UnknownOwner, Gid: 1000}).OwnerKnown() {
		t.Error("uid desconhecido deveria tornar o dono indeterminado")
	}
}
