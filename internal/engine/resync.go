package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/hash"
	"github.com/lcarlin/synkronyx/internal/scan"
	"github.com/lcarlin/synkronyx/internal/state"
)

// firstSync estabelece o estado inicial (seção 10).
//
// Roda depois de os watchers subirem, não antes: eventos ocorridos durante o
// scan ficam enfileirados no debouncer e são aplicados na sequência. A ordem
// inversa deixaria uma janela cega.
func (e *Engine) firstSync(ctx context.Context) error {
	done, err := e.db.Meta(ctx, state.MetaFirstSync)
	if err != nil {
		return err
	}
	empty, err := e.db.IsEmpty(ctx)
	if err != nil {
		return err
	}

	switch {
	case done == "":
		e.log.Info("iniciando first sync", "policy", string(e.cfg.FirstSyncPolicy))
	case empty && e.rootsHaveContent():
		// O First Sync já rodou, o banco não tem entradas e ainda há dados no
		// disco: o arquivo de estado foi apagado, recriado ou perdido junto
		// com o disco. Com as duas árvores vazias não haveria o que
		// ressuscitar, e o aviso seria só ruído.
		e.warnEmptyState(done)
	default:
		// Reconciliar mesmo assim: o serviço pode ter ficado parado enquanto
		// a árvore mudava, e nada disso gerou evento.
		e.log.Info("reconciliando árvores após restart", "first_sync_em", done)
	}

	if err := e.reconcile(ctx, "first-sync", "."); err != nil {
		return err
	}
	return e.db.SetMeta(ctx, state.MetaFirstSync, time.Now().Format(time.RFC3339))
}

// warnEmptyState avisa sobre a consequência de reconciliar sem estado.
//
// Com o banco vazio, `union` não tem como distinguir "arquivo novo neste
// lado" de "arquivo apagado do outro lado": as duas situações têm exatamente
// a mesma aparência no disco. O resultado é que remoções feitas enquanto o
// serviço estava parado são desfeitas — o arquivo volta, copiado do lado onde
// ainda existe.
//
// Isso é comportamento esperado, não defeito: sem histórico, ressuscitar um
// arquivo é o erro recuperável (basta apagar de novo) e apagá-lo é o erro
// irrecuperável. Fail Safe manda escolher o primeiro. O que não é aceitável é
// fazer isso em silêncio, então o aviso é explícito.
func (e *Engine) warnEmptyState(firstSyncAt string) {
	e.log.Warn("banco de estado está vazio, mas o first sync já havia rodado antes — "+
		"remoções feitas com o serviço parado serão desfeitas, porque não há histórico "+
		"para distingui-las de arquivos novos",
		"first_sync_anterior", firstSyncAt,
		"state_path", e.cfg.StatePath,
		"policy", string(e.cfg.FirstSyncPolicy))
}

// rootsHaveContent informa se alguma das raízes tem algo dentro.
func (e *Engine) rootsHaveContent() bool {
	for _, root := range e.roots {
		empty, err := isEmptyDir(root)
		if err != nil {
			// Não conseguir ler a raiz é problema de outra ordem, e vai
			// aparecer no scan logo em seguida; aqui basta não engolir o aviso.
			return true
		}
		if !empty {
			return true
		}
	}
	return false
}

// FullResync reconstrói o estado a partir do disco (seção 11).
//
// Diferença em relação ao First Sync: aqui o estado gravado é tratado como
// suspeito, não como verdade. Só o que está no disco conta.
func (e *Engine) FullResync(ctx context.Context) error {
	start := time.Now()
	if err := e.reconcile(ctx, "full-resync", "."); err != nil {
		return err
	}
	if err := e.db.SetMeta(ctx, state.MetaLastResync, time.Now().Format(time.RFC3339)); err != nil {
		return err
	}
	e.log.Info("full resync concluído", "duracao", time.Since(start).String())
	return nil
}

// reconcileSubtree reconcilia apenas rel e o que está abaixo dele.
//
// É a resposta certa quando uma operação localizada falha de forma que
// deixa a subárvore em estado desconhecido — um rename cuja origem não existe
// no destino, por exemplo. Varrer as duas árvores inteiras resolveria também,
// mas cobrar o preço de um Full Resync por uma divergência de um diretório é
// desproporcional.
func (e *Engine) reconcileSubtree(ctx context.Context, rel string) error {
	return e.reconcile(ctx, "subtree", rel)
}

