// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

// Package event define o vocabulário de eventos que circula entre os
// watchers e o Sync Engine. É a fronteira entre "o que o filesystem
// disse" (inotify, cru) e "o que o sincronizador entende" (domínio).
package event

import (
	"fmt"
	"time"
)

// Side identifica um dos dois lados da árvore lógica.
type Side uint8

const (
	SideA Side = iota
	SideB
)

func (s Side) String() string {
	switch s {
	case SideA:
		return "A"
	case SideB:
		return "B"
	default:
		return fmt.Sprintf("Side(%d)", uint8(s))
	}
}

// Opposite devolve o lado oposto — o destino da propagação.
func (s Side) Opposite() Side {
	if s == SideA {
		return SideB
	}
	return SideA
}

// Kind é a operação observada, já normalizada.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindCreate       // arquivo ou diretório passou a existir
	KindModify       // conteúdo alterado (write fechado)
	KindDelete       // removido
	KindMove         // rename/move; From e Path estão ambos preenchidos
	KindAttrib       // metadados/atributos (chmod, chown, timestamps)
)

func (k Kind) String() string {
	switch k {
	case KindCreate:
		return "CREATE"
	case KindModify:
		return "MODIFY"
	case KindDelete:
		return "DELETE"
	case KindMove:
		return "MOVE"
	case KindAttrib:
		return "ATTRIB"
	default:
		return "UNKNOWN"
	}
}

// Event é uma alteração observada em um dos lados.
//
// Path e From são sempre relativos à raiz do lado (ex.: "foo/bar.txt"),
// nunca absolutos: é o path relativo que identifica a mesma entidade
// lógica nos dois lados.
type Event struct {
	Side  Side
	Kind  Kind
	Path  string // path relativo; destino, no caso de KindMove
	From  string // path relativo de origem; só para KindMove
	IsDir bool
	At    time.Time
}

func (e Event) String() string {
	if e.Kind == KindMove {
		return fmt.Sprintf("%s %s %s -> %s", e.Side, e.Kind, e.From, e.Path)
	}
	return fmt.Sprintf("%s %s %s", e.Side, e.Kind, e.Path)
}
