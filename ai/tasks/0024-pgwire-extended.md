---
task_id: "0024"
type: feature
title: Stage 3 — extended pgwire и prepared statements
created_at: 2026-11-16T14:20:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 3 — extended pgwire и prepared statements

## Проблема

pgwire поддерживает только simple Query; JDBC/pgx не работают.

## Задачи

- Extended protocol: Parse/Bind/Describe/Execute/Sync/Close.
- Prepared statements, parameter binding ($1), text/binary formats для INT/TEXT.
- Portal suspension, RowDescription/ParameterDescription.
- SQLSTATE, error recovery.
- Тесты raw protocol + psql/pgx, если доступны.

## Критерии

- [x] Parse/Bind/Execute.
- [x] Prepared statement с параметрами.
- [x] Binary format INT/TEXT.
- [x] Error recovery.
- [x] E2E.

## Ограничения

- Без TLS/auth/catalog.

