# Decisões e limitações conhecidas

Este arquivo já foi uma lista de pendências duas vezes. Agora é o registro das
**decisões tomadas e das limitações que sobreviveram a elas** — nada aqui é
TODO disfarçado. Onde uma limitação permanece, ela permanece por escolha, e a
escolha está justificada.

O que continua genuinamente em aberto está na última seção.

---

## Comportamento sob condições difíceis

### Prevenção de loops

Duas camadas independentes. Detalhes em
[adr-001-prevencao-de-loops.md](adr-001-prevencao-de-loops.md).

Expectativas de subárvore cobrem as operações que tocam um número
indeterminado de paths — cópia de árvore, remoção recursiva, rename de
diretório. Sem elas, cada uma dessas operações geraria eventos que voltariam
como alteração externa.

### Digest de conteúdo

Completo até `hash_max_bytes`; acima, amostrado (tamanho + as duas
extremidades). O tipo é persistido junto com o valor e `hash.Compare` recusa
comparar tipos diferentes.

**Limitação aceita:** alteração no meio de um arquivo grande que preserve o
tamanho não é detectada. Append, truncamento, cabeçalho e cauda são.
`TestPartialDetectsRealisticChanges` fixa o contrato, ponto cego incluído.

O padrão é `0` — digest completo, sem pontos cegos.

### Symlinks

A identidade de um symlink é **o alvo, como texto**, não o conteúdo apontado
(`hash.KindLink`). Consequências: um link quebrado é sincronizável, dois links
para alvos diferentes divergem mesmo com conteúdo idêntico, e mudar o alvo é
uma alteração.

Todo stat usa `Lstat`. Seguir o link faria um link quebrado virar erro e um
link para fora da árvore virar cópia do alvo.

### O quick check do rsync

`CopyFile` passa `--ignore-times`; `CopyTree` não.

A assimetria é deliberada. Por padrão o rsync decide se precisa transferir
comparando tamanho e mtime — boa heurística para varrer uma árvore, errada
quando o engine já decidiu, comparando digests, que o arquivo precisa ser
atualizado.

E ela erra no pior momento: dois arquivos que divergiram mas têm mesmo tamanho
e mesmo mtime — o desfecho comum de um conflito, em que os dois lados foram
editados quase juntos — passam no quick check, e a transferência é pulada em
silêncio. A resolução "termina com sucesso" sem ter copiado nada. Custou um
bug para ficar claro, e `TestCopyFileTransfersDespiteMatchingSizeAndMtime`
existe para que não volte.

Em `CopyTree` o quick check é justamente o que se quer: evita retransferir o
que não mudou numa árvore inteira.

### Preservar copiando, não renomeando

Preservar a versão perdedora de um conflito é uma cópia, nunca um rename.

Um rename emite `MOVED_FROM` no path original e `MOVED_TO` no nome preservado.
O segundo é descartado pelo watcher, porque o nome preservado casa com o
exclude. Sobra um `MOVED_FROM` órfão, que só pode ser lido como remoção — e
essa remoção era propagada de volta, apagando justamente a versão vencedora do
outro lado.

Tentar cobrir o caso contando eventos não funciona: o rsync emite um número
variável de `ATTRIB`, e qualquer contagem fixa erra. Copiando, o evento
perigoso não existe — o path original nunca deixa de existir.

A camada de idempotência não protegia contra isso, e vale entender por quê: o
`DELETE` descrevia o disco corretamente. Layer 2 detecta ecos, não decisões
erradas tomadas a partir de fatos verdadeiros.

### Hardlinks

Opcionais via `preserve_hardlinks`.

**Limitação aceita:** `--hard-links` só enxerga ligações dentro de uma mesma
invocação do rsync — vale para cópias de árvore (First Sync, Full Resync,
diretório novo), não para arquivos propagados um a um por evento. Quem depende
de hardlinks conta com o Full Resync para restabelecê-los.

### Arquivos especiais

Sockets, FIFOs e device nodes são ignorados (`special_files: skip`).

Não é omissão: um socket unix só existe enquanto o processo que o criou está
escutando, e copiá-lo produz um arquivo inerte de mesmo nome. Um FIFO copiado
é um FIFO vazio, sem relação com o original. Device nodes exigem privilégio e
apontam para o hardware local. `special_files: error` registra cada ocorrência
para quem prefere ser avisado.

### Remoção de diretórios

`dir_delete_policy: preserve-unknown` inspeciona o destino antes de remover e
põe de lado o que o estado não conhece — criado ali e nunca propagado, ou
alterado ali depois da última sincronização. Vai para
`<dir>.sync-conflict-<lado>-<data>/`, que o exclude ignora.

### Estado perdido

Detectado e avisado, com a condição precisa: banco vazio **e** First Sync já
tendo rodado **e** raízes com conteúdo.

**Comportamento aceito:** sem histórico, remoções feitas com o serviço parado
são desfeitas — não há como distingui-las de arquivos novos. Ressuscitar um
arquivo é o erro recuperável; apagá-lo é o irrecuperável. Fail Safe manda
escolher o primeiro, em voz alta.

