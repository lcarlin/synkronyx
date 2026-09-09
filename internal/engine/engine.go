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
	"strconv"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/debounce"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/guard"
	"github.com/lcarlin/synkronyx/internal/hash"
	"github.com/lcarlin/synkronyx/internal/preflight"
	"github.com/lcarlin/synkronyx/internal/state"
	"github.com/lcarlin/synkronyx/internal/transfer"
	"github.com/lcarlin/synkronyx/internal/watcher"
)

// sweepInterval é a periodicidade da limpeza de expectativas vencidas do guard.
const sweepInterval = time.Minute

// retryTick é a frequência com que a fila de retry é consultada. Não precisa
// ser fina: o menor backoff configurável já é da ordem de segundos.
const retryTick = time.Second

// Engine coordena watchers, guard, estado e transferência.
type Engine struct {
	cfg   config.Config
	log   *slog.Logger
	db    *state.DB
	guard *guard.Guard
	xfer  *transfer.Transfer
	debo  *debounce.Debouncer
	retry *retryQueue

	watchers map[event.Side]*watcher.Watcher
	roots    map[event.Side]string

	// syncOwnership diz se dono e grupo entram na reconciliação. Depende de
	// privilégio, não de configuração: comparar sem poder aplicar produziria
	// divergência detectada e nunca resolvida, refeita em todo resync.
	syncOwnership bool

	resync chan string  // pedidos de Full Resync, com o motivo
	fatal  chan error   // falhas que exigem encerrar o serviço
	failed chan failure // falhas de propagação, vindas dos workers
}

// failure é uma propagação que não deu certo. Trafega dos workers para o loop
// principal, que é o único a mexer na fila de retry — assim ela continua
// sendo estado de uma goroutine só, sem precisar de trava.
type failure struct {
	ev  event.Event
	err error
}

// New monta o engine. Não toca em disco nem sobe watchers — isso é Run.
func New(cfg config.Config, db *state.DB, log *slog.Logger) *Engine {
	return &Engine{
		cfg:   cfg,
		log:   log,
		db:    db,
		guard: guard.New(cfg.SelfWriteTTL),
		xfer: transfer.New(cfg.RsyncPath, cfg.RsyncArgs, cfg.PreserveHardlinks,
			preflight.PreservesOwnership()),
		syncOwnership: preflight.PreservesOwnership(),
		debo:          debounce.New(cfg.Debounce),
		retry:         newRetryQueue(cfg.RetryMaxAttempts, cfg.RetryInitialDelay, cfg.RetryMaxDelay),
		watchers:      make(map[event.Side]*watcher.Watcher, 2),
		roots:         map[event.Side]string{event.SideA: cfg.A, event.SideB: cfg.B},
		resync:        make(chan string, 1),
		fatal:         make(chan error, 2),
		failed:        make(chan failure, 256),
	}
}

// Run executa o serviço até ctx ser cancelado ou até uma falha terminal.
//
// A ordem importa e é a da seção 10 do escopo: watchers primeiro, First Sync
// depois. Invertida, alterações ocorridas durante o scan não gerariam evento
// e ficariam invisíveis até o próximo restart.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.xfer.Check(ctx); err != nil {
		return err
	}
	e.runPreflight(ctx)

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
		go e.watchSignals(ctx, side, w)
	}
	e.checkWatchPressure()

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

