# ADR 001 — Prevenção de loops de sincronização

Status: aceito
Data: 2026-09-09

## Contexto

A seção 6 do escopo trata a prevenção de loops como requisito arquitetural, e
com razão. Toda escrita que o sincronizador faz no destino é, para o inotify,
indistinguível de uma alteração feita por um usuário. Sem tratamento:

```text
usuário altera A → sync escreve em B → inotify B dispara
                 → sync escreve em A → inotify A dispara → ...
```

O loop não é um bug de borda: é o comportamento padrão da composição ingênua
de `inotify + rsync` nos dois sentidos.

## Alternativas consideradas

**Pausar o watcher do destino durante a escrita.** Simples e errado: uma
alteração legítima feita por um usuário no destino durante a janela de pausa
seria perdida silenciosamente. Perder alteração de usuário é pior do que
qualquer custo de processamento.

**Marcar arquivos com xattr.** Funciona, mas exige suporte a atributos
estendidos no filesystem, não sobrevive a todas as ferramentas de cópia e
transforma cada evento em uma syscall extra. Dependência forte demais para o
princípio KISS da seção 16.

**Comparar apenas hashes, sem registro de intenção.** Correto, porém caro:
todo eco exigiria ler e hashear o arquivo inteiro para descobrir que nada
mudou — exatamente o que a seção 8 pede para evitar.

## Decisão

Duas camadas independentes, com papéis distintos.

### Camada 1 — expectativa de escrita própria (`internal/guard`)

Antes de escrever em `(lado, path)`, o engine registra uma expectativa com
TTL. O evento resultante casa com o registro e é descartado, antes mesmo de
entrar no debouncer.

O registro é feito **antes** da escrita, nunca depois: registrar depois abre
uma corrida em que o evento chega antes do registro.

Uma escrita pode gerar mais de um evento (`CREATE` + `CLOSE_WRITE` +
`ATTRIB`), então a expectativa tem contador, não é booleana.

### Camada 2 — idempotência (`engine.alreadySynced`)

Antes de agir sobre qualquer evento, o engine compara o que está no disco com
o que o estado registra como última sincronização, nos dois lados. Iguais,
a ação é no-op.

## Consequências

A camada 1 é uma otimização; a camada 2 é a garantia. A distinção importa,
porque a camada 1 depende de tempo — uma escrita mais lenta que o TTL faz o
evento chegar depois de a expectativa expirar, e ela falha. Só a camada 2 não
depende de premissa temporal nenhuma.

Daí a regra prática para quem for mexer nisso: **a camada 2 nunca deve ser
removida por parecer redundante.** Sem ela, a ausência de loop deixa de ser
uma propriedade do sistema e passa a ser uma aposta na latência do disco.

O custo é manter estado consistente no SQLite após cada operação — o que o
escopo já exige na seção 7 por outros motivos.

## Verificação

`TestNoSyncLoop` escreve de um lado, espera a propagação estabilizar e
verifica que nenhum dos dois arquivos é reescrito espontaneamente depois
disso, e que nenhum arquivo de conflito apareceu.
