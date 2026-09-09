# Decisões abertas e limitações conhecidas

O que ainda não está resolvido no esqueleto atual. Cada item diz o que existe
hoje e o que falta.

## 1. Rename de diretório com conteúdo, no destino

**Hoje:** `engine.propagateMove` faz `os.Rename` no destino, o que é correto e
barato. Se a origem não existir no destino, degrada para cópia.

**Falta:** a cópia de fallback usa o path de destino, mas não reconcilia o
subtree inteiro quando o que se moveu é um diretório grande. Um Full Resync
corrige, mas seria melhor tratar no momento.

## 2. Hardlinks e links simbólicos

**Hoje:** `rsync --archive` preserva symlinks. O estado guarda `Lstat`, então
um symlink é tratado como entrada própria.

**Falta:** hardlinks não são preservados entre os lados (`--hard-links` não
está nos args padrão, por custo). Decidir se é requisito.

## 3. Arquivos grandes e `hash_max_bytes`

**Hoje:** acima do limite, a comparação cai para tamanho + mtime. Dois
arquivos de mesmo tamanho com conteúdo diferente seriam considerados iguais.

**Falta:** um hash parcial (primeiros e últimos N bytes + tamanho) daria uma
resposta muito melhor pelo mesmo custo. Padrão atual é `0` (sem limite), que é
seguro mas caro em árvores com arquivos muito grandes.

## 4. Política de First Sync

**Hoje:** `union`, `a-wins` e `b-wins` implementadas. Em `union`, um arquivo
presente só de um lado é copiado — a menos que o estado registre que ele já
existiu do outro, caso em que é interpretado como remoção.

**Falta:** validar essa heurística contra o caso de banco de estado perdido
(disco novo, `state.db` apagado). Nessa situação tudo vira "novo" e uma
remoção feita offline seria desfeita. Documentar como comportamento esperado
ou detectar o estado vazio explicitamente.

## 5. Ordem de remoção de diretórios não vazios

**Hoje:** `transfer.Remove` usa `os.RemoveAll` para diretórios.

**Falta:** decidir se remover um diretório no lado A deve apagar,
incondicionalmente, conteúdo criado em B que nunca chegou a A. Hoje apaga.
Sob o princípio Fail Safe, talvez devesse preservar.

## 6. Métricas e observabilidade

**Hoje:** log estruturado em journald, com contadores do guard em nível debug.

**Falta:** um endpoint ou comando de status (`synkronyx -status`) que reporte
fila pendente, conflitos não resolvidos e número de watches ativos.

## 7. Retry de transferências que falharam

**Hoje:** falha de rsync é logada e o evento é descartado. O estado não é
atualizado, então um Full Resync posterior corrige.

**Falta:** fila de retry com backoff para falhas transitórias (disco cheio,
arquivo temporariamente travado), em vez de depender do próximo resync.

## 8. Watch da raiz removida

**Hoje:** se a própria raiz observada for removida, o watcher loga erro e para
de receber eventos daquele lado.

**Falta:** o serviço deveria falhar explicitamente (e deixar o systemd
reiniciar) em vez de continuar rodando meio-cego.
