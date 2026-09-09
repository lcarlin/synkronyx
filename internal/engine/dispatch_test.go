package engine

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/event"
)

func TestPartitionKeepsSubtreeTogether(t *testing.T) {
	d := &dispatcher{workers: make([]chan event.Event, 8)}

	// Tudo sob "docs" precisa cair no mesmo worker, senão o CREATE do
	// diretório pode ser aplicado depois do CREATE do arquivo dentro dele.
	base := d.partition("docs")
	for _, p := range []string{
		filepath.Join("docs", "a.txt"),
		filepath.Join("docs", "sub", "b.txt"),
		filepath.Join("docs", "sub", "mais", "fundo", "c.txt"),
	} {
		if got := d.partition(p); got != base {
			t.Errorf("partition(%q) = %d, quero %d (mesma subárvore)", p, got, base)
		}
	}
}

func TestPartitionSpreadsAcrossWorkers(t *testing.T) {
	d := &dispatcher{workers: make([]chan event.Event, 4)}

	seen := make(map[int]bool)
	for _, p := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		seen[d.partition(p)] = true
	}
	if len(seen) < 2 {
		t.Errorf("todas as subárvores caíram em %d worker(s); a distribuição não funciona", len(seen))
	}
}

func TestCrossPartitionMoveNeedsBarrier(t *testing.T) {
	d := &dispatcher{workers: make([]chan event.Event, 8)}

	dentro := event.Event{Kind: event.KindMove, From: filepath.Join("docs", "a"), Path: filepath.Join("docs", "b")}
	if d.crossesPartitions(dentro) {
		t.Error("rename dentro da mesma subárvore não precisa de barreira")
	}

	// Renomear entre subárvores de primeiro nível toca duas partições.
	fora := event.Event{Kind: event.KindMove, From: "docs", Path: "fotos"}
	if !d.crossesPartitions(fora) {
		t.Error("rename entre subárvores precisa de barreira")
	}

	naoMove := event.Event{Kind: event.KindModify, Path: "docs/a"}
	if d.crossesPartitions(naoMove) {
		t.Error("evento que não é move nunca cruza partições")
	}
}

func TestTopComponent(t *testing.T) {
	cases := map[string]string{
		"a.txt":        "a.txt",
		"docs/a.txt":   "docs",
		"docs/sub/a":   "docs",
		"./docs/a.txt": "docs",
		".":            ".",
	}
	for in, want := range cases {
		if got := topComponent(filepath.FromSlash(in)); got != want {
			t.Errorf("topComponent(%q) = %q, quero %q", in, got, want)
		}
	}
}

// Eventos da mesma subárvore precisam ser aplicados na ordem em que chegaram,
// mesmo com vários workers — é a garantia que o particionamento oferece.
func TestDispatchPreservesOrderWithinSubtree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	order := make(map[string][]string)

	handle := func(_ context.Context, ev event.Event) error {
		mu.Lock()
		top := topComponent(ev.Path)
		order[top] = append(order[top], ev.Path)
		mu.Unlock()
		return nil
	}
	fail := func(event.Event, error) { t.Error("não deveria falhar") }

	d := newDispatcher(ctx, 4, handle, fail)

	var want []string
	for i := range 200 {
		p := filepath.Join("docs", "sub", string(rune('a'+i%26)), "f.txt")
		want = append(want, p)
		d.dispatch(ctx, event.Event{Kind: event.KindModify, Path: p}, handle, fail)
	}
	d.close()

	got := order["docs"]
	if len(got) != len(want) {
		t.Fatalf("processados %d eventos, quero %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ordem quebrada na posição %d: %q, quero %q", i, got[i], want[i])
		}
	}
}

// A barreira precisa garantir que nada mais está em voo quando um rename
// entre partições é aplicado.
func TestDispatchBarrierDrainsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	inFlight := 0
	maxDuringBarrier := 0
	barrierRan := false

	handle := func(_ context.Context, ev event.Event) error {
		if ev.Kind == event.KindMove {
			mu.Lock()
			barrierRan = true
			if inFlight > maxDuringBarrier {
				maxDuringBarrier = inFlight
			}
			mu.Unlock()
			return nil
		}
		mu.Lock()
		inFlight++
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}
	fail := func(event.Event, error) {}

	d := newDispatcher(ctx, 4, handle, fail)
	for i := range 40 {
		d.dispatch(ctx, event.Event{Kind: event.KindModify,
			Path: filepath.Join(string(rune('a'+i%8)), "f.txt")}, handle, fail)
	}
	d.dispatch(ctx, event.Event{Kind: event.KindMove, From: "a", Path: "z"}, handle, fail)
	d.close()

	mu.Lock()
	defer mu.Unlock()
	if !barrierRan {
		t.Fatal("o evento de barreira não foi processado")
	}
	if maxDuringBarrier != 0 {
		t.Errorf("%d eventos ainda em voo durante a barreira, quero 0", maxDuringBarrier)
	}
}
