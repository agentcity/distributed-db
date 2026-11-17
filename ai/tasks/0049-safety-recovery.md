---
task_id: "0049"
type: sprint
title: Safety/recovery wave — 8 срезов
created_at: 2026-11-17T23:45:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Safety/recovery wave

1. Joint consensus core (Raft).
2. Атомарное завершение split/merge.
3. 1PC apply barrier / commit lease.
4. Remote lock-scan RPC.
5. PITR archive MVP.
6. KV admin auth.
7. Decommission orchestration.
8. Release image + chart bootstrap.

## Результат

- [x] Joint consensus C_old,new + auto-finalize + snapshot.
- [x] Split/merge replicated finish/clear + crash recovery.
- [x] MVCC commit-lease/apply barrier.
- [x] ScanForBackup RPC + strict RemoteScanner.
- [x] PITR archive MVP.
- [x] KVService admin token.
- [x] Decommission orchestration.
- [x] Multi-binary image + chart bootstrap.
- [x] `go build/vet/gofmt`, `go test -race ./...`.

## Осталось

- decommission RPC/CLI + 1000 random changes;
- meta↔ranges.json atomicity;
- PITR integration + 1PC barrier wiring + restore drill;
- metaserver admin fan-out token, RBAC/audit/at-rest;
- 1 ТБ multi-host.
