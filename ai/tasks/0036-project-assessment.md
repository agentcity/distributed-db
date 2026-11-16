---
task_id: "0036"
type: docs
title: Честная оценка проекта и применимость
created_at: 2026-11-16T23:50:00Z
author: saron
scope: {backend: true, console: false, mobile: true}
---

# Честная оценка проекта

## Цель

Свести фактическое состояние v1/v2/SQL/edgehash к одной честной оценке:
что готово, что не доказано, где можно использовать, где нельзя.

## Сделано

- [x] `docs/ASSESSMENT.md`: readiness по компонентам, доказательства, пробелы,
  use cases, сравнение, риски, P0.
- [x] `PRODUCT-READINESS.md` обновлён до HEAD 13c03c2.
- [x] README: ссылка на assessment, таблица применимости, актуальные замеры,
  исправлен pgwire extended.
- [x] ROADMAP: batch/edgehash/актуальные цифры.
- [x] PERFORMANCE: режим SyncWrites=false, batch config, ограничения методики.

## Вывод

- v2 — pre-alpha/internal prototype, не production DB.
- edgehash — самая готовая часть для read-only membership; mobile scaffold.
- Не подходит: публичный DBaaS, regulated, OLAP, замена PostgreSQL,
  хранение весов модели, телефоны как узлы консенсуса.
