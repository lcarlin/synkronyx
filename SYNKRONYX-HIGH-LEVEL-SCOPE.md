# Synkronyx — Especificação de Alto Nível

## 1. Visão Geral

**Synkronyx** é um serviço Linux de sincronização bidirecional contínua entre duas árvores de diretórios.

O objetivo é manter dois diretórios raiz logicamente equivalentes, de forma que alterações realizadas em qualquer um dos lados sejam detectadas e refletidas no outro lado.

Exemplo:

```text
A/
├── subdir1/
├── subdir2/
└── ...

B/
├── subdir1/
├── subdir2/
└── ...
```

A sincronização deve funcionar recursivamente para todos os subdiretórios.

---

## 2. Objetivo

O sistema deverá detectar e propagar alterações entre A e B.

Operações contempladas:

- criação de arquivos;
- alteração de conteúdo;
- criação de diretórios;
- rename/move;
- exclusão;
- alterações relevantes de metadados e atributos.

Fluxo esperado:

```text
A ───────────────► B
▲                  │
│                  ▼
└──────────────────┘
```

Uma alteração em A deve chegar a B, e uma alteração em B deve chegar a A.

---

## 3. Arquitetura de Alto Nível

A solução será composta por quatro elementos principais:

```text
                 ┌─────────────────────┐
                 │   Sync Controller   │
                 └──────────┬──────────┘
                            │
             ┌──────────────┴──────────────┐
             │                             │
      ┌──────▼──────┐               ┌──────▼──────┐
      │  Watcher A  │               │  Watcher B  │
      │  inotify    │               │  inotify    │
      └──────┬──────┘               └──────┬──────┘
             │                             │
             └──────────────┬──────────────┘
                            │
                     ┌──────▼──────┐
                     │ Sync Engine │
                     └──────┬──────┘
                            │
                       ┌────▼────┐
                       │  State  │
                       │ / Logs  │
                       └─────────┘
```

O componente central será o **Sync Engine**, responsável por interpretar eventos, determinar a operação necessária e coordenar a sincronização.

---

## 4. Detecção de Alterações

A detecção será baseada no mecanismo **Linux inotify**.

Os diretórios A e B serão monitorados recursivamente.

Eventos relevantes:

```text
CREATE
MODIFY
DELETE
MOVED_FROM
MOVED_TO
```

A arquitetura deve considerar que um único evento de filesystem pode representar apenas uma etapa de uma operação maior. Portanto, eventos deverão ser agrupados/debounced quando necessário antes da sincronização.

---

## 5. Motor de Sincronização

O Sync Engine receberá os eventos dos watchers e determinará a ação correspondente no diretório oposto.

Exemplo:

```text
A/foo/bar.txt
      │
      │ MODIFY
      ▼
Sync Engine
      │
      ▼
B/foo/bar.txt
```

Para transferência eficiente de arquivos, será utilizado **rsync**.

O rsync é adequado porque evita transferências desnecessárias e trabalha muito bem com árvores de diretórios e arquivos grandes.

---

## 6. Bidirecionalidade e Prevenção de Loops

Este é um dos requisitos mais importantes do projeto.

Não será utilizada simplesmente a abordagem:

```text
inotify A → rsync A → B
inotify B → rsync B → A
```

pois isso pode produzir loops:

```text
A altera
 ↓
A → B
 ↓
B detecta alteração
 ↓
B → A
 ↓
A detecta alteração
 ↓
...
```

O Sync Engine deverá identificar alterações originadas pelo próprio processo de sincronização.

A alteração propagada pelo sistema deve ser marcada/controlada para não ser interpretada novamente como uma alteração externa.

Esse mecanismo deverá ser tratado como requisito arquitetural, e não como uma correção posterior.

---

## 7. Controle de Estado

O projeto utilizará **SQLite** para manter o estado conhecido da sincronização.

Informações conceituais:

```text
path
lado
hash
tamanho
mtime
última_sincronização
status
```

O estado permitirá distinguir:

```text
alteração externa
        x
alteração produzida pelo sincronizador
        x
conflito
        x
arquivo já sincronizado
```

O SQLite deverá permanecer pequeno e operacionalmente simples.

---

## 8. Integridade e Identificação de Conteúdo

Será utilizado **SHA-256** para identificar o conteúdo dos arquivos quando necessário.

Exemplo:

```text
A/foo.txt
SHA256 = ABC123...

B/foo.txt
SHA256 = ABC123...
```

Os conteúdos são equivalentes.

Caso:

```text
A/foo.txt
SHA256 = ABC123

B/foo.txt
SHA256 = DEF456
```

o sistema deverá considerar que os conteúdos são diferentes.

Hashes não precisam necessariamente ser recalculados para todos os arquivos a cada evento. O mecanismo deverá utilizar o estado e metadados do filesystem para minimizar custo computacional.

---

## 9. Tratamento de Conflitos

O sistema deverá detectar alterações concorrentes.

Exemplo:

```text
A/file.txt ← alterado
B/file.txt ← alterado
```

Quando ambas as versões forem diferentes e não for possível determinar uma origem única, o evento deverá ser tratado como conflito.

A política inicial recomendada é:

```text
Conflito
   ↓
registrar no log
   ↓
preservar as versões
   ↓
não sobrescrever silenciosamente
```

Uma possível estratégia de preservação:

```text
file.txt
file.sync-conflict-A-20260909.txt
```

A política poderá posteriormente evoluir para:

- last-write-wins;
- versionamento;
- resolução manual;
- merge específico para determinados tipos de arquivo.

---

## 10. Inicialização

O sistema deverá possuir um processo explícito de **First Sync**.

Antes de habilitar o monitoramento contínuo, A e B deverão ser comparados para estabelecer o estado inicial.

