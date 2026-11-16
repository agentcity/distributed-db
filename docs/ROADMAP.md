# Roadmap: KV first, SQL second

Цель — два продукта в одном репозитории:

1. **kvdb** — распределённая транзакционная key/value база: ordered keys,
   MVCC, range-шардирование, Multi-Raft, metadata/placement driver, smart client.
2. **sqldb** — SQL-слой поверх kvdb: каталог, схема, secondary indexes,
   planner/optimizer/executor, транзакции.

Текущий код (`cmd/db`, `internal/raft`, `internal/kv`, `internal/node`,
`internal/coordinator`) — рабочий **учебный CP-KV** и одновременно референс
Raft/тестов. Он остаётся зелёным, пока v2 строится рядом; затем заменяется
по слоям.

## Продукт 1: kvdb

### Целевые SLO (v1)

| Параметр | Цель |
|---|---|
| Объём | 1–10 ТБ на кластер |
| Репликация | 3 реплики, 3 AZ |
| Запись | 10k–30k ops/s, p99 < 20 мс в регионе |
| Чтение | 50k–100k ops/s, p99 < 10 мс |
| Консистентность | linearizable; snapshot isolation для транзакций |
| Доступность | переживает потерю 1 узла и 1 AZ |
| RPO / RTO | 0 / < 5 минут |
| Восстановление | рестарт узла < 30 с на 100 ГБ данных |

Это цели, а не обещание: цифры уточняются после M0-бенчмарков на реальном
железе.

### Архитектура

```
client SDK (range cache, retries, TSO, 2PC)
        │
        ▼
meta service (PD-like): range map, placement, TSO, heartbeats
        │  (Raft-реплицируемый control plane)
        ▼
node: Multi-Raft host + MVCC + LSM (Pebble)
  ├─ range [a,c): Raft group + MVCC state machine
  ├─ range [c,f): Raft group + MVCC state machine
  └─ ...
        │
        ▼
raft log (WAL) + snapshots; state machine data in Pebble
```

Ключевые решения:

- **Range-based sharding**, не hash: ключи упорядочены, range можно split/merge,
  сканы и локальность работают естественно.
- **Multi-Raft**: один узел держит много range-групп; каждая группа — свой
  Raft-лог, лидер, snapshot.
- **MVCC + Percolator-style 2PC**: данные версионируются commit timestamp;
  транзакции переживают падение координатора и узлов.
- **TSO в meta service**: монотонные timestamp, батчатся; клиент получает
  `start_ts`/`commit_ts`.
- **LSM (Pebble)**: memtable/SSTable, block cache, bloom, compression,
  compaction; в памяти только кэш, не вся база.
- **Один durable log на range**: Raft-лог — источник истины; state machine
  восстанавливается из снапшота + лога, без второго WAL на каждую запись.
- **Read path**: ReadIndex/lease на лидере; follower reads — после M3.
- **Smart client**: кэш `key → range → leader`, retry, backoff, идемпотентность.

### Не-цели v1

- мультирегион active-active;
- OLAP/аналитика;
- полная совместимость с PostgreSQL;
- вторичные индексы и JOIN до SQL-этапа;
- автоматический шардинг по произвольным политикам (начнём с размера/нагрузки).

## Продукт 2: sqldb

SQL-слой строится **только** на транзакционном KV API:

- каталог и схема (database/table/column/index) в системных ключах;
- row encoding: `table_id + primary_key` → columns; secondary index:
  `index_id + index_key + primary_key`;
- parser → AST → logical plan → physical plan → distributed executor;
- secondary indexes, point/range scans, joins, aggregates, ORDER BY/LIMIT;
- транзакции через KV 2PC; изоляция snapshot isolation;
- PostgreSQL wire protocol — опционально, после базового SQL.

SQL не начинается, пока KV не даёт стабильные транзакции, TSO и range scans.

## Этапы

### M0 — решения и измерения (1–2 недели)

