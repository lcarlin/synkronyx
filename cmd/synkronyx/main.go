// Command synkronyx é o daemon de sincronização bidirecional.
//
// Uso típico, sob systemd:
//
//	synkronyx -config /etc/synkronyx/synkronyx.yaml
//
// Sinais:
//
//	SIGINT, SIGTERM  encerram o serviço de forma ordenada
//	SIGHUP           dispara um Full Resync sem reiniciar o processo
//
// O estado operacional é consultável sem falar com o processo:
//
//	synkronyx -config ... -status
//	synkronyx -config ... -conflicts
//	synkronyx -config ... -resolve docs/a.txt -with a
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/lcarlin/synkronyx/internal/config"
	"github.com/lcarlin/synkronyx/internal/engine"
	"github.com/lcarlin/synkronyx/internal/logging"
	"github.com/lcarlin/synkronyx/internal/state"
)

// version é sobrescrita no build via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "synkronyx: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "/etc/synkronyx/synkronyx.yaml", "caminho do arquivo de configuração")
		checkOnly   = flag.Bool("check", false, "validar a configuração e sair")
		showStatus  = flag.Bool("status", false, "reportar o estado operacional e sair")
		showConf    = flag.Bool("conflicts", false, "listar os conflitos abertos e sair")
		resolvePath = flag.String("resolve", "", "resolver o conflito no path dado (relativo às raízes)")
		resolveWith = flag.String("with", "", "qual versão vence: a, b ou both")
		showVersion = flag.Bool("version", false, "exibir a versão e sair")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("synkronyx", buildVersion())
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *checkOnly {
		fmt.Printf("configuração válida: A=%s B=%s estado=%s\n", cfg.A, cfg.B, cfg.StatePath)
		return nil
	}
	if *showStatus {
		return printStatus(context.Background(), os.Stdout, cfg)
	}
	if *showConf {
		return printConflicts(context.Background(), os.Stdout, cfg)
	}
	if *resolvePath != "" {
		if *resolveWith == "" {
			return errors.New("-resolve exige -with a|b|both")
		}
		return resolveConflictCmd(context.Background(), os.Stdout, cfg, *resolvePath, *resolveWith)
	}
	if *resolveWith != "" {
		return errors.New("-with só faz sentido junto com -resolve")
	}

	log := logging.Default(cfg.LogLevel)
	log.Info("iniciando synkronyx",
		"version", buildVersion(), "a", cfg.A, "b", cfg.B,
		"debounce", cfg.Debounce.String(), "conflict_policy", string(cfg.ConflictPolicy))

	db, err := state.Open(cfg.StatePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Warn("falha fechando estado", "err", err)
		}
	}()

	eng := engine.New(cfg, db, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-hup:
				eng.RequestResync("SIGHUP recebido")
			case <-ctx.Done():
				return
			}
		}
	}()

	err = eng.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("synkronyx encerrado")
	return nil
}

// buildVersion prefere a versão gravada pelo -ldflags e cai para a informação
// que o próprio Go embute no binário.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				if len(s.Value) > 12 {
					return s.Value[:12]
				}
				return s.Value
			}
		}
	}
	return version
}
