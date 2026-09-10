# Manual de operação

O que fazer com o Synkronyx depois de instalado: rodar, entender o que ele
reporta, diagnosticar quando algo parece errado, ajustar, e reagir a desastres.

Para instalar, ver o [manual de instalação](INSTALACAO.md).

---

## Sumário

1. [Comandos do dia a dia](#1-comandos-do-dia-a-dia)
2. [Ler o log: dicionário de mensagens](#2-ler-o-log-dicionário-de-mensagens)
3. [`-status` campo por campo](#3--status-campo-por-campo)
4. [Conflitos](#4-conflitos)
5. [Full Resync](#5-full-resync)
6. [O que monitorar](#6-o-que-monitorar)
7. [Ajustar parâmetros](#7-ajustar-parâmetros)
8. [Backup do estado](#backup-do-estado)
9. [Cenários de desastre](#9-cenários-de-desastre)
10. [Limitações que aparecem na operação](#10-limitações-que-aparecem-na-operação)
11. [Árvore de diagnóstico](#11-árvore-de-diagnóstico)
12. [Inspecionar o estado à mão](#12-inspecionar-o-estado-à-mão)

---

## 1. Comandos do dia a dia

### Ciclo de vida

```bash
sudo systemctl start synkronyx
sudo systemctl stop synkronyx
sudo systemctl restart synkronyx
systemctl status synkronyx
```

Parar o serviço **não** é destrutivo: alterações feitas enquanto ele está parado
são reconciliadas na próxima subida, pelo First Sync. O que se perde é a
latência — as alterações esperam até o serviço voltar.

### Inspeção, sem falar com o processo

```bash
synkronyx -config /etc/synkronyx/synkronyx.yaml -status
synkronyx -config /etc/synkronyx/synkronyx.yaml -conflicts
synkronyx -config /etc/synkronyx/synkronyx.yaml -check
synkronyx -version
```

Os três primeiros leem o banco de estado, não o processo. Funcionam com o serviço
rodando ou parado, e não interferem nele. Isso é deliberado: o daemon publica um
heartbeat na tabela `meta` do próprio banco, e os comandos leem de lá — sem
socket de controle, sem protocolo, sem superfície de rede nova.

A consequência a ter em mente: o relatório é do **último heartbeat**, não do
instante da consulta. Com `heartbeat_interval: 15s`, os números têm até 15 s de
atraso.

### Full Resync sem reiniciar

```bash
sudo systemctl kill -s HUP synkronyx
```

Ver [seção 5](#5-full-resync).

### Log

```bash
journalctl -u synkronyx -f                    # acompanhar
journalctl -u synkronyx --since "1 hour ago"  # janela
journalctl -u synkronyx -p warning            # só avisos e erros
journalctl -u synkronyx -p err                # só erros
journalctl -u synkronyx | grep propagando     # o que foi sincronizado
```

O filtro por prioridade (`-p`) funciona porque o daemon prefixa cada linha com a
prioridade sd-daemon, então o journald classifica corretamente.

---

## 2. Ler o log: dicionário de mensagens

### Nível INFO — operação normal

| Mensagem | Significa |
|---|---|
| `iniciando synkronyx` | Subida, com as raízes e os principais parâmetros |
| `filesystem da raiz` | Tipo de filesystem detectado por raiz. Aparece uma vez por lado |
| `watchers registrados side=A dirs=N` | `N` diretórios sob observação naquele lado. Compare com o limite do inotify |
| `iniciando first sync` | Reconciliação inicial começando |
| `scan concluído phase=… entradas_a=N entradas_b=M` | Varredura terminada. As duas árvores foram percorridas **em paralelo** |
| `reconciliação concluída` | Fim de um First Sync ou Full Resync. Os contadores são explicados abaixo |
| `processamento paralelo ativo workers=N` | `sync_workers > 1`. Não aparece com `1` |
| `propagando event="A CREATE x.txt" para=B` | Uma alteração está sendo aplicada no outro lado |
| `propagando atributos divergentes` | Modo, `mtime` ou dono diferiam sem o conteúdo diferir |
| `removendo por política de reconciliação` | Uma remoção sendo propagada, decidida pela `first_sync_policy` |
| `conflito resolvido` | Um pedido de `-resolve` foi aplicado |
| `retentando` / `retentativa bem-sucedida` | Fila de retry em ação |
| `full resync concluído duracao=…` | Fim de um resync manual ou automático |
| `dono e grupo entram na reconciliação` | Rodando com privilégio; ownership será sincronizado |

Os contadores de `reconciliação concluída`:

| Campo | Significa |
|---|---|
| `aplicadas` | Operações executadas: cópias, remoções, ajustes de atributo |
| `sem_acao` | Paths conferidos e já convergidos. Em um resync de árvore estável, **tudo** deve cair aqui |
| `conflitos` | Pares sem origem única, tratados pela `conflict_policy` |
| `falhas` | Paths que erraram. Falhar em um não invalida os outros |
| `especiais_ignorados` | Sockets, FIFOs e device nodes |

### Nível WARN — atenção, não necessariamente problema

| Mensagem | O que fazer |
|---|---|
| `conflito detectado path=… policy=…` | Ver [seção 4](#4-conflitos). Com `preserve`, nada se perdeu |
| `versão preservada original=… preservada=…` | A versão perdedora está no arquivo indicado, recuperável |
| `remoção de diretório preservaria dados não sincronizados` | Havia conteúdo só no destino. Foi posto de lado antes de remover |
| `tipo divergente entre os lados` | Um lado tem arquivo, o outro diretório. Nenhuma política automática resolve — exige `-resolve` |
| `falha ao propagar; reenfileirado` | Transitório. A fila de retry vai tentar de novo |
| `os argumentos do rsync pedem preservação de dono e grupo, mas o processo não roda como root` | Esperado no unit padrão |
| `raiz em filesystem de rede com --numeric-ids` | Confira se as duas pontas compartilham a base de usuários |
| `consumo de watches do inotify perto do limite` | Aumente `fs.inotify.max_user_watches` |
| `fila do inotify estourou; eventos perdidos` | Um resync automático já foi disparado. Se repete, aumente `max_queued_events` |
| `banco de estado está vazio, mas o first sync já havia rodado antes` | Estado perdido. Ver [cenário 1](#cenário-1-o-estado-foi-perdido) |
| `full resync solicitado reason=…` | Um resync começou, e o motivo está no campo |
| `origem do rename ausente no destino; reconciliando subárvore` | Estado divergente. O engine se recuperou reconciliando a subárvore |

### Nível ERROR — precisa de atenção

| Mensagem | Causa provável |
|---|---|
| `falha ao propagar; tentativas esgotadas` | Falha persistente. O path foi marcado com `status=error` e um resync foi pedido |
| `falha processando evento` | Erro em uma operação isolada |
| `falha reconciliando path=…` | Um path falhou durante o resync; os outros seguiram |
| `arquivo especial encontrado; não é sincronizável` | Só com `special_files: error` |
| `lado A: a raiz observada … foi removida` | **Terminal.** O serviço encerra e o systemd reinicia |
| `falha resolvendo conflito` | Um pedido de `-resolve` não pôde ser aplicado. O motivo fica gravado |

### Nível DEBUG — só com `log_level: debug`

| Mensagem | Significa |
|---|---|
| `evento próprio descartado` | O guard reconheceu um eco da própria escrita. **É o mecanismo funcionando** |
| `evento já refletido no estado; ignorado` | A camada de idempotência barrou um evento redundante |
| `guard sweep` | Limpeza periódica de expectativas vencidas |
| `arquivo especial ignorado` | Socket, FIFO ou device node pulado |
| `scan em andamento` | Progresso de uma varredura longa |

Ver muitos `evento próprio descartado` é **normal e desejável** — é a prevenção
de loops trabalhando. Em condições normais são 3 a 4 por arquivo propagado.

Custo do debug, medido: ~485 bytes de log por arquivo propagado, contra ~135 em
info. Cerca de 3,6×.

---

## 3. `-status` campo por campo

```
raiz A:             /dados/A
raiz B:             /dados/B
estado:             /var/lib/synkronyx/state.db (schema v4)
processo:           último heartbeat há 3s (pid 17366)
watches:            1843
fila de retry:      0
first sync:         2026-09-10T09:00:01-03:00
último resync:      -
entradas:           A=9241 B=9241
por status:         synced=18480 error=2
conflitos abertos:  2
```

| Campo | Como ler |
|---|---|
| `estado` | Caminho do banco e versão do schema. `v4` é a atual |
| `processo` | Idade do último heartbeat e PID. Acima de **3× `heartbeat_interval`** aparece `— vencido; o daemon provavelmente não está rodando` |
| `watches` | Diretórios sob observação, somando os dois lados. Compare com `/proc/sys/fs/inotify/max_user_watches` |
| `fila de retry` | Operações aguardando nova tentativa. Deve ser **0** em regime estável |
| `first sync` | Quando o First Sync concluiu por último |
| `último resync` | Quando o último Full Resync concluiu. `-` significa nenhum desde a instalação |
| `entradas` | Paths conhecidos por lado. **Devem ser iguais** em árvore convergida |
| `por status` | `synced`, `pending`, `conflict`, `error`. Qualquer coisa fora de `synced` merece atenção |
| `conflitos abertos` | Conflitos sem resolução. Com `preserve`, o normal é 0, porque a política resolve automaticamente |

### Sinais de alerta

| Observação | Interpretação |
|---|---|
| `entradas: A=9241 B=9105` | Assimetria. Algo não convergiu — dispare um resync |
| `error=N` com N > 0 | Paths que esgotaram as tentativas. Procure `tentativas esgotadas` no log |
| `fila de retry` crescendo | Falha persistente, não transitória. Investigue espaço em disco e permissões |
| `processo: … vencido` | O daemon não está rodando, ou travou. `systemctl status` |
| `watches` perto do limite | Ver [seção 6](#6-o-que-monitorar) |
| `conflitos abertos` crescendo com `preserve` | Anômalo. Com `preserve` os conflitos são resolvidos na hora |

### Aviso de estado perdido

Se o banco estiver vazio, o First Sync já tiver rodado antes **e** as raízes
tiverem conteúdo, o `-status` alerta:

```
aviso: o banco de estado não tem entradas, mas o first sync já rodou antes
e as raízes têm conteúdo. A próxima reconciliação vai desfazer remoções
feitas com o serviço parado
```

As três condições juntas importam: com as árvores vazias, estado vazio é o
estado correto, e o aviso seria ruído.

---

## 4. Conflitos

### O que é conflito, e o que não é

Conflito é **ausência de origem única**, não diferença de conteúdo.

- Se apenas um lado se afastou do que foi sincronizado por último, aquele lado
  **é** a origem: a alteração é propagada normalmente, sem conflito.
- Se os dois mudaram, ou não há linha de base que permita afirmar qual mudou,
  aí é conflito.

Essa distinção não é acadêmica. Uma reconciliação que trate toda diferença como
conflito enche as raízes de arquivos `.sync-conflict-` a cada edição feita com o
serviço parado, e obriga o operador a decidir conflitos que não existem.

### Inspecionar

```bash
synkronyx -config /etc/synkronyx/synkronyx.yaml -conflicts
```

```
docs/relatorio.md
  detectado em 2026-09-10T09:15:42-03:00
  lado  disco                                   estado
  A     arquivo, 12.4 KiB, 2026-09-10 09:12:03  a1b2c3d4e5f6, sync em 2026-09-10 08:30:00
  B     arquivo, 11.9 KiB, 2026-09-10 09:14:55  9f8e7d6c5b4a, sync em 2026-09-10 08:30:00

resolver com:
  synkronyx -resolve <path> -with a      # versão de A vence; a de B fica preservada localmente
  synkronyx -resolve <path> -with b      # o simétrico
  synkronyx -resolve <path> -with both   # A vence o path e a versão de B é mantida, sincronizada
```

A coluna `disco` é o estado **atual**, que é o que decide. A coluna `estado` é o
que foi sincronizado por último — útil para ver de onde os dois divergiram.

Um digest terminado em `~` indica **amostrado**, não completo. Um prefixado com
`link:` é o digest do alvo de um symlink.

### Resolver

```bash
synkronyx -config … -resolve docs/relatorio.md -with a
```

| Escolha | O que acontece |
|---|---|
| `-with a` | A versão de A ocupa o path nos dois lados. A de B é preservada como `<nome>.sync-conflict-B-<data><ext>`, com nome que o `exclude` ignora — recuperável, **não** propagada |
| `-with b` | Simétrico |
| `-with both` | A de A ocupa o path, e a de B é preservada com nome `<nome>.sync-kept-B-<data><ext>`, **sincronizável** — as duas versões passam a existir nos dois lados |

Funciona também para **conflito de tipo** (arquivo de um lado, diretório do
outro): o perdedor inteiro é preservado antes de o vencedor ocupar o path.

### Com o daemon rodando, o CLI não escreve nas árvores

Isso é a parte não óbvia. Com o serviço ativo, `-resolve` **grava um pedido** no
banco e o daemon o aplica no ritmo do heartbeat:

```
pedido registrado: docs/relatorio.md -> a
o daemon aplica em até 15s; acompanhe com -conflicts
```

Escrever direto nas árvores faria o daemon ler as escritas como alteração externa
e propagá-las de volta, **desfazendo exatamente o que se acabou de decidir**. Só
quem tem o guard em mãos pode aplicar com segurança.

Com o daemon parado não há ninguém observando, e o CLI aplica na hora. A lógica é
a mesma nos dois casos, para que não divirjam.

A resolução explícita **passa por cima da `conflict_policy`**. Com `manual`, a
política se recusa a tocar em nada — mas quando alguém resolve à mão, não há mais
o que decidir.

### Limpar arquivos preservados

O Synkronyx **nunca apaga** arquivos de conflito preservados: são seus dados.

```bash
# localizar
find /dados/A /dados/B -name '*sync-conflict*'

# comparar com a versão que venceu
diff /dados/A/docs/relatorio.md /dados/A/docs/relatorio.sync-conflict-B-*.md

# apagar depois de conferir
rm /dados/A/docs/relatorio.sync-conflict-B-*.md
```

Apagá-los não gera propagação: o padrão `*.sync-conflict-*` está no `exclude`, e
o watcher os ignora por completo.

**Cuidado com `.sync-kept-`**: esse nome é sincronizável de propósito. Apagá-lo
de um lado propaga a remoção para o outro.

---

## 5. Full Resync

### O que faz

Percorre as duas árvores inteiras, compara e reconcilia — tratando o estado
gravado como suspeito, não como verdade. Três fases:

1. **Scan** das duas árvores, em paralelo
2. **Comparação** de conteúdo, em paralelo (fase cara, e puramente leitura)
3. **Aplicação**, sequencial e em ordem de profundidade

A separação é o que permite paralelizar sem abrir mão de determinismo: na fase 3
a ordem importa, porque criar um filho antes do pai não funciona.

### Quando disparar à mão

```bash
sudo systemctl kill -s HUP synkronyx
```

- Depois de mexer nas árvores por fora com o serviço parado
- Se o `-status` mostra assimetria entre `A=` e `B=`
- Se `por status` mostra `error=N`
- Depois de restaurar backup em um dos lados
- Ao suspeitar que algo não convergiu

Não é destrutivo, e é **idempotente**: sobre árvores já convergidas, o resultado
é `aplicadas=0` e todo o resto em `sem_acao`.

### Quando ele dispara sozinho

| Gatilho | Log |
|---|---|
| Overflow da fila do inotify | `reason="overflow da fila do inotify no lado A"` |
| Tentativas de retry esgotadas | `reason="tentativas esgotadas para <path>"` |

### Ler o resultado

```
scan concluído phase=full-resync entradas_a=9241 entradas_b=9241 duracao=1.2s
reconciliação concluída phase=full-resync aplicadas=0 sem_acao=9241 conflitos=0 falhas=0 duracao=3.4s
```

`aplicadas=0` com todo o resto em `sem_acao` é o desfecho saudável. Se
`aplicadas` for alto num resync logo após outro, algo não está convergindo —
ver [cenário 5](#cenário-5-um-path-não-converge-nunca).

### Custo

Depende do tamanho e de quantos arquivos passam do limite de digest. Medições em
árvores pequenas nesta base de código:

| Árvore | Duração |
|---|---|
| ~20 entradas, arquivos pequenos | 2–3 ms |
| ~13 entradas com um arquivo de 110 MiB | ~100 ms |
| Divergência offline em 9 casos distintos | ~580 ms |

Em árvores grandes o custo é dominado por I/O de metadados no scan e por leitura
de conteúdo na comparação. O `progress_interval` faz ele dar sinal de vida.

---

## 6. O que monitorar

### Os quatro números que importam

```bash
synkronyx -config /etc/synkronyx/synkronyx.yaml -status
```

| Métrica | Saudável | Investigar quando |
|---|---|---|
| `entradas A=` vs `B=` | iguais | diferentes |
| `por status` | só `synced` | qualquer `error` ou `pending` persistente |
| `fila de retry` | `0` | > 0 por mais de alguns minutos |
| `processo` | heartbeat recente | `vencido` |

### Pressão de watches

```bash
usados=$(synkronyx -config /etc/synkronyx/synkronyx.yaml -status | awk '/watches:/{print $2}')
limite=$(cat /proc/sys/fs/inotify/max_user_watches)
echo "$usados / $limite = $(( usados * 100 / limite ))%"
```

O serviço avisa por conta própria acima de 80%, na subida e a cada heartbeat.

### Verificação independente

O `-status` reporta o que o **estado** diz. Para conferir o **disco**:

```bash
# conteúdo, ignorando arquivos de conflito
diff -r --no-dereference /dados/A /dados/B | grep -v sync-conflict

# permissões e dono, que o diff NÃO compara
sudo find /dados/A -mindepth 1 -printf '%P %m %u:%g\n' | sort > /tmp/a.txt
sudo find /dados/B -mindepth 1 -printf '%P %m %u:%g\n' | sort > /tmp/b.txt
diff /tmp/a.txt /tmp/b.txt
```

Vale insistir no segundo: **`diff -r` não compara permissões nem dono**. Uma
divergência de modo passaria por uma verificação superficial dizendo que as
árvores estão idênticas.

### Um script de verificação periódica

```bash
#!/bin/sh
# /usr/local/bin/synkronyx-check
set -e
CFG=/etc/synkronyx/synkronyx.yaml
S=$(synkronyx -config "$CFG" -status)

echo "$S" | grep -q 'vencido' && { echo "ALERTA: heartbeat vencido"; exit 1; }
echo "$S" | grep -qE 'error=[1-9]' && { echo "ALERTA: paths com erro"; exit 1; }

a=$(echo "$S" | sed -n 's/.*entradas:.*A=\([0-9]*\).*/\1/p')
b=$(echo "$S" | sed -n 's/.*entradas:.*B=\([0-9]*\).*/\1/p')
[ "$a" = "$b" ] || { echo "ALERTA: assimetria A=$a B=$b"; exit 1; }

echo "ok: A=$a B=$b"
```

Não há endpoint Prometheus. O `-status` é feito para humanos, e a saída não é um
formato estável para scraping — se um dia precisar disso, é melhor pedir do que
parsear.

---

## 7. Ajustar parâmetros

Editar `/etc/synkronyx/synkronyx.yaml` e reiniciar:

```bash
sudo $EDITOR /etc/synkronyx/synkronyx.yaml
sudo -u synkronyx synkronyx -config /etc/synkronyx/synkronyx.yaml -check
sudo systemctl restart synkronyx
```

O `-check` antes do restart evita descobrir um erro de configuração com o serviço
já derrubado.

### Latência alta demais

Sintoma: alterações levam mais tempo do que o aceitável para aparecer do outro
lado.

```yaml
debounce: 300ms      # padrão 1s
```

Com `debounce: 1s`, a latência medida de uma escrita isolada é **~1,04 s**.
Reduzir aumenta o número de transferências, porque rajadas deixam de ser
agrupadas. Um editor salvando gera quatro eventos para uma alteração lógica.

### Transferências demais, disco ocupado

```yaml
debounce: 3s
```

Aumentar agrupa rajadas mais agressivamente. Bom para builds e extração de
arquivos, onde muitos arquivos mudam em sequência.

### Árvore grande, First Sync lento

```yaml
sync_workers: 8
progress_interval: 15s
```

Mais workers paralelizam a aplicação; o particionamento por subárvore de
primeiro nível preserva a ordem onde ela importa. Se a árvore tem poucas
subárvores de primeiro nível, aumentar não ajuda — a partição é por elas.

### Arquivos muito grandes, CPU alta em hash

```yaml
hash_max_bytes: 10485760    # 10 MiB
hash_sample_bytes: 1048576  # 1 MiB de cada ponta
```

Baixar o limite faz mais arquivos usarem digest amostrado, que tem custo fixo em
vez de proporcional ao tamanho. O preço é o ponto cego descrito na
[seção 10](#10-limitações-que-aparecem-na-operação).

A validação recusa `hash_sample_bytes × 2 ≥ hash_max_bytes`, porque aí a amostra
cobriria o arquivo inteiro e a configuração não faria o que promete.

### Falhas transitórias frequentes

```yaml
retry_max_attempts: 12
retry_initial_delay: 10s
retry_max_delay: 30m
```

Com o padrão (8 tentativas, início em 5 s), as esperas são 5 s, 10 s, 20 s,
40 s, 1 m20 s, 2 m40 s e 5 m20 s — janela de **10 m35 s** antes de desistir.
Esgotadas, o path é marcado com `error` e um Full Resync é pedido.

### Investigar um comportamento

```yaml
log_level: debug
```

Acrescenta os ecos descartados pelo guard, eventos já refletidos no estado e as
varreduras. Custo medido: ~485 bytes por arquivo propagado contra ~135 em info.
**Volte para `info` depois** — em árvores com muita escrita isso pressiona o
journal.

Limitar o journal, se necessário:

```bash
sudo journalctl --vacuum-size=500M
# ou, permanente, em /etc/systemd/journald.conf:
#   SystemMaxUse=1G
```

### Conflitos indesejados

```yaml
conflict_policy: manual
```

Com `manual`, nada é tocado: o par é marcado e espera `-resolve`. Bom para dados
em que sobrescrever automaticamente é inaceitável, ao custo de exigir
intervenção.

### Dados que usam hardlinks

```yaml
preserve_hardlinks: true
```

Vale para cópias de árvore, não para propagação incremental por evento. Ligações
criadas incrementalmente só voltam a existir no próximo Full Resync.

---

## Backup do estado

O `state.db` não contém seus dados — contém o que foi sincronizado por último.
Perdê-lo não perde arquivo nenhum, mas tem consequência: sem histórico, a
reconciliação não distingue "arquivo novo" de "arquivo apagado do outro lado", e
remoções feitas com o serviço parado são desfeitas.

### Backup correto

```bash
sudo sqlite3 /var/lib/synkronyx/state.db ".backup /backup/synkronyx-state.db"
```

Use `.backup`, não `cp`. O banco opera em modo WAL, e copiar o arquivo com o
serviço rodando pode capturar um estado inconsistente — faltando o `-wal` que
contém as escritas recentes.

Com o serviço parado, `cp` de `state.db`, `state.db-wal` e `state.db-shm` juntos
também funciona.

### Restaurar

```bash
sudo systemctl stop synkronyx
sudo cp /backup/synkronyx-state.db /var/lib/synkronyx/state.db
sudo rm -f /var/lib/synkronyx/state.db-wal /var/lib/synkronyx/state.db-shm
sudo chown synkronyx:synkronyx /var/lib/synkronyx/state.db
sudo systemctl start synkronyx
sudo systemctl kill -s HUP synkronyx     # resync para reconciliar a defasagem
```

O resync depois da restauração importa: o backup é de um instante anterior, e o
disco mudou desde então.

### Backup dos dados

Faça de **um** dos lados, não dos dois — eles são equivalentes por construção, e
backup duplicado só gasta espaço. Inclua os arquivos `*.sync-conflict-*`: são
versões que só existem ali.

---

## 9. Cenários de desastre

### Cenário 1: o estado foi perdido

**Sintoma.** No log da subida, ou no `-status`:

```
banco de estado está vazio, mas o first sync já havia rodado antes —
remoções feitas com o serviço parado serão desfeitas
```

**O que vai acontecer.** O First Sync trata tudo como novo. Arquivos apagados de
um lado enquanto o serviço estava parado **voltam**, copiados do lado onde ainda
existem.

**Por que é intencional.** Ressuscitar um arquivo é o erro recuperável — basta
apagar de novo. Apagá-lo é o irrecuperável. Fail Safe manda escolher o primeiro.

**O que fazer.**

1. Se tem backup do estado, restaure antes de subir o serviço
2. Se não tem, deixe reconciliar e depois apague de novo o que precisava sair
3. Confira `-conflicts` ao terminar: sem linha de base, divergências viram
   conflito, o que é o comportamento conservador correto

### Cenário 2: uma raiz foi desmontada ou removida

**Sintoma.**

```
lado A: a raiz observada /dados/A foi removida
```

O serviço **encerra** com erro, e o systemd reinicia por causa do
`Restart=always`.

**Por que encerrar em vez de continuar.** Um lado cego enquanto o outro segue
propagando é pior que parar: alterações do lado vivo seriam aplicadas sem
contrapartida, e a divergência cresceria sem ninguém notar.

**O que fazer.** Remontar ou recriar a raiz. O serviço volta sozinho na próxima
tentativa de restart, e o First Sync reconcilia.

Se a raiz ficar indisponível por muito tempo, o systemd entra em ciclo de
restart. Pare o serviço enquanto resolve:

```bash
sudo systemctl stop synkronyx
# resolver a montagem
sudo systemctl start synkronyx
```

### Cenário 3: disco cheio

**Sintoma.** `falha ao propagar; reenfileirado` com erro de espaço, e a fila de
retry crescendo.

**Comportamento.** O retry tenta por até ~10 minutos com backoff. Esgotado,
marca o path com `error` e pede um Full Resync — que também falhará enquanto o
disco estiver cheio.

**O que fazer.** Liberar espaço e disparar um resync. Nada foi perdido: o lado de
origem está intacto, e o `--partial` do rsync preserva o que já havia sido
transferido.

Candidatos a liberar espaço:

```bash
find /dados/A /dados/B -name '*sync-conflict*' -printf '%s %p\n' | sort -rn | head
```

### Cenário 4: os dois lados divergiram muito

Depois de um período longo com o serviço parado e edições dos dois lados.

**Antes de subir o serviço**, meça o tamanho do problema:

```bash
diff -rq --no-dereference /dados/A /dados/B | tee /tmp/divergencia.txt | wc -l
grep -c 'differ' /tmp/divergencia.txt      # conflitos em potencial
grep -c 'Only in' /tmp/divergencia.txt     # unilaterais, propagação simples
```

Os `differ` são os que podem virar conflito; os `Only in` são propagação normal.
Só viram conflito de fato se **os dois** lados tiverem se afastado do estado
gravado.

**Se o volume for grande e você quiser um lado autoritativo:**

```yaml
first_sync_policy: a-wins    # ou b-wins
```

Isso resolve todas as divergências pela origem escolhida, sem conflitos, e sem
preservar as versões perdedoras. **Volte para `union` depois** — `a-wins`
permanente significa que edições em B nunca sobrevivem.

### Cenário 5: um path não converge nunca

**Sintoma.** Resyncs sucessivos reportando `aplicadas > 0` para o mesmo path,
indefinidamente.

Isso é o pior tipo de defeito num serviço de longa execução: não falha, não
alerta, só nunca acaba. Vale saber reconhecer.

**Diagnóstico.**

```bash
sudo systemctl kill -s HUP synkronyx ; sleep 5
sudo systemctl kill -s HUP synkronyx ; sleep 5
journalctl -u synkronyx --since "1 min ago" | grep -E 'reconciliação concluída|atributos divergentes'
```

Se o segundo resync não der `aplicadas=0`, veja qual path reaparece.

**Causas conhecidas, todas corrigidas nas versões atuais:**

| Causa | Já corrigida |
|---|---|
| `mtime` de symlink que não era ajustado | sim, via `UtimesNanoAt` |
| Bits `setuid`/`setgid`/`sticky` mascarados por `Perm()` | sim, `hash.PermMask` |
| Alteração só de `mtime` não propagada | sim |
| `chmod` em diretório ignorado | sim |

Se aparecer com a versão atual, é defeito novo. Colete o log com
`log_level: debug` e o `stat` do path nos dois lados antes de reportar.

### Cenário 6: suspeita de loop de sincronização

**Sintoma.** Um arquivo sendo reescrito continuamente, sem ninguém mexendo nele.

**Diagnóstico.**

```yaml
log_level: debug
```

```bash
journalctl -u synkronyx -f | grep -E 'propagando|evento próprio descartado'
```

Em operação normal, uma escrita produz **uma** linha `propagando` e 3 a 4
`evento próprio descartado`. Um loop apareceria como `propagando` alternando
entre os lados para o mesmo path, sem parar.

**Se acontecer**, é defeito sério — a prevenção de loops tem duas camadas
independentes justamente para isso. Aumente `self_write_ttl` como paliativo
imediato e reporte com o log em debug.

---

## 10. Limitações que aparecem na operação

### Digest amostrado acima de 100 MiB

Acima de `hash_max_bytes`, o digest cobre tamanho, início e fim. Uma alteração no
meio que preserve o tamanho exato não é vista **pelo digest**.

Na prática o ponto cego é estreito:

- No fluxo de eventos, o `mtime` é comparado antes do digest, e escrever sempre
  altera o `mtime`
- Na reconciliação, digests amostrados iguais com `mtime`s diferentes contam como
  divergência

Sobra a janela em que a alteração cabe na tolerância de `mtime` de um segundo em
relação à última sincronização. Se isso for inaceitável, `hash_max_bytes: 0`.

### `touch` em arquivo grande custa uma cópia

A reconciliação não consegue distinguir "só o `mtime` mudou" de "alteração no
meio que a amostra não vê" — as duas situações são idênticas para ela. Escolhe o
lado seguro e copia o conteúdo. Um `touch` num arquivo de 100 GiB custa uma
leitura de 100 GiB de cada lado, uma vez.

### Hardlinks só em cópia de árvore

`--hard-links` só enxerga ligações dentro de uma mesma invocação do rsync. Vale
para First Sync, Full Resync e diretório novo; não para arquivos propagados um a
um por evento. Conte com o Full Resync para restabelecê-los.

### Dono e grupo exigem privilégio

Sem root, não são comparados. É condição de privilégio, não de configuração,
porque comparar sem poder aplicar produziria divergência detectada e nunca
resolvida.

### Arquivos especiais não são sincronizados

Sockets, FIFOs e device nodes são ignorados. Um socket unix só existe enquanto o
processo que o criou está escutando; copiá-lo produz um arquivo inerte.

### Conflito de tipo exige intervenção

Arquivo de um lado, diretório do outro: nenhuma política automática resolve,
porque sobrescrever apagaria uma árvore inteira. Use `-resolve`.

### Janela cega de `self_write_ttl` após resolução

Depois de resolver um conflito, a expectativa do guard é mantida de propósito até
o TTL vencer — se fosse esquecida antes, um eco atrasado desfaria a resolução.
Nesse intervalo (padrão 60 s), alterações externas **naquele path específico**
são ignoradas. É estreito e localizado, e o Full Resync cobre o caso extremo.

### Sincronização é local

Dois diretórios na mesma máquina. Um pode ser montagem remota, mas não há
protocolo próprio, autenticação nem transporte.

---

## 11. Árvore de diagnóstico

```
Algo parece errado
│
├─ O serviço está rodando?
│  └─ systemctl status synkronyx
│     ├─ inactive/failed → journalctl -u synkronyx -p err | tail -20
│     │   ├─ "raiz ... foi removida"     → cenário 2
│     │   ├─ "rsync não encontrado"      → instalação, problema 5
│     │   ├─ "configuração inválida"     → -check mostra o campo
│     │   └─ "permission denied"         → instalação, problema 3
│     └─ active → siga abaixo
│
├─ Alterações não aparecem do outro lado
│  ├─ Já esperou mais que o debounce? (padrão 1s)
│  ├─ O path está no exclude?
│  │   └─ grep -A15 '^exclude:' /etc/synkronyx/synkronyx.yaml
│  ├─ É socket, FIFO ou device node?  → não é sincronizável, por projeto
│  ├─ watches perto do limite?        → -status, campo watches
│  ├─ fila de retry > 0?              → journalctl | grep reenfileirado
│  └─ Nenhum dos acima               → SIGHUP e observe o resync
│
├─ As árvores divergem
│  ├─ diff -r ignorando *sync-conflict*   → conteúdo
│  ├─ comparar find -printf '%m %u:%g'    → permissões e dono
│  ├─ -status: entradas A= vs B= iguais?
│  └─ SIGHUP; se o segundo resync não der aplicadas=0 → cenário 5
│
├─ Arquivos de conflito surgindo
│  ├─ -conflicts para ver o contexto
│  ├─ Alteração unilateral virando conflito? → é defeito, reporte
│  └─ Os dois lados mudaram mesmo?          → comportamento correto
│
├─ Consumo alto de CPU ou I/O
│  ├─ Arquivos grandes com hash completo?  → baixar hash_max_bytes
│  ├─ Rajadas de escrita?                  → aumentar debounce
│  ├─ log_level em debug?                   → voltar para info
│  └─ Resync em curso?                      → journalctl | grep 'em andamento'
│
└─ -status mostra heartbeat vencido
   ├─ systemctl status  → processo vivo?
   ├─ Vivo mas sem heartbeat → travado; colete o log e reinicie
   └─ Morto → journalctl -p err
```

---

## 12. Inspecionar o estado à mão

O banco é SQLite comum. Ler com o serviço rodando é seguro; **não escreva** nele.

```bash
sudo sqlite3 /var/lib/synkronyx/state.db
```

### Esquema

| Tabela | Conteúdo |
|---|---|
| `entries` | Um registro por `(path, side)`: tamanho, `mtime`, modo, digest, `uid`, `gid`, status |
| `conflicts` | Conflitos detectados, com os digests dos dois lados e a resolução aplicada |
| `resolutions` | Pedidos de `-resolve` pendentes ou aplicados |
| `meta` | Heartbeat, contagem de watches, `first_sync`, `last_full_resync`, PID |

### Consultas úteis

```sql
-- versão do schema (atual: 4)
PRAGMA user_version;

-- paths com problema
SELECT path, side, status FROM entries WHERE status <> 'synced';

-- assimetria: paths que só um lado conhece
SELECT path FROM entries GROUP BY path HAVING COUNT(*) <> 2;

-- os maiores arquivos rastreados
SELECT path, size, digest FROM entries WHERE side = 0
ORDER BY size DESC LIMIT 10;

-- quais usam digest amostrado
SELECT COUNT(*) FROM entries WHERE digest LIKE 'sha256p:%';

-- conflitos e como foram resolvidos
SELECT path, datetime(detected_at/1000000000, 'unixepoch', 'localtime') AS quando,
       resolution
FROM conflicts ORDER BY detected_at DESC LIMIT 20;

-- pedidos de resolução pendentes
SELECT path, want FROM resolutions WHERE applied_at IS NULL;

-- estado operacional publicado pelo daemon
SELECT key, value FROM meta;
```

### Formato dos digests

| Prefixo | Significa |
|---|---|
| `sha256:<hex>` | SHA-256 completo do conteúdo. Prova igualdade |
| `sha256p:<hex>:<tamanho>` | Amostrado: tamanho + as duas extremidades. Evidência forte |
| `link:<hex>` | SHA-256 do **alvo** de um symlink, como texto |
| vazio | Diretório, ou arquivo especial |

O tipo é persistido junto com o valor de propósito: comparar um digest amostrado
com um completo daria uma resposta sem significado, e o código recusa fazê-lo.

---

## Onde ir depois

- [Manual de instalação](INSTALACAO.md) — requisitos, instalação, FAQ
- [Decisões e limitações](DECISOES-ABERTAS.md) — o que o projeto escolheu não
  fazer, e por quê
- [ADR 001](adr-001-prevencao-de-loops.md) — a prevenção de loops em detalhe
