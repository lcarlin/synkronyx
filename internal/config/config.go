// Package config carrega e valida a configuração do daemon.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lcarlin/synkronyx/internal/hash"

	"gopkg.in/yaml.v3"
)

// Config é a configuração completa do serviço.
type Config struct {
	// A e B são as duas raízes da árvore lógica. Caminhos absolutos.
	A string `yaml:"a"`
	B string `yaml:"b"`

	// StatePath é o arquivo SQLite de estado.
	StatePath string `yaml:"state_path"`

	// Debounce é a janela de silêncio aplicada por path antes de agir.
	// Um write grande gera vários MODIFY; só o fim da rajada interessa.
	Debounce time.Duration `yaml:"debounce"`

	// SelfWriteTTL é por quanto tempo uma escrita feita pelo próprio
	// sincronizador continua sendo reconhecida como tal (ver internal/guard).
	SelfWriteTTL time.Duration `yaml:"self_write_ttl"`

	// Exclude são padrões (filepath.Match, aplicados a cada componente do
	// path relativo) ignorados nos dois lados.
	Exclude []string `yaml:"exclude"`

	// ConflictPolicy define o que fazer quando os dois lados divergem.
	ConflictPolicy ConflictPolicy `yaml:"conflict_policy"`

	// FirstSyncPolicy define como resolver divergências pré-existentes na
	// primeira execução, antes de os watchers subirem.
	FirstSyncPolicy FirstSyncPolicy `yaml:"first_sync_policy"`

	// RsyncPath e RsyncArgs permitem ajustar a chamada ao rsync.
	RsyncPath string   `yaml:"rsync_path"`
	RsyncArgs []string `yaml:"rsync_args"`

	// LogLevel: debug|info|warn|error.
	LogLevel string `yaml:"log_level"`

	// HashMaxBytes é o tamanho a partir do qual o digest passa a ser
	// amostrado em vez de completo. 0 = sempre completo.
	HashMaxBytes int64 `yaml:"hash_max_bytes"`

	// HashSampleBytes é quanto se lê de cada extremidade no digest amostrado.
	HashSampleBytes int64 `yaml:"hash_sample_bytes"`

	// PreserveHardlinks adiciona --hard-links ao rsync nas cópias de árvore.
	// Ver a nota em transfer.CopyTree sobre o alcance real da opção.
	PreserveHardlinks bool `yaml:"preserve_hardlinks"`

	// DirDeletePolicy decide o que fazer ao propagar a remoção de um
	// diretório que, no destino, contém conteúdo que nunca foi sincronizado.
	DirDeletePolicy DirDeletePolicy `yaml:"dir_delete_policy"`

	// Retry* controlam a fila de reenvio de operações que falharam.
	RetryMaxAttempts  int           `yaml:"retry_max_attempts"`
	RetryInitialDelay time.Duration `yaml:"retry_initial_delay"`
	RetryMaxDelay     time.Duration `yaml:"retry_max_delay"`

	// HeartbeatInterval é a periodicidade com que o daemon publica seu
	// estado operacional no banco, para o -status poder lê-lo.
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`

	// SpecialFiles decide o que fazer com sockets, FIFOs e device nodes.
	SpecialFiles SpecialFilesPolicy `yaml:"special_files"`

	// SyncWorkers é quantos eventos podem ser processados em paralelo. O
	// particionamento é por subárvore de primeiro nível — ver engine.dispatch.
	SyncWorkers int `yaml:"sync_workers"`

	// ProgressInterval é de quanto em quanto tempo um scan longo reporta
	// progresso. 0 desliga o relatório.
	ProgressInterval time.Duration `yaml:"progress_interval"`
}

// SpecialFilesPolicy — o que fazer com o que não é arquivo, diretório nem
// symlink.
type SpecialFilesPolicy string

const (
	// SpecialFilesSkip ignora sockets, FIFOs e device nodes. É o padrão, e a
	// razão está em engine.skipSpecial.
	SpecialFilesSkip SpecialFilesPolicy = "skip"
	// SpecialFilesError registra erro para cada um encontrado, em vez de
	// ignorá-lo em silêncio. Para quem prefere descobrir que a árvore tem
	// conteúdo não sincronizável.
	SpecialFilesError SpecialFilesPolicy = "error"
)

// DirDeletePolicy — resposta ao caso da seção 16 (Fail Safe) em que a remoção
// de um diretório apagaria dados que só existem no destino.
type DirDeletePolicy string

const (
	// DirDeletePreserveUnknown remove o diretório, mas antes salva em um
	// diretório de conflito tudo o que o estado não conhece — isto é, o que
	// foi criado no destino e nunca chegou à origem. É o padrão.
	DirDeletePreserveUnknown DirDeletePolicy = "preserve-unknown"
	// DirDeleteForce remove o diretório inteiro sem inspecionar.
	DirDeleteForce DirDeletePolicy = "force"
)

// ConflictPolicy — política inicial recomendada pelo escopo é Preserve.
type ConflictPolicy string

const (
	// ConflictPreserve registra em log e mantém as duas versões, renomeando
	// a perdedora para <nome>.sync-conflict-<lado>-<timestamp><ext>.
	ConflictPreserve ConflictPolicy = "preserve"
	// ConflictNewerWins sobrescreve pelo mtime mais recente.
	ConflictNewerWins ConflictPolicy = "newer-wins"
	// ConflictManual não toca em nada; apenas marca o par como em conflito.
	ConflictManual ConflictPolicy = "manual"
)

// FirstSyncPolicy — resolução de divergências no First Sync.
type FirstSyncPolicy string

const (
	// FirstSyncUnion propaga o que existe só de um lado para o outro; para
	// arquivos presentes nos dois com conteúdo diferente, aplica ConflictPolicy.
	FirstSyncUnion FirstSyncPolicy = "union"
	// FirstSyncAWins trata A como autoritativo.
	FirstSyncAWins FirstSyncPolicy = "a-wins"
	// FirstSyncBWins trata B como autoritativo.
	FirstSyncBWins FirstSyncPolicy = "b-wins"
)

// Default devolve a configuração padrão, antes de qualquer arquivo.
func Default() Config {
	return Config{
		StatePath:       "/var/lib/synkronyx/state.db",
		Debounce:        time.Second,
		SelfWriteTTL:    30 * time.Second,
		ConflictPolicy:  ConflictPreserve,
		FirstSyncPolicy: FirstSyncUnion,
		RsyncPath:       "rsync",
		RsyncArgs:       []string{"--archive", "--partial", "--inplace", "--numeric-ids"},
		LogLevel:        "info",
		Exclude:         []string{".synkronyx", ".synkronyx-tmp-*", "*.sync-conflict-*"},

		HashSampleBytes:   hash.DefaultSampleBytes,
		PreserveHardlinks: false,
		DirDeletePolicy:   DirDeletePreserveUnknown,

		RetryMaxAttempts:  8,
		RetryInitialDelay: 2 * time.Second,
		RetryMaxDelay:     5 * time.Minute,

		HeartbeatInterval: 30 * time.Second,

		SpecialFiles:     SpecialFilesSkip,
		SyncWorkers:      4,
		ProgressInterval: 15 * time.Second,
	}
}

// Load lê o YAML em path sobre os defaults e valida o resultado.
func Load(path string) (Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("lendo config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parseando config %s: %w", path, err)
	}
	if err := cfg.normalize(); err != nil {
		return Config{}, err
	}
	return cfg, cfg.Validate()
}

func (c *Config) normalize() error {
	for _, p := range []*string{&c.A, &c.B, &c.StatePath} {
		if *p == "" {
			continue
		}
		abs, err := filepath.Abs(*p)
		if err != nil {
			return fmt.Errorf("resolvendo path %q: %w", *p, err)
		}
		*p = filepath.Clean(abs)
	}
	return nil
}

// Validate rejeita configurações que não teriam como funcionar.
func (c Config) Validate() error {
	var errs []error

	if c.A == "" || c.B == "" {
		errs = append(errs, errors.New("a e b são obrigatórios"))
	}
	if c.A != "" && c.A == c.B {
		errs = append(errs, errors.New("a e b não podem ser o mesmo diretório"))
	}
	if c.A != "" && c.B != "" {
		// Aninhamento produz recursão infinita de eventos: A escreve em B,
		// que está dentro de A, que dispara evento em A...
		if isUnder(c.B, c.A) || isUnder(c.A, c.B) {
			errs = append(errs, fmt.Errorf("a (%s) e b (%s) não podem estar aninhados", c.A, c.B))
		}
	}
	for _, root := range []string{c.A, c.B} {
		if root == "" {
			continue
		}
		st, err := os.Stat(root)
		if err != nil {
			errs = append(errs, fmt.Errorf("raiz %s: %w", root, err))
			continue
		}
		if !st.IsDir() {
			errs = append(errs, fmt.Errorf("raiz %s não é um diretório", root))
		}
	}
	if c.StatePath == "" {
		errs = append(errs, errors.New("state_path é obrigatório"))
	}
	if c.Debounce <= 0 {
		errs = append(errs, errors.New("debounce deve ser > 0"))
	}
	if c.SelfWriteTTL <= 0 {
		errs = append(errs, errors.New("self_write_ttl deve ser > 0"))
	}
	switch c.ConflictPolicy {
	case ConflictPreserve, ConflictNewerWins, ConflictManual:
	default:
		errs = append(errs, fmt.Errorf("conflict_policy inválida: %q", c.ConflictPolicy))
	}
	switch c.FirstSyncPolicy {
	case FirstSyncUnion, FirstSyncAWins, FirstSyncBWins:
	default:
		errs = append(errs, fmt.Errorf("first_sync_policy inválida: %q", c.FirstSyncPolicy))
	}
	switch c.DirDeletePolicy {
	case DirDeletePreserveUnknown, DirDeleteForce:
	default:
		errs = append(errs, fmt.Errorf("dir_delete_policy inválida: %q", c.DirDeletePolicy))
	}
	if c.HashMaxBytes < 0 {
		errs = append(errs, errors.New("hash_max_bytes não pode ser negativo"))
	}
	if c.HashMaxBytes > 0 && c.HashSampleBytes <= 0 {
		errs = append(errs, errors.New("hash_sample_bytes deve ser > 0 quando hash_max_bytes está ativo"))
	}
	if c.HashMaxBytes > 0 && c.HashSampleBytes*2 >= c.HashMaxBytes {
		// Amostrar 2x mais do que o limite significa ler o arquivo todo de
		// qualquer jeito: a configuração não faria o que promete.
		errs = append(errs, fmt.Errorf(
			"hash_sample_bytes (%d) x2 deve ser menor que hash_max_bytes (%d), senão a amostra cobre o arquivo inteiro",
			c.HashSampleBytes, c.HashMaxBytes))
	}
	if c.RetryMaxAttempts < 0 {
		errs = append(errs, errors.New("retry_max_attempts não pode ser negativo"))
	}
	if c.RetryMaxAttempts > 0 {
		if c.RetryInitialDelay <= 0 {
			errs = append(errs, errors.New("retry_initial_delay deve ser > 0"))
		}
		if c.RetryMaxDelay < c.RetryInitialDelay {
			errs = append(errs, errors.New("retry_max_delay deve ser >= retry_initial_delay"))
		}
	}
	if c.HeartbeatInterval <= 0 {
		errs = append(errs, errors.New("heartbeat_interval deve ser > 0"))
	}
	switch c.SpecialFiles {
	case SpecialFilesSkip, SpecialFilesError:
	default:
		errs = append(errs, fmt.Errorf("special_files inválida: %q", c.SpecialFiles))
	}
	if c.SyncWorkers < 1 {
		errs = append(errs, errors.New("sync_workers deve ser >= 1"))
	}
	if c.ProgressInterval < 0 {
		errs = append(errs, errors.New("progress_interval não pode ser negativo"))
	}

	return errors.Join(errs...)
}

// isUnder informa se child está dentro de parent (ou é o próprio parent).
func isUnder(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !filepath.IsAbs(rel) &&
		!hasDotDotPrefix(rel))
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[0] == '.' && rel[1] == '.' && rel[2] == filepath.Separator
}
