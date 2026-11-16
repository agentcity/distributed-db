---
task_id: "0039"
type: docs
title: Практическое руководство по запуску и работе
created_at: 2026-11-17T00:40:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# User guide

## Цель

Убрать «чёрный ящик»: показать реальные команды терминала, порты, данные,
SQL/psql, backup, edgehash и troubleshooting.

## Сделано

- [x] `docs/USER-GUIDE.md`: v1 curl, Docker, v2 meta/KV/SQL/pgwire,
  raw KV Go SDK, backup, edgehash, метрики, ошибки, ограничения.
- [x] Ссылка в README.
- [x] Проверены v1 smoke и v2 SQL HTTP на реальном кластере.
