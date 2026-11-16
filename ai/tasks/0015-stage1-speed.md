---
task_id: "0015"
type: feature
title: Stage 1 — group commit, TSO batch, 1PC и range cache
created_at: 2026-11-16T13:00:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 1 — ускорение транзакционного пути

## Проблема

Транзакция стоила 4 RPC (TSO→Prewrite→TSO→Commit), каждый Raft-коммит
делал отдельный fsync, клиент ходил в meta на каждый retry, а одна
транзакция в range тратила два Raft-round.

## Решение

- **Raft group commit**: очередь предложений, один `AppendBatch`/fsync на
  пачку, окно 1 мс, max 64; fallback на per-entry store.
- **TSO batch**: клиент резервирует 128 timestamp’ов одним `AllocTS`,
  тратит локально; проверяет meta leader по TTL/generation.
- **Range cache**: TTL 1 с, до 4096 range’ей, инвалидация по stale epoch/
  meta failover, meta connection pool.
- **1PC**: `KVService.CommitSingle` — prewrite+commit в одном Raft-entry;
  клиент использует для single-range транзакций.
- **Benchmarks**: 3-узловой sequential/parallel/get/scan + smoke.

## Критерии приёмки

- [x] Group commit с одним WAL/fsync на батч, fallback и тесты.
- [x] TSO batch + range cache + meta pool с тестами.
- [x] CommitSingle (1PC) на сервере и в клиенте.
- [x] 3-узловой benchmark harness и smoke.
- [x] Все тесты зелёные.
- [ ] SQL CBO/parallel scan.
- [ ] Streaming split/snapshot (Stage 2).

## Результаты (M1 Pro, loopback)

- 3-node sequential: ~40 мс/транзакция (было ~47).
- 3-node parallel: ~28 мс/op, ~350 tx/s агрегатно.
- Get: ~0.15–1 мс.
- Raft group commit: ~7.8× throughput на конкурентных proposal’ах
  (single-node benchmark 535 µs vs 4.15 мс/op).

## Ссылки

- `docs/ROADMAP.md`
- `internal/raft/batch_bench_test.go`
- `test/speed_bench_test.go`
