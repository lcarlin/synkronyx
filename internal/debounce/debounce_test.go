package debounce

import (
	"context"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/event"
)

func TestCoalescesBurstIntoOneEvent(t *testing.T) {
	d := New(30 * time.Millisecond)
	defer d.Close()
	ctx := context.Background()

	for range 5 {
		d.Push(ctx, event.Event{Side: event.SideA, Kind: event.KindModify, Path: "f.txt"})
	}

	got := drain(t, d, 500*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("rajada de 5 produziu %d eventos, quero 1: %v", len(got), got)
	}
	if got[0].Kind != event.KindModify {
		t.Errorf("kind = %s, quero MODIFY", got[0].Kind)
	}
}

func TestDeleteWinsOverEarlierKinds(t *testing.T) {
	d := New(30 * time.Millisecond)
	defer d.Close()
	ctx := context.Background()

	d.Push(ctx, event.Event{Side: event.SideA, Kind: event.KindCreate, Path: "tmp"})
	d.Push(ctx, event.Event{Side: event.SideA, Kind: event.KindModify, Path: "tmp"})
	d.Push(ctx, event.Event{Side: event.SideA, Kind: event.KindDelete, Path: "tmp"})

	got := drain(t, d, 500*time.Millisecond)
	if len(got) != 1 || got[0].Kind != event.KindDelete {
		t.Fatalf("quero um único DELETE, obtive %v", got)
	}
}

func TestCreateSurvivesFollowingModify(t *testing.T) {
	d := New(30 * time.Millisecond)
	defer d.Close()
	ctx := context.Background()

	d.Push(ctx, event.Event{Side: event.SideA, Kind: event.KindCreate, Path: "novo.txt"})
	d.Push(ctx, event.Event{Side: event.SideA, Kind: event.KindModify, Path: "novo.txt"})

	got := drain(t, d, 500*time.Millisecond)
	if len(got) != 1 || got[0].Kind != event.KindCreate {
		t.Fatalf("quero um único CREATE, obtive %v", got)
	}
}

func TestMoveIsEmittedImmediately(t *testing.T) {
	d := New(10 * time.Second) // janela longa: só um bypass faria o teste passar
	defer d.Close()

	d.Push(context.Background(), event.Event{
		Side: event.SideA, Kind: event.KindMove, From: "a.txt", Path: "b.txt",
	})

	select {
	case ev := <-d.Out():
		if ev.Kind != event.KindMove || ev.From != "a.txt" || ev.Path != "b.txt" {
			t.Errorf("evento inesperado: %v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("MOVE não foi emitido imediatamente")
	}
}

func TestDistinctPathsAreIndependent(t *testing.T) {
	d := New(30 * time.Millisecond)
	defer d.Close()
	ctx := context.Background()

	d.Push(ctx, event.Event{Side: event.SideA, Kind: event.KindModify, Path: "um.txt"})
	d.Push(ctx, event.Event{Side: event.SideA, Kind: event.KindModify, Path: "dois.txt"})
	d.Push(ctx, event.Event{Side: event.SideB, Kind: event.KindModify, Path: "um.txt"})

	if got := drain(t, d, 500*time.Millisecond); len(got) != 3 {
		t.Fatalf("quero 3 eventos independentes, obtive %d: %v", len(got), got)
	}
}

// drain coleta os eventos liberados até o canal ficar quieto.
func drain(t *testing.T, d *Debouncer, timeout time.Duration) []event.Event {
	t.Helper()

	var got []event.Event
	deadline := time.After(timeout)
	quiet := time.NewTimer(150 * time.Millisecond)
	defer quiet.Stop()

	for {
		select {
		case ev := <-d.Out():
			got = append(got, ev)
			quiet.Reset(150 * time.Millisecond)
		case <-quiet.C:
			return got
		case <-deadline:
			return got
		}
	}
}
