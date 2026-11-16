---
task_id: "0037"
type: docs
title: Production plan и ворота G1–G8
created_at: 2026-11-17T00:05:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Production plan

## Цель

Превратить честную оценку в исполнимый backlog: что реализовать, в каком
порядке, с измеримыми воротами.

## Сделано

- [x] `docs/PRODUCTION-PLAN.md`: ICP, G1–G8, P0/P1/P2, фазы, оценки, спринт.
- [x] Ссылки из README/ASSESSMENT/PRODUCT-READINESS.
- [x] Явно зафиксировано: G1 durability и G2 membership нельзя обходить.
- [x] Production-профиль: `SyncStateMachine=true` по умолчанию; fast — opt-in.
