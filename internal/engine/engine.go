// Package engine é o Sync Engine: o componente central descrito na seção 3 do
// escopo. Recebe eventos já agrupados, decide a operação correspondente no
// lado oposto, executa e atualiza o estado.
//
// Todo o processamento de eventos roda em uma única goroutine. Isso é
// deliberado: serializar elimina a classe inteira de corridas entre decidir e
// aplicar, e o gargalo real do sistema é I/O de disco, não CPU. Paralelismo,
// se um dia for necessário, deve ser introduzido por particionamento de path,
// nunca por acesso concorrente ao mesmo par.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/debounce"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/guard"
	"github.com/lcarlin/synkronyx/internal/hash"
	"github.com/lcarlin/synkronyx/internal/state"
	"github.com/lcarlin/synkronyx/internal/transfer"
	"github.com/lcarlin/synkronyx/internal/watcher"
)

// metaFirstSync é a chave em state.meta que marca o First Sync concluído.
const metaFirstSync = "first_sync_completed_at"

// sweepInterval é a periodicidade da limpeza de expectativas vencidas do guard.
const sweepInterval = time.Minute

// Engine coordena watchers, guard, estado e transferência.
type Engine struct {
	cfg   config.Config
	log   *slog.Logger
	db    *state.DB
	guard *guard.Guard
	xfer  *transfer.Transfer
	debo  *debounce.Debouncer

	watchers map[event.Side]*watcher.Watcher
	roots    map[event.Side]string

	resync chan string // pedidos de Full Resync, com o motivo
}

// New monta o engine. Não toca em disco nem sobe watchers — isso é Run.
func New(cfg config.Config, db *state.DB, log *slog.Logger) *Engine {
	return &Engine{
		cfg:      cfg,
		log:      log,
		db:       db,
		guard:    guard.New(cfg.SelfWriteTTL),
		xfer:     transfer.New(cfg.RsyncPath, cfg.RsyncArgs),
		debo:     debounce.New(cfg.Debounce),
		watchers: make(map[event.Side]*watcher.Watcher, 2),
		roots:    map[event.Side]string{event.SideA: cfg.A, event.SideB: cfg.B},
		resync:   make(chan string, 1),
	}
}

// Run executa o serviço até ctx ser cancelado.
//
// A ordem importa e é a da seção 10 do escopo: watchers primeiro, First Sync
// depois. Invertida, alterações ocorridas durante o scan não gerariam evento
// e ficariam invisíveis até o próximo restart.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.xfer.Check(ctx); err != nil {
		return err
	}

	excluder := config.NewExcluder(e.cfg.Exclude)
	for side, root := range e.roots {
		w, err := watcher.New(watcher.Options{
			Side: side, Root: root, Exclude: excluder, Logger: e.log,
		})
		if err != nil {
			return fmt.Errorf("criando watcher %s: %w", side, err)
		}
		e.watchers[side] = w
	}
	defer e.stopWatchers()

	for side, w := range e.watchers {
		if err := w.Start(ctx); err != nil {
			return fmt.Errorf("iniciando watcher %s: %w", side, err)
		}
		go e.pump(ctx, w)
		go e.watchOverflow(ctx, side, w)
	}

	if err := e.firstSync(ctx); err != nil {
		return fmt.Errorf("first sync: %w", err)
	}

	return e.loop(ctx)
}

// pump encaminha os eventos de um watcher para o debouncer.
func (e *Engine) pump(ctx context.Context, w *watcher.Watcher) {
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			// O guard é consultado ANTES do debouncer, e não depois: um eco
			// do próprio sincronizador não deve nem ocupar uma janela de
			// debounce, senão ele atrasa a alteração externa seguinte no
			// mesmo path.
			if e.guard.Consume(ev) {
				e.log.Debug("evento próprio descartado", "event", ev.String())
				continue
			}
			e.debo.Push(ctx, ev)
		case <-ctx.Done():
			return
		}
	}
}

func (e *Engine) watchOverflow(ctx context.Context, side event.Side, w *watcher.Watcher) {
	for {
		select {
		case <-w.Overflow():
			e.RequestResync(fmt.Sprintf("overflow da fila do inotify no lado %s", side))
		case <-ctx.Done():
			return
		}
	}
}

// RequestResync agenda um Full Resync. Não bloqueia: se já houver um pedido
// pendente, o novo é descartado — um resync cobre qualquer motivo acumulado.
func (e *Engine) RequestResync(reason string) {
	select {
	case e.resync <- reason:
	default:
	}
}

