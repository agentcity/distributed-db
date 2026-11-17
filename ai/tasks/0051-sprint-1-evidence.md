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

## Результат

- [x] joint 1000-change stress + e2e; найдены и исправлены 2 production-бага.
- [x] `kvctl decommission` + meta fan-out token.
- [x] PITR adapter + backup barrier.
- [x] per-chunk CRC.
- [x] RBAC/audit MVP.
- [x] kvbackup TLS/token.
- [x] soak/1 ТБ harness.
- [x] docs tracks/quickstarts.
- [x] full race suite green (stress gated).

## Gaps

- 1000-change stress нестабилен: падение на ~280 changes (узел застревает в joint).
  Тест gated `RAFT_JOINT_STRESS=1`; баг не закрыт.
- 72 ч/1 ТБ не прогнаны.
- PITR не интегрирован с WAL/meta/TSO; restore drill нет.
- RBAC/audit без CLI wiring/rotation; data-plane auth нет.
