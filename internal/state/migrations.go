package state

import (
	"database/sql"
	"fmt"
)

// schemaVersion é a versão de schema que este binário espera. Guardada no
// PRAGMA user_version do próprio arquivo SQLite.
const schemaVersion = 3

// migrations[i] leva o banco da versão i para i+1. Só se acrescenta ao fim;
// nunca se edita uma entrada já publicada, porque bancos em produção já a
// aplicaram.
var migrations = []string{
	// v1 -> v2: a coluna sha256 passou a guardar um digest tipado
	// ("sha256:<hex>" ou "sha256p:<hex>:<tamanho>"), então o nome antigo
	// virou mentira. Os valores gravados continuam válidos: hash.ParseDigest
	// aceita hex puro como digest completo.
	`ALTER TABLE entries RENAME COLUMN sha256 TO digest;`,

	// v2 -> v3: resolução assistida de conflitos. A tabela é o canal entre o
	// CLI e o daemon: resolver um conflito escrevendo direto nas árvores
	// enquanto o serviço roda dispara eventos que desfazem a resolução, então
	// o pedido é gravado aqui e aplicado por quem tem o guard em mãos.
	`CREATE TABLE IF NOT EXISTS resolutions (
		path         TEXT    NOT NULL PRIMARY KEY,
		want         TEXT    NOT NULL,
		requested_at INTEGER NOT NULL,
		applied_at   INTEGER,
		error        TEXT
	) WITHOUT ROWID;`,
}

// migrate aplica as migrações pendentes.
func migrate(db *sql.DB) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("lendo user_version: %w", err)
	}

	if current > schemaVersion {
		// Um binário mais antigo abrindo um banco mais novo. Seguir em frente
		// corromperia dados de forma silenciosa; melhor recusar.
		return fmt.Errorf("banco de estado está na versão %d, mas este binário só entende até %d: "+
			"atualize o synkronyx", current, schemaVersion)
	}
	if current == schemaVersion {
		return nil
	}

	// Um banco recém-criado pelo schema.sql está na v1: as tabelas existem,
	// mas o user_version nunca foi gravado.
	if current == 0 {
		current = 1
	}

	for v := current; v < schemaVersion; v++ {
		stmt := migrations[v-1]
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migração v%d -> v%d: %w", v, v+1, err)
		}
	}

	// PRAGMA não aceita parâmetro vinculado; a interpolação é de uma
	// constante do próprio código, não de entrada externa.
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("gravando user_version: %w", err)
	}
	return nil
}
