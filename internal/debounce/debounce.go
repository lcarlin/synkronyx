// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

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

	// done é fechado por Close e libera qualquer emissão bloqueada. Sem ele,
	// Close esperaria para sempre por um emit preso num canal cheio que
	// ninguém mais lê.
	done chan struct{}

	// emitting conta as emissões em voo. Close espera por elas antes de
	// fechar out — timer.Stop() não ajuda aqui, porque um timer que já
	// disparou e liberou a trava está a caminho do envio, e Stop() devolve
	// false sem poder impedi-lo.
	emitting sync.WaitGroup

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
		done:    make(chan struct{}),
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
		// O registro da emissão acontece sob a mesma trava que decide se o
		// debouncer já fechou. É isso que garante que, depois de Close
		// liberar a trava, nenhuma emissão nova possa começar — e portanto
		// que esperar as em voo seja suficiente.
		if ok && !d.closed {
			d.emitting.Add(1)
		} else {
			ok = false
		}
		d.mu.Unlock()

		if ok {
			defer d.emitting.Done()
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
	case <-d.done:
		// Fechando: o evento é descartado. O First Sync do próximo boot
		// cobre o que ficou pelo caminho.
	case <-ctx.Done():
	}
}

// Close libera as pendências e fecha o canal de saída. Os eventos ainda em
// janela são descartados: quem chama Close está desligando o serviço, e o
// estado no disco é recuperado no próximo First Sync.
//
// A ordem das três etapas é o que torna o fechamento seguro, e cada uma
// resolve um problema distinto:
//
//  1. Marcar como fechado e liberar `done`, sob a trava. A partir daqui
//     nenhuma emissão nova começa, e as bloqueadas em um canal cheio saem.
//  2. Esperar as emissões em voo. Um timer que já disparou e liberou a trava
//     está a caminho do envio, e timer.Stop() não tem como impedi-lo — só
//     devolve false. Fechar o canal aqui seria enviar em canal fechado.
//  3. Fechar o canal, agora que ninguém mais escreve nele.
func (d *Debouncer) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	close(d.done)
	for k, s := range d.pending {
		s.timer.Stop()
		delete(d.pending, k)
	}
	d.mu.Unlock()

	d.emitting.Wait()
	close(d.out)
}
