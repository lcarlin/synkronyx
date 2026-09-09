# Synkronyx

BINARY  := synkronyx
PREFIX  ?= /usr/local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt lint clean install uninstall run check

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
	@echo "Instalado. Próximos passos:"
	@echo
	@echo "  1. Configuração (as raízes /dados/A e /dados/B já vêm definidas):"
	@echo "       cp /etc/synkronyx/synkronyx.example.yaml /etc/synkronyx/synkronyx.yaml"
	@echo
	@echo "  2. Criar as raízes e dar acesso ao usuário do serviço."
	@echo "     O install não faz isso: são seus dados, e a escolha de dono e"
	@echo "     modo depende de quem mais precisa acessá-los."
	@echo "       mkdir -p /dados/A /dados/B"
	@echo "       chown synkronyx:synkronyx /dados/A /dados/B   # se usar o unit padrão"
	@echo
	@echo "  3. Usuário do serviço e limites do inotify:"
	@echo "       systemd-sysusers && sysctl --system && systemctl daemon-reload"
	@echo
	@echo "  4. Validar antes de habilitar — recusa com mensagem clara se as"
	@echo "     raízes não existirem ou não forem acessíveis:"
	@echo "       synkronyx -config /etc/synkronyx/synkronyx.yaml -check"
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
