# Performance: distributed-db vs PostgreSQL

- Дата: 2026-11-17
- Железо: Apple M1 Pro, macOS, loopback.
- PostgreSQL: 13-alpine в Docker, один узел, `synchronous_commit=on`.
- Наш кластер: 1 meta + 3 KV, loopback.
- Профили: `fast` (fsync только Raft-лога) и `sync` (production default,
  fsync state machine).

## 1. Методика

Наш benchmark: `test/speed_bench_test.go`, одна транзакция Begin→Set→Commit
на уникальный ключ; parallel — `b.RunParallel` (10 goroutines).

PostgreSQL: `pgbench -M simple` с custom script:

```sql
\set id random(1, 100000)
UPDATE bench SET v = v + 1 WHERE id = :id;
```

Это одна строка/одна транзакция, ближайший аналог нашего single-key commit.
Это не TPC-C и не YCSB; цифры нужны для порядка величины, не для маркетинга.

## 2. Текущие результаты

### Один in-flight на клиента

| Сценарий | distributed-db | PostgreSQL 13 | Разрыв |
|---|---:|---:|---:|
| 1 клиент, fast | ~11.6 мс / ~86 tx/s | ~0.68 мс / ~1 469 tx/s | ~17× |
| 10 клиентов, fast | ~1.2–2.1 мс/op / ~480–820 tx/s | ~1.47 мс/op / ~6 835 tx/s | ~8–11× |
| 1 клиент, sync | ~22.6 мс / ~44 tx/s | ~0.68 мс / ~1 469 tx/s | ~33× |
| 10 клиентов, sync | ~5.4 мс/op / ~185 tx/s | ~1.47 мс/op / ~6 835 tx/s | ~37× |

`ns/op` в parallel-бенчмарке — это время **одной операции**, а не десяти.
Aggregate throughput = `1e9 / ns/op`, а не `10 * 1e9 / ns/op`.

### Batch/pipeline

`CommitBatch` объединяет независимые транзакции в один Raft-entry;
серверный `maxBatch` = 256, окно 500 µs.

10 параллельных клиентов (эмуляция рабочих станций), loopback:

| Pipeline на клиента | fast, tx/s | sync, tx/s | sync batch latency |
|---:|---:|---:|---:|
| 1 | ~712 | ~200 | ~5 мс |
| 16 | ~4 400 | ~1 470 | ~11 мс |
| 64 | ~11 800 | ~3 980 | ~16 мс |
| 256 | ~19 500 | ~8 570 | ~30 мс |

При pipeline 64–256 aggregate throughput **сопоставим или выше** PostgreSQL 13
(~6.8k tx/s на 10 клиентах с одним in-flight). Но это throughput-режим:
batch latency 16–30 мс. Сравнение с PostgreSQL с таким же pipeline/batch
нужно делать отдельно.

## 3. Что ограничивает скорость

1. **Fsync.** На macOS Go делает `F_FULLFSYNC` (~4 мс). Один commit fast
   платит за Raft-лог; sync добавляет fsync state machine, но теперь один
   Pebble batch/fsync на весь Raft-батч, а не на команду.
2. **Raft-консенсус.** Один commit = RTT до кворума + fsync. PostgreSQL на
   одном узле не платит за консенсус.
3. **Один диапазон/один лидер.** Все текущие бенчмарки пишут в один range;
   несколько range-лидеров могут писать параллельно, но это ещё не измерено.
4. **Apply/encode.** Батч уменьшает накладные, но JSON-encoding и
   последовательный apply остаются.
5. **Latency одного клиента.** Один in-flight не может дать 6–8k tx/s:
   при 12–25 мс на commit потолок ~40–830 tx/s. Нужен pipeline.

## 4. Где можно выиграть

- **Multi-range fan-out** — главный архитектурный резерв; нужен стенд
  3 узла × 4–8 range.
- **Parallel apply** по range’ам.
- **Pipelined AppendEntries** и больше in-flight entry.
- **Linux/NVMe** — на macOS fsync дороже; цифры нельзя переносить на Linux.
- **Adaptive batch** — окно/размер под нагрузку.
- **Async commit/local quorum** — только как opt-in с RPO>0, не default.

## 5. Ограничения методики

- loopback, а не реальная сеть; нет packet loss/partitions;
- маленький набор ключей, без compaction на ТБ;
- нет p99 для fast/batch, только средние ns/op;
- PostgreSQL — один узел, без Patroni/Citus;
- batch/pipeline throughput — не latency одного клиента;
- 1 ТБ harness есть, но 1 ТБ не прогнан.

## 6. Вывод

- **Fast, один in-flight:** ~8–11× медленнее PostgreSQL на 10 клиентах,
  ~17× на одном. Это цена Raft-консенсуса и fsync.
- **Sync, один in-flight:** ~37× медленнее; production durability стоит
  ещё одного fsync.
- **Pipeline 64–256:** aggregate throughput сопоставим/выше PostgreSQL,
  но batch latency 16–30 мс.
- Правильная цель — не «быстрее PostgreSQL на одном узле», а
  **«лучше PostgreSQL+Patroni/Citus на шардированной записи»**.
