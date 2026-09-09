// Package watcher observa recursivamente uma raiz e traduz eventos crus do
// inotify em eventos de domínio.
//
// Três responsabilidades que o escopo (seção 4) exige e que não são triviais:
//
//  1. Watch recursivo. O inotify não é recursivo: cada diretório precisa do
//     seu próprio watch, e diretórios criados em runtime precisam ser
//     registrados — inclusive os que já nasceram com conteúdo dentro, criados
//     entre o CREATE e o nosso AddWatch (daí o rescan do subtree novo).
//
//  2. Pareamento de rename. IN_MOVED_FROM e IN_MOVED_TO chegam como dois
//     eventos ligados por um `cookie`. Pareados, viram um KindMove. Órfãos,
//     significam que a entrada saiu da árvore (delete) ou entrou nela
//     (create) — e só o tempo diz qual dos dois é.
//
//  3. Overflow. Se a fila do kernel estourar (IN_Q_OVERFLOW) perdemos
//     eventos, e a única resposta correta é admitir a perda: sinalizamos ao
//     engine que ele precisa de um resync completo.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/inotify"
)

// movePairWindow é quanto se espera pelo par de um MOVED_FROM/MOVED_TO antes
// de tratá-lo como órfão. O par costuma vir no mesmo lote de leitura; a
// janela existe para o caso de ele cair no lote seguinte.
const movePairWindow = 200 * time.Millisecond

// Options configura um Watcher.
type Options struct {
	Side    event.Side
	Root    string // caminho absoluto da raiz
	Exclude Matcher
	Logger  *slog.Logger
}

// Matcher decide se um path relativo deve ser ignorado.
type Matcher interface {
	Excluded(rel string, isDir bool) bool
}

// Watcher observa uma raiz e emite eventos de domínio.
type Watcher struct {
	opts Options
	in   *inotify.Inotify
	tree *tree
	log  *slog.Logger

	out      chan event.Event
	overflow chan struct{}

	mu      sync.Mutex
	pending map[uint32]*pendingMove // cookie -> metade de um rename ainda sem par

	wg sync.WaitGroup
}

type pendingMove struct {
	rel      string
	isDir    bool
	isFrom   bool // true = MOVED_FROM (saída), false = MOVED_TO (entrada)
	deadline time.Time
	timer    *time.Timer
}

// New cria um watcher já com o inotify aberto, mas ainda sem watches: quem
// registra a árvore é Start.
func New(opts Options) (*Watcher, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	in, err := inotify.New()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		opts:     opts,
		in:       in,
		tree:     newTree(),
		log:      opts.Logger.With("side", opts.Side.String()),
		out:      make(chan event.Event, 1024),
		overflow: make(chan struct{}, 1),
		pending:  make(map[uint32]*pendingMove),
	}, nil
}

// Events é o canal de eventos de domínio. Fecha quando o watcher para.
func (w *Watcher) Events() <-chan event.Event { return w.out }

// Overflow sinaliza que a fila do kernel estourou e eventos foram perdidos.
// O engine deve responder com um Full Resync.
func (w *Watcher) Overflow() <-chan struct{} { return w.overflow }

// Start registra a árvore inteira e começa a consumir eventos.
//
// Há uma janela inerente entre registrar os watches e terminar o walk: uma
// alteração ocorrida nesse intervalo pode não gerar evento. Por isso o First
// Sync roda depois de Start, nunca antes — o scan cobre o que a janela perdeu.
func (w *Watcher) Start(ctx context.Context) error {
	if err := w.addTree("."); err != nil {
		return err
	}
	w.log.Info("watchers registrados", "dirs", w.tree.size(), "root", w.opts.Root)

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer close(w.out)
		w.loop(ctx)
	}()
	return nil
}

// Stop encerra o watcher e espera o loop terminar.
func (w *Watcher) Stop() error {
	err := w.in.Close()
	w.wg.Wait()
	return errors.Join(err, w.in.Shutdown())
}

