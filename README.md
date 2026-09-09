# Synkronyx

[![CI](https://github.com/lcarlin/synkronyx/actions/workflows/ci.yml/badge.svg)](https://github.com/lcarlin/synkronyx/actions/workflows/ci.yml)

Serviço Linux de sincronização bidirecional contínua entre duas árvores de
diretórios. Uma alteração em `A` chega a `B`, uma alteração em `B` chega a
`A`, e o sistema não entra em loop no meio do caminho.

A especificação completa está em [SYNKRONYX-HIGH-LEVEL-SCOPE.md](SYNKRONYX-HIGH-LEVEL-SCOPE.md).

## Estado atual

Funcional. Propagação nos dois sentidos, rename, delete, subdiretórios,
symlinks, First Sync, Full Resync, conflitos com resolução assistida, retry
com backoff, remoção segura de diretórios, processamento paralelo opcional e
prevenção de loops — tudo coberto por testes de integração contra inotify e
rsync reais.

As decisões tomadas, as limitações que sobreviveram a elas e o que continua em
aberto estão em [docs/DECISOES-ABERTAS.md](docs/DECISOES-ABERTAS.md).

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

Dois units são instalados. O padrão (`synkronyx.service`) roda como usuário
dedicado e sem capabilities; nele o engine **não** compara dono nem grupo,
porque sem privilégio o `chown` falharia sempre e a divergência seria detectada
e nunca resolvida. Se a propriedade dos arquivos precisa ser sincronizada, use
`synkronyx-root.service`, que roda como root com apenas `CAP_CHOWN`,
`CAP_FOWNER` e `CAP_DAC_OVERRIDE` — nunca os dois ao mesmo tempo sobre as
mesmas raízes.

`SIGHUP` dispara um Full Resync sem reiniciar o processo:

```bash
sudo systemctl kill -s HUP synkronyx
```

Estado operacional, sem falar com o processo:

```bash
synkronyx -config /etc/synkronyx/synkronyx.yaml -status
```

```text
raiz A:            /dados/A
raiz B:            /dados/B
estado:            /var/lib/synkronyx/state.db (schema v2)
processo:          último heartbeat há 12s (pid 4821)
watches:           1843
fila de retry:     0
first sync:        2026-09-09T11:02:31-03:00
último resync:     -
entradas:          A=9241 B=9241
por status:        synced=9240 error=1
conflitos abertos: 2
```

O daemon publica o heartbeat na tabela `meta` do próprio banco e o comando lê
de lá — sem socket de controle e sem protocolo novo para manter. O relatório
é do último heartbeat, não do instante da consulta, e avisa quando o
heartbeat está vencido.

Conflitos, com contexto para decidir e comando para resolver:

```bash
synkronyx -config ... -conflicts
synkronyx -config ... -resolve docs/relatorio.md -with a
```

Com o daemon rodando, `-resolve` grava um pedido que o daemon aplica no ritmo
do heartbeat. Escrever direto nas árvores faria o daemon ler as escritas como
alteração externa e propagá-las de volta, desfazendo a resolução — só quem tem
o guard em mãos pode aplicar com segurança. Com o daemon parado, o CLI aplica
na hora.

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
| `internal/engine` | decisão e execução da sincronização; conflitos; retry; First Sync e Full Resync |
| `internal/state` | estado persistente em SQLite (Go puro, sem CGO) |
| `internal/transfer` | execução do rsync e operações locais de filesystem |
| `internal/scan` | inventário das árvores para reconciliação |
| `internal/preflight` | verificações de ambiente na subida (dono, filesystem, watches) |
| `internal/hash` | digest de conteúdo (completo ou amostrado), com filtro barato por metadados |

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

### Remoção de diretórios

Propagar a remoção de um diretório pode destruir o que só existe no destino:
arquivos criados lá e nunca propagados não têm cópia na origem para serem
recuperados. Por isso a política padrão (`dir_delete_policy:
preserve-unknown`) inspeciona o destino antes de remover e move o que o
estado não conhece para `<dir>.sync-conflict-<lado>-<data>/`.

### Digest de conteúdo

Até `hash_max_bytes` o digest é o SHA-256 completo. Acima, passa a ser
amostrado — tamanho mais as duas extremidades —, o que detecta append,
truncamento e reescrita de cabeçalho por custo fixo, mas não uma alteração no
meio do arquivo que preserve o tamanho. O tipo do digest é persistido junto
com o valor, e comparar tipos diferentes é recusado em vez de dar uma resposta
sem significado.

O padrão é `100 MiB`, com amostra de 4 MiB de cada ponta. Abaixo do limite —
onde vive a esmagadora maioria dos arquivos — o digest é completo e prova
igualdade.

O ponto cego acima do limite é estreito. No fluxo de eventos, uma escrita
sempre altera o mtime, comparado antes do digest, então a alteração é
propagada de qualquer forma. Na reconciliação, digests amostrados iguais com
mtimes diferentes contam como divergência — um par sincronizado tem mtimes
idênticos, porque o rsync preserva o da origem. Resta a janela em que a
alteração cabe na tolerância de mtime de um segundo. `0` desliga a amostragem.

### Paralelismo

`sync_workers` habilita processamento paralelo de eventos, particionado pelo
primeiro componente do path. Isso garante que um diretório de primeiro nível e
tudo abaixo dele caiam sempre no mesmo worker, preservando a ordem relativa
dentro da subárvore — a única ordem que importa. Renames entre subárvores
passam por uma barreira que espera todos os workers ficarem ociosos.

O padrão é `4`. A ordenação dentro de cada subárvore é garantida por
construção e coberta por teste, então o paralelismo não muda o resultado — só
o tempo. `1` volta ao processamento estritamente sequencial.

A reconciliação usa outra estratégia: scan das duas árvores em paralelo,
comparação de conteúdo em paralelo (fase cara e puramente leitura) e aplicação
sequencial em ordem de profundidade, onde a ordem importa.

### Verificações de ambiente

Na subida, o serviço avisa sobre condições em que funciona mas não faz o que a
configuração promete: preservação de dono pedida sem privilégio para tanto,
`--numeric-ids` sobre filesystem de rede, e consumo de watches perto do limite
do kernel. Nenhuma delas impede a subida — são todas recuperáveis, e derrubar
o serviço por um aviso seria pior que operar com a limitação conhecida.

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
watcher sinaliza e o engine responde com um Full Resync automático. Se a raiz
observada for removida, movida ou desmontada, o watcher reporta falha terminal
e o serviço encerra para o systemd reiniciá-lo — seguir rodando deixaria um
lado cego enquanto o outro continua propagando.

## Licença

GPL-3.0-or-later — ver [LICENSE](LICENSE). Versões modificadas distribuídas a
terceiros precisam ter o código-fonte disponibilizado sob a mesma licença. Cada
arquivo `.go` carrega o identificador SPDX correspondente.

## Layout

```text
cmd/synkronyx/        entrypoint do daemon
internal/             implementação (ver tabela acima)
configs/              configuração de exemplo
deploy/               unit systemd, sysusers, limites do inotify
docs/                 decisões de arquitetura e pendências
```
