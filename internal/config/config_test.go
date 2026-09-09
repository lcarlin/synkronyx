package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
