---
task_id: "0030"
type: feature
title: Stage 1/3 — метрики SQL/pgwire и экспорт
created_at: 2026-11-16T14:30:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Stage 1/3 — метрики SQL/pgwire и экспорт

## Осталось

- Инструментировать SQL statements/plans и pgwire connections.
- Экспорт `/metrics` из kvserver/sqlsrv/pgsrv (сейчас metricsrv отдельный).
- SLO-алерты/дашборд.
- Нагрузочный SLO-прогон с p99.

## Критерии

- [ ] SQL/pgwire SLO.
- [ ] Встроенный endpoint.
- [ ] SLO-прогон.