- SLO, workload profile, benchmark harness (YCSB-like).
- ADR: LSM (Pebble), MVCC/Percolator, Multi-Raft, range metadata, TSO.
- Контракты пакетов и границы ответственности.
- Baseline текущего кода, чтобы видеть регрессии.

**Выход:** `docs/ROADMAP.md`, `docs/CONTRACTS-V2.md`, ADR, `bench/`,
пустые/скелетные пакеты с тестами.

### M1 — storage + MVCC + single-range (4–6 недель)

- Pebble storage engine: batch, iterator, snapshot, stats, compression.
- MVCC: версии, locks, prewrite/commit/rollback, scan, GC.
- Key encoding: ordered user key + timestamp; range boundaries.
- Single-range KV API (gRPC) на одном узле.
- Бенчмарки: 100 ГБ, 10k+ ops/s, p99.

**Выход:** можно записать/прочитать/просканировать данные с MVCC и
транзакциями на одном узле; данные переживают рестарт; старый кластер не сломан.

### M2 — Multi-Raft + ranges + metadata (6–8 недель)

- Host многих Raft-групп на узле; routing по range ID.
- Raft snapshot/InstallSnapshot/truncation; быстрый рестарт.
- meta service: range map, split/merge, placement, heartbeats, TSO.
- 3 узла, 2+ range, failover range-лидера, split по размеру.

**Выход:** кластер 3×(2 range), ключи распределены, range переживает падение
лидера, snapshot не даёт логу расти бесконечно.

### M3 — транзакции и client SDK (4–6 недель)

- Percolator 2PC end-to-end: prewrite/commit/rollback, lock resolution, GC.
- TSO client, range cache, retries, idempotency.
- Snapshot isolation tests, conflict tests, Jepsen-style linearizability.
- Follower reads / ReadIndex (если успеваем).

**Выход:** клиент делает транзакции через кластер; kill -9 узлов не теряет
подтверждённые данные.

### M4 — эксплуатация (4 недели)

- Backup/PITR в S3, restore, snapshots.
- mTLS, RBAC, encryption at rest, audit.
- Multi-AZ, rolling upgrade, Kubernetes operator/manifests.
- Metrics/tracing/alerts, capacity planning, quotas, admission control.
- Chaos/fault injection, performance regression CI.

**Выход:** можно эксплуатировать кластер без ручных операций.

### M5 — SQL (3–6 месяцев)

- Каталог/схема, row/index encoding.
- Parser/planner/executor; point/range scans; secondary indexes.
- JOIN/aggregate/order/limit; transactions; PostgreSQL wire (опционально).
- SQL benchmarks и совместимость.

**Выход:** `CREATE TABLE`, `INSERT`, `SELECT`, `UPDATE`, `DELETE`, индексы,
транзакции; SQL-слой не знает о Raft, только о KV API.

## Команда и оценки

| Роль | Зона |
|---|---|
| Storage/MVCC | Pebble, версии, locks, GC |
| Raft/Multi-Raft | snapshots, группы, routing |
| Metadata/TSO | range map, placement, timestamps |
| Client/API | SDK, retry, идемпотентность |
| SRE/Perf | benchmarks, chaos, deploy |

Грубо: **6–12 месяцев** до production-grade KV малой командой (3–5 человек с
опытом распределённых систем); SQL — ещё **3–6 месяцев** для базового слоя и
**1–2 года** до зрелого продукта. Это оценка, а не гарантия.

## Риски

- **Raft snapshot/InstallSnapshot** — самая частая причина «залипания» lagging
  реплик; делать до Multi-Raft.
- **Двойной fsync** (Raft log + state machine) убивает latency; один durable
  log и batch/group commit обязательны.
- **Percolator GC и lock resolution** — источник вечных локов; нужны
  TTL, cleanup, тесты на падения в середине 2PC.
- **Range split/merge** — гонки с лидерством; нужна версия range и
  metadata-транзакция.
- **SQL planner** может оказаться больше, чем весь KV; не начинать до M3.
- **Build vs buy**: если цель — продукт, TiKV/TiDB/CockroachDB дешевле; если
  цель — своё ядро/обучение, план выше.

## Метрики прогресса

