// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package engine

import (
	"time"

	"github.com/lcarlin/synkronyx/internal/event"
)

// retryQueue reenfileira operações que falharam.
//
// A alternativa anterior era descartar o evento e confiar no próximo Full
// Resync, o que funciona mas é lento e desproporcional: um disco
// momentaneamente cheio ou um arquivo travado por outro processo não deveriam
// exigir uma varredura completa das duas árvores para se resolver.
//
// A fila é intencionalmente simples — um mapa por (lado, path), sem
// persistência. Se o processo morrer, o First Sync do próximo boot cobre o que
// estava pendente; persistir a fila daria durabilidade que o resync já
// oferece, ao custo de mais estado para manter coerente.
type retryQueue struct {
	items map[retryKey]*retryItem

	maxAttempts  int
	initialDelay time.Duration
	maxDelay     time.Duration
}

type retryKey struct {
	side event.Side
	path string
}

type retryItem struct {
	ev       event.Event
	attempts int
	nextAt   time.Time
	lastErr  error
}

func newRetryQueue(maxAttempts int, initialDelay, maxDelay time.Duration) *retryQueue {
	return &retryQueue{
		items:        make(map[retryKey]*retryItem),
		maxAttempts:  maxAttempts,
		initialDelay: initialDelay,
		maxDelay:     maxDelay,
	}
}

// enabled informa se o retry está ligado na configuração.
func (q *retryQueue) enabled() bool { return q.maxAttempts > 0 }

// schedule registra uma falha e devolve o item agendado, ou nil se as
// tentativas se esgotaram (caso em que o item sai da fila).
func (q *retryQueue) schedule(ev event.Event, err error, now time.Time) *retryItem {
	if !q.enabled() {
		return nil
	}

	k := retryKey{ev.Side, ev.Path}
	it, ok := q.items[k]
	if !ok {
		it = &retryItem{ev: ev}
		q.items[k] = it
	}
	// Um evento mais novo para o mesmo path substitui o antigo: é o estado
	// atual do disco que interessa, não a tentativa que falhou.
	it.ev = ev
	it.attempts++
	it.lastErr = err

	if it.attempts >= q.maxAttempts {
		delete(q.items, k)
		return nil
	}

	it.nextAt = now.Add(q.backoff(it.attempts))
	return it
}

// backoff é exponencial, limitado por maxDelay.
func (q *retryQueue) backoff(attempt int) time.Duration {
	d := q.initialDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= q.maxDelay {
			return q.maxDelay
		}
	}
	return d
}

// cancel remove qualquer pendência para (lado, path). Chamado quando uma
// tentativa dá certo, ou quando um evento novo torna a antiga irrelevante.
func (q *retryQueue) cancel(side event.Side, path string) {
	delete(q.items, retryKey{side, path})
}

// due devolve os itens cujo prazo venceu, removendo-os da fila. Quem chama
// reprocessa e, se falhar de novo, reagenda via schedule.
func (q *retryQueue) due(now time.Time) []*retryItem {
	var out []*retryItem
	for k, it := range q.items {
		if !it.nextAt.After(now) {
			out = append(out, it)
			delete(q.items, k)
		}
	}
	return out
}

func (q *retryQueue) len() int { return len(q.items) }
