# Performance: distributed-db vs PostgreSQL

- Дата: 2026-11-16
- HEAD: `8c173bf`
- Железо: Apple M1 Pro, macOS, loopback.
- PostgreSQL: 13-alpine в Docker, один узел, `synchronous_commit=on`.
- Наш кластер: 1 meta + 3 KV, loopback, **fast profile**
  (`DurabilityMode=fast`, `SyncWrites=false`, durability — Raft-лог).
  Production default — `sync` (`DurabilityMode=sync`), он медленнее; цифры
  ниже не являются production-обещанием.

## Методика

Наш benchmark: `test/speed_bench_test.go`, одна транзакция Begin→Set→Commit
на уникальный ключ; parallel — `b.RunParallel` (10 goroutines).

PostgreSQL: `pgbench -M simple` с custom script:

```sql
\set id random(1, 100000)
UPDATE bench SET v = v + 1 WHERE id = :id;
```

Это одна строка/одна транзакция, ближайший аналог нашего single-key commit.
Это не TPC-C и не YCSB; цифры нужны для порядка величины, не для маркетинга.

## Результаты

Историческая таблица (до отказа от второго fsync и до batch):

| Сценарий | distributed-db | PostgreSQL 13 | Разрыв |
|---|---:|---:|---:|
| 1 клиент, latency | ~23 мс | ~0.68 мс | ~34× медленнее |
| 1 клиент, throughput | ~43 tx/s | ~1 469 tx/s | ~34× |
| 10 клиентов, latency/op | ~16 мс | ~1.47 мс | ~11× медленнее |
| 10 клиентов, throughput | ~625 tx/s | ~6 835 tx/s | ~11× |

Актуальные замеры — ниже.

До оптимизаций было: 3-node sequential ~33 мс, parallel ~24 мс/op (~420 tx/s).
После atomic 1PC + parallel cross-range: sequential ~23 мс, parallel ~16 мс/op
(~625 tx/s). После отказа от отдельного fsync state machine (durability —
Raft-лог): **sequential ~11.6 мс, parallel ~1.2–2.1 мс/op (~480–820 tx/s)**.

Итоговая таблица (5s benchmark, 2 прогона):

| Сценарий | distributed-db | PostgreSQL 13 | Разрыв |
|---|---:|---:|---:|
| 1 клиент (fast) | ~11.6 мс / ~86 tx/s | ~0.68 мс / ~1 469 tx/s | ~17× |
| 10 клиентов (fast) | ~1.2–2.1 мс/op / ~480–820 tx/s | ~1.47 мс/op / ~6 835 tx/s | ~8–11× |
| batch 256 (fast) | ~0.14–0.20 мс/txn / ~5–7k tx/s; batch latency 36–51 мс | ~1.47 мс/op / ~6 835 tx/s | ~1.0× по throughput |
| production sync, 1 клиент | ~25 мс / ~40 tx/s | ~0.68 мс / ~1 469 tx/s | ~37× |
| production sync, 10 клиентов | p50 ~151 мс / ~70 tx/s | ~1.47 мс/op / ~6 835 tx/s | ~98× |

Batch-режим (`CommitBatch`, 256 транзакций в одном batch, серверный
batch-entry 64 × 500 µs, 256 in-flight) достигает ~5–7k tx/s — на уровне
PostgreSQL 13 на этом же железе, но это **амортизированная** величина:
латентность всего batch 36–51 мс, а не 0.15 мс на пользователя. Это **не**
atomic batch: каждая транзакция имеет свой commit point. Размер/окно —
компромисс latency/throughput и настраиваются в `internal/kvserver/batch.go`.

На одном клиенте всё ещё платим за Raft-консенсус и один fsync. Цифра
~11.6 мс — fast-профиль без batch; `CommitBatch`/batch-window добавляет
~0.5–1 мс одиночному запросу. **Production default `sync`** добавляет fsync
на state machine: ~25 мс на клиента и ~70 tx/s на 10 клиентах — это цена
durability, а не регрессия.

## Почему мы медленнее

1. **Raft-консенсус**: минимум один RTT до кворума + fsync Raft-лога.
   PostgreSQL на одном узле не платит за консенсус.
2. **Один fsync на транзакцию** после отказа от Pebble SyncWrites; durability
   state machine обеспечивается реплеем Raft-лога. Snapshot fsync'ится.
   Вернуть отдельный fsync можно через `kvserver.Config.SyncStateMachine=true`
   (медленнее, но state machine durable независимо от лога).
3. **gRPC/RPC overhead**: TSO (батчится), CommitSingle, range lookup.
4. **Apply loop / group commit window**: 1 мс batching window добавляет
   latency одиночному клиенту.
5. **SQL executor** последовательный по range’ам, без parallel/distributed
   join.
6. **PostgreSQL** имеет buffer cache, зрелый WAL, planner, async I/O и
  十年的 оптимизаций.

## Что это значит

На **single-node OLTP** мы не обгоним PostgreSQL в обозримом будущем. Это
другая архитектура: мы платим за консенсус и распределённость на каждом
commit. Сравнение честно только в классе «распределённая БД с 3 репликами»,
и там PostgreSQL+Patroni/Citus — зрелый конкурент, а не «просто Postgres».

## Где можно выиграть

- **Горизонтальная запись по range’ам**: разные range-лидеры могут писать
  параллельно; PostgreSQL single-primary упирается в один узел.
- **Автоматический split/merge и rebalance** без внешнего Citus.
- **Cross-range 2PC** встроен, а не через координатор.
- **Raft-native failover** без Patroni.

Но чтобы это стало измеримым преимуществом, нужно:

1. Pipeline/батчинг AppendEntries и apply, а не один commit за RTT.
2. Один durable log или async fsync для state machine (риск durability).
3. Параллельный commit по range’ам (сделано для cross-range secondary).
4. Снизить TSO/meta-задержки: lease, локальные батчи, fewer RPC.
5. Parallel SQL execution и distributed join.
6. Нагрузочный стенд 3/5/7 узлов, YCSB/TPC-C, 1 ТБ soak.

## Ограничения методики

- loopback, а не реальная сеть; нет packet loss/partitions;
- маленький набор ключей, без compaction на ТБ;
- нет p99, только средние ns/op;
- PostgreSQL запущен как один узел, без Patroni/Citus;
- batch throughput — не latency одного клиента;
- production default `sync` делает лишний fsync на state machine; fast —
  opt-in и требует crash-тестов (они есть в `test/crash_consistency_test.go`).

## Вывод

**На 10 клиентах fast-профиль всё ещё в ~8–11× медленнее PostgreSQL**, а
production `sync` — примерно в ~100×. На одном клиенте fast ~17×, sync ~37×.
Это ожидаемо: один Raft-commit платит за консенсус и fsync, PostgreSQL — нет.

Batch-режим достигает ~5–7k tx/s (на уровне PostgreSQL) только как
амортизированный throughput: batch из 256 транзакций занимает 36–51 мс.
Interactive latency и production `sync` не догнали PostgreSQL. Правильная
цель — не «быстрее PostgreSQL на одном узле», а **«лучше
PostgreSQL+Patroni/Citus на шардированной записи и автоматическом
размещении»**.
