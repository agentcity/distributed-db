---
task_id: "0023"
type: feature
title: Stage 3 — cross-range SQL, planner и plan cache
created_at: 2026-11-16T14:20:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 3 — cross-range SQL, planner и plan cache

## Проблема

SQL executor не использует cross-range 2PC, нет CBO/plan cache.

## Задачи

- SQL scan/update через несколько range: разбиение по range, cross-range
  транзакции v2client.
- Range predicates (`WHERE id > x`), index selection, ORDER BY/LIMIT pushdown.
- Cost-based planner: stats (row count, distinct), join order для 2 таблиц.
- Plan cache по нормализованному SQL + schema version.
- Тесты: таблица после split, cross-range update, range scan, план.

## Критерии

- [ ] SQL-таблица переживает split.
- [ ] Cross-range UPDATE/DELETE/SELECT.
- [ ] CBO выбирает индекс/join order.
- [ ] Plan cache invalidation.
- [ ] E2E 3 узла.

## Ограничения

- Без distributed shuffle/parallel join в этом срезе.

