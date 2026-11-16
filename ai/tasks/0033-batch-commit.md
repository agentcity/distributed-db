---
task_id: "0033"
type: feature
title: Stage 4 — batch entry и pipelining
created_at: 2026-11-16T20:15:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Stage 4 — batch entry и pipelining

## Цель

Амортизировать Raft-RTT/fsync: несколько независимых транзакций в одном
log entry, плюс клиентский pipeline. Гарантии strong mode сохраняются.

## Срез

- model.CommandTxnBatch: []CommitSingle в одном Raft-entry.
- kvserver batcher: окно/размер, per-command results, отмена/stop.
- v2client.CommitBatch: несколько Txn одновременно.
- Benchmarks: throughput batch vs single.
- Тесты: частичные ошибки, конкуренция, idempotency.

## Критерии

- [x] Один Raft-entry на пачку.
- [x] Индивидуальные результаты.
- [x] Batch API клиента.
- [x] Benchmark до/после.
- [x] Все тесты зелёные.

## Результат

- batch 256: ~0.15 мс/txn, ~6.6–6.9k tx/s;
- PostgreSQL 13: ~6.8k tx/s (10 клиентов);
- single txn: ~14 мс (batch window), всё ещё ~20× PostgreSQL;
- batch не атомарен: каждая txn имеет свой commit point.
