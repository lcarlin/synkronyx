package engine

import (
	"path/filepath"
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
