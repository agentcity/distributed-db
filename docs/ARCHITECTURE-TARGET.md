# Целевая архитектура: M2 / M3 / M5 + AI control plane

> Схема: [target-architecture.png](diagrams/target-architecture.png)
> Это проект архитектуры, а не описание уже реализованного. Текущее состояние —
> `docs/ROADMAP.md`.

## 1. Принципы

1. **Control plane и data plane разделены.** Raft хранит метаданные, placement,
   очередь и lease. Тензоры, активации и веса через Raft не идут.
2. **Range — единица репликации, placement и split/merge.**
3. **У каждого range есть epoch и leader lease.** Запрос со старым epoch
   отвергается; это fencing, а не «доверие последнему лидеру».
4. **Multi-Raft — внутри одного процесса.** Один `kvserver` держит N
   `RangeGroup`; каждая группа — свой Raft log, snapshot и state machine.
5. **SQL — отдельный процесс.** SQL-слой ходит в KV через SDK/транзакции; он не
   встраивается в Raft-группы данных.
6. **AI inference — отдельный продукт.** Общие идеи (Raft, lease, epoch,
   placement), но отдельный кластер и другой data plane.

## 2. Компоненты

### 2.1 Meta Raft Group (control plane)

Маленький кластер из 3 или 5 узлов. Не размещается на каждом data-узле.

Состояние (`MetaState`), реплицируемое через Raft:

```text
ranges:      id, [start,end), replicas, leader, epoch, state
nodes:       id, addr, capacity, last_heartbeat, status
tso:         last_physical, last_logical
leases:      range_leader, txn_lease
transactions: active startTS / safe point
placement:   range -> process / group
```

API:

- `GetRange(key)`, `ListRanges`, `WatchRanges` (stream);
- `AllocTS(n)` — только лидер, батчами, монотонно;
- `SplitRange`, `MergeRange`, `UpdatePlacement`, `Heartbeat`.

TSO:

- лидер выдаёт диапазон `[start, start+n)` и фиксирует upper bound в Raft;
- после смены лидера новый лидер продолжает с зафиксированного значения;
- клиентский TSO-кэш уменьшается, чтобы не «сжигать» диапазоны.

Отказ control plane:

- клиенты и data-узлы живут по последней карте и leases;
- новые split/merge/placement не начинаются, пока нет кворума;
- уже запущенные KV-транзакции не обязаны останавливаться.

### 2.2 KV Data Plane: `RangeHost`

```text
kvserver process
├── gRPC: KVService / RaftService
├── RangeHost
│   ├── RangeGroup 1: raft.Node + MVCC + Pebble(dir 1)
│   ├── RangeGroup 2: raft.Node + MVCC + Pebble(dir 2)
│   └── ...
└── shared cache / metrics / admin
```

- RPC содержит `range_id` (клиент уже знает range из meta). Сервер диспетчеризует
  запрос в нужную группу; при отсутствии группы — `not hosted` + leader hint.
- Raft-транспорт — тот же gRPC-порт, `RaftService` с `range_id` в metadata.
- В M2 — **Pebble на range**: проще snapshot/restore и удаление при merge.
  Позже можно перейти на общий engine с range-префиксом, если файлов станет много.
- У каждой группы свой `epoch`. Запросы несут epoch; старый лидер не может
  записать после split/merge.

### 2.3 Split: реальный перенос данных

```mermaid
sequenceDiagram
    participant A as Admin/Client
    participant M as Meta Leader
    participant R1 as Old Range replicas
    participant R2 as New Range replicas
    A->>M: SplitRange(rangeID, splitKey)
    M->>M: validate; state=SPLITTING; epoch++
    M->>R1: Freeze(rangeID, epoch)
    R1-->>M: fenced at log index I
    M->>R2: CreateRange(newID, [splitKey,end))
    R2->>R1: copy keys >= splitKey at index I
    R1-->>R2: snapshot / iterator chunks
    R2->>R2: apply catch-up, elect leader
    M->>M: commit map: left + right, new epoch
    M-->>A: new range map
    R1->>R1: GC keys >= splitKey after safe point
```

Ключевые решения:

