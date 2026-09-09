-- Estado do Synkronyx (seção 7 do escopo).
--
-- O banco é deliberadamente pequeno: guarda o que foi sincronizado por
-- último, por lado, para responder a uma única pergunta — "o que estou vendo
-- agora é diferente do que eu mesmo escrevi?". É essa resposta que distingue
-- alteração externa, eco do sincronizador e conflito.

PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS entries (
    path        TEXT    NOT NULL,          -- path relativo à raiz do lado
    side        INTEGER NOT NULL,          -- 0 = A, 1 = B
    is_dir      INTEGER NOT NULL DEFAULT 0,
    size        INTEGER NOT NULL DEFAULT 0,
    mtime_ns    INTEGER NOT NULL DEFAULT 0,
    mode        INTEGER NOT NULL DEFAULT 0,
    sha256      TEXT,                      -- NULL quando não calculado (diretório, ou acima do limite)
    synced_at   INTEGER NOT NULL DEFAULT 0, -- unix ns da última sincronização bem-sucedida
    status      TEXT    NOT NULL DEFAULT 'synced', -- synced | pending | conflict | error
    PRIMARY KEY (path, side)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_entries_status ON entries (status) WHERE status <> 'synced';

CREATE TABLE IF NOT EXISTS conflicts (
    path         TEXT    NOT NULL PRIMARY KEY,
    detected_at  INTEGER NOT NULL,
    sha_a        TEXT,
    sha_b        TEXT,
    resolution   TEXT,                     -- NULL enquanto não resolvido
    resolved_at  INTEGER
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
) WITHOUT ROWID;
