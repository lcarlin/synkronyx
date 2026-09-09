// Package scan percorre uma árvore inteira, produzindo o inventário usado
// pelo First Sync (seção 10) e pelo Full Resync (seção 11).
package scan

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lcarlin/synkronyx/internal/hash"
)

// Matcher decide se um path relativo deve ser ignorado.
type Matcher interface {
	Excluded(rel string, isDir bool) bool
}

// Inventory mapeia path relativo -> identidade barata do arquivo.
type Inventory map[string]hash.Stat

// progressEvery é de quantas em quantas entradas o callback de progresso é
// chamado. Chamá-lo a cada entrada custaria mais que o próprio walk em
// árvores grandes.
const progressEvery = 2000

// Walk percorre root inteiro e devolve o inventário.
//
// Só metadados são coletados: hashear a árvore inteira num scan de partida
// seria caro e, na maioria dos casos, desnecessário — o hash é calculado sob
// demanda, quando tamanho e mtime não bastam para decidir.
func Walk(ctx context.Context, root string, exclude Matcher) (Inventory, error) {
	return WalkSubtree(ctx, root, ".", exclude, nil)
}

// WalkSubtree percorre apenas rel (relativo a root) e devolve o inventário,
// com as chaves ainda relativas a root — o que permite compor o resultado com
// o de um Walk completo sem reescrever paths.
//
// Se rel não existir, devolve inventário vazio sem erro: o path ter
// desaparecido é justamente uma das respostas possíveis.
//
// onProgress, se não for nil, é chamado periodicamente com o total percorrido
// até ali. Serve para que uma árvore de milhões de arquivos dê sinal de vida
// em vez de parecer travada.
func WalkSubtree(ctx context.Context, root, rel string, exclude Matcher,
	onProgress func(seen int)) (Inventory, error) {

	inv := make(Inventory)
	seen := 0

	start := root
	if rel != "." && rel != "" {
		start = filepath.Join(root, rel)
	}

	err := filepath.WalkDir(start, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// Um path que sumiu durante o walk simplesmente não entra no
			// inventário; o watcher já emitiu o evento correspondente.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if abs == root {
			return nil
		}

		r, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		if exclude != nil && exclude.Excluded(r, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		st, err := hash.StatOf(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		inv[r] = st

		seen++
		if onProgress != nil && seen%progressEvery == 0 {
			onProgress(seen)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return inv, nil
		}
		return nil, err
	}
	return inv, nil
}

// Union devolve todos os paths presentes em qualquer um dos inventários,
// ordenados de forma que um diretório sempre venha antes de seu conteúdo —
// criar o filho antes do pai não funciona.
func Union(a, b Inventory) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for p := range a {
		seen[p] = struct{}{}
	}
	for p := range b {
		seen[p] = struct{}{}
	}

	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sortByDepth(paths)
	return paths
}

// sortByDepth ordena por profundidade e depois lexicograficamente, para que
// um diretório seja sempre processado antes de seu conteúdo.
func sortByDepth(paths []string) {
	sort.Slice(paths, func(i, j int) bool {
		di, dj := depth(paths[i]), depth(paths[j])
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})
}

func depth(p string) int {
	return strings.Count(p, string(filepath.Separator))
}