- **MVP: короткий freeze** старого range на время копирования. Это проще
  доказать, чем dual-write; для больших range — копирование из snapshot.
- Копирование идёт через Raft новой группы (или snapshot + InstallSnapshot),
  а не прямая запись в Pebble в обход лога.
- Meta коммитит новую карту только после того, как новый range поднял лидера и
  применил snapshot.
- Если узел упал в середине: meta state `SPLITTING` позволяет повторить
  идемпотентно; осиротевшая группа удаляется GC.
- После split старый range не должен обслуживать ключи справа: epoch +
  проверка границ.

### 2.4 Merge

```mermaid
sequenceDiagram
    participant M as Meta Leader
    participant L as Left Range
    participant R as Right Range
    M->>M: state=MERGING; epoch++
    M->>R: Freeze(epoch)
    R-->>M: fenced
    M->>L: CopyRange(right [start,end))
    L->>L: apply keys via Raft
    M->>M: commit merged descriptor
    M->>R: RemoveRange
    R->>R: stop group, delete dir after safe point
```

- Merge только для соседних range с одинаковым replica set.
- Правый range сначала перестаёт принимать записи; данные переносятся в левый
  через его Raft log, чтобы не потерять ordering/epoch.
- Удаление правой группы — после того, как все клиенты получили новую карту
  (watch/lease), иначе возможны запросы к мёртвому range.

### 2.5 Межrange-транзакции (M3)

Клиент — координатор 2PC:

```text
AllocTS -> startTS
Prewrite keys in range A (primary)
Prewrite keys in range B
AllocTS -> commitTS
Commit primary in A
Commit secondaries in B
```

Отказ:

- клиент/координатор упал → lock resolver на лидере range находит lock;
- `CheckTxnStatus(primary)`:
  - committed → commit все lock;
  - rolled back → rollback;
  - expired → rollback;
  - live → подождать/retry.
- `ResolveLock` идёт через Raft соответствующего range.

Safe point для MVCC GC:

```text
safePoint = min(active startTS, now - maxTxnDuration)
```

### 2.6 M3: GC locks и stress/Jepsen

- **Lock resolver** — фоновая задача лидера range: сканирует lock CF, проверяет
  TTL по TSO, разрешает expired/committed locks, пишет метрики.
- **MVCC GC** — удаляет версии старше safePoint и rollback-записи; никогда не
  трогает версии, видимые активной транзакции.
- **Stress/Jepsen** — рандомные partition/kill/restart/clock skew; проверка
  инвариантов (сумма балансов, отсутствие потерянных/двойных списаний);
  линеаризуемость истории через checker.

### 2.7 M5: SQL

```text
psql/pgwire ──► Parser ──► Binder/Catalog ──► Planner/Optimizer
                                                   │
                                                   ▼
                                              Executor
                                                   │
                                                   ▼
                                                KV SDK
```

- **pgwire**: startup, simple query, extended protocol, prepared statements;
  типы INT/TEXT/NUMERIC/BOOL/TIMESTAMP; `psql`/JDBC.
- **Catalog**: таблицы, колонки, индексы, статистика; версия схемы; schema lease.
- **Planner**: predicate pushdown, index selection, JOIN order, cost model.
- **JOIN**: nested loop / hash join; маленькая таблица — broadcast; позже shuffle.
- **Aggregates**: COUNT/SUM/AVG/MIN/MAX, GROUP BY; partial/final aggregation.
- **Transactions**: один statement — одна KV-транзакция; multi-statement —
  explicit txn; cross-range — 2PC.
- **Distribution**: co-located tables — один range; иначе 2PC; позже distributed
  join/shuffle.

### 2.8 AI inference control plane (отдельный продукт)

Это **не часть** `distributed-db`; другой репозиторий/кластер. Общие идеи, но
другие SLO и data plane.

```text
API → durable queue → scheduler/placement → worker registry/model catalog
                                  │
                                  ▼
workers ── gRPC/TCP activations ──► workers
workers ── weight chunks ◄──────── artifact store
```

- **Control plane (Raft 3/5):** worker inventory (GPU/VRAM), model catalog,
  stage→worker placement, epoch/fencing, job queue/leases, idempotency.
