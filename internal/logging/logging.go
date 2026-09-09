// Package logging configura o logger estruturado do daemon.
//
// Sob systemd o stderr do processo vai para o journald, então basta escrever
// em stderr; não há dependência de biblioteca de journal. O prefixo de nível
// no formato sd-daemon ("<6>") faz o journald classificar a prioridade
// corretamente, o que dá `journalctl -p` funcionando de graça.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// New devolve um logger para o nível textual dado (debug|info|warn|error).
// Se journald for true, aplica o prefixo de prioridade sd-daemon.
func New(w io.Writer, level string, journald bool) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	h := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// journald já carimba o timestamp de cada linha.
			if journald && len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	if journald {
		return slog.New(&priorityHandler{Handler: h, w: w})
	}
	return slog.New(h)
}

// Default devolve um logger para stderr, com detecção de journald pelo
// ambiente que o systemd injeta.
func Default(level string) *slog.Logger {
	return New(os.Stderr, level, UnderSystemd())
}

// UnderSystemd informa se o processo foi iniciado pelo systemd.
func UnderSystemd() bool {
	return os.Getenv("JOURNAL_STREAM") != "" || os.Getenv("INVOCATION_ID") != ""
}

// priorityHandler prefixa cada registro com "<N>" (RFC 5424) para o journald.
type priorityHandler struct {
	slog.Handler
	w io.Writer
}

func (h *priorityHandler) Handle(ctx context.Context, r slog.Record) error {
	var prio string
	switch {
	case r.Level >= slog.LevelError:
		prio = "<3>"
	case r.Level >= slog.LevelWarn:
		prio = "<4>"
	case r.Level >= slog.LevelInfo:
		prio = "<6>"
	default:
		prio = "<7>"
	}
	if _, err := io.WriteString(h.w, prio); err != nil {
		return err
	}
	return h.Handler.Handle(ctx, r)
}

func (h *priorityHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &priorityHandler{Handler: h.Handler.WithAttrs(attrs), w: h.w}
}

func (h *priorityHandler) WithGroup(name string) slog.Handler {
	return &priorityHandler{Handler: h.Handler.WithGroup(name), w: h.w}
}
