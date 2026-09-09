// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package debounce

import (
	"context"
	"strconv"
	"sync"
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

// TestCloseDuringEmitIsSafe é regressão de uma corrida real, encontrada pelo
// detector durante uma execução da suíte: Close fechava o canal de saída
// enquanto um timer que já havia disparado estava a caminho do envio.
//
// Não era só flake de teste. Em produção o desfecho é um panic de "send on
// closed channel" no desligamento do serviço — raro, dependente de tempo, e
// exatamente o tipo de falha que só aparece sob carga.
//
// timer.Stop() não resolvia: um timer que já disparou devolve false, e nesse
// ponto o callback já passou da verificação e está indo enviar.
func TestCloseDuringEmitIsSafe(t *testing.T) {
	// A janela é curta de propósito, para que os timers disparem justamente
	// enquanto Close acontece.
	for range 50 {
		d := New(time.Millisecond)
		ctx := context.Background()

		var wg sync.WaitGroup
		for i := range 40 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d.Push(ctx, event.Event{
					Side: event.SideA, Kind: event.KindModify,
					Path: "f" + strconv.Itoa(i) + ".txt",
				})
			}()
		}

		// Fecha no meio da tempestade, sem drenar o canal.
		go d.Close()

		wg.Wait()
		d.Close() // idempotente
	}
}

// Push depois de Close não pode entregar nada nem entrar em pânico.
func TestPushAfterCloseIsIgnored(t *testing.T) {
	d := New(10 * time.Millisecond)
	d.Close()

	d.Push(context.Background(), event.Event{Side: event.SideA, Path: "f.txt"})

	// O canal está fechado: uma leitura devolve imediatamente com ok falso.
	select {
	case _, ok := <-d.Out():
		if ok {
			t.Error("um evento foi entregue depois de Close")
		}
	case <-time.After(time.Second):
		t.Error("leitura em canal fechado deveria retornar de imediato")
	}
}

// Close precisa retornar mesmo com o canal de saída cheio e ninguém lendo —
// senão o desligamento do serviço travaria.
func TestCloseReturnsWithFullChannelAndNoReader(t *testing.T) {
	d := New(time.Millisecond)
	ctx := context.Background()

	// Mais eventos do que a capacidade do canal, e nenhum leitor.
	for i := range 2000 {
		d.Push(ctx, event.Event{Side: event.SideA, Path: "f" + strconv.Itoa(i) + ".txt"})
	}
	time.Sleep(50 * time.Millisecond) // deixa os timers dispararem

	done := make(chan struct{})
	go func() { d.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close travou: emissões bloqueadas não foram liberadas")
	}
}