- **Data plane:** прямые gRPC/TCP-стримы активаций; backpressure; никакого Raft
  в пути токена.
- **Artifact plane:** content-addressed чанки весов, pre-stage, warm cache.
- **Failure:** stage dies → запрос падает/retry; scheduler переназначает stage;
  warm spare; KV-cache/активации не сохраняются в Raft.
- **Optional:** KV как durable metadata/queue (не тензоры), но лучше отдельный
  небольшой кластер, чтобы inference-нагрузка не влияла на SQL/KV.

## 3. Потоки запросов

### KV Get

```text
client range cache → range leader
  → ReadBarrier (quorum lease + apply to commit index)
  → MVCC.Get(ts)
  → response
```

### KV transaction

```text
AllocTS(start) → Prewrite(primary range) → Prewrite(other ranges)
→ AllocTS(commit) → Commit(primary) → Commit(others)
→ on failure: CheckTxnStatus/ResolveLock
```

### SQL query

```text
pgwire → parse → bind catalog → logical plan → optimize
→ physical plan → executor → KV txn(s) → rows → pgwire
```

## 4. Этапы и критерии приёмки

### M2

- [x] Meta Raft 3 узла; `AllocTS` монотонен после failover.
- [x] `RangeHost` держит 2+ группы в одном процессе; gRPC `range_id`.
- [x] Split: freeze → copy → new group → commit map; e2e обеих половин, распределённо.
- [x] Потоковый split/merge (батчи, без range в RAM).
- [x] Shared Pebble на узел (range-префиксы).
- [x] Потоковый state-machine snapshot и chunked InstallSnapshot.
- [x] Merge: freeze right → copy → drop group; e2e распределённо.
- [x] Partial-merge retry идемпотентен.
- [ ] Kill meta/range во время split/merge.
- [x] Stale epoch отвергается.
- [ ] Watch/refresh карты (клиент читает meta при операции).

### M3

- [x] Cross-range 2PC primary-first; distributed deadlock/recovery — нет.
- [x] Background lock resolver + MVCC GC safe point.
- [x] Jepsen kill/restart.
- [x] Network partitions, clock skew (client-side); longer soak — нет.
- [x] SLO-метрики KV/SQL/pgwire; lock/GC метрики — нет.

### M5

- [x] pgwire simple query (psql).
- [x] Extended protocol/prepared statements; TLS/auth — нет.
- [x] Planner: range predicates, index selection, cost-based join order, plan cache.
- [x] Hash join + GROUP BY + COUNT/SUM/AVG/MIN/MAX.
- [x] Cross-range SQL txn через 2PC; SQL после split (0023).

### AI

- [ ] Отдельный control plane: registry, placement, queue/lease.
- [ ] 3B–8B pipeline на 2–3 машинах; gRPC активации.
- [ ] Kill stage → retry/reassign; warm spare.
- [ ] 70B — только после стабильного 8B и замеров сети/VRAM.

## 5. Риски и решения

| Риск | Решение |
|---|---|
| Split/merge останавливает range | короткий freeze в MVP; snapshot copy; позже dual-write/catch-up |
| Meta — единая точка для routing | клиентский кэш + lease; data plane живёт без meta |
| TSO после failover | Raft-фиксация upper bound; батчи; не выдавать вне кворума |
| Много Pebble на процесс | лимит групп/файлов; позже общий engine с range-префиксом |
| Cross-range 2PC latency | co-location, batch, parallel prewrite; SQL planner избегает cross-range |
| SQL JOIN по разным range | broadcast/hash join; позже shuffle |
| AI и KV в одном процессе | не смешивать; отдельный кластер/репозиторий |
| Raft в data plane | запрещено; активации/веса — напрямую |

## 6. Что делать первым

1. **M2.1** — Meta Raft с `MetaState` и TSO; без split/merge.
2. **M2.2** — `RangeHost` с 2 группами в одном процессе и `range_id` в RPC.
3. **M2.3** — split MVP с коротким freeze; затем merge.
4. **M3** — cross-range 2PC и lock resolver.
5. **M5** — pgwire + planner + JOIN/aggregates.
6. **AI** — отдельный P0 control plane, не в этом репозитории.

