# Performance: distributed-db vs PostgreSQL

- Дата: 2026-11-16
- HEAD: `8c173bf`
- Железо: Apple M1 Pro, macOS, loopback.
- PostgreSQL: 13-alpine в Docker, один узел, `synchronous_commit=on`.
- Наш кластер: 1 meta + 3 KV, loopback, `SyncWrites=true`.

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

| Сценарий | distributed-db | PostgreSQL 13 | Разрыв |
|---|---:|---:|---:|
| 1 клиент, latency | ~23 мс | ~0.68 мс | ~34× медленнее |
| 1 клиент, throughput | ~43 tx/s | ~1 469 tx/s | ~34× |
| 10 клиентов, latency/op | ~16 мс | ~1.47 мс | ~11× медленнее |
| 10 клиентов, throughput | ~625 tx/s | ~6 835 tx/s | ~11× |

До оптимизаций было: 3-node sequential ~33 мс, parallel ~24 мс/op (~420 tx/s).
После atomic 1PC + parallel cross-range: sequential ~23 мс, parallel ~16 мс/op
(~625 tx/s). То есть **~30% лучше по latency и ~1.5× по throughput**, но
PostgreSQL всё ещё быстрее.

## Почему мы медленнее

1. **Raft-консенсус**: минимум один RTT до кворума + fsync Raft-лога +
   fsync state machine. PostgreSQL на одном узле не платит за консенсус.
2. **Два fsync на транзакцию**: Raft log и Pebble state machine. Atomic 1PC
   убрал второй fsync state machine, но не fsync Raft-лога.
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

## Вывод

**Мы не обогнали PostgreSQL по скорости.** Мы сократили разрыв с ~2× до
~11× на 10 клиентах, но на одном клиенте он всё ещё ~34×.

Правильная цель — не «быстрее PostgreSQL на одном узле», а
**«лучше PostgreSQL+Patroni/Citus на шардированной записи и автоматическом
размещении»**. Это отдельная программа оптимизаций и бенчмарков, а не один
инкремент.
