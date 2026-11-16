---
task_id: "0044"
type: perf
title: Один Pebble batch/fsync на Raft-батч
created_at: 2026-11-17T01:55:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Sync batching

## Проблема

В production `sync` state machine fsync'ила каждую команду внутри одного
Raft-батча: 64 команды = 64 fsync. Sync 10 клиентов: ~151 мс p50, ~70 tx/s.

## Сделано

- [x] `mvcc.CommitSingles`: планирование N 1PC, один Pebble batch/Commit.
- [x] `CommandTxnBatch` использует CommitSingles.
- [x] per-command конфликты индивидуальны; identical retries разрешены.
- [x] `BenchmarkV2Txn3NodeSync*`.
- [x] тест per-command outcomes.

## Результат

- sync 1 клиент: ~22.6 мс / ~44 tx/s;
- sync 10 клиентов: ~5.4 мс/op / ~185 tx/s;
- быстрый профиль не затронут.
