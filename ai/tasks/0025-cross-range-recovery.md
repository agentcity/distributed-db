---
task_id: "0025"
type: feature
title: Stage 3 — cross-range 2PC recovery и deadlock
created_at: 2026-11-16T14:20:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 3 — cross-range 2PC recovery и deadlock

## Проблема

Primary-first 2PC без fault injection и deadlock detection.

## Задачи

- Fault injection: kill между prewrite/primary commit/secondary commit.
- Recovery: resolver доводит secondary locks; проверка atomicity.
- Deadlock detection: wait-for graph или timeout+abort с диагностикой.
- Idempotent retry cross-range commit.
- Тесты: crash points, concurrent cross-range txns, deadlock.

## Критерии

- [ ] Тесты crash after primary commit.
- [ ] Secondary locks разрешаются.
- [ ] Deadlock detection/abort.
- [ ] Нет lost/partial visibility.

## Ограничения

- Без distributed transaction manager; primary lock как commit point.

