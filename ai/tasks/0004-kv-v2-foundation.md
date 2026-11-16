---
task_id: "0004"
type: feature
title: KV v2 — storage, MVCC, Multi-Raft, metadata, client, benchmarks
created_at: 2026-11-16T01:30:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# KV v2 — фундамент распределённой транзакционной базы

## Проблема

v1 — учебный CP-KV: map в RAM, один Raft-груп на процесс, статическая карта
шардов, лидерские чтения, двойной fsync, нет snapshot/компакции Raft-лога.
Для ТБ данных и 10k+ ops/s нужна другая база: LSM, MVCC, Multi-Raft,
range-шардирование, metadata/TSO.

## Требования

- `internal/storage`: Pebble LSM, batch/iterator/snapshot, compression, stats.
- `internal/mvcc`: версии по commitTS, locks, prewrite/commit/rollback, scan, GC.
- `internal/raft`: snapshot, `InstallSnapshot`, усечение лога, catch-up реплик.
- `internal/multiraft`: много Raft-групп на процесс, routing по range ID.
- `internal/meta`: range map, split/merge, placement, TSO, heartbeats.
- `internal/client`: smart-клиент с кэшем лидера, retry/backoff, timeout.
- `bench/`: workload generator (uniform/zipfian/sequential), latency p50/p99.
- Контракты: `docs/CONTRACTS-V2.md`; план: `docs/ROADMAP.md`.

## Критерии приёмки

- [ ] Каждый пакет собирается и проходит `go test -race`.
- [ ] Storage переживает reopen, batch атомарен, snapshot изолирован.
- [ ] MVCC даёт snapshot isolation, конфликты и rollback корректны.
- [ ] Raft-снапшот позволяет отстающей реплике догнать лидера.
- [ ] Multi-Raft маршрутизирует Propose по group ID.
- [ ] TSO монотонен под конкурентностью; split/merge range корректны.
- [ ] Клиент ретраит not-leader и кэширует лидера.
- [ ] Bench показывает throughput и p50/p95/p99.
- [ ] v1-кластер и его тесты остаются зелёными.

## Ссылки

- `docs/ROADMAP.md`
- `docs/CONTRACTS-V2.md`
