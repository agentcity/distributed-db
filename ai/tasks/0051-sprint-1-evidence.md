---
task_id: "0051"
type: sprint
title: Sprint 1 — Evidence & closure
created_at: 2026-11-18T01:00:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Sprint 1 — Evidence & closure

1. Joint consensus stress + e2e.
2. Decommission RPC/CLI + meta fan-out token.
3. PITR integration + 1PC barrier wiring.
4. Per-chunk CRC + corruption injection.
5. 72h soak + 1TB multi-host harness.
6. RBAC/audit MVP.
7. kvbackup TLS/admin-token + optional edgehash build.
8. Docs tracks + quickstarts.

## Критерии

- [ ] 8 срезов собираются, race-тесты.
- [ ] Нет конфликтов по файлам.
- [ ] Full `go test -race ./...`.
- [ ] Gaps перечислены.
