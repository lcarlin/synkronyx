// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package state

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/hash"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestPutGetRoundTrip(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	want := Entry{
		Path:   filepath.Join("dir", "f.txt"),
		Side:   event.SideB,
		Size:   42,
		MTime:  time.Unix(1_700_000_000, 123),
		Mode:   0o644,
		Digest: hash.Digest{Kind: hash.KindPartial, Hex: "abcdef", Size: 42},
	}
	if err := db.Put(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err := db.Get(ctx, event.SideB, want.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("entrada não encontrada")
	}
	if got.Size != want.Size || !got.MTime.Equal(want.MTime) || got.Mode != want.Mode {
		t.Errorf("metadados = %+v, quero %+v", got, want)
	}
	if hash.Compare(got.Digest, want.Digest) != hash.Same {
		t.Errorf("digest = %+v, quero %+v", got.Digest, want.Digest)
	}
}

func TestGetMissingReturnsNil(t *testing.T) {
	db, _ := openTemp(t)
	got, err := db.Get(context.Background(), event.SideA, "inexistente")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("quero nil para entrada inexistente, obtive %+v", got)
	}
}

func TestIsEmpty(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	empty, err := db.IsEmpty(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Error("banco novo deveria estar vazio")
	}

	if err := db.Put(ctx, Entry{Path: "f", Side: event.SideA}); err != nil {
		t.Fatal(err)
	}
	empty, err = db.IsEmpty(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Error("banco com entrada não deveria estar vazio")
	}
}

func TestDeleteSubtreeMatchesOnlyDescendants(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	paths := []string{"a", filepath.Join("a", "b"), filepath.Join("a", "b", "c"), "ab", "outro"}
	for _, p := range paths {
		if err := db.Put(ctx, Entry{Path: p, Side: event.SideA}); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.DeleteSubtree(ctx, "a"); err != nil {
		t.Fatal(err)
	}

	remaining, err := db.Paths(ctx, event.SideA)
	if err != nil {
		t.Fatal(err)
	}
	// "ab" tem "a" como prefixo textual, mas não é descendente.
	for _, want := range []string{"ab", "outro"} {
		if _, ok := remaining[want]; !ok {
			t.Errorf("%q não deveria ter sido removido", want)
		}
	}
	if len(remaining) != 2 {
		t.Errorf("restaram %d entradas, quero 2: %v", len(remaining), remaining)
	}
}

// Paths com curinga de LIKE não podem virar padrão de busca.
func TestDeleteSubtreeEscapesLikeWildcards(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	for _, p := range []string{"10%", filepath.Join("10%", "dentro"), "100", "1x_y"} {
		if err := db.Put(ctx, Entry{Path: p, Side: event.SideA}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DeleteSubtree(ctx, "10%"); err != nil {
		t.Fatal(err)
	}

	remaining, err := db.Paths(ctx, event.SideA)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := remaining["100"]; !ok {
		t.Error("\"100\" foi removido: o %% do path virou curinga")
	}
}

func TestSubtreeReturnsScopedEntries(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	for _, p := range []string{"dir", filepath.Join("dir", "a"), filepath.Join("dir", "sub", "b"), "fora"} {
		if err := db.Put(ctx, Entry{Path: p, Side: event.SideB}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.Subtree(ctx, event.SideB, "dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("Subtree devolveu %d entradas, quero 3: %v", len(got), got)
	}
	if _, ok := got["fora"]; ok {
		t.Error("Subtree incluiu path fora do escopo")
	}
}

func TestSummarizeCountsBySideAndStatus(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	if err := db.Put(ctx, Entry{Path: "a", Side: event.SideA, Status: StatusSynced}); err != nil {
		t.Fatal(err)
	}
	if err := db.Put(ctx, Entry{Path: "a", Side: event.SideB, Status: StatusSynced}); err != nil {
		t.Fatal(err)
	}
	if err := db.Put(ctx, Entry{Path: "b", Side: event.SideA, Status: StatusError}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConflict(ctx, "c", hash.Digest{}, hash.Digest{}); err != nil {
		t.Fatal(err)
	}

	sum, err := db.Summarize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.EntriesA != 2 || sum.EntriesB != 1 {
		t.Errorf("entradas A=%d B=%d, quero 2 e 1", sum.EntriesA, sum.EntriesB)
	}
	if sum.ByStatus[StatusError] != 1 {
		t.Errorf("status error = %d, quero 1", sum.ByStatus[StatusError])
	}
	if sum.Conflicts != 1 {
		t.Errorf("conflitos = %d, quero 1", sum.Conflicts)
	}
	if sum.SchemaVer != schemaVersion {
		t.Errorf("schema = %d, quero %d", sum.SchemaVer, schemaVersion)
	}
}

func TestResolveConflictRemovesFromOpenList(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	if err := db.RecordConflict(ctx, "f.txt", hash.Digest{Kind: hash.KindFull, Hex: "aa"},
		hash.Digest{Kind: hash.KindFull, Hex: "bb"}); err != nil {
		t.Fatal(err)
	}
	open, err := db.UnresolvedConflicts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("conflitos abertos = %d, quero 1", len(open))
	}

	if err := db.ResolveConflict(ctx, "f.txt", "preserve:x"); err != nil {
		t.Fatal(err)
	}
	open, err = db.UnresolvedConflicts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("conflitos abertos = %d após resolver, quero 0", len(open))
	}
}

// Um banco da v1 (coluna sha256, hex puro, sem user_version) precisa migrar
// sem perder os digests já gravados.
func TestMigrationFromV1PreservesDigests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legado.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		INSERT INTO entries (path, side, is_dir, size, mtime_ns, mode, sha256, synced_at, status)
		VALUES ('legado.txt', 0, 0, 10, 0, 420, 'aabbcc', 0, 'synced')`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open não migrou o banco v1: %v", err)
	}
	defer db.Close()

	got, err := db.Get(context.Background(), event.SideA, "legado.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("entrada legada desapareceu na migração")
	}
	if got.Digest.Kind != hash.KindFull || got.Digest.Hex != "aabbcc" {
		t.Errorf("digest migrado = %+v, quero sha256:aabbcc", got.Digest)
	}

	sum, err := db.Summarize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.SchemaVer != schemaVersion {
		t.Errorf("schema após migração = %d, quero %d", sum.SchemaVer, schemaVersion)
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	_, path := openTemp(t)

	// Reabrir várias vezes não pode reaplicar migrações.
	for i := range 3 {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("abertura %d falhou: %v", i+1, err)
		}
		db.Close()
	}
}

// Um binário antigo abrindo um banco mais novo deve recusar, não seguir em
// frente e corromper dados em silêncio.
func TestOpenRejectsFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "futuro.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 9999`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	if _, err := Open(path); err == nil {
		t.Error("Open aceitou um banco de versão futura")
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("banco não foi criado: %v", err)
	}
}
