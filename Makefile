# Synkronyx

BINARY  := synkronyx
PREFIX  ?= /usr/local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt lint ci hooks clean install uninstall run check

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/synkronyx

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint: fmt vet test

# Verificação completa, sem alterar arquivo nenhum. É o que rodaria num CI, e
# roda aqui: a suíte precisa de Linux de verdade — os testes de watcher falam
# com o inotify do kernel e os de engine invocam o rsync —, então não haveria
# ganho em terceirizar isto para uma máquina remota.
#
# Diferença em relação a `lint`: `fmt` reescreve arquivos, e uma verificação
# que conserta o que está errado não verifica nada. Aqui a formatação errada
# falha.
ci:
	@printf '== dependências externas ==\n'
	@rsync --version | head -1
	@printf 'inotify max_user_watches: %s\n' "$$(cat /proc/sys/fs/inotify/max_user_watches)"
	@printf 'go: %s\n' "$$(go version)"
	@printf '\n== formatação ==\n'
	@arquivos=$$(gofmt -l .); \
	if [ -n "$$arquivos" ]; then echo "não formatados:"; echo "$$arquivos"; exit 1; fi
	@echo "ok"
	@printf '\n== vet ==\n'
	go vet ./...
	@printf '\n== testes com detector de corrida ==\n'
	go test -race -count=1 ./...
	@printf '\n== build estático ==\n'
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/synkronyx
	@./bin/$(BINARY) -version

# Liga o hook de pre-push, que roda `make ci` antes de cada push.
hooks:
	git config core.hooksPath .githooks
	@echo "core.hooksPath = .githooks"
	@echo "pre-push roda 'make ci'; pule com 'git push --no-verify' quando precisar"


clean:
	rm -rf bin

# Instala binário, unit, config de exemplo e limites do inotify.
install: build
	install -Dm0755 bin/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	install -Dm0644 deploy/synkronyx.service $(DESTDIR)/etc/systemd/system/synkronyx.service
	install -Dm0644 deploy/synkronyx-root.service $(DESTDIR)/etc/systemd/system/synkronyx-root.service
	install -Dm0644 deploy/synkronyx.sysusers $(DESTDIR)/usr/lib/sysusers.d/synkronyx.conf
	install -Dm0644 deploy/99-synkronyx-inotify.conf $(DESTDIR)/etc/sysctl.d/99-synkronyx-inotify.conf
	install -Dm0640 configs/synkronyx.example.yaml $(DESTDIR)/etc/synkronyx/synkronyx.example.yaml
	@echo
	@echo "Instalado. Próximos passos, NESTA ordem:"
	@echo
	@echo "  1. Criar o usuário e o grupo do serviço, e aplicar os limites do inotify."
	@echo "     Vem antes do chown do passo 3, que precisa que eles já existam:"
	@echo "       systemd-sysusers && sysctl --system && systemctl daemon-reload"
	@echo
	@echo "  2. Configuração (as raízes /dados/A e /dados/B já vêm definidas):"
	@echo "       cp /etc/synkronyx/synkronyx.example.yaml /etc/synkronyx/synkronyx.yaml"
	@echo
	@echo "  3. Criar as raízes e dar acesso ao usuário do serviço."
	@echo "     O install não cria as raízes: são seus dados, e a escolha de dono"
	@echo "     e modo depende de quem mais precisa acessá-los."
	@echo "       mkdir -p /dados/A /dados/B"
	@echo "       chown synkronyx:synkronyx /dados/A /dados/B   # só para o unit padrão"
	@echo
	@echo "  4. Validar antes de habilitar. Rodar como o usuário do serviço confere"
	@echo "     também o acesso, não só a existência das raízes:"
	@echo "       sudo -u synkronyx synkronyx -config /etc/synkronyx/synkronyx.yaml -check"
	@echo
	@echo "  5. Habilitar. Use UM dos dois units, nunca ambos:"
	@echo "       systemctl enable --now synkronyx        # sem privilégio, sem dono/grupo"
	@echo "       systemctl enable --now synkronyx-root   # com dono e grupo sincronizados"

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	rm -f $(DESTDIR)/etc/systemd/system/synkronyx.service
	rm -f $(DESTDIR)/etc/systemd/system/synkronyx-root.service
	rm -f $(DESTDIR)/usr/lib/sysusers.d/synkronyx.conf
	rm -f $(DESTDIR)/etc/sysctl.d/99-synkronyx-inotify.conf

# Valida um arquivo de configuração sem subir o serviço.
check: build
	./bin/$(BINARY) -config $(CONFIG) -check

run: build
	./bin/$(BINARY) -config $(CONFIG)
