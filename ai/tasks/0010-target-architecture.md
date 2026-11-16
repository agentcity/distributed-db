---
task_id: "0010"
type: docs
title: Целевая архитектура M2/M3/M5 + AI control plane
created_at: 2026-11-16T06:00:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Целевая архитектура M2/M3/M5 + AI control plane

## Проблема

Нужно до начала реализации зафиксировать, как соотносятся replicated meta/TSO,
Multi-Raft host, split/merge, cross-range 2PC, SQL planner/pgwire и отдельный
AI inference control plane. Без схемы легко смешать control/data plane.

## Решение

- `docs/ARCHITECTURE-TARGET.md` — компоненты, потоки, критерии приёмки, риски.
- `docs/diagrams/target-architecture.png` — общая схема.
- Ключевой принцип: Raft только control plane; тензоры/активации — напрямую.
- AI inference — отдельный продукт/кластер, не часть distributed-db.

## Критерии приёмки

- [x] Схема control plane / data plane / SQL / AI.
- [x] Sequence-диаграммы split и merge.
- [x] Критерии M2/M3/M5/AI и порядок работ.
- [ ] Согласование с Юрием и старт M2.1 (Meta Raft).

## Ссылки

- `docs/ARCHITECTURE-TARGET.md`
- `docs/ROADMAP.md`
