// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/event"
	"github.com/lcarlin/synkronyx/internal/hash"
)

// removeDir propaga a remoção de um diretório para o lado dst.
//
// O caso difícil não é apagar: é apagar o que nunca foi visto. Se um usuário
// remove um diretório em A enquanto outro usuário criou arquivos dentro do
// diretório correspondente em B, propagar a remoção crua destrói dados que
// nunca chegaram a existir em A — e portanto não podem ser recuperados de lá.
//
// Sob o princípio Fail Safe da seção 16, a política padrão inspeciona o
// destino antes de remover e põe de lado tudo o que o estado não conhece.
func (e *Engine) removeDir(ctx context.Context, dst event.Side, rel, dstAbs string) error {
	if e.cfg.DirDeletePolicy == config.DirDeleteForce {
		return e.xfer.Remove(dstAbs, true)
	}

	unsynced, err := e.unsyncedUnder(ctx, dst, rel, dstAbs)
	if err != nil {
		return err
	}
	if len(unsynced) == 0 {
		return e.xfer.Remove(dstAbs, true)
	}

	preserveRel := preservedDirName(rel, dst, time.Now())
	preserveAbs := e.abs(dst, preserveRel)

	// O diretório de resguardo casa com o exclude padrão, então o que for
	// movido para dentro dele não volta a ser sincronizado.
	e.guard.ExpectSubtree(dst, preserveRel)

	e.log.Warn("remoção de diretório preservaria dados não sincronizados",
		"path", rel, "arquivos", len(unsynced), "preservado_em", preserveRel)

	for _, r := range unsynced {
		sub, err := filepath.Rel(rel, r)
		if err != nil {
			return fmt.Errorf("path %s fora de %s: %w", r, rel, err)
		}
		target := filepath.Join(preserveAbs, sub)
		if err := e.xfer.Move(e.abs(dst, r), target); err != nil {
			return fmt.Errorf("preservando %s: %w", r, err)
		}
		e.log.Info("arquivo preservado", "de", r, "para", filepath.Join(preserveRel, sub))
	}

	if err := e.db.RecordConflict(ctx, rel, hash.Digest{}, hash.Digest{}); err != nil {
		return err
	}
	if err := e.db.ResolveConflict(ctx, rel, "preserve-unknown:"+preserveRel); err != nil {
		return err
	}
	return e.xfer.Remove(dstAbs, true)
}

// unsyncedUnder lista os arquivos sob rel, no lado side, que não constam do
// estado ou divergem do que o estado registra.
//
// As duas condições importam por motivos diferentes. Ausente do estado
// significa criado localmente e nunca propagado. Divergente significa que
// existia, foi sincronizado e depois alterado só deste lado — a alteração
// também nunca chegou ao outro.
func (e *Engine) unsyncedUnder(ctx context.Context, side event.Side, rel, abs string) ([]string, error) {
	known, err := e.db.Subtree(ctx, side, rel)
	if err != nil {
		return nil, err
	}
	excluder := config.NewExcluder(e.cfg.Exclude)
	root := e.roots[side]

	var out []string
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		r, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if excluder.Excluded(r, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}

		entry, ok := known[r]
		if !ok {
			out = append(out, r)
			return nil
		}

		st, statErr := hash.StatOf(p)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				return nil
			}
			return statErr
		}
		if st.Unchanged(entry.Stat()) {
			return nil
		}
		// Metadados mudaram: confirmar pelo digest antes de declarar
		// divergência, porque um mtime diferente sozinho não prova nada.
		d2, digestErr := e.digestOf(p)
		if digestErr != nil {
			if errors.Is(digestErr, fs.ErrNotExist) {
				return nil
			}
			return digestErr
		}
		if hash.Compare(d2, entry.Digest) != hash.Same {
			out = append(out, r)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

// preservedDirName monta "dir.sync-conflict-<lado>-<timestamp>".
//
// Ao contrário de conflictName, o sufixo vai no fim, sem tentar respeitar
// extensão: diretórios não têm extensão significativa, e mexer no meio do
// nome só dificultaria reconhecê-lo.
func preservedDirName(rel string, side event.Side, at time.Time) string {
	return fmt.Sprintf("%s.sync-conflict-%s-%s", rel, side, at.Format(conflictTimeLayout))
}
