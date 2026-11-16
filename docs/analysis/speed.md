# Скорость: текущее состояние

- Дата: 2026-11-17
- Железо: Apple M1 Pro (10 CPU), macOS, loopback
- Профили: fast (fsync Raft-лога) и sync (production default)

## 1. Текущие замеры

| Режим | Latency | Throughput |
|---|---:|---:|
| fast, 1 клиент | ~11.6 мс | ~86 tx/s |
| fast, 10 клиентов, 1 in-flight | ~1.2–2.1 мс/op | ~480–820 tx/s |
| fast, batch/pipeline 256 | ~138 µs/txn (batch ~35 мс) | ~7.2k tx/s |
| sync, 1 клиент | ~22.6 мс | ~44 tx/s |
| sync, 10 клиентов, 1 in-flight | ~5.4 мс/op | ~185 tx/s |
| sync, pipeline 256 | ~117 µs/txn (batch ~30 мс) | ~8.6k tx/s |

10 рабочих станций (эмуляция), pipeline на клиента:

| Pipeline | fast | sync | sync batch latency |
|---:|---:|---:|---:|
| 1 | ~712 tx/s | ~200 tx/s | ~5 мс |
| 16 | ~4.4k tx/s | ~1.47k tx/s | ~11 мс |
| 64 | ~11.8k tx/s | ~3.98k tx/s | ~16 мс |
| 256 | ~19.5k tx/s | ~8.57k tx/s | ~30 мс |

PostgreSQL 13 на том же железе: ~0.68 мс / 1 469 tx/s (1 клиент),
~1.47 мс / 6 835 tx/s (10 клиентов, один in-flight).

## 2. Бюджет commit

| Компонент | Значение |
|---|---:|
| `F_FULLFSYNC` на macOS | p50 ~4 мс, p95 ~7.9 мс |
| Raft group commit (10 конкурентов) | ~505 µs/op |
| Fast commit, 3 узла, 1 клиент | ~11.6 мс |
| Sync commit, 3 узла, 1 клиент | ~22.6 мс |

Fast платит за Raft-лог; sync добавляет fsync state machine, но один
Pebble batch/fsync на весь Raft-батч, не на команду.

## 3. Где предел

- **Один in-flight** упирается в latency 12–25 мс: 10 клиентов дают
  ~480–820 tx/s fast и ~185 tx/s sync. Батчинг не уменьшает latency
  одного commit, он увеличивает aggregate throughput.
- **Pipeline 64–256** даёт 4–8.6k tx/s sync, но batch latency 16–30 мс.
- **Один range** — один Raft-лидер. Несколько range-лидеров могут писать
  параллельно; это следующий архитектурный резерв.
- **macOS fsync** дороже Linux/NVMe; цифры нельзя переносить без прогона.

## 4. Приоритеты

1. Multi-range benchmark и parallel apply по range’ам.
2. Pipelined AppendEntries/больше in-flight entry.
3. Linux/NVMe замер.
4. Adaptive batch.
5. Async/local quorum — только opt-in с RPO>0.

## 5. Ограничения

- loopback, не реальная сеть;
- нет p99 для fast/batch;
- 1 ТБ не прогнан;
- PostgreSQL — один узел, без Patroni/Citus;
- batch throughput — не latency одного клиента.
