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

// Chaves conhecidas da tabela meta.
const (
	// MetaFirstSync guarda quando o First Sync concluiu.
	MetaFirstSync = "first_sync_completed_at"
	// MetaLastResync guarda quando o último Full Resync concluiu.
	MetaLastResync = "last_full_resync_at"
	// MetaHeartbeat guarda o instante do último heartbeat do daemon.
	MetaHeartbeat = "heartbeat_at"
	// MetaWatches guarda o número de diretórios observados no último heartbeat.
	MetaWatches = "watch_count"
	// MetaRetryQueue guarda o tamanho da fila de retry no último heartbeat.
	MetaRetryQueue = "retry_queue_len"
	// MetaPID guarda o PID do processo que está rodando.
	MetaPID = "pid"
)

// Entry é o último estado conhecido de um path em um dos lados.
type Entry struct {
	Path     string
	Side     event.Side
	IsDir    bool
	Size     int64
	MTime    time.Time
	Mode     os.FileMode
	Digest   hash.Digest
	SyncedAt time.Time
	Status   string

	// Uid e Gid valem hash.UnknownOwner em entradas gravadas antes da v4 do
	// schema.
	Uid int
	Gid int
}

// Stat converte a entrada para a identidade barata usada nas comparações.
func (e Entry) Stat() hash.Stat {
	return hash.Stat{
		Size: e.Size, MTime: e.MTime, Mode: e.Mode, IsDir: e.IsDir,
		Uid: e.Uid, Gid: e.Gid,
	}
}

// DB é o estado persistente.
type DB struct{ db *sql.DB }

// Open abre (criando se preciso) o banco em path, aplica o schema e migra.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("criando diretório de estado %s: %w", dir, err)
		}
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	sqlDB, err := sql.Open("sqlite", dsn)
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
	if err := migrate(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return &DB{db: sqlDB}, nil
}

func (d *DB) Close() error { return d.db.Close() }

const entryColumns = `path, side, is_dir, size, mtime_ns, mode, COALESCE(digest, ''), synced_at, status, uid, gid`

// scanEntry lê uma linha na ordem de entryColumns.
func scanEntry(sc interface{ Scan(...any) error }) (Entry, error) {
	var (
		e       Entry
		sideInt int
		isDir   int
		mtimeNS int64
		mode    int64
		digest  string
		synced  int64
	)
	if err := sc.Scan(&e.Path, &sideInt, &isDir, &e.Size, &mtimeNS, &mode, &digest,
		&synced, &e.Status, &e.Uid, &e.Gid); err != nil {
		return Entry{}, err
	}

	parsed, err := hash.ParseDigest(digest)
	if err != nil {
		return Entry{}, fmt.Errorf("entrada %s: %w", e.Path, err)
	}

	e.Side = event.Side(sideInt)
	e.IsDir = isDir != 0
	e.MTime = time.Unix(0, mtimeNS)
	e.Mode = os.FileMode(uint32(mode))
	e.Digest = parsed
	e.SyncedAt = time.Unix(0, synced)
	return e, nil
}

