package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
)

func TestConflictNameKeepsExtension(t *testing.T) {
	at := time.Date(2026, 9, 9, 14, 30, 12, 0, time.UTC)

	cases := []struct {
		rel  string
		side event.Side
		want string
	}{
		{"file.txt", event.SideB, "file.sync-conflict-B-20260909-143012.txt"},
		{filepath.Join("dir", "sub", "doc.md"), event.SideA,
			filepath.Join("dir", "sub", "doc.sync-conflict-A-20260909-143012.md")},
		{"semext", event.SideA, "semext.sync-conflict-A-20260909-143012"},
		{"arquivo.tar.gz", event.SideB, "arquivo.tar.sync-conflict-B-20260909-143012.gz"},
	}
	for _, c := range cases {
		if got := conflictName(c.rel, c.side, at); got != c.want {
			t.Errorf("conflictName(%q) = %q, quero %q", c.rel, got, c.want)
		}
	}
}

// O nome gerado precisa casar com o exclude padrão, senão o próprio arquivo
// de conflito seria sincronizado — e conflitaria de novo.
func TestConflictNameIsExcludedByDefault(t *testing.T) {
	e := config.NewExcluder(config.Default().Exclude)
	name := conflictName(filepath.Join("dir", "file.txt"), event.SideB, time.Now())

	if !e.Excluded(name, false) {
		t.Errorf("%q não é excluído pelo exclude padrão", name)
	}
}

// A política padrão precisa fazer o que promete: nada é sobrescrito sem que a
// versão anterior continue no disco.
func TestPreservePolicyKeepsBothVersions(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, nil)
	ctx := context.Background()

	rel := "disputado.txt"
	srcAbs := filepath.Join(cfg.A, rel)
	dstAbs := filepath.Join(cfg.B, rel)
	if err := os.WriteFile(srcAbs, []byte("versao de A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dstAbs, []byte("versao de B, diferente"), 0o644); err != nil {
		t.Fatal(err)
	}

	ev := event.Event{Side: event.SideA, Kind: event.KindModify, Path: rel, At: time.Now()}
	if err := eng.resolveConflict(ctx, ev, srcAbs, dstAbs); err != nil {
		t.Fatalf("resolveConflict: %v", err)
	}

	// A versão de A venceu o path original...
	got, err := os.ReadFile(dstAbs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "versao de A" {
		t.Errorf("destino = %q, quero a versão da origem", got)
	}

	// ...e a de B continua existindo, com outro nome.
	entries, err := os.ReadDir(cfg.B)
	if err != nil {
		t.Fatal(err)
	}
	var preserved string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".sync-conflict-") {
			preserved = filepath.Join(cfg.B, e.Name())
		}
	}
	if preserved == "" {
		t.Fatal("versão do destino não foi preservada")
	}
	kept, err := os.ReadFile(preserved)
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != "versao de B, diferente" {
		t.Errorf("versão preservada = %q", kept)
	}

	// E o episódio fica registrado, com a resolução aplicada.
	open, err := db.UnresolvedConflicts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("conflito ficou aberto após resolução automática: %v", open)
	}
}

func TestNewerWinsPolicyPropagatesFromNewerSide(t *testing.T) {
	eng, cfg, _ := newIdleEngine(t, func(c *config.Config) {
		c.ConflictPolicy = config.ConflictNewerWins
	})
	ctx := context.Background()

	rel := "disputado.txt"
	srcAbs := filepath.Join(cfg.A, rel)
	dstAbs := filepath.Join(cfg.B, rel)
	if err := os.WriteFile(srcAbs, []byte("antigo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dstAbs, []byte("mais recente"), 0o644); err != nil {
		t.Fatal(err)
	}

	// O destino é explicitamente mais novo que a origem.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(srcAbs, old, old); err != nil {
		t.Fatal(err)
	}

	ev := event.Event{Side: event.SideA, Kind: event.KindModify, Path: rel, At: time.Now()}
	if err := eng.resolveConflict(ctx, ev, srcAbs, dstAbs); err != nil {
		t.Fatal(err)
	}

	// A propagação foi na direção contrária à do evento, porque o mtime manda.
	got, err := os.ReadFile(srcAbs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "mais recente" {
		t.Errorf("origem = %q, quero a versão mais recente do destino", got)
	}
}

// A política manual não pode tocar em arquivo nenhum: ela existe para quem
// prefere resolver à mão.
func TestManualPolicyTouchesNothing(t *testing.T) {
	eng, cfg, db := newIdleEngine(t, func(c *config.Config) {
		c.ConflictPolicy = config.ConflictManual
	})
	ctx := context.Background()

	rel := "disputado.txt"
	srcAbs := filepath.Join(cfg.A, rel)
	dstAbs := filepath.Join(cfg.B, rel)
	if err := os.WriteFile(srcAbs, []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dstAbs, []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}

	ev := event.Event{Side: event.SideA, Kind: event.KindModify, Path: rel, At: time.Now()}
	if err := eng.resolveConflict(ctx, ev, srcAbs, dstAbs); err != nil {
		t.Fatal(err)
	}

	for abs, want := range map[string]string{srcAbs: "A", dstAbs: "B"} {
		got, err := os.ReadFile(abs)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, quero %q intocado", abs, got, want)
		}
	}

	open, err := db.UnresolvedConflicts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Errorf("conflitos abertos = %d, quero 1 aguardando intervenção", len(open))
	}
}
