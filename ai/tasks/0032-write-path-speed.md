---
task_id: "0032"
type: feature
title: Stage 4 — write path speed
created_at: 2026-11-16T16:00:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Stage 4 — write path speed

## Цель

Сократить latency транзакции и поднять throughput, чтобы честно сравниваться
с PostgreSQL на согласованном workload, а не «на глаз».

## Срез 1

- MVCC `CommitSingle`: prewrite+commit в одном Pebble batch (один fsync).
- Cross-range commit: параллельные prewrite и secondary commit после primary.
- Benchmark до/после: 1-node/3-node txn, parallel, cross-range.
- Сравнение с PostgreSQL — отдельный стенд, не обещание.

## Критерии

- [x] 1PC один batch/fsync.
- [x] Параллельный cross-range commit.
- [x] Benchmark до/после.
- [x] Все тесты зелёные.

## Результат

- 3-node sequential: ~33 мс → ~11.6 мс.
- 3-node parallel: ~24 мс/op → ~1.2–2.1 мс/op (~480–820 tx/s; старая запись 5.5k была ошибкой единиц).
- PostgreSQL 13 на том же железе: ~0.68 мс/1 469 tps (1 клиент),
  ~1.47 мс/6 835 tps (10 клиентов).
- Разрыв: ~1.2× на 10 клиентах, ~17× на одном.
