// Package debounce agrupa eventos por path antes de entregá-los ao engine.
//
// A seção 4 do escopo pede isso explicitamente: um único evento de filesystem
// costuma ser uma etapa de uma operação maior. Salvar um arquivo em um editor
// pode produzir CREATE de um temporário, escrita, rename por cima do original
// e remoção do temporário — quatro eventos para uma alteração lógica.
//
// A regra é uma janela de silêncio por path: o evento só é liberado quando
// aquele path fica quieto pelo período configurado. Eventos subsequentes para
// o mesmo path reiniciam a janela e substituem o evento pendente, exceto por
// duas regras de precedência descritas em merge.
package debounce

import (
	"context"
	"sync"
	"time"

	"github.com/lcarlin/synkronyx/internal/event"
)

// Debouncer coalesce eventos por (lado, path).
type Debouncer struct {
	window time.Duration
	out    chan event.Event

	mu      sync.Mutex
	pending map[key]*slot
	closed  bool
}

type key struct {
	side event.Side
	path string
}

type slot struct {
	ev    event.Event
	timer *time.Timer
}

// New cria um Debouncer com a janela de silêncio dada.
func New(window time.Duration) *Debouncer {
	return &Debouncer{
		window:  window,
		out:     make(chan event.Event, 1024),
		pending: make(map[key]*slot),
	}
}

// Out é o canal de eventos já agrupados.
func (d *Debouncer) Out() <-chan event.Event { return d.out }

// Push entrega um evento ao debouncer.
func (d *Debouncer) Push(ctx context.Context, ev event.Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}

	// Move é o único evento que não pode esperar em silêncio: ele carrega o
	// path de origem, que só faz sentido enquanto ninguém mais mexeu ali.
	// Liberamos imediatamente, e de quebra invalidamos qualquer pendência
	// nos dois paths envolvidos.
	if ev.Kind == event.KindMove {
		d.cancelLocked(key{ev.Side, ev.From})
		d.cancelLocked(key{ev.Side, ev.Path})
		d.emit(ctx, ev)
		return
	}

	k := key{ev.Side, ev.Path}
	if s, ok := d.pending[k]; ok {
		s.ev = merge(s.ev, ev)
		s.timer.Reset(d.window)
		return
	}

	s := &slot{ev: ev}
	s.timer = time.AfterFunc(d.window, func() {
		d.mu.Lock()
		cur, ok := d.pending[k]
		if ok && cur == s {
			delete(d.pending, k)
		} else {
			ok = false
		}
		d.mu.Unlock()
		if ok {
			d.emit(ctx, s.ev)
		}
	})
	d.pending[k] = s
}

// merge decide qual evento representa a rajada.
//
// Duas regras de precedência:
//   - DELETE vence tudo. Se o arquivo acabou removido, o que aconteceu antes
//     é irrelevante.
//   - CREATE seguido de MODIFY continua sendo CREATE. O destino não conhece
//     o arquivo; tratá-lo como modificação de algo inexistente seria errado.
func merge(prev, next event.Event) event.Event {
	switch {
	case next.Kind == event.KindDelete:
		return next
	case prev.Kind == event.KindCreate && next.Kind == event.KindModify:
		prev.At = next.At
		return prev
	case prev.Kind == event.KindDelete && next.Kind == event.KindCreate:
		// Recriado dentro da janela: o resultado líquido é uma modificação.
		next.Kind = event.KindModify
		return next
	default:
		return next
	}
}

func (d *Debouncer) cancelLocked(k key) {
	if s, ok := d.pending[k]; ok {
		s.timer.Stop()
		delete(d.pending, k)
	}
}

func (d *Debouncer) emit(ctx context.Context, ev event.Event) {
	select {
	case d.out <- ev:
	case <-ctx.Done():
	}
}

// Close libera as pendências e fecha o canal de saída. Os eventos ainda em
// janela são descartados: quem chama Close está desligando o serviço, e o
// estado no disco é recuperado no próximo First Sync.
func (d *Debouncer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	for k, s := range d.pending {
		s.timer.Stop()
		delete(d.pending, k)
	}
	close(d.out)
}
