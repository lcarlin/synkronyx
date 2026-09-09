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
	@echo "  1. cp /etc/synkronyx/synkronyx.example.yaml /etc/synkronyx/synkronyx.yaml e editar A e B"
	@echo "  2. ajustar ReadWritePaths em /etc/systemd/system/synkronyx.service"
	@echo "  3. systemd-sysusers && sysctl --system && systemctl daemon-reload"
	@echo "  4. synkronyx -config /etc/synkronyx/synkronyx.yaml -check"
	@echo "  5. systemctl enable --now synkronyx"

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