- `go test -race ./...` зелёный на каждом этапе.
- Benchmark: throughput, p50/p95/p99, ошибки, объём, время рестарта.
- Chaos: kill -9 лидера/фолловера, партиция, медленный диск.
- Correctness: linearizability checker, snapshot isolation tests, Jepsen.
- Recovery: snapshot + replay, RTO, целостность после сбоя.

## Статус на 2026-11-16

Сделано (M0–M2, часть M3/M5):

- storage на Pebble, MVCC/2PC, Raft snapshots/InstallSnapshot, Multi-Raft host,
  meta/range/TSO, smart client, benchmark harness;
- single-range v2: gRPC KVService/RaftService, `kv2 node|meta`, клиентский
  транзакционный SDK;
- e2e 3 узла: prewrite/commit/get/scan, kill лидера, новая транзакция и
  клиентский commit после failover;
- InstallSnapshot по gRPC, state-machine snapshot MVCC и restart из snapshot;
- e2e catch-up отставшей реплики через InstallSnapshot;
- adapter `multiraft` → `raft.Node`;
- benchmark v2 на реальном gRPC; Stage 1: group commit, TSO batch,
  range cache, 1PC; 3 узла ~40 мс/транзакция sequential, ~350 tx/s parallel,
  Get ~0.15–1 мс.

M1/0006 закрыт. В M2 сделано: `meta.SetRanges`, `GetRange/ListRanges`,
range identity в Status, клиентский routing по range ID и e2e
2 диапазона × 3 реплики с failover. M2.1/0011: meta Raft 3 узла,
replicated TSO, `Leader`, клиентский redirect и failover.

В M2.2/0012 сделан RangeHost (N Raft-групп в процессе, range_id RPC) и
host-local split/merge с переносом MVCC-записей. В M2.4/0013 — распределённый
split: prepare через Raft, freeze всех реплик, новая группа на каждой,
meta SetRanges после успеха. Распределённый merge тоже сделан. Epoch/fencing,
meta scheduler Split/Merge, retry fan-out и partial-merge idempotency готовы;
range map persist. Остались durable scheduler queue/leader lease.
Stage 1 speed закрыт: group commit, TSO batch, range cache, 1PC.
Stage 2.1: потоковый `StreamRange`/split/merge и `AutoSplit` по размеру.
Stage 2.2a: shared Pebble на узел. Stage 2.2b: потоковый state-machine
snapshot и chunked InstallSnapshot. Stage 3.1: cross-range 2PC primary-first. Stage 3.2: SQL после split,
planner/plan cache, extended pgwire, recovery, Jepsen partitions.
Stage 2.3: auto-merge, backup/restore, gzip snapshot. Leader transfer/rebalance, snapshot resume и SQL/pgwire-метрики сделаны.
Остались Add/Remove replica + joint consensus, per-chunk CRC, SLO p99.

В M3/0008 сделано: `CheckTxnStatus`, `ResolveLock`, expired-lock rollback
по TSO/TTL, RPC и авто-разрешение в `Get`; e2e commit/rollback paths.
Клиентские методы, resolver, GC safe point и opt-in Jepsen kill/restart
добавлены. Осталось: network partitions, clock skew, longer soak.

В M5/0009 сделан SQL P0: parser, каталог, row encoding, executor на KV,
HTTP `POST /v1/sql`, secondary indexes (`CREATE INDEX`, WHERE по indexed-
колонке), multi-row UPDATE/DELETE по индексу, агрегаты COUNT/SUM/AVG/
MIN/MAX; e2e CREATE/INSERT/SELECT/UPDATE/DELETE прошёл.
GROUP BY, INNER JOIN и минимальный pgwire сделаны. Осталось: planner
(JOIN order/cost), extended protocol, LEFT/RIGHT JOIN. SQL на 3 узлах
с kill лидера проверен.

## Целевая архитектура

См. [ARCHITECTURE-TARGET.md](ARCHITECTURE-TARGET.md) и
[схему](diagrams/target-architecture.png): M2/M3/M5 и отдельный AI control plane.