// watchSignals repassa overflow e falhas terminais de um watcher.
func (e *Engine) watchSignals(ctx context.Context, side event.Side, w *watcher.Watcher) {
	for {
		select {
		case <-w.Overflow():
			e.RequestResync(fmt.Sprintf("overflow da fila do inotify no lado %s", side))
		case err := <-w.Fatal():
			select {
			case e.fatal <- err:
			default:
			}
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
	beat := time.NewTicker(e.cfg.HeartbeatInterval)
	defer beat.Stop()
	retryT := time.NewTicker(retryTick)
	defer retryT.Stop()

	fail := func(ev event.Event, err error) {
		select {
		case e.failed <- failure{ev: ev, err: err}:
		case <-ctx.Done():
		}
	}
	disp := newDispatcher(ctx, e.cfg.SyncWorkers, e.process, fail)
	defer disp.close()
	if e.cfg.SyncWorkers > 1 {
		e.log.Info("processamento paralelo ativo",
			"workers", e.cfg.SyncWorkers, "particao", "subárvore de primeiro nível")
	}

	e.heartbeat(ctx)

	for {
		select {
		case <-ctx.Done():
			e.debo.Close()
			return nil

		// Uma raiz que deixou de existir é terminal: continuar rodando
		// deixaria um lado cego enquanto o outro segue propagando. Sair faz o
		// systemd reiniciar o serviço, que é o comportamento correto.
		case err := <-e.fatal:
			e.debo.Close()
			return err

		case ev, ok := <-e.debo.Out():
			if !ok {
				return nil
			}
			// Um evento novo torna irrelevante qualquer tentativa pendente
			// para o mesmo path.
			e.retry.cancel(ev.Side, ev.Path)
			disp.dispatch(ctx, ev, e.process, fail)

		case f := <-e.failed:
			e.handleFailure(ctx, f.ev, f.err)

		case reason := <-e.resync:
			e.log.Warn("full resync solicitado", "reason", reason)
			// O resync reconcilia as duas árvores inteiras; deixá-lo correr
			// junto com os workers seria duas decisões simultâneas sobre o
			// mesmo path.
			disp.inflight.Wait()
			if err := e.FullResync(ctx); err != nil {
				e.log.Error("full resync falhou", "err", err)
			}

		case now := <-retryT.C:
			e.processRetries(ctx, now, disp, fail)

		case <-beat.C:
			e.heartbeat(ctx)
			e.checkWatchPressure()
			// Resoluções pedidas pelo CLI são aplicadas aqui, com o guard em
			// mãos: aplicá-las de fora do processo faria os eventos
			// resultantes serem lidos como alteração externa e desfazerem a
			// própria resolução.
			disp.inflight.Wait()
			e.applyPendingResolutions(ctx)

		case <-sweep.C:
			if n := e.guard.Sweep(); n > 0 {
				pending, matched, expired := e.guard.Stats()
				e.log.Debug("guard sweep", "removidas", n, "pendentes", pending,
					"casadas", matched, "vencidas", expired)
			}
		}
	}
}

// handleFailure decide o destino de um evento que falhou: reenfileirar com
// backoff, ou desistir e marcar o estado.
func (e *Engine) handleFailure(ctx context.Context, ev event.Event, cause error) {
	if it := e.retry.schedule(ev, cause, time.Now()); it != nil {
		e.log.Warn("falha ao propagar; reenfileirado",
			"event", ev.String(), "tentativa", it.attempts,
			"proxima_em", time.Until(it.nextAt).Round(time.Millisecond).String(), "err", cause)
		return
	}

	e.log.Error("falha ao propagar; tentativas esgotadas",
		"event", ev.String(), "max_tentativas", e.cfg.RetryMaxAttempts, "err", cause)
	if err := e.db.MarkStatus(ctx, ev.Side, ev.Path, state.StatusError); err != nil {
		e.log.Error("falha marcando estado de erro", "path", ev.Path, "err", err)
	}
	// Desistir de um path sem mais nada seria deixar uma divergência
	// silenciosa; o resync é a rede de segurança.
	e.RequestResync("tentativas esgotadas para " + ev.Path)
}

func (e *Engine) processRetries(ctx context.Context, now time.Time,
	disp *dispatcher, fail func(event.Event, error)) {

	for _, it := range e.retry.due(now) {
		e.log.Info("retentando", "event", it.ev.String(), "tentativa", it.attempts)
		disp.dispatch(ctx, it.ev, e.process, fail)
	}
}

// heartbeat publica no banco o estado operacional do daemon, para que
// `synkronyx -status` possa reportá-lo sem precisar de IPC.
//
// Reaproveitar a tabela meta em vez de abrir um socket é a escolha KISS da
// seção 16: o banco já existe, já é aberto pelo comando de status e não
// acrescenta superfície de rede ao serviço.
func (e *Engine) heartbeat(ctx context.Context) {
	watches := 0
	for _, w := range e.watchers {
		watches += w.WatchCount()
	}

	kv := map[string]string{
		state.MetaHeartbeat:  time.Now().Format(time.RFC3339),
		state.MetaWatches:    strconv.Itoa(watches),
		state.MetaRetryQueue: strconv.Itoa(e.retry.len()),
		state.MetaPID:        strconv.Itoa(os.Getpid()),
	}
	if err := e.db.SetMetaMany(ctx, kv); err != nil {
		e.log.Warn("falha publicando heartbeat", "err", err)
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

	// Arquivos especiais não são sincronizáveis; ver engine.skipSpecial.
	if ev.Kind != event.KindDelete {
		if st, err := hash.StatOf(srcAbs); err == nil && e.skipSpecial(ev.Path, st.Kind, e.log) {
			return nil
		}
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
	// registram o mesmo digest: nada a fazer.
	return hash.Compare(srcEntry.Digest, dstEntry.Digest) == hash.Same, nil
}

func (e *Engine) propagateContent(ctx context.Context, ev event.Event, srcAbs, dstAbs string) error {
	if ev.IsDir {
		return e.propagateDir(ctx, ev, srcAbs, dstAbs)
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
	dst := ev.Side.Opposite()
	e.guard.Expect(dst, ev.Path, 3)
	if err := e.xfer.CopyFile(ctx, srcAbs, dstAbs); err != nil {
		e.guard.Forget(dst, ev.Path)
		return err
	}
	return e.recordPair(ctx, ev.Path, srcAbs, dstAbs, false)
}

// propagateDir cria o diretório no destino. Se a origem já tiver conteúdo,
// copia a árvore inteira em uma única invocação de rsync — que é também a
// única forma de --hard-links surtir efeito.
func (e *Engine) propagateDir(ctx context.Context, ev event.Event, srcAbs, dstAbs string) error {
	dst := ev.Side.Opposite()

	empty, err := isEmptyDir(srcAbs)
	if err != nil {
		return err
	}

	// A cópia de árvore toca um número indeterminado de paths no destino.
	e.guard.ExpectSubtree(dst, ev.Path)
	defer e.guard.ForgetSubtree(dst, ev.Path)

	if empty {
		if err := e.xfer.Mkdir(srcAbs, dstAbs); err != nil {
			return err
		}
	} else if err := e.xfer.CopyTree(ctx, srcAbs, dstAbs); err != nil {
		return err
	}

	if err := e.recordPair(ctx, ev.Path, srcAbs, dstAbs, true); err != nil {
		return err
	}
	if empty {
		return nil
	}
	// O conteúdo copiado também precisa entrar no estado, senão o primeiro
	// evento em cada arquivo o trataria como desconhecido.
	return e.recordSubtree(ctx, ev.Path)
}

func (e *Engine) propagateDelete(ctx context.Context, ev event.Event, dstAbs string) error {
	dst := ev.Side.Opposite()

	if ev.IsDir {
		// Remover recursivamente gera um evento por entrada apagada.
		e.guard.ExpectSubtree(dst, ev.Path)
		defer e.guard.ForgetSubtree(dst, ev.Path)

		if err := e.removeDir(ctx, dst, ev.Path, dstAbs); err != nil {
			return err
		}
		return e.db.DeleteSubtree(ctx, ev.Path)
	}

	e.guard.Expect(dst, ev.Path, 1)
	if err := e.xfer.Remove(dstAbs, false); err != nil {
		e.guard.Forget(dst, ev.Path)
		return err
	}
	if err := e.db.Delete(ctx, ev.Side, ev.Path); err != nil {
		return err
	}
	return e.db.Delete(ctx, dst, ev.Path)
}

func (e *Engine) propagateMove(ctx context.Context, ev event.Event) error {
	dst := ev.Side.Opposite()
	fromAbs := e.abs(dst, ev.From)
	toAbs := e.abs(dst, ev.Path)

	// O rename toca dois paths no destino: MOVED_FROM em um, MOVED_TO no
	// outro. Para diretórios, todo o conteúdo muda de path junto.
	if ev.IsDir {
		e.guard.ExpectSubtree(dst, ev.From)
		e.guard.ExpectSubtree(dst, ev.Path)
		defer e.guard.ForgetSubtree(dst, ev.From)
		defer e.guard.ForgetSubtree(dst, ev.Path)
	} else {
		e.guard.Expect(dst, ev.From, 1)
		e.guard.Expect(dst, ev.Path, 1)
	}

	if err := e.xfer.Move(fromAbs, toAbs); err != nil {
		if !ev.IsDir {
			e.guard.Forget(dst, ev.From)
			e.guard.Forget(dst, ev.Path)
		}

		// A origem pode não existir no destino — estado divergente. Em vez de
		// insistir no rename, reconciliar a subárvore é a resposta correta:
		// para um arquivo é equivalente a copiá-lo, e para um diretório
		// resolve também o conteúdo, que uma cópia simples do path deixaria
		// pela metade.
		if errors.Is(err, os.ErrNotExist) {
			e.log.Warn("origem do rename ausente no destino; reconciliando subárvore",
				"from", ev.From, "to", ev.Path, "is_dir", ev.IsDir)
			if err := e.db.DeleteSubtree(ctx, ev.From); err != nil {
				return err
			}
			return e.reconcileSubtree(ctx, ev.Path)
		}
		return err
	}

	if err := e.db.DeleteSubtree(ctx, ev.From); err != nil {
		return err
	}
	if err := e.recordPair(ctx, ev.Path, e.abs(ev.Side, ev.Path), toAbs, ev.IsDir); err != nil {
		return err
	}
	if !ev.IsDir {
		return nil
	}
	// Os paths de todo o conteúdo mudaram; o estado precisa acompanhar.
	if err := e.db.DeleteSubtree(ctx, ev.Path); err != nil {
		return err
	}
	if err := e.recordPair(ctx, ev.Path, e.abs(ev.Side, ev.Path), toAbs, true); err != nil {
		return err
	}
	return e.recordSubtree(ctx, ev.Path)
}

func (e *Engine) propagateAttrs(ctx context.Context, ev event.Event, srcAbs, dstAbs string) error {
	dst := ev.Side.Opposite()
	e.guard.Expect(dst, ev.Path, 1)
	if err := e.xfer.SyncAttrs(ctx, srcAbs, dstAbs); err != nil {
		e.guard.Forget(dst, ev.Path)
		return err
	}
	return e.recordPair(ctx, ev.Path, srcAbs, dstAbs, ev.IsDir)
}

// digestOf calcula o digest de abs conforme a configuração de amostragem.
func (e *Engine) digestOf(abs string) (hash.Digest, error) {
	return hash.Compute(abs, e.cfg.HashMaxBytes, e.cfg.HashSampleBytes)
}

// entryFor monta a entrada de estado de um path absoluto.
func (e *Engine) entryFor(side event.Side, rel, abs string, at time.Time) (state.Entry, error) {
	st, err := hash.StatOf(abs)
	if err != nil {
		return state.Entry{}, err
	}
	entry := state.Entry{
		Path: rel, Side: side, IsDir: st.IsDir, Size: st.Size,
		MTime: st.MTime, Mode: st.Mode, SyncedAt: at, Status: state.StatusSynced,
		Uid: st.Uid, Gid: st.Gid,
	}
	if !st.IsDir {
		d, err := e.digestOf(abs)
		if err != nil {
			return state.Entry{}, err
		}
		entry.Digest = d
	}
	return entry, nil
}

// recordPair grava o estado dos dois lados após uma propagação bem-sucedida.
func (e *Engine) recordPair(ctx context.Context, rel, srcAbs, dstAbs string, isDir bool) error {
	now := time.Now()

	srcEntry, err := e.entryFor(e.sideOf(srcAbs), rel, srcAbs, now)
	if err != nil {
		return err
	}
	dstEntry, err := e.entryFor(e.sideOf(dstAbs), rel, dstAbs, now)
	if err != nil {
		return err
	}
	return e.db.PutBoth(ctx, srcEntry, dstEntry)
}

// recordSubtree grava o estado de tudo sob rel, nos dois lados. Usado depois
// de operações que movem ou copiam árvores inteiras de uma vez.
func (e *Engine) recordSubtree(ctx context.Context, rel string) error {
	excluder := config.NewExcluder(e.cfg.Exclude)
	now := time.Now()

	for _, side := range []event.Side{event.SideA, event.SideB} {
		root := e.roots[side]
		start := root
		if rel != "." && rel != "" {
			start = filepath.Join(root, rel)
		}

		err := filepath.WalkDir(start, func(abs string, d os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			r, err := filepath.Rel(root, abs)
			if err != nil {
				return err
			}
			if excluder.Excluded(r, d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			entry, err := e.entryFor(side, r, abs, now)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			return e.db.Put(ctx, entry)
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func isEmptyDir(abs string) (bool, error) {
	f, err := os.Open(abs)
	if err != nil {
		return false, err
	}
	defer f.Close()

	names, err := f.Readdirnames(1)
	if err != nil && err.Error() != "EOF" {
		if len(names) == 0 {
			return true, nil
		}
		return false, err
	}
	return len(names) == 0, nil
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