func (e *Engine) loop(ctx context.Context) error {
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()

	for {
		select {
		case <-ctx.Done():
			e.debo.Close()
			return nil

		case ev, ok := <-e.debo.Out():
			if !ok {
				return nil
			}
			if err := e.process(ctx, ev); err != nil {
				e.log.Error("falha processando evento", "event", ev.String(), "err", err)
			}

		case reason := <-e.resync:
			e.log.Warn("full resync solicitado", "reason", reason)
			if err := e.FullResync(ctx); err != nil {
				e.log.Error("full resync falhou", "err", err)
			}

		case <-sweep.C:
			if n := e.guard.Sweep(); n > 0 {
				pending, matched, expired := e.guard.Stats()
				e.log.Debug("guard sweep", "removidas", n, "pendentes", pending,
					"casadas", matched, "vencidas", expired)
			}
		}
	}
}

// process trata um evento já agrupado e não reconhecido como eco.
func (e *Engine) process(ctx context.Context, ev event.Event) error {
	src, dst := ev.Side, ev.Side.Opposite()
	srcAbs := e.abs(src, ev.Path)
	dstAbs := e.abs(dst, ev.Path)

	// Camada 2 da prevenção de loops (ver internal/guard): antes de agir,
	// verificar se o que se observa já é o que o estado registra como
	// sincronizado. Se for, a ação é no-op e o ciclo morre aqui,
	// independentemente de qualquer janela de tempo ter expirado.
	if noop, err := e.alreadySynced(ctx, ev, srcAbs); err != nil {
		return err
	} else if noop {
		e.log.Debug("evento já refletido no estado; ignorado", "event", ev.String())
		return nil
	}

	e.log.Info("propagando", "event", ev.String(), "para", dst.String())

	switch ev.Kind {
	case event.KindCreate, event.KindModify:
		return e.propagateContent(ctx, ev, srcAbs, dstAbs)
	case event.KindDelete:
		return e.propagateDelete(ctx, ev, dstAbs)
	case event.KindMove:
		return e.propagateMove(ctx, ev)
	case event.KindAttrib:
		return e.propagateAttrs(ctx, ev, srcAbs, dstAbs)
	default:
		return fmt.Errorf("tipo de evento não tratado: %s", ev.Kind)
	}
}

