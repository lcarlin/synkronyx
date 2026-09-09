package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
)

var errBoom = errors.New("boom")

func TestRetryBackoffIsExponentialAndCapped(t *testing.T) {
	q := newRetryQueue(10, time.Second, 10*time.Second)

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := q.backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %s, quero %s", i+1, got, w)
		}
	}
}

func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	q := newRetryQueue(3, time.Millisecond, time.Second)
	ev := event.Event{Side: event.SideA, Kind: event.KindModify, Path: "f.txt"}
	now := time.Now()

	if it := q.schedule(ev, errBoom, now); it == nil || it.attempts != 1 {
		t.Fatalf("primeira falha deveria agendar tentativa 1, obtive %+v", it)
	}
	if it := q.schedule(ev, errBoom, now); it == nil || it.attempts != 2 {
		t.Fatalf("segunda falha deveria agendar tentativa 2, obtive %+v", it)
	}
	if it := q.schedule(ev, errBoom, now); it != nil {
		t.Fatalf("terceira falha deveria esgotar as tentativas, obtive %+v", it)
	}
	if q.len() != 0 {
		t.Errorf("fila = %d, quero 0 após esgotar", q.len())
	}
}

func TestRetryDisabledWhenMaxAttemptsZero(t *testing.T) {
	q := newRetryQueue(0, time.Second, time.Second)
	if q.enabled() {
		t.Error("fila deveria estar desabilitada")
	}
	if it := q.schedule(event.Event{Path: "f"}, errBoom, time.Now()); it != nil {
		t.Error("fila desabilitada não deveria agendar nada")
	}
}

func TestRetryDueRespectsDeadline(t *testing.T) {
	q := newRetryQueue(5, time.Minute, time.Hour)
	now := time.Now()
	q.schedule(event.Event{Side: event.SideA, Path: "f.txt"}, errBoom, now)

	if got := q.due(now); len(got) != 0 {
		t.Errorf("nada deveria estar vencido ainda, obtive %d", len(got))
	}
	if got := q.due(now.Add(2 * time.Minute)); len(got) != 1 {
		t.Errorf("item deveria ter vencido, obtive %d", len(got))
	}
	if q.len() != 0 {
		t.Error("due deveria remover o item da fila")
	}
}

func TestRetryCancelDropsPending(t *testing.T) {
	q := newRetryQueue(5, time.Millisecond, time.Second)
	ev := event.Event{Side: event.SideB, Path: "f.txt"}
	q.schedule(ev, errBoom, time.Now())

	q.cancel(event.SideB, "f.txt")
	if q.len() != 0 {
		t.Error("cancel não removeu a pendência")
	}
}

// Um evento novo para o mesmo path substitui o antigo: o que importa é o
// estado atual do disco, não a tentativa que falhou.
func TestRetryNewerEventReplacesQueued(t *testing.T) {
	q := newRetryQueue(5, time.Millisecond, time.Second)
	now := time.Now()

	q.schedule(event.Event{Side: event.SideA, Path: "f.txt", Kind: event.KindCreate}, errBoom, now)
	q.schedule(event.Event{Side: event.SideA, Path: "f.txt", Kind: event.KindDelete}, errBoom, now)

	if q.len() != 1 {
		t.Fatalf("fila = %d, quero 1", q.len())
	}
	due := q.due(now.Add(time.Hour))
	if len(due) != 1 || due[0].ev.Kind != event.KindDelete {
		t.Errorf("evento na fila = %v, quero o DELETE mais recente", due)
	}
}

// A sequência de esperas dos padrões é o que o exemplo de configuração
// documenta; se ela mudar sem o comentário mudar junto, o exemplo passa a
// mentir.
func TestDefaultBackoffSequence(t *testing.T) {
	cfg := config.Default()
	q := newRetryQueue(cfg.RetryMaxAttempts, cfg.RetryInitialDelay, cfg.RetryMaxDelay)

	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 320 * time.Second,
	}

	var total time.Duration
	for i, w := range want {
		got := q.backoff(i + 1)
		if got != w {
			t.Errorf("backoff(%d) = %s, quero %s", i+1, got, w)
		}
		total += got
	}
	if total != 10*time.Minute+35*time.Second {
		t.Errorf("janela total = %s, quero 10m35s", total)
	}

	// Com estes padrões o teto não é alcançado: a maior espera é 5m20s,
	// abaixo dos 10m de retry_max_delay. Ele só passa a cortar se
	// retry_max_attempts subir para 9 ou mais.
	if last := q.backoff(len(want)); last >= cfg.RetryMaxDelay {
		t.Errorf("backoff(%d) = %s, esperado abaixo do teto de %s",
			len(want), last, cfg.RetryMaxDelay)
	}
	if q.backoff(len(want)+1) != cfg.RetryMaxDelay {
		t.Error("a oitava espera deveria ser cortada pelo teto")
	}
}