### Falhas de transferência

Fila em memória com backoff exponencial. Esgotadas as tentativas: `status =
error` e Full Resync solicitado.

**Escolha aceita:** a fila não é persistida. O First Sync do próximo boot cobre
o pendente, e persistir daria durabilidade que o resync já oferece ao custo de
mais estado para manter coerente.

### Raiz que desaparece

Removida, movida ou desmontada, o watcher reporta falha terminal e `Run`
retorna erro. O systemd reinicia. Seguir rodando deixaria um lado cego
enquanto o outro continua propagando.

---

## Ambiente

`runPreflight` avisa, na subida, sobre condições em que o serviço funciona mas
não faz o que a configuração promete:

- **Dono e grupo.** Se os argumentos do rsync pedem preservação (`--archive`
  implica `-o -g`) e o processo não roda como root, os arquivos ficam com o
  dono do serviço — e o rsync não reclama. É silencioso por natureza, daí o
  aviso.
- **Filesystem de rede.** `--numeric-ids` preserva o número do usuário, não o
  nome. Em NFS, CIFS, SSHFS e afins, o mesmo número é outra pessoa se as duas
  pontas não compartilham a base de usuários.
- **Pressão de watches.** Acima de 80% de `fs.inotify.max_user_watches`, um
  punhado de diretórios novos basta para cegar parte da árvore.

---

## Escala

Três fases, e a separação entre elas é o que permite paralelizar sem abrir mão
de determinismo:

1. **Scan** das duas árvores, em paralelo — independentes por construção.
2. **Comparação** de conteúdo, em paralelo. É a fase cara (lê e hasheia) e é
   puramente leitura: paralelizar não muda resultado, só tempo.
3. **Aplicação**, sequencial e em ordem de profundidade. Aqui a ordem importa:
   criar um filho antes do pai não funciona, e a lista de subárvores já
   resolvidas por inteiro só faz sentido construída em ordem.

`progress_interval` faz scans e reconciliações longas darem sinal de vida.

No regime de eventos, `sync_workers` habilita processamento paralelo
particionado por subárvore de primeiro nível — o que preserva a ordem dentro
de cada subárvore, a única que importa. Renames entre subárvores passam por
uma barreira que espera todos os workers ficarem ociosos.

**Escolha aceita:** o padrão é `1`. O paralelismo é correto por construção e
testado (`TestDispatchPreservesOrderWithinSubtree`,
`TestDispatchBarrierDrainsWorkers`), mas num daemon que escreve nos dados de
alguém a opção conservadora é o padrão, e subir o número é decisão de quem
conhece a carga.

---

## Conflitos

`synkronyx -conflicts` lista os conflitos abertos com o estado dos dois lados
no disco — tipo, tamanho, mtime — e o digest da última sincronização. É o
contexto necessário para decidir.

`synkronyx -resolve <path> -with a|b|both` resolve. `a` e `b` fazem um lado
vencer e preservam o outro com nome que o exclude ignora — recuperável, não
propagado. `both` preserva com nome sincronizável, de modo que as duas versões
passem a existir nos dois lados.

Conflito de tipo (arquivo de um lado, diretório do outro) é resolvido pelo
mesmo comando: o perdedor inteiro é preservado antes de o vencedor ocupar o
path.

A resolução explícita **passa por cima da `conflict_policy`**. Delegar a cópia
ao caminho normal de propagação faria a política automática opinar de novo — e
`manual` existe justamente para não tocar em nada, então a decisão do operador
seria ignorada com o log ainda reportando sucesso. Quando alguém resolve um
conflito à mão, não há mais o que decidir.

**A parte não óbvia:** com o daemon rodando, o CLI **não** escreve nas
árvores. Ele grava o pedido no banco e o daemon aplica no ritmo do heartbeat.
Escrever direto faria o daemon ler as escritas como alteração externa e
propagá-las de volta, desfazendo exatamente o que se acabou de decidir — só
quem tem o guard em mãos pode aplicar com segurança. Com o daemon parado não
há ninguém observando, e o CLI aplica na hora. A lógica é a mesma nos dois
casos (`engine.ApplyResolution`), para que não divirjam.

---

## Ainda em aberto

**Sincronização entre máquinas.** O Synkronyx é local: dois diretórios na
mesma máquina, ainda que um deles seja um ponto de montagem remoto. Não há
protocolo, autenticação nem transporte próprio.

**Compressão e limite de banda.** Configuráveis manualmente via `rsync_args`,
sem tratamento de primeira classe nem interação com o retry.

**Exclusões por conteúdo ou tamanho.** O `exclude` casa por nome. Não há como
dizer "ignore arquivos acima de 1 GiB" ou "ignore o que o .gitignore ignora".

**Métricas para coleta automática.** `-status` é feito para humanos. Não há
endpoint Prometheus nem saída estruturada estável para scraping.

**Teste de longa duração.** A suíte cobre comportamento; não há teste que rode
por horas sob carga contínua para expor vazamento de memória, crescimento do
banco ou degradação do guard.
