// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/hash"
	"github.com/lcarlin/synkronyx/internal/state"
)

func TestConflictsListsBothSidesWithContext(t *testing.T) {
	cfg := statusConfig(t)
	ctx := context.Background()
	for _, root := range []string{cfg.A, cfg.B} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.A, "d.txt"), []byte("versão de A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.B, "d.txt"), []byte("versão de B, maior"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConflict(ctx, "d.txt", hash.Digest{}, hash.Digest{}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var buf bytes.Buffer
	if err := printConflicts(ctx, &buf, cfg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// O relatório precisa dar base para decidir: o path, os dois lados e o
	// comando que resolve.
	for _, want := range []string{"d.txt", "lado", "-resolve", "-with"} {
		if !strings.Contains(out, want) {
			t.Errorf("saída não contém %q:\n%s", want, out)
		}
	}
	// Tamanhos diferentes precisam aparecer, senão não há o que comparar.
	if !strings.Contains(out, "12 B") || !strings.Contains(out, "19 B") {
		t.Errorf("os tamanhos dos dois lados não aparecem:\n%s", out)
	}
}

func TestConflictsEmptyIsExplicit(t *testing.T) {
	cfg := statusConfig(t)
	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	var buf bytes.Buffer
	if err := printConflicts(context.Background(), &buf, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "nenhum conflito") {
		t.Errorf("saída = %q", buf.String())
	}
}

// Com o daemon vivo, o CLI enfileira em vez de escrever nas árvores: escrever
// direto faria o daemon ler as escritas como alteração externa e desfazer a
// resolução.
func TestResolveEnqueuesWhenDaemonAlive(t *testing.T) {
	cfg := statusConfig(t)
	cfg.HeartbeatInterval = time.Minute
	ctx := context.Background()
	for _, root := range []string{cfg.A, cfg.B} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, state.MetaHeartbeat, time.Now().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var buf bytes.Buffer
	if err := resolveConflictCmd(ctx, &buf, cfg, "d.txt", "a"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "pedido registrado") {
		t.Errorf("saída = %q, quero confirmação de enfileiramento", buf.String())
	}

	db, err = state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pending, err := db.PendingResolutions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Path != "d.txt" || pending[0].Want != "a" {
		t.Errorf("fila = %+v, quero um pedido para d.txt com \"a\"", pending)
	}
}

// Sem daemon, não há ninguém observando e o CLI aplica direto.
func TestResolveAppliesDirectlyWhenDaemonStopped(t *testing.T) {
	cfg := statusConfig(t)
	ctx := context.Background()
	for _, root := range []string{cfg.A, cfg.B} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.A, "d.txt"), []byte("de A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.B, "d.txt"), []byte("de B"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConflict(ctx, "d.txt", hash.Digest{}, hash.Digest{}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var buf bytes.Buffer
	if err := resolveConflictCmd(ctx, &buf, cfg, "d.txt", "a"); err != nil {
		t.Fatalf("resolveConflictCmd: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(cfg.B, "d.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "de A" {
		t.Errorf("B = %q, quero a versão de A aplicada na hora", got)
	}
}

func TestResolveRejectsUnknownChoice(t *testing.T) {
	cfg := statusConfig(t)
	if err := resolveConflictCmd(context.Background(), &bytes.Buffer{}, cfg, "d.txt", "ambos"); err == nil {
		t.Error("escolha inválida deveria ser recusada")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		1024:    "1.0 KiB",
		1536:    "1.5 KiB",
		1 << 20: "1.0 MiB",
		1 << 30: "1.0 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, quero %q", in, got, want)
		}
	}
}