// alreadySynced é a camada de idempotência. Devolve true quando o estado já
// registra, para os DOIS lados, exatamente o conteúdo que está no disco.
func (e *Engine) alreadySynced(ctx context.Context, ev event.Event, srcAbs string) (bool, error) {
	if ev.Kind == event.KindMove {
		return false, nil // renames são sempre avaliados
	}

	srcEntry, err := e.db.Get(ctx, ev.Side, ev.Path)
	if err != nil {
		return false, err
	}
	dstEntry, err := e.db.Get(ctx, ev.Side.Opposite(), ev.Path)
	if err != nil {
		return false, err
	}
	if srcEntry == nil || dstEntry == nil {
		return false, nil
	}

	if ev.Kind == event.KindDelete {
		// Deleção só é no-op se o destino também já não existe.
		_, statErr := os.Lstat(e.abs(ev.Side.Opposite(), ev.Path))
		return errors.Is(statErr, os.ErrNotExist), nil
	}

	st, err := hash.StatOf(srcAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !st.Unchanged(srcEntry.Stat()) {
		return false, nil
	}
	// Metadados batem com o último estado sincronizado e os dois lados
	// registram o mesmo hash: nada a fazer.
	return srcEntry.SHA256 != "" && srcEntry.SHA256 == dstEntry.SHA256, nil
}

func (e *Engine) propagateContent(ctx context.Context, ev event.Event, srcAbs, dstAbs string) error {
	if ev.IsDir {
		e.guard.Expect(ev.Side.Opposite(), ev.Path, 1)
		if err := e.xfer.Mkdir(srcAbs, dstAbs); err != nil {
			e.guard.Forget(ev.Side.Opposite(), ev.Path)
			return err
		}
		return e.recordPair(ctx, ev.Path, srcAbs, dstAbs, true)
	}

	conflict, err := e.detectConflict(ctx, ev, srcAbs, dstAbs)
	if err != nil {
		return err
	}
	if conflict {
		return e.resolveConflict(ctx, ev, srcAbs, dstAbs)
	}

	// Uma cópia de arquivo gera tipicamente CREATE + CLOSE_WRITE + ATTRIB no
	// destino; a expectativa cobre os três.
	e.guard.Expect(ev.Side.Opposite(), ev.Path, 3)
	if err := e.xfer.CopyFile(ctx, srcAbs, dstAbs); err != nil {
		e.guard.Forget(ev.Side.Opposite(), ev.Path)
		return err
	}
	return e.recordPair(ctx, ev.Path, srcAbs, dstAbs, false)
}

func (e *Engine) propagateDelete(ctx context.Context, ev event.Event, dstAbs string) error {
	e.guard.Expect(ev.Side.Opposite(), ev.Path, 1)
	if err := e.xfer.Remove(dstAbs, ev.IsDir); err != nil {
		e.guard.Forget(ev.Side.Opposite(), ev.Path)
		return err
	}
	if ev.IsDir {
		return e.db.DeleteSubtree(ctx, ev.Path)
	}
	if err := e.db.Delete(ctx, ev.Side, ev.Path); err != nil {
		return err
	}
	return e.db.Delete(ctx, ev.Side.Opposite(), ev.Path)
}

func (e *Engine) propagateMove(ctx context.Context, ev event.Event) error {
	dst := ev.Side.Opposite()
	fromAbs := e.abs(dst, ev.From)
	toAbs := e.abs(dst, ev.Path)

	// O rename toca dois paths no destino: MOVED_FROM em um, MOVED_TO no outro.
	e.guard.Expect(dst, ev.From, 1)
	e.guard.Expect(dst, ev.Path, 1)

	if err := e.xfer.Move(fromAbs, toAbs); err != nil {
		e.guard.Forget(dst, ev.From)
		e.guard.Forget(dst, ev.Path)

		// A origem pode não existir no destino — estado divergente. Em vez de
		// insistir no rename, degradar para uma cópia do path de destino é
		// a resposta segura.
		if errors.Is(err, os.ErrNotExist) {
			e.log.Warn("origem do rename ausente no destino; copiando", "from", ev.From, "to", ev.Path)
			copyEv := event.Event{Side: ev.Side, Kind: event.KindCreate, Path: ev.Path, IsDir: ev.IsDir, At: ev.At}
			return e.propagateContent(ctx, copyEv, e.abs(ev.Side, ev.Path), toAbs)
		}
		return err
	}

	if err := e.db.DeleteSubtree(ctx, ev.From); err != nil {
		return err
	}
	return e.recordPair(ctx, ev.Path, e.abs(ev.Side, ev.Path), toAbs, ev.IsDir)
}

func (e *Engine) propagateAttrs(ctx context.Context, ev event.Event, srcAbs, dstAbs string) error {
	e.guard.Expect(ev.Side.Opposite(), ev.Path, 1)
	if err := e.xfer.SyncAttrs(ctx, srcAbs, dstAbs); err != nil {
		e.guard.Forget(ev.Side.Opposite(), ev.Path)
		return err
	}
	return e.recordPair(ctx, ev.Path, srcAbs, dstAbs, ev.IsDir)
}

// recordPair grava o estado dos dois lados após uma propagação bem-sucedida.
func (e *Engine) recordPair(ctx context.Context, rel, srcAbs, dstAbs string, isDir bool) error {
	now := time.Now()

	build := func(side event.Side, abs string) (state.Entry, error) {
		st, err := hash.StatOf(abs)
		if err != nil {
			return state.Entry{}, err
		}
		entry := state.Entry{
			Path: rel, Side: side, IsDir: st.IsDir, Size: st.Size,
			MTime: st.MTime, Mode: st.Mode, SyncedAt: now, Status: state.StatusSynced,
		}
		if !st.IsDir {
			sum, ok, err := hash.FileLimited(abs, e.cfg.HashMaxBytes)
			if err != nil {
				return state.Entry{}, err
			}
			if ok {
				entry.SHA256 = sum
			}
		}
		return entry, nil
	}

	// Determinar qual abs pertence a qual lado a partir das raízes.
	srcSide, dstSide := e.sideOf(srcAbs), e.sideOf(dstAbs)
	srcEntry, err := build(srcSide, srcAbs)
	if err != nil {
		return err
	}
	dstEntry, err := build(dstSide, dstAbs)
	if err != nil {
		return err
	}
	return e.db.PutBoth(ctx, srcEntry, dstEntry)
}

func (e *Engine) abs(side event.Side, rel string) string {
	if rel == "." || rel == "" {
		return e.roots[side]
	}
	return filepath.Join(e.roots[side], rel)
}

// sideOf identifica a que lado um path absoluto pertence.
func (e *Engine) sideOf(abs string) event.Side {
	if rel, err := filepath.Rel(e.roots[event.SideA], abs); err == nil && !isEscaping(rel) {
		return event.SideA
	}
	return event.SideB
}

func isEscaping(rel string) bool {
	return rel == ".." || (len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator))
}

func (e *Engine) stopWatchers() {
	for side, w := range e.watchers {
		if err := w.Stop(); err != nil {
			e.log.Warn("falha parando watcher", "side", side.String(), "err", err)
		}
	}
}