Exemplo:

```text
FIRST SYNC
    ↓
Scan A
    ↓
Scan B
    ↓
Comparação
    ↓
Sincronização inicial
    ↓
Construção do estado
    ↓
Ativação dos watchers
```

A ordem e política para resolver divergências existentes deverão ser definidas antes da execução do First Sync.

---

## 11. Full Resync

O sistema deverá possuir uma operação de **Full Resync** para reconstrução do estado.

Objetivos:

- detectar inconsistências;
- reconstruir o banco de estado;
- validar os dois lados;
- corrigir divergências conforme a política configurada.

Exemplo conceitual:

```text
FULL RESYNC
     ↓
Scan completo A
     ↓
Scan completo B
     ↓
Comparação
     ↓
Resolução
     ↓
Atualização do estado
```

---

## 12. Serviço Linux

O Synkronyx será executado como serviço **systemd**.

Nome sugerido:

```text
synkronyx.service
```

Operações esperadas:

```bash
systemctl start synkronyx
systemctl stop synkronyx
systemctl restart synkronyx
systemctl status synkronyx
```

O serviço deverá:

- iniciar automaticamente com o sistema;
- reiniciar em caso de falha;
- executar com usuário dedicado;
- possuir permissões mínimas necessárias;
- gerar logs no journald.

Consulta de logs:

```bash
journalctl -u synkronyx
```

---

## 13. Tecnologias

Stack inicial proposta:

```text
Linux Ubuntu
    │
    ├── Go
    │       └── Sync Engine / daemon
    │
    ├── inotify
    │       └── detecção de alterações
    │
    ├── rsync
    │       └── transferência eficiente
    │
    ├── SQLite
    │       └── estado / controle / conflitos
    │
    ├── SHA-256
    │       └── integridade / identificação de conteúdo
    │
    ├── systemd
    │       └── gerenciamento do serviço
    │
    └── journald
            └── logging
```

### Go

Go será utilizado para implementar o daemon principal.

Motivos:

- binário único;
- baixo consumo de recursos;
- excelente suporte a concorrência;
- boa integração com Linux;
- facilidade de deployment;
- manutenção simples;
- adequado para serviços de longa execução.

### inotify

Responsável pela observação das árvores de filesystem.

### rsync

Responsável pela cópia/sincronização eficiente dos dados.

### SQLite

Responsável pelo estado persistente do sincronizador.

### SHA-256

Responsável pela identificação de conteúdo quando necessário para validação e detecção de divergências.

### systemd

Responsável pelo ciclo de vida do serviço.

### journald

Responsável pelo armazenamento e consulta dos logs operacionais.

---

## 14. Arquitetura Consolidada

```text
                 ┌─────────────────────────┐
                 │     synkronyx.service   │
                 │         systemd         │
                 └────────────┬────────────┘
                              │
                    ┌─────────▼─────────┐
                    │    Sync Engine    │
                    │        Go         │
                    └─────────┬─────────┘
                              │
              ┌───────────────┼────────────────┐
              │               │                │
        ┌─────▼─────┐   ┌─────▼─────┐   ┌──────▼─────┐
        │  inotify  │   │   SQLite  │   │   SHA-256  │
        │  Watchers │   │   State   │   │  Integrity │
        └─────┬─────┘   └───────────┘   └────────────┘
              │
       ┌──────┴──────┐
       │             │
      DIR A         DIR B
       │             │
       └──────┬──────┘
              │
           rsync
```

---

## 15. Requisitos Não Funcionais

A solução deverá priorizar:

- simplicidade operacional;
- baixo consumo de CPU e memória;
- baixa latência na propagação das alterações;
- tolerância a grande quantidade de subdiretórios;
- segurança contra loops;
- preservação de arquivos em conflitos;
- recuperação após restart;
- observabilidade através de logs;
- execução sem necessidade de terminal aberto;
- configuração simples;
- facilidade de manutenção.

---

## 16. Princípios de Projeto

O projeto deverá seguir principalmente:

**KISS — Keep It Simple, Stupid**

Evitar dependências desnecessárias e arquiteturas distribuídas quando o problema pode ser resolvido localmente.

**Fail Safe**

Em situações ambíguas, como conflitos, o sistema deve preferir preservar dados a sobrescrever silenciosamente.

**Event Driven**

Alterações deverão ser detectadas por eventos de filesystem, evitando polling agressivo.

**Idempotência**

A execução repetida de uma operação de sincronização não deve produzir corrupção ou efeitos colaterais.

**Observabilidade**

Toda operação relevante deverá poder ser rastreada através dos logs.

**Recovery**

Após crash ou reboot, o serviço deverá ser capaz de reconstruir seu estado e continuar a operação sem exigir intervenção manual em condições normais.

---

## 17. Resultado Esperado

Ao final, o usuário deverá poder configurar:

```text
SOURCE A = /dados/A
SOURCE B = /dados/B
```

iniciar:

```bash
systemctl enable --now synkronyx
```

e deixar o serviço funcionando continuamente.

O comportamento esperado será:

```text
                     SYNKRONYX

                 ┌───────────────┐
                 │   Sync Engine │
                 └───────┬───────┘
                         │
              ┌──────────┴──────────┐
              │                     │
          monitor A             monitor B
              │                     │
              ▼                     ▼
         ┌─────────┐           ┌─────────┐
         │    A    │◄─────────►│    B    │
         └─────────┘           └─────────┘
              ▲                     ▲
              │                     │
           qualquer               qualquer
           alteração              alteração
```

A premissa fundamental do projeto é que **A e B são dois lados equivalentes da mesma árvore lógica de dados**, e o Synkronyx atua como o mecanismo responsável por manter essa equivalência continuamente.