// addTree registra rel e todos os subdiretórios abaixo dele.
func (w *Watcher) addTree(rel string) error {
	root := w.abs(rel)
	return filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// Um diretório que sumiu no meio do walk não é erro fatal: o
			// evento de DELETE correspondente vai chegar.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		r, err := w.rel(abs)
		if err != nil {
			return err
		}
		if w.excluded(r, true) {
			return fs.SkipDir
		}
		wd, err := w.in.Add(abs, inotify.DefaultMask)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		w.tree.add(wd, r)
		return nil
	})
}

func (w *Watcher) loop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := w.in.Read()
		if err != nil {
			if errors.Is(err, inotify.ErrClosed) || ctx.Err() != nil {
				return
			}
			w.log.Error("falha lendo inotify", "err", err)
			return
		}
		for _, raw := range events {
			w.handle(ctx, raw)
		}
	}
}

func (w *Watcher) handle(ctx context.Context, raw inotify.Event) {
	if raw.Is(unix.IN_Q_OVERFLOW) {
		w.log.Warn("fila do inotify estourou; eventos perdidos, resync necessário")
		select {
		case w.overflow <- struct{}{}:
		default:
		}
		return
	}
	if raw.Is(unix.IN_IGNORED) {
		w.tree.remove(raw.Wd)
		return
	}

	dir, ok := w.tree.pathOf(raw.Wd)
	if !ok {
		// Watch já descartado; evento residual.
		return
	}

	rel := dir
	if raw.Name != "" {
		rel = filepath.Join(dir, raw.Name)
	}
	if w.excluded(rel, raw.IsDir()) {
		return
	}

	now := time.Now()
	isDir := raw.IsDir()

	switch {
	case raw.Is(unix.IN_MOVED_FROM):
		w.halfMove(ctx, raw.Cookie, rel, isDir, true)

	case raw.Is(unix.IN_MOVED_TO):
		w.halfMove(ctx, raw.Cookie, rel, isDir, false)

	case raw.Is(unix.IN_CREATE):
		if isDir {
			// Registrar o subtree inteiro, não só o diretório: arquivos
			// criados dentro dele antes deste AddWatch não geraram evento.
			if err := w.addTree(rel); err != nil {
				w.log.Warn("falha registrando subtree novo", "path", rel, "err", err)
			}
			w.emit(ctx, event.Event{Side: w.opts.Side, Kind: event.KindCreate, Path: rel, IsDir: true, At: now})
			w.rescan(ctx, rel)
			return
		}
		w.emit(ctx, event.Event{Side: w.opts.Side, Kind: event.KindCreate, Path: rel, At: now})

	case raw.Is(unix.IN_CLOSE_WRITE):
		w.emit(ctx, event.Event{Side: w.opts.Side, Kind: event.KindModify, Path: rel, At: now})

	case raw.Is(unix.IN_DELETE | unix.IN_DELETE_SELF):
		if isDir || raw.Is(unix.IN_DELETE_SELF) {
			for _, wd := range w.tree.removeSubtree(rel) {
				_ = w.in.Remove(wd)
			}
		}
		if raw.Is(unix.IN_DELETE_SELF) && rel == "." {
			w.log.Error("raiz observada foi removida", "root", w.opts.Root)
			return
		}
		w.emit(ctx, event.Event{Side: w.opts.Side, Kind: event.KindDelete, Path: rel, IsDir: isDir, At: now})

	case raw.Is(unix.IN_ATTRIB):
		w.emit(ctx, event.Event{Side: w.opts.Side, Kind: event.KindAttrib, Path: rel, IsDir: isDir, At: now})

	case raw.Is(unix.IN_MODIFY):
		// Deliberadamente ignorado: o sinal de conteúdo pronto é o
		// IN_CLOSE_WRITE. Ver comentário em inotify.DefaultMask.
	}
}