// Get devolve a entrada de (path, side). Devolve (nil, nil) se não existir.
func (d *DB) Get(ctx context.Context, side event.Side, path string) (*Entry, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE path = ? AND side = ?`, path, int(side))

	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lendo entrada %s/%s: %w", side, path, err)
	}
	return &e, nil
}

const upsertEntry = `
	INSERT INTO entries (path, side, is_dir, size, mtime_ns, mode, digest, synced_at, status, uid, gid)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(path, side) DO UPDATE SET
		is_dir = excluded.is_dir, size = excluded.size, mtime_ns = excluded.mtime_ns,
		mode = excluded.mode, digest = excluded.digest,
		synced_at = excluded.synced_at, status = excluded.status,
		uid = excluded.uid, gid = excluded.gid`

// entryArgs monta os argumentos de upsertEntry, aplicando os defaults.
func entryArgs(e Entry) []any {
	var digest any
	if !e.Digest.IsZero() {
		digest = e.Digest.String()
	}
	if e.SyncedAt.IsZero() {
		e.SyncedAt = time.Now()
	}
	if e.Status == "" {
		e.Status = StatusSynced
	}
	return []any{
		e.Path, int(e.Side), boolInt(e.IsDir), e.Size, e.MTime.UnixNano(),
		int64(e.Mode), digest, e.SyncedAt.UnixNano(), e.Status, e.Uid, e.Gid,
	}
}

// Put grava (ou substitui) uma entrada.
func (d *DB) Put(ctx context.Context, e Entry) error {
	if _, err := d.db.ExecContext(ctx, upsertEntry, entryArgs(e)...); err != nil {
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

	for _, e := range []Entry{a, b} {
		if _, err := tx.ExecContext(ctx, upsertEntry, entryArgs(e)...); err != nil {
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

// Subtree devolve as entradas conhecidas de um lado sob rel (incluindo rel).
func (d *DB) Subtree(ctx context.Context, side event.Side, rel string) (map[string]Entry, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM entries
		 WHERE side = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
		int(side), rel, escapeLike(rel)+`/%`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return collectEntries(rows)
}

// Paths devolve todos os paths conhecidos de um lado. Usado pelo Full Resync
// para detectar o que existe no estado mas não no disco.
func (d *DB) Paths(ctx context.Context, side event.Side) (map[string]Entry, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE side = ?`, int(side))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return collectEntries(rows)
}

func collectEntries(rows *sql.Rows) (map[string]Entry, error) {
	out := make(map[string]Entry)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out[e.Path] = e
	}
	return out, rows.Err()
}

// IsEmpty informa se o banco não tem nenhuma entrada.
//
// A pergunta importa: com estado vazio não há como distinguir "arquivo novo
// deste lado" de "arquivo apagado do outro lado", e a reconciliação precisa
// avisar em vez de escolher em silêncio.
func (d *DB) IsEmpty(ctx context.Context) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM entries)`).Scan(&n)
	return n == 0, err
}

// RecordConflict registra um conflito não resolvido.
func (d *DB) RecordConflict(ctx context.Context, path string, a, b hash.Digest) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO conflicts (path, detected_at, sha_a, sha_b)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			detected_at = excluded.detected_at,
			sha_a = excluded.sha_a, sha_b = excluded.sha_b,
			resolution = NULL, resolved_at = NULL`,
		path, time.Now().UnixNano(), a.String(), b.String())
	return err
}

