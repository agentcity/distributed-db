---
task_id: "0008"
type: feature
title: M3 — lock resolution и транзакционные конфликты
created_at: 2026-11-16T03:00:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# M3 — lock resolution и транзакционные конфликты

## Проблема

Percolator-транзакция может оставить lock после падения клиента/узла.
Без `CheckTxnStatus`/`ResolveLock` чтение вечно получает `ErrLocked`, а SQL
не может разрешать конфликты.

## Требования

- `mvcc.CheckTxnStatus`: committed / rolled back / live / expired, ключи и TTL.
- `mvcc.ResolveLock`: commit или rollback всех ключей транзакции.
- Просроченный lock откатывается по TSO-времени и TTL.
- `KVService.CheckTxnStatus/ResolveLock` RPC.
- `Get` автоматически разрешает committed/rolled-back/expired lock и
  повторяет чтение; живой lock возвращает `locked`.
- Тесты: committed, rollback, live lock, expired lock, ResolveLock commit/rollback.

## Критерии приёмки

- [x] CheckTxnStatus и ResolveLock в MVCC.
- [x] RPC и авто-разрешение в Get.
- [x] E2E: prewrite → CheckTxnStatus → ResolveLock commit → Get.
- [x] E2E: rollback path.
- [ ] Клиентские методы CheckTxnStatus/ResolveLock и retry на live lock.
- [ ] Фоновый GC locks и stress/Jepsen-проверка.

## Ссылки

- `docs/ROADMAP.md` (M3)
- ADR 0002
