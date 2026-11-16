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

- [ ] 1PC один batch/fsync.
- [ ] Параллельный cross-range commit.
- [ ] Benchmark до/после.
- [ ] Все тесты зелёные.
