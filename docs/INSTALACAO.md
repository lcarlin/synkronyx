# Manual de instalação

Guia completo para instalar o Synkronyx como serviço systemd, do código-fonte
até o serviço rodando. A última seção é um catálogo de problemas conhecidos com
sintoma, causa e correção.

Se o objetivo é apenas experimentar, pule para
[Instalação de teste, sem root](#instalação-de-teste-sem-root).

---

## Sumário

1. [Requisitos](#1-requisitos)
2. [Obter e compilar](#2-obter-e-compilar)
3. [Instalação de teste, sem root](#instalação-de-teste-sem-root)
4. [O que o install coloca onde](#3-o-que-o-install-coloca-onde)
5. [A ordem dos passos, e por que ela importa](#4-a-ordem-dos-passos-e-por-que-ela-importa)
6. [Escolher entre os dois units](#5-escolher-entre-os-dois-units)
7. [Limites do inotify](#6-limites-do-inotify)
8. [O arquivo de configuração](#7-o-arquivo-de-configuração)
9. [Validar antes de habilitar](#8-validar-antes-de-habilitar)
10. [Primeira subida: o que esperar](#9-primeira-subida-o-que-esperar)
11. [Desinstalar](#10-desinstalar)
12. [Problemas conhecidos](#11-problemas-conhecidos)
13. [Perguntas frequentes](#12-perguntas-frequentes)

---

## 1. Requisitos

| Item | Versão / detalhe | Por quê |
|---|---|---|
| Linux | kernel com `inotify` | A detecção de alterações é toda baseada em `inotify(7)`. Não há fallback por polling |
| `rsync` | qualquer versão recente, no `PATH` | Faz a transferência de conteúdo. O serviço recusa subir se não encontrar |
| systemd | qualquer versão com `sysusers.d` e `StateDirectory=` | Usado para ciclo de vida, usuário dedicado e diretório de estado |
| Go | **1.22 ou mais novo** | Só para compilar. O binário final não depende do Go |

O binário é **único e estático** (`CGO_ENABLED=0`), então além do `rsync` não há
bibliotecas a instalar. O driver SQLite é Go puro.

### O que NÃO é requisito

- **Rede.** O Synkronyx é local: sincroniza dois diretórios na mesma máquina.
  Um deles pode ser um ponto de montagem remoto, mas não há protocolo próprio.
- **Root para operar.** O unit padrão roda como usuário dedicado sem
  privilégio. Root só é necessário se você quiser que dono e grupo sejam
  sincronizados — ver [seção 5](#5-escolher-entre-os-dois-units).
- **SQLite instalado.** O driver é compilado dentro do binário. O comando
  `sqlite3` é útil para inspecionar o estado à mão, mas é opcional.

### Verificar os requisitos

```bash
uname -r                                          # kernel
rsync --version | head -1                         # rsync
systemctl --version | head -1                     # systemd
go version                                        # só para compilar
cat /proc/sys/fs/inotify/max_user_watches         # ver seção 6
```

---

## 2. Obter e compilar

```bash
git clone https://github.com/lcarlin/synkronyx.git
cd synkronyx
make build
./bin/synkronyx -version
```

O binário sai em `bin/synkronyx`. Para conferir que é realmente estático:

```bash
file bin/synkronyx      # deve dizer "statically linked"
ldd bin/synkronyx       # deve dizer "not a dynamic executable"
```

### Rodar a verificação completa antes de instalar

```bash
make ci
```

Isso confere as dependências externas, recusa código não formatado, roda
`go vet`, executa a suíte com o detector de corrida e produz o binário. Leva
cerca de 30 s, quase tudo na suíte com `-race`.

Não há CI hospedado, e a escolha é deliberada: os testes de watcher falam com o
`inotify` do kernel e os de engine invocam o `rsync`. Mockar isso seria testar
as suposições do código em vez do comportamento, então a verificação roda onde o
kernel está.

---

## Instalação de teste, sem root

Para ver funcionando em cinco comandos, sem tocar em `/etc`, sem `sudo`, sem
usuário de serviço:

```bash
mkdir -p ~/sync-teste/A ~/sync-teste/B
cat > ~/sync-teste/cfg.yaml <<EOF
a: $HOME/sync-teste/A
b: $HOME/sync-teste/B
state_path: $HOME/sync-teste/state.db
debounce: 300ms
EOF

./bin/synkronyx -config ~/sync-teste/cfg.yaml -check
./bin/synkronyx -config ~/sync-teste/cfg.yaml
```

Em outro terminal, escreva nos dois lados e veja a propagação:

```bash
echo "de A" > ~/sync-teste/A/teste.txt ; sleep 1 ; cat ~/sync-teste/B/teste.txt
echo "de B" > ~/sync-teste/B/volta.txt ; sleep 1 ; cat ~/sync-teste/A/volta.txt
```

`-status` e `-conflicts` funcionam do mesmo jeito, apontando para o mesmo
`-config`. Encerre com `Ctrl+C`.

O `debounce: 300ms` é menor que o padrão de 1 s só para a demonstração ficar
ágil. Sem privilégio, dono e grupo não são reconciliados — o log avisa isso na
subida.

---

## 3. O que o install coloca onde

```bash
sudo make install
```

| Destino | Origem | Modo |
|---|---|---|
| `/usr/local/bin/synkronyx` | `bin/synkronyx` | 0755 |
| `/etc/systemd/system/synkronyx.service` | `deploy/synkronyx.service` | 0644 |
| `/etc/systemd/system/synkronyx-root.service` | `deploy/synkronyx-root.service` | 0644 |
| `/usr/lib/sysusers.d/synkronyx.conf` | `deploy/synkronyx.sysusers` | 0644 |
| `/etc/sysctl.d/99-synkronyx-inotify.conf` | `deploy/99-synkronyx-inotify.conf` | 0644 |
| `/etc/synkronyx/synkronyx.example.yaml` | `configs/synkronyx.example.yaml` | 0640 |

`PREFIX` e `DESTDIR` funcionam como de hábito:

```bash
sudo make install PREFIX=/opt/synkronyx      # binário em /opt/synkronyx/bin
make install DESTDIR=/tmp/pacote             # empacotamento, sem tocar no sistema
```

### O que o install NÃO faz, e por quê

| Não faz | Motivo |
|---|---|
| Não cria as raízes de sincronização | São seus dados. A escolha de dono e modo depende de quem mais precisa acessá-los, e um `chown` automático poderia tirar o acesso de quem já usava o diretório |
| Não cria `/var/lib/synkronyx` | O systemd cria, via `StateDirectory=synkronyx` no unit, com modo 0750 |
| Não cria o usuário `synkronyx` | É `systemd-sysusers` que faz isso, lendo o arquivo que o install acabou de colocar |
| Não copia o exemplo para `synkronyx.yaml` | Sobrescrever a configuração de alguém num reinstall seria destrutivo |
| Não habilita nenhum serviço | Ver [seção 5](#5-escolher-entre-os-dois-units): há dois, e usar os dois ao mesmo tempo é errado |

---

## 4. A ordem dos passos, e por que ela importa

Há uma cadeia de dependências que não é óbvia, e trocar a ordem produz um erro
confuso:

```
sudo make install
      │  coloca /usr/lib/sysusers.d/synkronyx.conf
      ▼
sudo systemd-sysusers
      │  lê aquele arquivo e cria o usuário E o grupo synkronyx
      ▼
sudo chown synkronyx:synkronyx /dados/A /dados/B
         só agora existe alguém para ser dono
```

Rodar `systemd-sysusers` antes do install **não falha** — simplesmente não
encontra arquivo nenhum e não cria nada. O erro só aparece depois, no `chown`,
com uma mensagem sobre usuário inválido que não menciona a causa real.

### Sequência completa

```bash
# 1. Instalar os arquivos
cd ~/synkronyx
sudo make install

# 2. Criar usuário e grupo, aplicar limites do inotify
sudo systemd-sysusers
sudo sysctl --system
sudo systemctl daemon-reload

# 3. Configuração
sudo cp /etc/synkronyx/synkronyx.example.yaml /etc/synkronyx/synkronyx.yaml
sudo $EDITOR /etc/synkronyx/synkronyx.yaml        # ajustar a: e b: se necessário

# 4. Criar as raízes e dar acesso
sudo mkdir -p /dados/A /dados/B
sudo chown synkronyx:synkronyx /dados/A /dados/B  # só para o unit padrão

# 5. Se as raízes não forem /dados/A e /dados/B, ajustar o unit
sudo $EDITOR /etc/systemd/system/synkronyx.service    # linha ReadWritePaths=
sudo systemctl daemon-reload

# 6. Validar — como o usuário do serviço, para conferir acesso
sudo -u synkronyx synkronyx -config /etc/synkronyx/synkronyx.yaml -check

# 7. Habilitar
sudo systemctl enable --now synkronyx
journalctl -u synkronyx -f
```

O passo 6 rodar como `synkronyx`, e não como root, é o que faz diferença: root
consegue ler qualquer coisa, então validar como root aprovaria uma configuração
que o serviço não consegue usar. Um `chown` esquecido ou um modo restritivo
aparece ali, antes do `enable`, em vez de virar erro no journal depois.

---

## 5. Escolher entre os dois units

| | `synkronyx.service` (padrão) | `synkronyx-root.service` |
|---|---|---|
| Usuário | `synkronyx` | `root` |
| Capabilities | nenhuma (`CapabilityBoundingSet=`) | `CAP_CHOWN`, `CAP_FOWNER`, `CAP_DAC_OVERRIDE` |
| Conteúdo, permissões, mtime | sincronizados | sincronizados |
| **Dono e grupo** | **não comparados** | sincronizados |
| Precisa de `chown` nas raízes | sim | não |

Os dois têm `Conflicts=` apontando um para o outro, então o systemd impede que
ambos fiquem ativos sobre as mesmas raízes. Trocar:

```bash
sudo systemctl disable --now synkronyx
sudo systemctl enable  --now synkronyx-root
```

### Por que dono e grupo não são comparados sem privilégio

Não é omissão. Sem privilégio, todo `chown` falha com `EPERM`: a divergência
seria **detectada e nunca resolvida**, e refeita em cada reconciliação, para
sempre. Um daemon de longa execução repetindo trabalho que não pode concluir é
pior que uma limitação declarada — não falha, não alerta, só nunca acaba.

Então a condição é de privilégio, não de configuração, e não há knob para
forçá-la. O aviso sai no log na subida:

```
os argumentos do rsync pedem preservação de dono e grupo, mas o processo não
roda como root: os arquivos no destino ficarão com o dono do serviço, o rsync
não vai reclamar, e a reconciliação não compara dono nem grupo
```

### Quando o unit root vale a pena

- Múltiplos usuários com arquivos em `/dados`, e a propriedade importa
- Backup em que preservar `uid`/`gid` faz parte do requisito
- Raízes em filesystem que outros serviços leem com usuários próprios

E quando não vale: se um único usuário é dono de tudo, o unit padrão faz o
mesmo trabalho com muito menos superfície de ataque.

---

## 6. Limites do inotify

Cada diretório observado consome **um watch**, e o Synkronyx observa as duas
árvores. O padrão de 8192 da maioria das distribuições é baixo para árvores
grandes; ao estourar, `AddWatch` falha com `ENOSPC` e parte da árvore fica sem
observação — silenciosamente, do ponto de vista de quem só olha o serviço
rodando.

### Dimensionar

```bash
# quantos diretórios existem nas duas raízes
find /dados/A /dados/B -type d 2>/dev/null | wc -l

# limite atual
cat /proc/sys/fs/inotify/max_user_watches
```

Regra de bolso: **limite ≥ (total de diretórios) × 1,5**. A margem cobre
crescimento e o fato de o limite ser por usuário, compartilhado com outros
programas que usam inotify (editores, indexadores de desktop, ferramentas de
build).

O `install` coloca um arquivo com valores generosos:

```
fs.inotify.max_user_watches = 524288
fs.inotify.max_queued_events = 65536
```

Aplicar sem reiniciar:

```bash
sudo sysctl --system
sysctl fs.inotify.max_user_watches      # confirmar
```

### O serviço avisa quando está perto

Acima de **80%** do limite, o log emite:

```
consumo de watches do inotify perto do limite; ao estourar, parte da árvore
deixa de ser observada (ajuste fs.inotify.max_user_watches)
usados=419430 limite=524288 fracao=80
```

A verificação roda na subida e a cada heartbeat, então um crescimento gradual
da árvore aparece antes de virar problema.

### Se a fila estourar

`max_queued_events` limita quantos eventos o kernel guarda antes de descartar.
Se estourar, o kernel emite `IN_Q_OVERFLOW` e **eventos são perdidos**. O
Synkronyx trata isso como o que é — perda de informação — e dispara um Full
Resync automático:

```
fila do inotify estourou; eventos perdidos, resync necessário
full resync solicitado reason="overflow da fila do inotify no lado A"
```

Nada é perdido de forma permanente, mas um resync é caro em árvores grandes.
Se isso acontecer com frequência, aumente `max_queued_events`.

---

## 7. O arquivo de configuração

Todos os parâmetros, com o padrão compilado e o que cada um custa. O arquivo de
exemplo instalado tem os mesmos comentários.

### Raízes e estado

```yaml
a: /dados/A
b: /dados/B
state_path: /var/lib/synkronyx/state.db
```

`a` e `b` são obrigatórios, absolutos, e **não podem estar aninhados** um no
outro — aninhamento produz recursão infinita de eventos, e a configuração é
recusada na validação. Também não podem ser o mesmo diretório.

`state_path` precisa ser gravável pelo usuário do serviço. Com o unit padrão,
`StateDirectory=synkronyx` já cria `/var/lib/synkronyx` com o dono certo.

### Tempo

```yaml
debounce: 1s
self_write_ttl: 60s
heartbeat_interval: 15s
progress_interval: 30s
```

| Parâmetro | Padrão | O que faz | Quando mexer |
|---|---|---|---|
| `debounce` | `1s` | Janela de silêncio por path antes de propagar. Um editor salvando gera CREATE de temporário, escrita, rename e remoção — quatro eventos para uma alteração lógica | **Aumentar** em cargas de rajada (builds, extração de arquivos). **Diminuir** se latência importa mais que número de transferências. Com 1 s, a latência medida de uma escrita isolada é ~1,04 s |
| `self_write_ttl` | `60s` | Por quanto tempo uma escrita do próprio sincronizador continua sendo reconhecida como tal | **Aumentar** se houver arquivos muito grandes: a escrita precisa caber na janela. Errar para mais é barato, porque não é o único mecanismo anti-loop |
| `heartbeat_interval` | `15s` | Periodicidade com que o daemon publica seu estado no banco, para o `-status` ler. Também é o ritmo em que pedidos de `-resolve` são aplicados, e a base do prazo para o daemon ser dado como parado (três intervalos) | Raramente. Diminuir deixa o `-status` mais fresco ao custo de uma escrita a mais no banco |
| `progress_interval` | `30s` | De quanto em quanto tempo um scan ou reconciliação longa reporta progresso. `0` desliga | Em árvores pequenas nada sai. Em árvores enormes é a diferença entre "está trabalhando" e "parece travado" |

### Conteúdo

```yaml
hash_max_bytes: 104857600   # 100 MiB
hash_sample_bytes: 4194304  # 4 MiB de cada ponta
```

Até `hash_max_bytes` o digest é o SHA-256 completo, e **prova** igualdade.
Acima, passa a ser amostrado — tamanho mais as duas extremidades — e vira
**evidência forte** em vez de prova.

O que a amostra detecta: append, truncamento, reescrita de cabeçalho, alteração
de cauda. O que não detecta pelo digest: alteração no meio que preserve o
tamanho exato.

Esse ponto cego é mais estreito do que parece:

- No fluxo de eventos, uma escrita sempre altera o `mtime`, comparado **antes**
  do digest. A alteração é propagada de qualquer forma.
- Na reconciliação, digests amostrados iguais com `mtime`s diferentes contam
  como divergência. Um par sincronizado tem `mtime`s idênticos, porque o rsync
  preserva o da origem.

Sobra a janela em que a alteração cabe na tolerância de `mtime` de um segundo em
relação à última sincronização.

`hash_max_bytes: 0` desliga a amostragem: digest completo sempre, sem pontos
cegos, ao custo de ler cada arquivo inteiro a cada evento.

### Conflitos e remoções

```yaml
conflict_policy: preserve
first_sync_policy: union
dir_delete_policy: preserve-unknown
```

| `conflict_policy` | Comportamento |
|---|---|
| `preserve` **(padrão)** | A versão perdedora é copiada para `<nome>.sync-conflict-<lado>-<data><ext>` e a vencedora ocupa o path original. Nada se perde |
| `newer-wins` | O `mtime` mais recente vence, sem preservar a outra versão |
| `manual` | Não toca em nada; marca o par como em conflito e espera `-resolve` |

| `first_sync_policy` | Comportamento em divergências pré-existentes |
|---|---|
| `union` **(padrão)** | O que existe só de um lado é copiado para o outro. Se o estado registra que já existiu do outro, é lido como remoção |
| `a-wins` / `b-wins` | O lado indicado é autoritativo |

| `dir_delete_policy` | Ao propagar a remoção de um diretório |
|---|---|
| `preserve-unknown` **(padrão)** | Inspeciona o destino e põe de lado o que o estado não conhece — criado ali e nunca propagado, ou alterado ali depois da última sincronização — em `<dir>.sync-conflict-<lado>-<data>/` |
| `force` | Remove o diretório inteiro sem inspecionar |

O padrão de `dir_delete_policy` existe porque propagar a remoção crua destrói o
que só existe no destino: arquivos criados lá e nunca propagados não têm cópia
na origem para serem recuperados.

### Transferência

```yaml
rsync_path: rsync
rsync_args:
  - --archive
  - --partial
  - --inplace
  - --numeric-ids
preserve_hardlinks: false
```

`--archive` implica preservação de permissões, timestamps, symlinks, dono e
grupo. `--inplace` escreve direto no arquivo de destino, sem temporário —
importante para arquivos grandes e para não gerar eventos extra.

`preserve_hardlinks` adiciona `--hard-links`, mas com um limite: a opção só
enxerga ligações dentro de **uma mesma invocação** do rsync. Vale para cópias de
árvore (First Sync, Full Resync, diretório novo), não para arquivos propagados
um a um por evento. Quem depende de hardlinks conta com o Full Resync para
restabelecê-los.

### Exclusões

```yaml
exclude:
  - .synkronyx
  - ".synkronyx-tmp-*"
  - "*.sync-conflict-*"
  - "*.swp"
  - "~$*"
  - .git/objects
```

Padrões sem `/` casam contra **qualquer componente** do path: `node_modules`
exclui `app/node_modules/x.js`. Padrões com `/` casam contra o path relativo
inteiro.

Os três primeiros são internos e **não devem ser removidos**:
`*.sync-conflict-*` é o que impede que arquivos de conflito preservados sejam
eles próprios sincronizados, gerando novos conflitos em cascata.

### Falhas e paralelismo

```yaml
retry_max_attempts: 8
retry_initial_delay: 5s
retry_max_delay: 10m
sync_workers: 4
special_files: skip
log_level: info
```

Com esses valores, as esperas do retry são **5 s, 10 s, 20 s, 40 s, 1 m20 s,
2 m40 s e 5 m20 s** — uma janela de **10 m35 s** antes de marcar o path com erro
e pedir um Full Resync. O teto de `retry_max_delay` não é alcançado; só passa a
cortar a partir de 9 tentativas. `retry_max_attempts: 0` desliga o retry e faz
cada falha depender do próximo resync.

`sync_workers` particiona por subárvore de primeiro nível, o que preserva a
ordem dentro de cada uma — criar um filho antes do pai nunca acontece. Renames
entre subárvores passam por uma barreira que espera todos os workers ficarem
ociosos. `1` volta ao processamento estritamente sequencial.

`special_files` decide o que fazer com sockets, FIFOs e device nodes:

- `skip` **(padrão)** — ignora, registrando em nível debug
- `error` — registra erro para cada um, para quem prefere descobrir que a árvore
  tem conteúdo não sincronizável

Não é omissão: um socket unix só existe enquanto o processo que o criou está
escutando, e copiá-lo produz um arquivo inerte. Um FIFO copiado é um FIFO vazio.
Device nodes exigem privilégio e apontam para o hardware local.

`log_level: debug` acrescenta os ecos descartados pelo guard, eventos já
refletidos no estado e as varreduras periódicas. É volumoso: medido em 50
arquivos propagados, debug rende **~485 bytes de log por arquivo** contra ~135
em info — 3,6×. Em árvores com muita escrita, confira o espaço do journal antes
de deixar ligado.

---

## 8. Validar antes de habilitar

```bash
sudo -u synkronyx synkronyx -config /etc/synkronyx/synkronyx.yaml -check
```

Saída de sucesso:

```
configuração válida: A=/dados/A B=/dados/B estado=/var/lib/synkronyx/state.db
```

O `-check` recusa, com `exit 1` e mensagem nomeando cada problema:

- raiz inexistente ou inacessível
- raízes aninhadas ou iguais
- `debounce`, `self_write_ttl` ou `heartbeat_interval` ≤ 0
- política inválida em qualquer um dos campos de política
- `hash_sample_bytes × 2 ≥ hash_max_bytes` — a amostra cobriria o arquivo
  inteiro, e a configuração não faria o que promete
- `retry_max_delay < retry_initial_delay`
- `sync_workers < 1`

O serviço faz a mesma verificação na subida, então uma configuração inválida
falha antes de qualquer escrita. Mas rodar o `-check` à mão, como o usuário do
serviço, é o que revela problemas de **acesso** — e esses o root não vê.

---

## 9. Primeira subida: o que esperar

```bash
sudo systemctl enable --now synkronyx
journalctl -u synkronyx -f
```

Sequência normal, na ordem:

```
iniciando synkronyx version=v0.1.0 a=/dados/A b=/dados/B debounce=1s conflict_policy=preserve
filesystem da raiz side=A root=/dados/A fstype=ext4 mount=/dados
filesystem da raiz side=B root=/dados/B fstype=ext4 mount=/dados
watchers registrados side=A dirs=1 root=/dados/A
watchers registrados side=B dirs=1 root=/dados/B
iniciando first sync policy=union
scan concluído phase=first-sync entradas_a=0 entradas_b=0 duracao=0s
reconciliação concluída phase=first-sync aplicadas=0 sem_acao=0 conflitos=0 falhas=0
processamento paralelo ativo workers=4 particao="subárvore de primeiro nível"
```

A ordem importa e é intencional: **watchers primeiro, First Sync depois**.
Invertida, alterações ocorridas durante o scan não gerariam evento e ficariam
invisíveis até o próximo restart.

### Avisos que podem aparecer, e são normais

| Aviso | Significa |
|---|---|
| `os argumentos do rsync pedem preservação de dono e grupo, mas o processo não roda como root` | Esperado no unit padrão. Dono e grupo não serão sincronizados |
| `raiz em filesystem de rede com --numeric-ids` | Os números de usuário só significam a mesma pessoa se as duas pontas compartilharem a base de usuários |
| `consumo de watches do inotify perto do limite` | Ver [seção 6](#6-limites-do-inotify) |
| `banco de estado está vazio, mas o first sync já havia rodado antes` | O `state.db` foi perdido. Ver [problema 9](#9-o-estado-foi-perdido-o-que-acontece) |

### Confirmar que funciona

```bash
sudo -u synkronyx touch /dados/A/teste.txt
sleep 2
ls -l /dados/B/teste.txt
sudo synkronyx -config /etc/synkronyx/synkronyx.yaml -status
```

---

## 10. Desinstalar

```bash
sudo systemctl disable --now synkronyx synkronyx-root
sudo make uninstall
```

O `uninstall` remove o binário, os dois units e os arquivos de `sysusers.d` e
`sysctl.d`. **Não remove**, de propósito:

| Fica | Onde | Remover à mão se quiser |
|---|---|---|
| Configuração | `/etc/synkronyx/` | `sudo rm -rf /etc/synkronyx` |
| Banco de estado | `/var/lib/synkronyx/` | `sudo rm -rf /var/lib/synkronyx` |
| Usuário `synkronyx` | base de usuários | `sudo userdel synkronyx` |
| **Seus dados** | `/dados/A`, `/dados/B` | você decide |

Depois de remover o usuário e o `sysctl.d`:

```bash
sudo systemctl daemon-reload
sudo sysctl --system          # volta os limites do inotify ao padrão da distro
```

### Arquivos de conflito preservados

Se houve conflitos, existem arquivos `*.sync-conflict-*` nas raízes. Eles são
dados seus — o Synkronyx nunca os apaga. Para localizá-los:

```bash
find /dados/A /dados/B -name '*sync-conflict*'
```

---

## 11. Problemas conhecidos

### 1. `chown: invalid user: 'synkronyx:synkronyx'`

**Sintoma.** O `chown` das raízes falha dizendo que o usuário não existe, mesmo
depois de rodar `systemd-sysusers`.

**Causa.** O `systemd-sysusers` lê de `/usr/lib/sysusers.d/`, e o arquivo do
Synkronyx só chega lá com o `make install`. Rodado antes, ele não encontra nada
e **não falha** — apenas não cria nada. O erro aparece depois, no `chown`.

**Correção.**

```bash
sudo make install          # se ainda não rodou
sudo systemd-sysusers
id synkronyx               # confirmar
sudo chown synkronyx:synkronyx /dados/A /dados/B
```

### 2. `raiz /dados/A: stat /dados/A: no such file or directory`

**Sintoma.** `-check` e a subida do serviço recusam, com `exit 1`.

**Causa.** O install não cria as raízes de sincronização — são seus dados.

**Correção.** `sudo mkdir -p /dados/A /dados/B`, e depois o `chown` da
[seção 4](#4-a-ordem-dos-passos-e-por-que-ela-importa).

### 3. `permission denied` no journal, com o serviço rodando

**Sintoma.** O serviço sobe, mas falha ao escrever no destino.

**Causas possíveis, em ordem de probabilidade.**

1. **Raízes não pertencem ao usuário do serviço.** Recém-criadas com `sudo
   mkdir` ficam `root:root` modo 755 — só root escreve.
   ```bash
   ls -ld /dados/A /dados/B
   sudo chown synkronyx:synkronyx /dados/A /dados/B
   ```
2. **`ReadWritePaths` do unit não cobre as raízes.** O unit tem
   `ProtectSystem=strict`, que torna o sistema todo somente-leitura exceto o que
   `ReadWritePaths` libera.
   ```bash
   grep ReadWritePaths /etc/systemd/system/synkronyx.service
   sudo $EDITOR /etc/systemd/system/synkronyx.service
   sudo systemctl daemon-reload && sudo systemctl restart synkronyx
   ```
3. **Modo restritivo em algum diretório intermediário.** `/dados` precisa ser
   ao menos travessável (`x`) pelo usuário do serviço.

Diagnóstico direto: `sudo -u synkronyx synkronyx -config ... -check`.

### 4. `limite de watches do inotify atingido (ajuste fs.inotify.max_user_watches)`

**Sintoma.** Erro `ENOSPC` ao registrar watches; parte da árvore fica sem
observação.

**Causa.** O limite é **por usuário** e compartilhado com todo o resto que usa
inotify.

**Correção.** Ver [seção 6](#6-limites-do-inotify). Se já está no valor do
arquivo instalado e ainda estoura, some os diretórios das duas árvores e
dimensione com folga de 50%.

### 5. `rsync não encontrado`

**Sintoma.** O serviço recusa subir.

**Causa.** O `rsync` não está no `PATH` do serviço. O unit tem
`ProtectSystem=strict` mas o `PATH` é o padrão do systemd, que inclui
`/usr/bin`.

**Correção.** Instale o rsync, ou aponte o caminho absoluto:

```yaml
rsync_path: /usr/bin/rsync
```

### 6. Dono e grupo não estão sendo sincronizados

**Sintoma.** Conteúdo e permissões sincronizam, `uid`/`gid` não.

**Causa.** Comportamento correto do unit padrão. Ver
[seção 5](#5-escolher-entre-os-dois-units).

**Correção.** Trocar para `synkronyx-root.service`. Se o serviço já roda como
root e ainda não sincroniza, confira `AmbientCapabilities` — o unit padrão tem
`CapabilityBoundingSet=` vazio, que remove `CAP_CHOWN` **até de root**.

### 7. Arquivos `*.sync-conflict-*` aparecendo

**Sintoma.** Arquivos com esse sufixo surgindo nas raízes.

**Causa.** Não é defeito: é a política `preserve` funcionando. O par divergiu
dos dois lados e nenhuma origem única existia, então a versão perdedora foi
preservada em vez de sobrescrita.

**O que fazer.** Ver o [manual de operação](OPERACAO.md#4-conflitos), seção de
conflitos. Em resumo: `-conflicts` mostra o contexto, `-resolve` decide, e os
arquivos preservados são seus para inspecionar e apagar.

**Se aparecerem em excesso**, o padrão importa. Alteração unilateral **não**
deveria gerar conflito — se estiver gerando, é bug e vale reportar. Conflito é
ausência de origem única, não diferença de conteúdo.

### 8. Os dois units habilitados ao mesmo tempo

**Sintoma.** `systemctl start` de um derruba o outro, ou o systemd recusa.

**Causa.** Os units têm `Conflicts=` apontando um para o outro, justamente para
impedir dois daemons sobre as mesmas raízes — o que produziria decisões
simultâneas e conflitantes sobre o mesmo path.

**Correção.**

```bash
sudo systemctl disable --now synkronyx
sudo systemctl enable  --now synkronyx-root
systemctl is-enabled synkronyx synkronyx-root   # confirmar que só um está ativo
```

### 9. O estado foi perdido: o que acontece

**Sintoma.** Na subida:

```
banco de estado está vazio, mas o first sync já havia rodado antes —
remoções feitas com o serviço parado serão desfeitas
```

**Causa.** O `state.db` foi apagado, recriado, ou perdido com o disco.

**Consequência.** Sem histórico, `union` não distingue "arquivo novo neste lado"
de "arquivo apagado do outro lado" — as duas situações são idênticas no disco.
Remoções feitas com o serviço parado **são desfeitas**: o arquivo volta,
copiado do lado onde ainda existe.

**Isso é intencional.** Ressuscitar um arquivo é o erro recuperável — basta
apagar de novo. Apagá-lo é o irrecuperável. O princípio Fail Safe manda escolher
o primeiro, e em voz alta.

**Prevenção.** Inclua `/var/lib/synkronyx/` no backup. Ver
[manual de operação](OPERACAO.md#backup-do-estado).

### 10. Go 1.21 ou anterior não compila

**Sintoma.** `go build` reclama de `updates to go.mod needed`.

**Causa.** O `go.mod` declara `go 1.22`, e as dependências foram fixadas em
versões compatíveis (`golang.org/x/sys v0.30.0`, `modernc.org/sqlite v1.34.0`).
Versões mais novas dessas dependências exigem Go 1.24 ou 1.26.

**Correção.** Atualize o Go para 1.22 ou mais novo. Baixar mais que isso exigiria
regredir o driver SQLite ainda mais, e ele é onde vive a durabilidade do banco de
estado.

### 11. `couldn't remove <id>` ao limpar sessões (não é do Synkronyx)

Se você chegou aqui procurando por isso: é do Claude Code, não do Synkronyx.
Nada a ver com este serviço.

---

## 12. Perguntas frequentes

**Posso sincronizar mais de dois diretórios?**
Não. O modelo é de exatamente dois lados de uma mesma árvore lógica. Três
diretórios exigiriam resolução de conflito entre três versões, que é um problema
diferente e mais difícil.

**Funciona entre máquinas?**
Não diretamente. O Synkronyx é local. Um dos lados pode ser um ponto de montagem
remoto (NFS, CIFS, SSHFS), e nesse caso o serviço avisa sobre `--numeric-ids`,
porque os números de usuário só significam a mesma pessoa se as duas pontas
compartilharem a base de usuários.

**As raízes podem estar em filesystems diferentes?**
Sim. Cada rename acontece dentro de uma raiz, então nunca cruza filesystems.

**E se eu escrever nos dois lados ao mesmo tempo, no mesmo arquivo?**
Isso é um conflito, e o comportamento depende de `conflict_policy`. Com o padrão
`preserve`, nada se perde: a versão perdedora é preservada com outro nome.

**O serviço aguenta quantos arquivos?**
O limite prático é o `max_user_watches` do inotify, que conta **diretórios**, não
arquivos. Com o valor instalado (524288), a árvore pode ter centenas de milhares
de diretórios. O First Sync de uma árvore assim leva tempo proporcional, e o
`progress_interval` faz ele dar sinal de vida.

**Preciso parar o serviço para fazer backup?**
Não para os dados. Para o `state.db`, prefira `sqlite3 ... ".backup"` em vez de
copiar o arquivo — ver [manual de operação](OPERACAO.md#backup-do-estado).

**O que acontece se a máquina desligar no meio de uma sincronização?**
O `--partial` do rsync mantém o que já foi transferido, e o First Sync da
próxima subida reconcilia. O banco usa WAL, então não corrompe por desligamento.

**Posso mudar as raízes depois?**
Pode, mas o estado antigo passa a não corresponder. O caminho seguro é parar o
serviço, apagar o `state.db`, ajustar a configuração e o `ReadWritePaths`, e
subir de novo — aceitando que o First Sync tratará tudo como novo. Ver
[problema 9](#9-o-estado-foi-perdido-o-que-acontece) para a consequência.

**Como sei que está realmente sincronizado?**
`diff -r --no-dereference /dados/A /dados/B`, ignorando os arquivos
`*.sync-conflict-*`. E `-status`, que mostra as contagens de entradas e os
conflitos abertos. Atenção: `diff -r` **não compara permissões nem dono** — para
isso, compare `stat`.

**Um `touch` num arquivo grande custa caro?**
Sim, e vale saber. Acima de `hash_max_bytes`, a reconciliação não consegue
distinguir "só o mtime mudou" de "alteração no meio que a amostra não vê" — as
duas situações são idênticas para ela. Ela escolhe o lado seguro e copia o
conteúdo. Um `touch` num arquivo de 100 GiB custa uma leitura de 100 GiB de cada
lado, uma vez.

**Por que não há CI no GitHub?**
Porque a suíte precisa de Linux de verdade: os testes de watcher falam com o
inotify do kernel e os de engine invocam o rsync. Mockar isso seria testar as
suposições do código em vez do comportamento. A verificação roda com `make ci`,
e `make hooks` a liga antes de cada push.

---

## Onde ir depois

- [Manual de operação](OPERACAO.md) — dia a dia, log, diagnóstico, ajuste
- [Decisões e limitações](DECISOES-ABERTAS.md) — o que o projeto escolheu não
  fazer, e por quê
- [ADR 001](adr-001-prevencao-de-loops.md) — como a prevenção de loops funciona
