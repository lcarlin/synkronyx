// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package config

import (
	"path/filepath"
	"strings"
)

// Excluder decide se um path relativo deve ser ignorado.
//
// O casamento é feito por componente: o padrão "node_modules" exclui
// "a/node_modules/b.js", e não apenas um arquivo chamado node_modules na
// raiz. Padrões contendo "/" são casados contra o path relativo inteiro.
type Excluder struct {
	componentPatterns []string
	pathPatterns      []string
}

// NewExcluder compila a lista de padrões da configuração.
func NewExcluder(patterns []string) *Excluder {
	e := &Excluder{}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(p, "/") {
			e.pathPatterns = append(e.pathPatterns, strings.Trim(p, "/"))
		} else {
			e.componentPatterns = append(e.componentPatterns, p)
		}
	}
	return e
}

// Excluded implementa watcher.Matcher.
func (e *Excluder) Excluded(rel string, isDir bool) bool {
	if rel == "" || rel == "." {
		return false
	}
	for _, p := range e.pathPatterns {
		if ok, err := filepath.Match(p, rel); err == nil && ok {
			return true
		}
	}
	for _, comp := range strings.Split(rel, string(filepath.Separator)) {
		for _, p := range e.componentPatterns {
			if ok, err := filepath.Match(p, comp); err == nil && ok {
				return true
			}
		}
	}
	return false
}