// reconcile compara as duas árvores sob scope e aplica a política configurada.
// scope "." abrange a árvore inteira.
//
// São três fases, e a separação entre elas é o que permite paralelizar sem
// abrir mão de determinismo:
//
//  1. Scan das duas árvores, em paralelo — são independentes por construção.
//  2. Comparação de conteúdo dos paths presentes dos dois lados, em paralelo.
//     É a fase cara (lê e hasheia arquivos) e é puramente leitura, então
//     paralelizar não muda resultado nenhum.
//  3. Aplicação, sequencial e em ordem de profundidade. Aqui a ordem importa:
//     criar um filho antes do pai não funciona, e a lista de subárvores já
//     resolvidas por inteiro só faz sentido se construída em ordem.
func (e *Engine) reconcile(ctx context.Context, phase, scope string) error {
	excluder := config.NewExcluder(e.cfg.Exclude)

	start := time.Now()
	invA, invB, err := e.scanBothSides(ctx, phase, scope, excluder)
	if err != nil {
		return err
	}
	e.log.Info("scan concluído", "phase", phase, "scope", scope,
		"entradas_a", len(invA), "entradas_b", len(invB),
		"duracao", time.Since(start).Round(time.Millisecond).String())

	comparisons := e.compareInParallel(ctx, invA, invB)

	var applied, skipped, conflicts, failed, special int

	total := len(scan.Union(invA, invB))
	progress := e.newProgress(phase, scope, total)
	defer progress.done()

	// Diretórios já resolvidos por inteiro (copiados ou removidos como
	// árvore). Seus descendentes precisam ser saltados, e não por economia:
	// o inventário foi tirado antes da operação, então para um filho ele
	// ainda diz "existe só de um lado" — enquanto o estado, já atualizado
	// pela cópia da árvore, diz que o path existe do outro lado. Processar
	// nessas condições faria reconcileOneSided ler a situação como "foi
	// apagado lá" e remover o arquivo que acabou de ser sincronizado.
	//
	// Union ordena por profundidade, então o diretório sempre aparece antes
	// do seu conteúdo e a lista está completa quando os filhos chegam.
	var handled []string

	for _, rel := range scan.Union(invA, invB) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		progress.tick()

		if underAny(rel, handled) {
			skipped++
			continue
		}

		stA, inA := invA[rel]
		stB, inB := invB[rel]

		// Arquivos especiais não são sincronizáveis dos dois lados; ver
		// engine.skipSpecial.
		if (inA && stA.IsSpecial()) || (inB && stB.IsSpecial()) {
			kind := stA.Kind
			if !inA {
				kind = stB.Kind
			}
			e.skipSpecial(rel, kind, e.log)
			special++
			continue
		}

		var res reconcileResult
		var err error
		switch {
		case inA && !inB:
			err = e.reconcileOneSided(ctx, rel, event.SideA, stA)
			if stA.IsDir {
				handled = append(handled, rel)
			}
		case inB && !inA:
			err = e.reconcileOneSided(ctx, rel, event.SideB, stB)
			if stB.IsDir {
				handled = append(handled, rel)
			}
		case inA && inB:
			res, err = e.reconcileBothSides(ctx, rel, stA, stB, comparisons[rel])
		}

		if err != nil {
			// Falhar em um path não invalida a reconciliação dos outros; o
			// retry e o próximo resync cobrem o que não passou agora.
			e.log.Error("falha reconciliando", "path", rel, "err", err)
			failed++
			continue
		}
		switch res {
		case reconcileNoop:
			skipped++
		case reconcileConflict:
			conflicts++
		case reconcileHandledSubtree:
			conflicts++
			handled = append(handled, rel)
		default:
			applied++
		}
	}

	e.log.Info("reconciliação concluída", "phase", phase, "scope", scope,
		"aplicadas", applied, "sem_acao", skipped, "conflitos", conflicts,
		"falhas", failed, "especiais_ignorados", special,
		"duracao", time.Since(start).Round(time.Millisecond).String())
	return nil
}

