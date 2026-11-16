---
task_id: "0006"
type: feature
title: M1 — single-range KV server и end-to-end
created_at: 2026-11-16T02:00:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# M1 — single-range KV server и end-to-end

## Проблема

Пакеты v2 (storage, MVCC, Raft snapshots, Multi-Raft, meta, client, bench)
готовы по отдельности, но ещё нет запускаемого KV-сервера: нельзя взять
кластер, сделать транзакцию через сетевой API и измерить это.

## Требования

- gRPC `KVService` поверх MVCC+storage: Get/Scan/Prewrite/Commit/Rollback,
  TSO из meta (пока in-memory), статус.
- Один Raft-group на range; `internal/multiraft` + `raft.Snapshot` адаптер.
- Node-адаптер v2 (отдельно от v1 `internal/node`), конфиг, graceful stop.
- `internal/client` adapter к gRPC (Router/Transport) + транзакционный API.
- Интеграционный тест: 3 узла, 1 range, PUT/GET/Scan, prewrite/commit,
  kill лидера, snapshot catch-up, reopen.
- Benchmark через `bench` на реальном сервере (не MemEngine).

## Критерии приёмки

- [ ] `go test -race ./...` зелёный, v1-кластер не сломан.
- [ ] Транзакция через клиент видна на всех репликах после commit.
- [ ] Kill лидера не теряет подтверждённые данные.
- [ ] Отставшая реплика догоняет через InstallSnapshot.
- [ ] Benchmark показывает throughput/p99 на реальном gRPC, не на map.

## Ссылки

- `docs/ROADMAP.md` (M1/M2)
- `docs/CONTRACTS-V2.md`
- ADR 0002, 0003
