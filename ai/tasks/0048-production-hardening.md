---
task_id: "0048"
type: sprint
title: Production hardening — 8 параллельных срезов
created_at: 2026-11-17T21:30:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Production hardening

1. Raft config в snapshot.
2. Split/merge freeze safety.
3. Storage lifecycle: periodic snapshot + MVCC GC.
4. Consistent backup / lock barrier.
5. SQL HTTP TLS/auth.
6. Admin-token для meta admin RPC.
7. Multi-host SLO/1 ТБ harness.
8. Helm/operator + runbooks/alerts.

## Результат

- [x] Raft config snapshot + proto/gRPC forwarding.
- [x] Split/merge freeze safety (partial atomicity; gaps listed).
- [x] Periodic snapshot + MVCC GC + ENOSPC guard.
- [x] Lock-aware backup (in-process); remote RPC gap.
- [x] SQL HTTP TLS/Bearer.
- [x] Meta admin-token.
- [x] Helm/runbooks/SLO harness.
- [x] `go build`, `go vet`, gofmt, `go test -race ./...`.

## Осталось (P0)

- joint consensus/decommission;
- атомарный split/merge finish/clear;
- remote lock-scan RPC + 1PC apply barrier + PITR;
- KV admin auth, RBAC/audit/at-rest;
- 1 ТБ multi-host прогон.
