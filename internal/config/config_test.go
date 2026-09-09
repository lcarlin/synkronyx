package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateRejectsNestedRoots(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "dentro")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := Default()
	cfg.A = dir
	cfg.B = inner

	// Raízes aninhadas fazem cada escrita em B disparar um evento em A,
	// indefinidamente. É melhor recusar a configuração do que descobrir isso
	// em produção.
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "aninhados") {
		t.Fatalf("Validate() = %v, quero erro de aninhamento", err)
	}
}

func TestValidateRejectsIdenticalRoots(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.A, cfg.B = dir, dir

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() aceitou A == B")
	}
}

func TestValidateAcceptsSiblingRoots(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "A"), filepath.Join(dir, "B")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := Default()
	cfg.A, cfg.B = a, b
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, quero nil", err)
	}
}

func TestLoadAppliesDefaultsForOmittedFields(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "A"), filepath.Join(dir, "B")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(dir, "cfg.yaml")
	body := "a: " + a + "\nb: " + b + "\nstate_path: " + filepath.Join(dir, "s.db") + "\ndebounce: 2s\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Debounce.String() != "2s" {
		t.Errorf("Debounce = %s, quero 2s", cfg.Debounce)
	}
	if cfg.ConflictPolicy != ConflictPreserve {
		t.Errorf("ConflictPolicy = %q, quero o default %q", cfg.ConflictPolicy, ConflictPreserve)
	}
	if len(cfg.RsyncArgs) == 0 {
		t.Error("RsyncArgs default foi perdido")
	}
}

func TestExcluderMatchesAnyPathComponent(t *testing.T) {
	e := NewExcluder([]string{"node_modules", "*.swp", ".git/objects"})

	cases := []struct {
		rel  string
		want bool
	}{
		{"node_modules", true},
		{filepath.Join("app", "node_modules", "x.js"), true},
		{filepath.Join("doc", "notas.swp"), true},
		{filepath.Join(".git", "objects"), true},
		{filepath.Join("src", "main.go"), false},
		{"nodemodules", false},
		{".", false},
	}
	for _, c := range cases {
		if got := e.Excluded(c.rel, false); got != c.want {
			t.Errorf("Excluded(%q) = %v, quero %v", c.rel, got, c.want)
		}
	}
}

func TestValidateRejectsSampleLargerThanLimit(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "A"), filepath.Join(dir, "B")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := Default()
	cfg.A, cfg.B = a, b
	cfg.HashMaxBytes = 1000
	cfg.HashSampleBytes = 800 // 2x800 > 1000: a amostra cobriria o arquivo todo

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "hash_sample_bytes") {
		t.Fatalf("Validate() = %v, quero erro sobre a amostra", err)
	}
}

func TestValidateRejectsInconsistentRetryDelays(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "A"), filepath.Join(dir, "B")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := Default()
	cfg.A, cfg.B = a, b
	cfg.RetryInitialDelay = time.Minute
	cfg.RetryMaxDelay = time.Second

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() aceitou retry_max_delay menor que o inicial")
	}
}

func TestValidateAcceptsRetryDisabled(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "A"), filepath.Join(dir, "B")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := Default()
	cfg.A, cfg.B = a, b
	cfg.RetryMaxAttempts = 0

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, quero nil com retry desligado", err)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	// O exemplo é a primeira coisa que alguém copia; ele quebrar é uma falha
	// de verdade, não um detalhe de documentação.
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "synkronyx.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	a, b := filepath.Join(dir, "A"), filepath.Join(dir, "B")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// As raízes do exemplo não existem na máquina de teste; o resto do
	// arquivo é o que interessa validar.
	body := strings.ReplaceAll(string(raw), "a: /dados/A", "a: "+a)
	body = strings.ReplaceAll(body, "b: /dados/B", "b: "+b)
	body = strings.ReplaceAll(body, "state_path: /var/lib/synkronyx/state.db",
		"state_path: "+filepath.Join(dir, "state.db"))

	path := filepath.Join(dir, "exemplo.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("configs/synkronyx.example.yaml não passa na validação: %v", err)
	}
}

// O padrão de sync_workers é uma decisão de projeto, não um detalhe: fixá-lo
// em teste faz uma mudança acidental aparecer no diff.
func TestDefaultSyncWorkers(t *testing.T) {
	if got := Default().SyncWorkers; got != 4 {
		t.Errorf("SyncWorkers padrão = %d, quero 4", got)
	}
}

func TestValidateRejectsZeroWorkers(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "A"), filepath.Join(dir, "B")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := Default()
	cfg.A, cfg.B = a, b
	cfg.SyncWorkers = 0

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() aceitou sync_workers = 0")
	}
}

func TestDefaultRetryAttempts(t *testing.T) {
	if got := Default().RetryMaxAttempts; got != 8 {
		t.Errorf("RetryMaxAttempts padrão = %d, quero 8", got)
	}
}
