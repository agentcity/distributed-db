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

## Результат

- [x] Pre-vote core + gRPC transport.
- [x] PITR WAL/checkpoint adapters + restore drill.
- [x] RBAC CLI + audit rotation/fail-closed.
- [x] Truthful range leader/health.
- [x] Durable split/merge intent.
- [x] Protocol handshake + release workflow/schema.
- [x] Full `go test -race ./...`, stress 1x.

## Gaps

- Pre-vote не проверен multi-host; gRPC adapters wired but no mixed-version cluster.
- PITR не tail-ит WAL и не имеет meta TSO RPC; drill in-process.
- Split intent не закрывает meta↔ranges.json distributed 2PC.
- Release workflow не запускался; registry/K8s не проверялись.
- Audit rotation без external hook; fan-out role tokens inbound-only.
- 72 ч/1 ТБ не запускались.
