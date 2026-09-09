package guard

import (
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/event"
)

func TestConsumeMatchesExpectedWrite(t *testing.T) {
	g := New(time.Minute)
	g.Expect(event.SideB, "foo/bar.txt", 1)

	ev := event.Event{Side: event.SideB, Kind: event.KindModify, Path: "foo/bar.txt"}
	if !g.Consume(ev) {
		t.Fatal("evento da própria escrita deveria ter sido consumido")
	}
	if g.Consume(ev) {
		t.Fatal("expectativa de 1 evento não deveria cobrir um segundo evento")
	}
}

func TestConsumeIgnoresOtherSideAndPath(t *testing.T) {
	g := New(time.Minute)
	g.Expect(event.SideB, "foo.txt", 1)

	if g.Consume(event.Event{Side: event.SideA, Path: "foo.txt"}) {
		t.Error("expectativa do lado B não deveria casar com evento do lado A")
	}
	if g.Consume(event.Event{Side: event.SideB, Path: "outro.txt"}) {
		t.Error("expectativa de foo.txt não deveria casar com outro.txt")
	}
}

func TestExpectCountCoversBurst(t *testing.T) {
	g := New(time.Minute)
	g.Expect(event.SideA, "big.bin", 3)

	for i := range 3 {
		if !g.Consume(event.Event{Side: event.SideA, Path: "big.bin"}) {
			t.Fatalf("evento %d da rajada não foi consumido", i)
		}
	}
	if g.Consume(event.Event{Side: event.SideA, Path: "big.bin"}) {
		t.Error("quarto evento deveria passar como alteração externa")
	}
}

func TestExpiredExpectationLetsEventThrough(t *testing.T) {
	g := New(time.Millisecond)
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Expect(event.SideB, "lento.bin", 1)

	// Simula a escrita demorando mais que o TTL: o evento chega tarde e a
	// camada 1 falha. É exatamente o caso que a camada 2 (engine) cobre.
	g.now = func() time.Time { return now.Add(time.Hour) }
	if g.Consume(event.Event{Side: event.SideB, Path: "lento.bin"}) {
		t.Error("expectativa vencida não deveria consumir o evento")
	}
}

func TestMoveConsumesEitherEnd(t *testing.T) {
	g := New(time.Minute)
	g.Expect(event.SideB, "novo.txt", 1)

	ev := event.Event{Side: event.SideB, Kind: event.KindMove, From: "velho.txt", Path: "novo.txt"}
	if !g.Consume(ev) {
		t.Error("rename deveria casar pelo path de destino")
	}
}

func TestSweepRemovesExpired(t *testing.T) {
	g := New(time.Millisecond)
	now := time.Now()
	g.now = func() time.Time { return now }
	g.Expect(event.SideA, "a", 1)
	g.Expect(event.SideB, "b", 1)

	g.now = func() time.Time { return now.Add(time.Hour) }
	if n := g.Sweep(); n != 2 {
		t.Errorf("Sweep() = %d, quero 2", n)
	}
	if pending, _, _ := g.Stats(); pending != 0 {
		t.Errorf("pendentes = %d, quero 0", pending)
	}
}
