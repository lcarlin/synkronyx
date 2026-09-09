// Package guard implementa a prevenção de loops de sincronização.
//
// É o requisito da seção 6 do escopo, e é tratado aqui como mecanismo
// arquitetural de primeira classe, não como filtro acessório.
//
// # O problema
//
// Toda escrita que o sincronizador faz no lado destino é, do ponto de vista
// do inotify, indistinguível de uma alteração feita por um usuário. Sem
// tratamento, propagar A→B faz B disparar um evento que propaga B→A, que faz
// A disparar, indefinidamente.
//
// # A defesa, em duas camadas
//
// Camada 1 — expectativa (aqui). Antes de escrever em (lado, path), o engine
// registra uma expectativa. O evento que essa escrita provoca chega, casa com
// a expectativa registrada e é descartado. É a defesa rápida e cobre o caso
// normal.
//
// Camada 2 — idempotência (no engine, ver engine.decide). Antes de agir sobre
// qualquer evento, o engine compara o conteúdo observado com o que o estado
// diz ter sido sincronizado por último. Se forem iguais, a ação é no-op e o
// ciclo morre ali.
//
// A camada 1 é baseada em tempo, e tempo é uma premissa frágil: uma escrita
// lenta pode estourar o TTL, e o evento chegaria depois de a expectativa
// expirar. A camada 2 não depende de tempo nenhum. É por isso que existem as
// duas: a primeira evita trabalho, a segunda garante a correção. Nunca
// remover a camada 2 por parecer redundante — ela é o que torna a
// propriedade verdadeira, não apenas provável.
package guard

import (
	"sync"
	"time"

	"github.com/lcarlin/synkronyx/internal/event"
)

type key struct {
	side event.Side
	path string
}

type expectation struct {
	expires time.Time
	count   int // uma escrita pode gerar mais de um evento (CREATE + CLOSE_WRITE + ATTRIB)
}

// Guard registra escritas feitas pelo próprio sincronizador para que os
// eventos resultantes não sejam reinterpretados como alterações externas.
//
// Seguro para uso concorrente.
type Guard struct {
	ttl time.Duration
	now func() time.Time

	mu   sync.Mutex
	seen map[key]*expectation

	// métricas simples, úteis no log periódico
	matched, expired uint64
}

// New cria um Guard com o TTL dado.
func New(ttl time.Duration) *Guard {
	return &Guard{ttl: ttl, now: time.Now, seen: make(map[key]*expectation)}
}

// Expect anuncia que o sincronizador está prestes a escrever em (side, path)
// e que os próximos `events` eventos para esse par devem ser ignorados.
//
// Deve ser chamado ANTES da escrita. Chamar depois abre uma corrida: o evento
// pode chegar antes do registro.
func (g *Guard) Expect(side event.Side, path string, events int) {
	if events <= 0 {
		events = 1
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	k := key{side, path}
	exp := g.seen[k]
	if exp == nil || exp.expires.Before(g.now()) {
		exp = &expectation{}
		g.seen[k] = exp
	}
	exp.count += events
	exp.expires = g.now().Add(g.ttl)
}

// Consume informa se o evento corresponde a uma escrita do próprio
// sincronizador. Se sim, gasta uma unidade da expectativa e devolve true —
// o chamador deve descartar o evento.
func (g *Guard) Consume(ev event.Event) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Para um rename, ambos os lados do par foram tocados pela escrita.
	paths := []string{ev.Path}
	if ev.Kind == event.KindMove && ev.From != "" {
		paths = append(paths, ev.From)
	}

	now := g.now()
	for _, p := range paths {
		k := key{ev.Side, p}
		exp, ok := g.seen[k]
		if !ok {
			continue
		}
		if exp.expires.Before(now) {
			delete(g.seen, k)
			g.expired++
			continue
		}
		exp.count--
		if exp.count <= 0 {
			delete(g.seen, k)
		}
		g.matched++
		return true
	}
	return false
}

// Forget descarta a expectativa de (side, path). Usado quando a escrita
// falhou e o evento esperado nunca virá.
func (g *Guard) Forget(side event.Side, path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seen, key{side, path})
}

// Sweep remove expectativas vencidas. Deve ser chamado periodicamente para o
// mapa não crescer com escritas que nunca geraram evento (arquivo removido
// logo em seguida, por exemplo). Devolve quantas foram removidas.
func (g *Guard) Sweep() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	var n int
	for k, exp := range g.seen {
		if exp.expires.Before(now) {
			delete(g.seen, k)
			n++
		}
	}
	g.expired += uint64(n)
	return n
}

// Stats devolve contadores acumulados para observabilidade.
func (g *Guard) Stats() (pending int, matched, expired uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen), g.matched, g.expired
}
