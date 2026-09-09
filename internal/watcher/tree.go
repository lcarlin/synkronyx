package watcher

import (
	"path/filepath"
	"sync"
)

// tree é o registro bidirecional entre descritores de watch e paths
// relativos. Todo path que sai daqui é relativo à raiz do lado — o Sync
// Engine nunca deve ver caminhos absolutos.
type tree struct {
	mu     sync.RWMutex
	byWd   map[int32]string // wd -> path relativo do diretório
	byPath map[string]int32 // path relativo -> wd
}

func newTree() *tree {
	return &tree{
		byWd:   make(map[int32]string),
		byPath: make(map[string]int32),
	}
}

func (t *tree) add(wd int32, rel string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Um mesmo wd pode ser reemitido para um path novo se o diretório foi
	// movido; limpa o mapeamento antigo para não deixar entrada órfã.
	if old, ok := t.byWd[wd]; ok && old != rel {
		delete(t.byPath, old)
	}
	t.byWd[wd] = rel
	t.byPath[rel] = wd
}

func (t *tree) pathOf(wd int32) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	rel, ok := t.byWd[wd]
	return rel, ok
}

func (t *tree) wdOf(rel string) (int32, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	wd, ok := t.byPath[rel]
	return wd, ok
}

func (t *tree) remove(wd int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rel, ok := t.byWd[wd]; ok {
		delete(t.byPath, rel)
	}
	delete(t.byWd, wd)
}

// removeSubtree descarta rel e tudo abaixo dele, devolvendo os wds afetados
// para que o chamador os remova do kernel.
func (t *tree) removeSubtree(rel string) []int32 {
	t.mu.Lock()
	defer t.mu.Unlock()

	var wds []int32
	for path, wd := range t.byPath {
		if path == rel || isUnder(path, rel) {
			wds = append(wds, wd)
			delete(t.byPath, path)
			delete(t.byWd, wd)
		}
	}
	return wds
}

// renameSubtree reaponta rel e seus descendentes para um novo prefixo, sem
// mexer nos watches do kernel: o wd continua válido após um rename, só o
// path que ele representa mudou.
func (t *tree) renameSubtree(from, to string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	moved := make(map[string]int32)
	for path, wd := range t.byPath {
		switch {
		case path == from:
			moved[to] = wd
		case isUnder(path, from):
			suffix, err := filepath.Rel(from, path)
			if err != nil {
				continue
			}
			moved[filepath.Join(to, suffix)] = wd
		default:
			continue
		}
		delete(t.byPath, path)
	}
	for path, wd := range moved {
		t.byPath[path] = wd
		t.byWd[wd] = path
	}
}

func (t *tree) size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byWd)
}

// isUnder informa se child é descendente estrito de parent (paths relativos
// já limpos; "." é a raiz e é ancestral de tudo).
func isUnder(child, parent string) bool {
	if parent == "." {
		return child != "."
	}
	if len(child) <= len(parent)+1 {
		return false
	}
	return child[:len(parent)] == parent && child[len(parent)] == filepath.Separator
}
