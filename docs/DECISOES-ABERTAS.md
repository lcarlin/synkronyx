# Decisões abertas e limitações conhecidas

Os oito itens originais foram implementados. O que sobra aqui são as
**limitações que sobreviveram às decisões** — nenhuma delas é um TODO
disfarçado: são consequências aceitas, documentadas para que ninguém as
descubra em produção.

A seção final lista o que de fato continua em aberto.

---

## Resolvido

### 1. Rename de diretório com conteúdo

Quando o rename falha porque a origem não existe no destino, o engine
reconcilia a subárvore (`engine.reconcileSubtree`) em vez de copiar só o path.
Um rename bem-sucedido de diretório também reescreve o estado de todo o
conteúdo, cujos paths mudaram junto.

Cobrir a subárvore em vez de disparar um Full Resync é proposital: pagar uma
varredura das duas árvores inteiras por uma divergência de um diretório é
desproporcional.

### 2. Hardlinks e symlinks

Symlinks já eram preservados por `rsync --archive`. Hardlinks passaram a ser
opcionais via `preserve_hardlinks`.

**Limitação aceita:** `--hard-links` só enxerga ligações dentro de uma mesma
invocação do rsync. A opção vale, portanto, para cópias de árvore — First
Sync, Full Resync, diretório novo — e não para arquivos propagados um a um por
evento. Dois arquivos ligados que chegam em eventos separados viram dois
arquivos independentes de mesmo conteúdo. Quem depende de hardlinks precisa
contar com o Full Resync para restabelecê-los.

Padrão desligado: o custo de detecção não se justifica para a maioria dos
usos.

### 3. Arquivos grandes

Acima de `hash_max_bytes` o digest passa a ser amostrado: tamanho + os
primeiros e os últimos `hash_sample_bytes` (`hash.KindPartial`). O tipo do
digest é persistido junto com o valor, e `hash.Compare` recusa comparar
tipos diferentes — um digest amostrado nunca é lido como se fosse completo.

**Limitação aceita:** uma alteração no meio de um arquivo grande que preserve
o tamanho não é detectada. Append, truncamento, reescrita de cabeçalho e
mudança de cauda são. O teste `TestPartialDetectsRealisticChanges` fixa esse
contrato, inclusive o ponto cego.

O padrão continua sendo `hash_max_bytes: 0` — digest completo, mais lento e
sem pontos cegos. A amostragem é para quem escolhe explicitamente trocar
certeza por custo.

### 4. Política de First Sync com estado perdido

O engine detecta o caso em que o First Sync já rodou antes mas o banco está
vazio (`state.IsEmpty`) e emite um aviso explícito.

**Comportamento aceito, não corrigido:** sem histórico, `union` não distingue
"arquivo novo neste lado" de "arquivo apagado do outro lado" — as duas
situações são idênticas no disco. Remoções feitas com o serviço parado são
desfeitas.

Isso é Fail Safe funcionando: ressuscitar um arquivo é o erro recuperável
(basta apagar de novo), apagá-lo é o irrecuperável. O que não era aceitável
era fazê-lo em silêncio, e isso mudou — o aviso sai no log na subida e no
`synkronyx -status`.

### 5. Remoção de diretórios não vazios

Padrão `dir_delete_policy: preserve-unknown`. Antes de propagar a remoção de
um diretório, o engine inspeciona o destino e põe de lado tudo o que o estado
não conhece — arquivos criados ali e nunca propagados, e arquivos alterados
localmente depois da última sincronização. O que é preservado vai para
`<dir>.sync-conflict-<lado>-<data>/`, que casa com o exclude padrão e portanto
não volta a ser sincronizado.

`force` mantém o comportamento anterior, para quem prefere velocidade.

### 6. Observabilidade

`synkronyx -status` reporta watches ativos, fila de retry, contagem de
entradas por lado e por status, conflitos abertos e idade do último
heartbeat — sinalizando quando o heartbeat está vencido e o daemon
provavelmente morreu.

O daemon publica o heartbeat na tabela `meta` do próprio banco; o comando lê
de lá. Sem socket de controle, sem protocolo, sem superfície de rede nova —
a opção KISS da seção 16.

**Limitação aceita:** o relatório é do último heartbeat, não do instante da
consulta. `heartbeat_interval` controla essa granularidade.

### 7. Retry de transferências

Fila em memória com backoff exponencial (`retry_max_attempts`,
`retry_initial_delay`, `retry_max_delay`). Esgotadas as tentativas, o path é
marcado com `status = error` e um Full Resync é solicitado — nunca um descarte
silencioso.

**Escolha aceita:** a fila não é persistida. Se o processo morrer, o First
Sync do próximo boot cobre o que estava pendente. Persistir daria durabilidade
que o resync já oferece, ao custo de mais estado para manter coerente.

### 8. Raiz removida

O watcher sinaliza falha terminal (`Watcher.Fatal`) quando a raiz observada é
removida, movida ou desmontada, e `Engine.Run` retorna erro. O systemd
reinicia o serviço, que volta quando a raiz existir de novo.

Continuar rodando seria pior: um lado ficaria cego enquanto o outro seguisse
propagando normalmente.

---

## Ainda em aberto

**Arquivos especiais.** Sockets, FIFOs e device nodes não têm tratamento
próprio nem teste. O `rsync --archive` os copia, mas o comportamento do
watcher e do digest sobre eles não foi verificado.

**UIDs e GIDs entre máquinas.** `--numeric-ids` preserva os números, o que só
faz sentido se os dois lados compartilharem a base de usuários. Não há
validação nem aviso quando não compartilham.

**First Sync de árvores muito grandes.** O scan é sequencial e a reconciliação
também. Não há paralelismo nem relatório de progresso; uma árvore de milhões
de arquivos vai demorar sem dar sinal de vida além do log final.

**Conflito de tipo (arquivo × diretório).** É detectado e registrado, mas
nenhuma política automática o resolve — sempre exige intervenção. É
deliberado, e a intervenção não tem ferramenta: hoje se resolve editando o
disco à mão.

**Resolução de conflito assistida.** Não existe comando para listar, comparar
e resolver conflitos. `-status` lista os paths; o resto é manual.

**Latência sob rajada muito alta.** O debounce é por path, mas o engine
processa em uma goroutine só. Uma rajada em milhares de arquivos distintos
serializa. O particionamento por path está previsto no comentário de abertura
do `internal/engine`, mas não implementado.