// halfMove guarda uma metade de rename à espera do par, ou fecha o par se a
// outra metade já estiver esperando.
func (w *Watcher) halfMove(ctx context.Context, cookie uint32, rel string, isDir, isFrom bool) {
	if cookie == 0 {
		// Sem cookie não há como parear; degrada para delete/create.
		w.orphanMove(ctx, rel, isDir, isFrom)
		return
	}

	w.mu.Lock()
	other, found := w.pending[cookie]
	if found {
		delete(w.pending, cookie)
		other.timer.Stop()
	}
	w.mu.Unlock()

	if found && other.isFrom != isFrom {
		from, to := other.rel, rel
		if isFrom {
			from, to = rel, other.rel
		}
		if isDir {
			w.tree.renameSubtree(from, to)
		}
		w.emit(ctx, event.Event{
			Side: w.opts.Side, Kind: event.KindMove,
			From: from, Path: to, IsDir: isDir, At: time.Now(),
		})
		return
	}

	pm := &pendingMove{rel: rel, isDir: isDir, isFrom: isFrom, deadline: time.Now().Add(movePairWindow)}
	pm.timer = time.AfterFunc(movePairWindow, func() {
		w.mu.Lock()
		cur, still := w.pending[cookie]
		if still && cur == pm {
			delete(w.pending, cookie)
		} else {
			still = false
		}
		w.mu.Unlock()
		if still {
			w.orphanMove(ctx, pm.rel, pm.isDir, pm.isFrom)
		}
	})

	w.mu.Lock()
	w.pending[cookie] = pm
	w.mu.Unlock()
}

// orphanMove traduz uma metade de rename sem par: saiu da árvore observada
// (equivale a delete) ou entrou vinda de fora (equivale a create).
func (w *Watcher) orphanMove(ctx context.Context, rel string, isDir, isFrom bool) {
	now := time.Now()
	if isFrom {
		if isDir {
			for _, wd := range w.tree.removeSubtree(rel) {
				_ = w.in.Remove(wd)
			}
		}
		w.emit(ctx, event.Event{Side: w.opts.Side, Kind: event.KindDelete, Path: rel, IsDir: isDir, At: now})
		return
	}
	if isDir {
		if err := w.addTree(rel); err != nil {
			w.log.Warn("falha registrando subtree movido para dentro", "path", rel, "err", err)
		}
	}
	w.emit(ctx, event.Event{Side: w.opts.Side, Kind: event.KindCreate, Path: rel, IsDir: isDir, At: now})
	if isDir {
		w.rescan(ctx, rel)
	}
}

// rescan emite eventos sintéticos para o conteúdo já existente de um
// diretório recém-observado — o que apareceu antes de o watch existir.
func (w *Watcher) rescan(ctx context.Context, rel string) {
	root := w.abs(rel)
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if abs == root {
			return nil
		}
		r, relErr := w.rel(abs)
		if relErr != nil {
			return relErr
		}
		if w.excluded(r, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		w.emit(ctx, event.Event{
			Side: w.opts.Side, Kind: event.KindCreate,
			Path: r, IsDir: d.IsDir(), At: time.Now(),
		})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		w.log.Warn("falha no rescan de subtree", "path", rel, "err", err)
	}
}

func (w *Watcher) emit(ctx context.Context, ev event.Event) {
	select {
	case w.out <- ev:
	case <-ctx.Done():
	}
}

func (w *Watcher) excluded(rel string, isDir bool) bool {
	if rel == "." {
		return false
	}
	return w.opts.Exclude != nil && w.opts.Exclude.Excluded(rel, isDir)
}

func (w *Watcher) abs(rel string) string {
	if rel == "." {
		return w.opts.Root
	}
	return filepath.Join(w.opts.Root, rel)
}

func (w *Watcher) rel(abs string) (string, error) {
	r, err := filepath.Rel(w.opts.Root, abs)
	if err != nil {
		return "", fmt.Errorf("path %s fora da raiz %s: %w", abs, w.opts.Root, err)
	}
	return r, nil
}
