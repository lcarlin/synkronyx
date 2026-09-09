# Synkronyx

Serviço Linux de sincronização bidirecional contínua entre duas árvores de
diretórios. Uma alteração em `A` chega a `B`, uma alteração em `B` chega a
`A`, e o sistema não entra em loop no meio do caminho.

A especificação completa está em [SYNKRONYX-HIGH-LEVEL-SCOPE.md](SYNKRONYX-HIGH-LEVEL-SCOPE.md).

## Estado atual

Esqueleto funcional. O caminho principal está implementado e coberto por
testes de integração contra inotify e rsync reais: propagação nos dois
sentidos, rename, delete, subdiretórios, First Sync e prevenção de loops.
O que ainda não está pronto está listado em
[docs/DECISOES-ABERTAS.md](docs/DECISOES-ABERTAS.md).

## Build

```bash
make build          # binário estático em bin/synkronyx (CGO desabilitado)
make test           # inclui testes de integração; exigem rsync no PATH
make lint           # fmt + vet + test
```

O binário é único e sem dependências de runtime além do `rsync`.

## Uso

```bash
cp configs/synkronyx.example.yaml /etc/synkronyx/synkronyx.yaml
$EDITOR /etc/synkronyx/synkronyx.yaml          # definir a e b
synkronyx -config /etc/synkronyx/synkronyx.yaml -check
```

Instalação como serviço:

```bash
sudo make install
sudo systemd-sysusers && sudo sysctl --system && sudo systemctl daemon-reload
sudo systemctl enable --now synkronyx
journalctl -u synkronyx -f
```

`SIGHUP` dispara um Full Resync sem reiniciar o processo:

```bash
sudo systemctl kill -s HUP synkronyx
```

## Arquitetura

```text
        inotify A ─┐                              ┌─ raiz A
                   ├─► guard ─► debounce ─► engine ┤
        inotify B ─┘                              └─ raiz B
                                    │
                          ┌─────────┴─────────┐
                       SQLite               rsync
                      (estado)           (transferência)
```

| Pacote | Responsabilidade |
|---|---|
| `internal/inotify` | wrapper cru sobre `inotify(7)`; expõe o `cookie`, necessário para parear renames |
| `internal/watcher` | watch recursivo, pareamento de rename, tradução para eventos de domínio |
| `internal/guard` | prevenção de loops — camada 1 (expectativa de escrita própria) |
| `internal/debounce` | agrupamento de rajadas por path |
| `internal/engine` | decisão e execução da sincronização; conflitos; First Sync e Full Resync |
| `internal/state` | estado persistente em SQLite (Go puro, sem CGO) |
| `internal/transfer` | execução do rsync e operações locais de filesystem |
| `internal/scan` | inventário das árvores para reconciliação |
| `internal/hash` | SHA-256 sob demanda, com filtro barato por metadados |

### Prevenção de loops

O ponto mais delicado do projeto (seção 6 do escopo) e o que mais merece
atenção antes de mexer no código. São **duas camadas independentes**:

1. **Expectativa** (`internal/guard`) — antes de escrever no destino, o engine
   registra a escrita; o evento que ela provoca casa com o registro e é
   descartado. Rápida, mas baseada em tempo.
2. **Idempotência** (`engine.alreadySynced`) — antes de agir sobre qualquer
   evento, o engine compara o que está no disco com o que o estado diz ter
   sincronizado. Se forem iguais, a ação é no-op. Não depende de tempo nenhum.

A camada 2 parece redundante e não é: ela é o que torna a propriedade
verdadeira em vez de provável, quando uma escrita demora mais que o TTL. Ver
[docs/adr-001-prevencao-de-loops.md](docs/adr-001-prevencao-de-loops.md).

`TestNoSyncLoop` (em `internal/engine/engine_test.go`) é o teste que guarda
essa propriedade.

### Limites do inotify

Cada diretório observado consome um watch, nas duas árvores. O padrão de
8192 da maioria das distribuições é baixo para árvores grandes; ao estourar,
`AddWatch` falha com `ENOSPC` e a árvore fica parcialmente observada.
`deploy/99-synkronyx-inotify.conf` eleva o limite. Verificar o consumo atual:

```bash
find /dados/A /dados/B -type d | wc -l
cat /proc/sys/fs/inotify/max_user_watches
```

Se a fila do kernel estourar (`IN_Q_OVERFLOW`), eventos são perdidos; o
watcher sinaliza e o engine responde com um Full Resync automático.

## Layout

```text
cmd/synkronyx/        entrypoint do daemon
internal/             implementação (ver tabela acima)
configs/              configuração de exemplo
deploy/               unit systemd, sysusers, limites do inotify
docs/                 decisões de arquitetura e pendências
```