// scanBothSides percorre as duas árvores em paralelo. São independentes, e o
// tempo de parada de uma árvore grande é dominado por I/O de metadados —
// então rodar as duas juntas quase divide o tempo por dois.
func (e *Engine) scanBothSides(ctx context.Context, phase, scope string,
	excluder scan.Matcher) (scan.Inventory, scan.Inventory, error) {

	type result struct {
		inv scan.Inventory
		err error
	}
	out := make([]result, 2)
	roots := []string{e.cfg.A, e.cfg.B}
	names := []string{"A", "B"}

	var wg sync.WaitGroup
	for i := range roots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last atomic.Int64
			inv, err := scan.WalkSubtree(ctx, roots[i], scope, excluder, func(seen int) {
				// Relatar por tempo, não por contagem: em árvore pequena não
				// sai nada, em árvore enorme sai a cada intervalo.
				if e.cfg.ProgressInterval <= 0 {
					return
				}
				now := time.Now().UnixNano()
				if prev := last.Load(); now-prev < int64(e.cfg.ProgressInterval) {
					return
				}
				last.Store(now)
				e.log.Info("scan em andamento", "phase", phase, "scope", scope,
					"side", names[i], "entradas", seen)
			})
			out[i] = result{inv: inv, err: err}
		}()
	}
	wg.Wait()

	if out[0].err != nil {
		return nil, nil, fmt.Errorf("scan de A: %w", out[0].err)
	}
	if out[1].err != nil {
		return nil, nil, fmt.Errorf("scan de B: %w", out[1].err)
	}
	return out[0].inv, out[1].inv, nil
}

// comparison é o resultado pré-calculado da comparação de um path presente
// nos dois lados.
//
// Carrega os digests, e não só o veredito, porque a fase sequencial precisa
// deles para descobrir QUAL lado mudou — e recalculá-los ali desfaria o
// ganho de ter comparado em paralelo.
type comparison struct {
	digestA hash.Digest
	digestB hash.Digest
	differs bool
	err     error
	known   bool
}