// ResolveConflict marca um conflito como resolvido.
func (d *DB) ResolveConflict(ctx context.Context, path, resolution string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE conflicts SET resolution = ?, resolved_at = ? WHERE path = ?`,
		resolution, time.Now().UnixNano(), path)
	return err
}

// Conflict é um conflito registrado.
type Conflict struct {
	Path       string
	DetectedAt time.Time
	SHAA       string
	SHAB       string
	Resolution string
}

// UnresolvedConflicts lista os conflitos ainda sem resolução, mais recentes
// primeiro. limit <= 0 devolve todos.
func (d *DB) UnresolvedConflicts(ctx context.Context, limit int) ([]Conflict, error) {
	query := `SELECT path, detected_at, COALESCE(sha_a, ''), COALESCE(sha_b, '')
	          FROM conflicts WHERE resolution IS NULL ORDER BY detected_at DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Conflict
	for rows.Next() {
		var (
			c        Conflict
			detected int64
		)
		if err := rows.Scan(&c.Path, &detected, &c.SHAA, &c.SHAB); err != nil {
			return nil, err
		}
		c.DetectedAt = time.Unix(0, detected)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Summary é o retrato do estado usado pelo -status.
type Summary struct {
	EntriesA     int
	EntriesB     int
	ByStatus     map[string]int
	Conflicts    int
	SchemaVer    int
	FirstSync    string
	LastResync   string
	Heartbeat    string
	Watches      string
	RetryQueue   string
	PID          string
	StateIsEmpty bool
}

// Summarize coleta os números para o relatório de status.
func (d *DB) Summarize(ctx context.Context) (Summary, error) {
	s := Summary{ByStatus: make(map[string]int)}

	rows, err := d.db.QueryContext(ctx,
		`SELECT side, status, COUNT(*) FROM entries GROUP BY side, status`)
	if err != nil {
		return s, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			side   int
			status string
			n      int
		)
		if err := rows.Scan(&side, &status, &n); err != nil {
			return s, err
		}
		if event.Side(side) == event.SideA {
			s.EntriesA += n
		} else {
			s.EntriesB += n
		}
		s.ByStatus[status] += n
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	s.StateIsEmpty = s.EntriesA == 0 && s.EntriesB == 0

	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conflicts WHERE resolution IS NULL`).Scan(&s.Conflicts); err != nil {
		return s, err
	}
	if err := d.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&s.SchemaVer); err != nil {
		return s, err
	}

	for key, dst := range map[string]*string{
		MetaFirstSync:  &s.FirstSync,
		MetaLastResync: &s.LastResync,
		MetaHeartbeat:  &s.Heartbeat,
		MetaWatches:    &s.Watches,
		MetaRetryQueue: &s.RetryQueue,
		MetaPID:        &s.PID,
	} {
		v, err := d.Meta(ctx, key)
		if err != nil {
			return s, err
		}
		*dst = v
	}
	return s, nil
}

// Meta lê um valor de configuração persistida.
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

// SetMetaMany grava vários valores em uma transação.
func (d *DB) SetMetaMany(ctx context.Context, kv map[string]string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for k, v := range kv {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO meta (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// Resolution é um pedido de resolução de conflito, gravado pelo CLI e
// aplicado pelo daemon.
type Resolution struct {
	Path        string
	Want        string
	RequestedAt time.Time
	Error       string
}

// RequestResolution grava (ou substitui) um pedido de resolução.
func (d *DB) RequestResolution(ctx context.Context, path, want string) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO resolutions (path, want, requested_at, applied_at, error)
		VALUES (?, ?, ?, NULL, NULL)
		ON CONFLICT(path) DO UPDATE SET
			want = excluded.want, requested_at = excluded.requested_at,
			applied_at = NULL, error = NULL`,
		path, want, time.Now().UnixNano())
	return err
}

// PendingResolutions lista os pedidos ainda não aplicados.
func (d *DB) PendingResolutions(ctx context.Context) ([]Resolution, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT path, want, requested_at, COALESCE(error, '')
		FROM resolutions WHERE applied_at IS NULL ORDER BY requested_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Resolution
	for rows.Next() {
		var (
			r         Resolution
			requested int64
		)
		if err := rows.Scan(&r.Path, &r.Want, &requested, &r.Error); err != nil {
			return nil, err
		}
		r.RequestedAt = time.Unix(0, requested)
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkResolutionApplied fecha um pedido. err vazio significa sucesso.
func (d *DB) MarkResolutionApplied(ctx context.Context, path, errMsg string) error {
	var e any
	if errMsg != "" {
		e = errMsg
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE resolutions SET applied_at = ?, error = ? WHERE path = ?`,
		time.Now().UnixNano(), e, path)
	return err
}

// ConflictDetail junta o conflito registrado com o estado conhecido dos dois
// lados, para o relatório de conflitos.
type ConflictDetail struct {
	Conflict
	EntryA *Entry
	EntryB *Entry
	Want   string // pedido de resolução pendente, se houver
}

// ConflictDetails devolve os conflitos abertos já com o contexto dos dois
// lados e eventual pedido pendente.
func (d *DB) ConflictDetails(ctx context.Context, limit int) ([]ConflictDetail, error) {
	conflicts, err := d.UnresolvedConflicts(ctx, limit)
	if err != nil {
		return nil, err
	}
	pending, err := d.PendingResolutions(ctx)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]string, len(pending))
	for _, r := range pending {
		wanted[r.Path] = r.Want
	}

	out := make([]ConflictDetail, 0, len(conflicts))
	for _, c := range conflicts {
		det := ConflictDetail{Conflict: c, Want: wanted[c.Path]}
		if det.EntryA, err = d.Get(ctx, event.SideA, c.Path); err != nil {
			return nil, err
		}
		if det.EntryB, err = d.Get(ctx, event.SideB, c.Path); err != nil {
			return nil, err
		}
		out = append(out, det)
	}
	return out, nil
}
