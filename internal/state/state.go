// Package state guarda em SQLite o que já foi sincronizado.
//
// O driver é o modernc.org/sqlite, em Go puro: mantém o binário único e
// cross-compilável, que é a razão declarada de o projeto ser em Go.
package state

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/hash"
)

//go:embed schema.sql
var schema string

// Status de uma entrada.
const (
	StatusSynced   = "synced"
	StatusPending  = "pending"
	StatusConflict = "conflict"
	StatusError    = "error"
)

// Entry é o último estado conhecido de um path em um dos lados.
type Entry struct {
	Path     string
	Side     event.Side
	IsDir    bool
	Size     int64
	MTime    time.Time
	Mode     os.FileMode
	SHA256   string // vazio se não calculado
	SyncedAt time.Time
	Status   string
}

// Stat converte a entrada para a identidade barata usada nas comparações.
func (e Entry) Stat() hash.Stat {
	return hash.Stat{Size: e.Size, MTime: e.MTime, Mode: e.Mode, IsDir: e.IsDir}
}

// DB é o estado persistente.
type DB struct{ db *sql.DB }

// Open abre (criando se preciso) o banco em path e aplica o schema.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("criando diretório de estado %s: %w", dir, err)
		}
	}

	sqlDB, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("abrindo estado %s: %w", path, err)
	}
	// Escritor único: o engine é serializado, e uma conexão só elimina
	// contenção de escrita no SQLite.
	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("aplicando schema: %w", err)
	}
	return &DB{db: sqlDB}, nil
}

func (d *DB) Close() error { return d.db.Close() }

// Get devolve a entrada de (path, side). Devolve (nil, nil) se não existir.
func (d *DB) Get(ctx context.Context, side event.Side, path string) (*Entry, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT path, side, is_dir, size, mtime_ns, mode, COALESCE(sha256, ''), synced_at, status
		FROM entries WHERE path = ? AND side = ?`, path, int(side))

	var (
		e       Entry
		sideInt int
		isDir   int
		mtimeNS int64
		mode    int64
		synced  int64
	)
	err := row.Scan(&e.Path, &sideInt, &isDir, &e.Size, &mtimeNS, &mode, &e.SHA256, &synced, &e.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lendo entrada %s/%s: %w", side, path, err)
	}

	e.Side = event.Side(sideInt)
	e.IsDir = isDir != 0
	e.MTime = time.Unix(0, mtimeNS)
	e.Mode = os.FileMode(uint32(mode))
	e.SyncedAt = time.Unix(0, synced)
	return &e, nil
}

// Put grava (ou substitui) uma entrada.
func (d *DB) Put(ctx context.Context, e Entry) error {
	var sha any
	if e.SHA256 != "" {
		sha = e.SHA256
	}
	syncedAt := e.SyncedAt
	if syncedAt.IsZero() {
		syncedAt = time.Now()
	}
	status := e.Status
	if status == "" {
		status = StatusSynced
	}

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO entries (path, side, is_dir, size, mtime_ns, mode, sha256, synced_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path, side) DO UPDATE SET
			is_dir = excluded.is_dir, size = excluded.size, mtime_ns = excluded.mtime_ns,
			mode = excluded.mode, sha256 = excluded.sha256,
			synced_at = excluded.synced_at, status = excluded.status`,
		e.Path, int(e.Side), boolInt(e.IsDir), e.Size, e.MTime.UnixNano(),
		int64(e.Mode), sha, syncedAt.UnixNano(), status)
	if err != nil {
		return fmt.Errorf("gravando entrada %s/%s: %w", e.Side, e.Path, err)
	}
	return nil
}

