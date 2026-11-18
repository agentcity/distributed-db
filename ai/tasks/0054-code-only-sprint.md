---
task_id: "0054"
type: sprint
title: Sprint 2 — code-only hardening
created_at: 2026-11-18T06:30:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Sprint 2 — code-only hardening (без стенда)

1. Pre-vote в Raft + local stress.
2. PITR WAL/meta/TSO adapters.
3. RBAC CLI + audit rotation/export.
4. Range leader/health truth.
5. Split/meta recovery: durable intent.
6. Protocol versioning/mixed-version.
7. Restore drill automation.
8. Registry/Helm release automation.

## Критерии

- [ ] 8 срезов собираются, race-тесты.
- [ ] Нет 72 ч/1 ТБ/registry/K8s заявок.
- [ ] Full `go test -race ./...`.
