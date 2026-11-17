---
task_id: "0050"
type: planning
title: Оценка предложения и план спринтов
created_at: 2026-11-18T00:45:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Предложение: разбор и план

- [x] Оценить 6 пунктов.
- [x] Отметить устаревшие: joint consensus и mTLS уже есть.
- [x] Приоритеты P0/P1/P2.
- [x] 3 спринта × 8 субагентов.
- [x] [docs/SPRINT-PLAN.md](../../docs/SPRINT-PLAN.md).

## P0

1. 72 ч soak + 1 ТБ multi-host p99.
2. Decommission RPC/CLI + meta fan-out token + 1000 changes.
3. PITR integration + 1PC barrier wiring + restore drill.
4. RBAC/audit/fail-closed TLS/at-rest.

## P1

Per-chunk CRC, kvbackup TLS/admin-token, decommission fan-out token,
PITR integration, RBAC/audit.

## P2

Edgehash остаётся встроенным. Опциональная сборка `cmd/edgehash` —
идея на подумать, не отдельный продукт. API freeze/SDK — только после спроса.

## P2

Roadmap tracks, quickstarts, CONTRIBUTING.
