// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package engine

import (
	"context"
	"log/slog"
	"os"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/hash"
	"github.com/lcarlin/synkronyx/internal/preflight"
)

// watchPressureWarn é a fração do limite de watches do inotify a partir da
// qual vale avisar. Acima disso, um punhado de diretórios novos basta para
// estourar o limite e cegar parte da árvore.
const watchPressureWarn = 0.8

// runPreflight avisa sobre condições em que o serviço funciona, mas não
// exatamente como a configuração promete.
//
// Nenhuma delas impede a subida: são todas recuperáveis, e derrubar o serviço
// por causa de um aviso seria pior que operar com a limitação conhecida.
func (e *Engine) runPreflight(_ context.Context) {
	e.checkOwnership()
	e.checkFilesystems()

	if e.syncOwnership {
		e.log.Info("dono e grupo entram na reconciliação", "euid", euid())
	}
}

// checkOwnership avisa quando a configuração pede preservação de dono e o
// processo não tem como fazê-la.
//
// O caso é silencioso por natureza: o rsync não falha, apenas escreve os
// arquivos com o dono do próprio processo. Quem configurou --archive esperando
// preservar propriedade descobriria só ao comparar `ls -l` dos dois lados.
func (e *Engine) checkOwnership() {
	if !preflight.WantsOwnership(e.cfg.RsyncArgs) {
		return
	}
	if preflight.PreservesOwnership() {
		return
	}
	e.log.Warn("os argumentos do rsync pedem preservação de dono e grupo, mas o processo "+
		"não roda como root: os arquivos no destino ficarão com o dono do serviço, "+
		"o rsync não vai reclamar, e a reconciliação não compara dono nem grupo — "+
		"comparar sem poder alterar produziria divergência detectada e nunca resolvida",
		"rsync_args", e.cfg.RsyncArgs, "euid", euid())
}

// checkFilesystems avisa quando uma das raízes está em filesystem de rede.
//
// --numeric-ids preserva o número do usuário, não o nome. Entre máquinas que
// não compartilham a base de usuários, o mesmo número é outra pessoa — os
// arquivos chegam íntegros e com a propriedade errada.
func (e *Engine) checkFilesystems() {
	for _, side := range []event.Side{event.SideA, event.SideB} {
		root := e.roots[side]
		mount, err := preflight.MountOf(root)
		if err != nil {
			e.log.Debug("não foi possível identificar o filesystem", "root", root, "err", err)
			continue
		}
		e.log.Info("filesystem da raiz", "side", side.String(),
			"root", root, "fstype", mount.FSType, "mount", mount.Point)

		if mount.IsNetwork() && usesNumericIDs(e.cfg.RsyncArgs) {
			e.log.Warn("raiz em filesystem de rede com --numeric-ids: os números de "+
				"usuário e grupo só significam a mesma pessoa se as duas pontas "+
				"compartilharem a base de usuários",
				"side", side.String(), "root", root, "fstype", mount.FSType)
		}
	}
}

// checkWatchPressure avisa quando o consumo de watches se aproxima do limite
// do kernel. Chamado depois de os watchers subirem, quando o número é real.
func (e *Engine) checkWatchPressure() {
	used := 0
	for _, w := range e.watchers {
		used += w.WatchCount()
	}
	pressure := preflight.WatchPressure(used)
	if pressure < watchPressureWarn {
		return
	}

	limit, _ := preflight.InotifyLimit()
	e.log.Warn("consumo de watches do inotify perto do limite; ao estourar, parte da "+
		"árvore deixa de ser observada (ajuste fs.inotify.max_user_watches)",
		"usados", used, "limite", limit,
		"fracao", int(pressure*100))
}

func euid() int { return os.Geteuid() }

func usesNumericIDs(args []string) bool {
	for _, a := range args {
		if a == "--numeric-ids" {
			return true
		}
	}
	return false
}

// skipSpecial decide se uma entrada deve ser ignorada por não ser
// sincronizável.
//
// Sockets, FIFOs e device nodes não têm conteúdo que faça sentido copiar. Um
// socket unix só existe enquanto o processo que o criou está vivo e escutando;
// copiá-lo produz um arquivo inerte com o mesmo nome. Um FIFO copiado é um
// FIFO vazio, sem relação nenhuma com o original. Device nodes exigem
// privilégio para criar e, se criados, apontam para o hardware da máquina
// local — o que raramente é o que se quer numa cópia.
//
// Ignorar é a resposta certa; o que não pode é ignorar em silêncio, daí o log.
func (e *Engine) skipSpecial(rel string, kind hash.FileKind, log *slog.Logger) bool {
	if kind != hash.KindSpecial {
		return false
	}
	if e.cfg.SpecialFiles == config.SpecialFilesError {
		log.Error("arquivo especial encontrado; não é sincronizável", "path", rel)
		return true
	}
	log.Debug("arquivo especial ignorado", "path", rel)
	return true
}
