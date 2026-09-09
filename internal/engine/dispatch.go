// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package engine

import (
	"context"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lcarlin/synkronyx/internal/event"
)

// dispatcher distribui eventos entre workers, particionando por subárvore.
//
// # Por que particionar por subárvore, e não por evento
//
// Processar eventos em paralelo sem critério quebra ordem: o CREATE de um
// diretório e o CREATE de um arquivo dentro dele podem ser aplicados fora de
// ordem, e o segundo falha porque o primeiro ainda não aconteceu.
//
// A partição é o primeiro componente do path relativo. Isso garante que um
// diretório de primeiro nível e tudo abaixo dele caem sempre no mesmo worker,
// preservando a ordem relativa dentro da subárvore — que é a única ordem que
// importa. Eventos em subárvores distintas são independentes por construção.
//
// # A exceção: renames entre partições
//
// Um rename de "docs/x" para "fotos/x" toca duas partições ao mesmo tempo, e
// nenhum worker sozinho pode aplicá-lo com segurança. Esses eventos passam por
// uma barreira: o dispatcher espera todos os workers ficarem ociosos e
// processa o evento ele mesmo. É raro o bastante para o custo não importar, e
// simples o bastante para não haver dúvida sobre a semântica.
type dispatcher struct {
	workers  []chan event.Event
	inflight sync.WaitGroup
	wg       sync.WaitGroup
}

// newDispatcher cria e inicia n workers. handle é chamado para cada evento;
// erros vão para fail.
func newDispatcher(ctx context.Context, n int, handle func(context.Context, event.Event) error,
	fail func(event.Event, error)) *dispatcher {

	if n < 1 {
		n = 1
	}
	d := &dispatcher{workers: make([]chan event.Event, n)}

	for i := range d.workers {
		ch := make(chan event.Event, 64)
		d.workers[i] = ch

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			// Drenar até o canal fechar, sem sair por ctx.Done(): sair no meio
			// deixaria o contador de eventos em voo desbalanceado, e a
			// barreira de dispatch esperaria para sempre por trabalho que
			// ninguém vai concluir.
			for ev := range ch {
				if ctx.Err() == nil {
					if err := handle(ctx, ev); err != nil {
						fail(ev, err)
					}
				}
				d.inflight.Done()
			}
		}()
	}
	return d
}

// dispatch encaminha o evento ao worker responsável, ou o processa sob
// barreira quando ele cruza partições.
//
// Só o chamador único do loop principal deve invocar isto: a barreira depende
// de não haver outro produtor enfileirando durante a espera.
func (d *dispatcher) dispatch(ctx context.Context, ev event.Event,
	handle func(context.Context, event.Event) error, fail func(event.Event, error)) {

	if d.crossesPartitions(ev) {
		d.inflight.Wait() // barreira: nada mais em voo
		if err := handle(ctx, ev); err != nil {
			fail(ev, err)
		}
		return
	}

	d.inflight.Add(1)
	select {
	case d.workers[d.partition(ev.Path)] <- ev:
	case <-ctx.Done():
		d.inflight.Done()
	}
}

// crossesPartitions informa se o evento afeta duas partições distintas.
func (d *dispatcher) crossesPartitions(ev event.Event) bool {
	if ev.Kind != event.KindMove || ev.From == "" {
		return false
	}
	return d.partition(ev.From) != d.partition(ev.Path)
}

// partition escolhe o worker a partir do primeiro componente do path.
func (d *dispatcher) partition(rel string) int {
	if len(d.workers) == 1 {
		return 0
	}
	return int(fnv32(topComponent(rel)) % uint32(len(d.workers)))
}

// topComponent devolve o primeiro componente do path relativo.
func topComponent(rel string) string {
	rel = strings.TrimPrefix(rel, "."+string(filepath.Separator))
	if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
		return rel[:i]
	}
	return rel
}

// fnv32 é o FNV-1a de 32 bits. Serve porque só precisa distribuir bem entre
// poucos workers; não há requisito criptográfico aqui.
func fnv32(s string) uint32 {
	const (
		offset = 2166136261
		prime  = 16777619
	)
	h := uint32(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return h
}

// close encerra os workers.
//
// Fecha os canais antes de esperar: os workers drenam o que sobrou (sem
// processar, se o contexto já foi cancelado) e terminam sozinhos. Esperar
// antes de fechar travaria se algum evento tivesse ficado para trás.
//
// Só pode ser chamado depois que o loop principal parou de despachar — ele é
// o único produtor.
func (d *dispatcher) close() {
	for _, ch := range d.workers {
		close(ch)
	}
	d.wg.Wait()
}
