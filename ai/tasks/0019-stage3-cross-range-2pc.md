---
task_id: "0019"
type: feature
title: Stage 3.1 — cross-range 2PC primary-first
created_at: 2026-11-16T14:00:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 3.1 — cross-range 2PC

## Проблема

Клиент запрещал транзакции через два range, поэтому SQL-таблица после split
ломалась.

## Решение

- `Txn.Commit` группирует мутации по range.
- single-range — 1PC `CommitSingle`.
- cross-range:
  - prewrite всех range с одним `startTS` и primary;
  - commit **primary range первым**, затем secondary;
  - prewrite failure откатывает уже prewritten;
  - reader, встретив secondary lock, разрешает его через primary.
- `Prewrite` больше не требует, чтобы primary был в том же range.

## Критерии

- [x] Cross-range commit.
- [x] Primary-first порядок.
- [x] Rollback prewritten при ошибке prewrite.
- [x] E2E на двух range.
- [ ] Distributed deadlock detection.
- [ ] Fault-injection: kill во время cross-range commit.
- [ ] SQL использует cross-range 2PC для таблиц после split.

## Ограничения

- Нет глобального transaction record; commit point — primary lock.
- При обрыве после partial commit secondary locks разрешает resolver.