// compareInParallel calcula, para cada path presente dos dois lados, se os
// conteúdos divergem.
//
// É a fase cara do resync — ler e hashear arquivos — e é puramente leitura:
// nenhuma decisão é tomada e nada é escrito, então o paralelismo não muda o
// resultado, só o tempo.
func (e *Engine) compareInParallel(ctx context.Context, invA, invB scan.Inventory) map[string]comparison {
	type job struct {
		rel      string
		absA     string
		absB     string
		stA, stB hash.Stat
	}

	var jobs []job
	for rel, stA := range invA {
		stB, ok := invB[rel]
		if !ok || stA.IsDir || stB.IsDir || stA.IsSpecial() || stB.IsSpecial() {
			continue
		}
		jobs = append(jobs, job{
			rel:  rel,
			absA: e.abs(event.SideA, rel), absB: e.abs(event.SideB, rel),
			stA: stA, stB: stB,
		})
	}
	if len(jobs) == 0 {
		return nil
	}

	workers := e.cfg.SyncWorkers
	if workers < 1 {
		workers = 1
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}

	out := make(map[string]comparison, len(jobs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan job)

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range next {
				differs, dA, dB, err := e.compareContent(j.absA, j.absB)
				if !differs && err == nil {
					differs = sampledButStale(dA, dB, j.stA, j.stB)
				}
				mu.Lock()
				out[j.rel] = comparison{
					digestA: dA, digestB: dB,
					differs: differs, err: err, known: true,
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		select {
		case next <- j:
		case <-ctx.Done():
		}
	}
	close(next)
	wg.Wait()

	return out
}

// progressReporter emite andamento de uma reconciliação longa.
type progressReporter struct {
	log      *slog.Logger
	phase    string
	scope    string
	total    int
	interval time.Duration
	seen     int
	last     time.Time
	start    time.Time
}

func (e *Engine) newProgress(phase, scope string, total int) *progressReporter {
	now := time.Now()
	return &progressReporter{
		log: e.log, phase: phase, scope: scope, total: total,
		interval: e.cfg.ProgressInterval, last: now, start: now,
	}
}

func (p *progressReporter) tick() {
	p.seen++
	if p.interval <= 0 || time.Since(p.last) < p.interval {
		return
	}
	p.last = time.Now()

	pct := 0
	if p.total > 0 {
		pct = p.seen * 100 / p.total
	}
	p.log.Info("reconciliação em andamento", "phase", p.phase, "scope", p.scope,
		"processados", p.seen, "total", p.total, "pct", pct,
		"decorrido", time.Since(p.start).Round(time.Second).String())
}

func (p *progressReporter) done() {}

// underAny informa se rel está sob algum dos prefixos dados.
func underAny(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		if rel == p {
			continue
		}
		if strings.HasPrefix(rel, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

type reconcileResult int

const (
	reconcileApplied reconcileResult = iota
	reconcileNoop
	reconcileConflict
	// reconcileHandledSubtree é um conflito que também impede tratar os
	// descendentes do path.
	reconcileHandledSubtree
)

// reconcileOneSided trata um path que existe em apenas um lado.
//
// A ambiguidade aqui é real e o escopo pede que a política seja decidida
// antes de rodar: um arquivo presente só em A pode ser novo (deve ser
// copiado) ou pode ter sido apagado em B (deve ser apagado em A). Sem estado
// anterior não há como distinguir — daí FirstSyncPolicy, e daí o aviso de
// warnEmptyState quando o estado se perdeu.
func (e *Engine) reconcileOneSided(ctx context.Context, rel string, present event.Side, st hash.Stat) error {
	absent := present.Opposite()

	// Se o estado registra que o path já existiu no lado ausente, a leitura
	// correta é "foi apagado lá", não "é novo aqui".
	prev, err := e.db.Get(ctx, absent, rel)
	if err != nil {
		return err
	}
	deletedRemotely := prev != nil

	switch e.cfg.FirstSyncPolicy {
	case config.FirstSyncAWins:
		if present == event.SideA {
			return e.copyOneSided(ctx, rel, present, st)
		}
		return e.deleteOneSided(ctx, rel, present, st)

	case config.FirstSyncBWins:
		if present == event.SideB {
			return e.copyOneSided(ctx, rel, present, st)
		}
		return e.deleteOneSided(ctx, rel, present, st)

	default: // FirstSyncUnion
		if deletedRemotely {
			return e.deleteOneSided(ctx, rel, present, st)
		}
		return e.copyOneSided(ctx, rel, present, st)
	}
}

func (e *Engine) copyOneSided(ctx context.Context, rel string, src event.Side, st hash.Stat) error {
	ev := event.Event{Side: src, Kind: event.KindCreate, Path: rel, IsDir: st.IsDir, At: time.Now()}
	return e.propagateContent(ctx, ev, e.abs(src, rel), e.abs(src.Opposite(), rel))
}

func (e *Engine) deleteOneSided(ctx context.Context, rel string, present event.Side, st hash.Stat) error {
	e.log.Info("removendo por política de reconciliação", "path", rel, "side", present.String())

	if st.IsDir {
		e.guard.ExpectSubtree(present, rel)
		defer e.guard.ForgetSubtree(present, rel)
		if err := e.removeDir(ctx, present, rel, e.abs(present, rel)); err != nil {
			return err
		}
		return e.db.DeleteSubtree(ctx, rel)
	}

	e.guard.Expect(present, rel, 1)
	if err := e.xfer.Remove(e.abs(present, rel), false); err != nil {
		e.guard.Forget(present, rel)
		return err
	}
	return e.db.DeleteSubtree(ctx, rel)
}

// reconcileBothSides trata um path presente nos dois lados. cmp é o resultado
// da comparação de conteúdo já calculada na fase paralela; se não houver
// (cmp.known falso), a comparação é feita aqui.
func (e *Engine) reconcileBothSides(ctx context.Context, rel string, stA, stB hash.Stat,
	cmp comparison) (reconcileResult, error) {
	if stA.IsDir && stB.IsDir {
		// Existir dos dois lados não significa estar igual: modo e mtime de
		// um diretório também são sincronizados. Retornar aqui sem comparar,
		// como se fazia, deixava um chmod em diretório invisível para sempre.
		if attrsDiverged(stA, stB) {
			return e.reconcileAttrs(ctx, rel, stA, stB)
		}
		return reconcileNoop, nil
	}
	if stA.IsDir != stB.IsDir {
		// Um é diretório e o outro é arquivo. É conflito por definição, e
		// nenhuma política automática deveria escolher — sobrescrever
		// apagaria uma árvore inteira.
		e.log.Warn("tipo divergente entre os lados", "path", rel,
			"a_dir", stA.IsDir, "b_dir", stB.IsDir)
		if err := e.db.RecordConflict(ctx, rel, hash.Digest{}, hash.Digest{}); err != nil {
			return reconcileConflict, err
		}
		// O tipo divergente contamina tudo abaixo: se A tem um arquivo onde B
		// tem um diretório, cada filho de B falharia ao ser copiado para
		// dentro de algo que não é diretório. Sinalizar como resolvido por
		// inteiro evita uma cascata de erros que descrevem, todos, o mesmo
		// problema — o do pai.
		return reconcileHandledSubtree, nil
	}

	absA, absB := e.abs(event.SideA, rel), e.abs(event.SideB, rel)

	differs, err := cmp.differs, cmp.err
	if !cmp.known {
		differs, cmp.digestA, cmp.digestB, err = e.compareContent(absA, absB)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return reconcileNoop, nil
		}
		return reconcileNoop, err
	}
	if !differs {
		// Conteúdos iguais não encerram o assunto: modo e permissões também
		// são sincronizados (seção 2 do escopo), e uma alteração de atributos
		// feita com o serviço parado não gerou evento nenhum. Só a
		// reconciliação pode notá-la.
		if attrsDiverged(stA, stB) {
			return e.reconcileAttrs(ctx, rel, stA, stB)
		}
		// Nada a fazer além de registrar o estado.
		return reconcileNoop, e.recordPair(ctx, rel, absA, absB, false)
	}

	switch e.cfg.FirstSyncPolicy {
	case config.FirstSyncAWins:
		ev := event.Event{Side: event.SideA, Kind: event.KindModify, Path: rel, At: time.Now()}
		return reconcileApplied, e.propagateContent(ctx, ev, absA, absB)

	case config.FirstSyncBWins:
		ev := event.Event{Side: event.SideB, Kind: event.KindModify, Path: rel, At: time.Now()}
		return reconcileApplied, e.propagateContent(ctx, ev, absB, absA)

	default:
		// Conteúdos divergentes NÃO implicam conflito. O que caracteriza
		// conflito, pela seção 9, é não haver origem única — e o estado sabe
		// dizer se há: se apenas um dos lados se afastou do que foi
		// sincronizado por último, aquele lado é a origem, e o caso é uma
		// alteração unilateral comum.
		//
		// Consultar o estado aqui é o que evita que toda edição feita com o
		// serviço parado vire um arquivo .sync-conflict- sem motivo.
		origin, decided, err := e.originOf(ctx, rel, cmp, stA, stB)
		if err != nil {
			return reconcileNoop, err
		}
		if decided {
			ev := event.Event{Side: origin, Kind: event.KindModify, Path: rel, At: time.Now()}
			return reconcileApplied, e.propagateContent(ctx, ev,
				e.abs(origin, rel), e.abs(origin.Opposite(), rel))
		}

		// Sem origem única: os dois lados mudaram, ou não há estado que
		// permita afirmar qual mudou. Delega para a política de conflito.
		src := event.SideA
		if stB.MTime.After(stA.MTime) {
			src = event.SideB
		}
		ev := event.Event{Side: src, Kind: event.KindModify, Path: rel, At: time.Now()}
		if err := e.resolveConflict(ctx, ev, e.abs(src, rel), e.abs(src.Opposite(), rel)); err != nil {
			return reconcileConflict, err
		}
		return reconcileConflict, nil
	}
}

// originOf descobre qual lado se afastou do último estado sincronizado.
//
// Devolve (lado, true) quando exatamente um dos lados mudou — há origem
// única, e a alteração deve ser propagada como qualquer outra. Devolve
// (_, false) quando os dois mudaram, quando nenhum mudou (estado
// inconsistente com o disco) ou quando não há estado para comparar: nesses
// casos ninguém pode afirmar de onde veio a versão boa, e a decisão cabe à
// política de conflito.
func (e *Engine) originOf(ctx context.Context, rel string, cmp comparison,
	stA, stB hash.Stat) (event.Side, bool, error) {

	entryA, err := e.db.Get(ctx, event.SideA, rel)
	if err != nil {
		return event.SideA, false, err
	}
	entryB, err := e.db.Get(ctx, event.SideB, rel)
	if err != nil {
		return event.SideA, false, err
	}
	if entryA == nil || entryB == nil {
		// Par nunca sincronizado: não há linha de base.
		return event.SideA, false, nil
	}

	changedA := sideChanged(cmp.digestA, stA, *entryA)
	changedB := sideChanged(cmp.digestB, stB, *entryB)

	switch {
	case changedA && !changedB:
		return event.SideA, true, nil
	case changedB && !changedA:
		return event.SideB, true, nil
	default:
		return event.SideA, false, nil
	}
}

// sampledButStale decide se dois digests amostrados iguais devem, mesmo
// assim, ser tratados como divergentes.
//
// Um digest amostrado não prova igualdade: ele cobre tamanho, início e fim, e
// uma alteração no meio de um arquivo grande passa por ele. No fluxo de
// eventos isso não importa, porque o mtime é comparado antes. Na
// reconciliação importava, e era um buraco de verdade — a única rede que
// deveria pegar o caso usava exatamente o mesmo digest cego.
//
// Mtimes iguais são o estado normal de um par sincronizado, porque o rsync
// preserva o mtime da origem. Mtimes diferentes sob digest amostrado são,
// portanto, evidência de que algo mudou onde a amostra não olha — e o
// conservador é acreditar no mtime.
//
// O custo de errar para mais é uma transferência a mais, na qual o algoritmo
// delta do rsync não vai copiar quase nada se os arquivos forem mesmo iguais.
// O custo de errar para menos é divergência silenciosa que sobrevive ao full
// resync.
func sampledButStale(dA, dB hash.Digest, stA, stB hash.Stat) bool {
	if dA.Kind != hash.KindPartial || dB.Kind != hash.KindPartial {
		return false
	}
	return !stA.Unchanged(stB)
}

// sideChanged informa se um lado se afastou do que o estado registra.
//
// O digest é a evidência principal. Quando ele é amostrado, porém, digest
// igual não prova nada — é o mesmo ponto cego de sampledButStale —, e o mtime
// entra como segunda evidência. Sem isso, uma alteração no meio de um arquivo
// grande seria detectada como divergência mas não teria origem atribuída, e
// acabaria tratada como conflito apesar de um lado só ter mudado.
func sideChanged(current hash.Digest, st hash.Stat, recorded state.Entry) bool {
	if hash.Compare(current, recorded.Digest) != hash.Same {
		return true
	}
	if current.Kind == hash.KindPartial {
		return !st.Unchanged(recorded.Stat())
	}
	return false
}

// reconcileAttrs propaga uma divergência de permissões entre lados cujo
// conteúdo é idêntico.
//
// A origem é decidida pelo estado, como no caso de conteúdo: se apenas um lado
// se afastou do modo registrado, aquele lado manda. Quando os dois mudaram,
// vence o mtime mais recente — permissões não são dados, e preservar as duas
// versões, como se faz com conteúdo em conflito, não significaria nada.
func (e *Engine) reconcileAttrs(ctx context.Context, rel string, stA, stB hash.Stat) (reconcileResult, error) {
	entryA, err := e.db.Get(ctx, event.SideA, rel)
	if err != nil {
		return reconcileNoop, err
	}
	entryB, err := e.db.Get(ctx, event.SideB, rel)
	if err != nil {
		return reconcileNoop, err
	}

	origin := event.SideA
	switch {
	case entryA != nil && entryB != nil:
		changedA := attrsDiverged(stA, entryA.Stat())
		changedB := attrsDiverged(stB, entryB.Stat())
		switch {
		case changedA && !changedB:
			origin = event.SideA
		case changedB && !changedA:
			origin = event.SideB
		default:
			origin = newerSide(stA, stB)
		}
	default:
		origin = newerSide(stA, stB)
	}

	e.log.Info("propagando atributos divergentes", "path", rel,
		"origem", origin.String(),
		"modo_a", stA.Mode.Perm().String(), "modo_b", stB.Mode.Perm().String(),
		"mtime_a", stA.MTime.Format(time.RFC3339), "mtime_b", stB.MTime.Format(time.RFC3339))

	ev := event.Event{Side: origin, Kind: event.KindAttrib, Path: rel, At: time.Now()}
	if err := e.propagateAttrs(ctx, ev,
		e.abs(origin, rel), e.abs(origin.Opposite(), rel)); err != nil {
		return reconcileNoop, err
	}
	return reconcileApplied, nil
}

func newerSide(stA, stB hash.Stat) event.Side {
	if stB.MTime.After(stA.MTime) {
		return event.SideB
	}
	return event.SideA
}

// attrsDiverged informa se modo ou mtime diferem entre dois stats.
//
// Propagar divergência de mtime não é preciosismo. Um par sincronizado tem
// mtimes idênticos, porque o rsync preserva o da origem, e sampledButStale
// conta com isso: mtimes divergentes sob digest amostrado são lidos como
// alteração invisível à amostra. Deixar o mtime divergir por conta de um
// `touch` faria cada resync retransferir o arquivo grande inteiro, para
// sempre, sem nunca convergir.
//
// A tolerância de um segundo vem de Stat.Unchanged, e existe porque nem todo
// filesystem guarda sub-segundo.
func attrsDiverged(a, b hash.Stat) bool {
	if a.Mode.Perm() != b.Mode.Perm() {
		return true
	}
	// Unchanged compara tamanho, tipo e mtime; aqui só o mtime interessa,
	// então os outros campos são igualados antes da comparação.
	a.Size, b.Size = 0, 0
	a.IsDir, b.IsDir = false, false
	return !a.Unchanged(b)
}