// PutBoth grava as entradas dos dois lados em uma transação. É a gravação
// normal depois de uma propagação bem-sucedida: os dois lados passam a
// existir no estado juntos, ou nenhum.
func (d *DB) PutBoth(ctx context.Context, a, b Entry) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// TODO(engine): reaproveitar a lógica de Put dentro da transação em vez
	// de duplicar o SQL, quando a interface de escrita estabilizar.
	for _, e := range []Entry{a, b} {
		var sha any
		if e.SHA256 != "" {
			sha = e.SHA256
		}
		status := e.Status
		if status == "" {
			status = StatusSynced
		}
		syncedAt := e.SyncedAt
		if syncedAt.IsZero() {
			syncedAt = time.Now()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO entries (path, side, is_dir, size, mtime_ns, mode, sha256, synced_at, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(path, side) DO UPDATE SET
				is_dir = excluded.is_dir, size = excluded.size, mtime_ns = excluded.mtime_ns,
				mode = excluded.mode, sha256 = excluded.sha256,
				synced_at = excluded.synced_at, status = excluded.status`,
			e.Path, int(e.Side), boolInt(e.IsDir), e.Size, e.MTime.UnixNano(),
			int64(e.Mode), sha, syncedAt.UnixNano(), status); err != nil {
			return fmt.Errorf("gravando par %s: %w", e.Path, err)
		}
	}
	return tx.Commit()
}

// Delete remove a entrada de (path, side).
func (d *DB) Delete(ctx context.Context, side event.Side, path string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM entries WHERE path = ? AND side = ?`, path, int(side))
	return err
}

// DeleteSubtree remove path e tudo abaixo dele, nos dois lados.
func (d *DB) DeleteSubtree(ctx context.Context, path string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM entries WHERE path = ? OR path LIKE ? ESCAPE '\'`,
		path, escapeLike(path)+`/%`)
	return err
}

// MarkStatus muda apenas o status de uma entrada.
func (d *DB) MarkStatus(ctx context.Context, side event.Side, path, status string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE entries SET status = ? WHERE path = ? AND side = ?`, status, path, int(side))
	return err
}

// RecordConflict registra um conflito não resolvido.
func (d *DB) RecordConflict(ctx context.Context, path, shaA, shaB string) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO conflicts (path, detected_at, sha_a, sha_b)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			detected_at = excluded.detected_at,
			sha_a = excluded.sha_a, sha_b = excluded.sha_b,
			resolution = NULL, resolved_at = NULL`,
		path, time.Now().UnixNano(), shaA, shaB)
	return err
}

// ResolveConflict marca um conflito como resolvido.
func (d *DB) ResolveConflict(ctx context.Context, path, resolution string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE conflicts SET resolution = ?, resolved_at = ? WHERE path = ?`,
		resolution, time.Now().UnixNano(), path)
	return err
}

// Meta lê um valor de configuração persistida (ex.: se o First Sync já rodou).
func (d *DB) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := d.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta grava um valor de configuração persistida.
func (d *DB) SetMeta(ctx context.Context, key, value string) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Paths devolve todos os paths conhecidos de um lado. Usado pelo Full Resync
// para detectar o que existe no estado mas não no disco.
func (d *DB) Paths(ctx context.Context, side event.Side) (map[string]Entry, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT path, is_dir, size, mtime_ns, mode, COALESCE(sha256, ''), synced_at, status
		FROM entries WHERE side = ?`, int(side))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]Entry)
	for rows.Next() {
		var (
			e       Entry
			isDir   int
			mtimeNS int64
			mode    int64
			synced  int64
		)
		if err := rows.Scan(&e.Path, &isDir, &e.Size, &mtimeNS, &mode, &e.SHA256, &synced, &e.Status); err != nil {
			return nil, err
		}
		e.Side = side
		e.IsDir = isDir != 0
		e.MTime = time.Unix(0, mtimeNS)
		e.Mode = os.FileMode(uint32(mode))
		e.SyncedAt = time.Unix(0, synced)
		out[e.Path] = e
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// escapeLike neutraliza os curingas do LIKE em um path literal.
func escapeLike(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%', '_', '\\':
			out = append(out, '\\', s[i])
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
