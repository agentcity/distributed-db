---
task_id: "0007"
type: feature
title: M2 — range map, routing и два диапазона
created_at: 2026-11-16T02:30:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# M2 — range map, routing и два диапазона

## Проблема

Single-range v2 знает только один Raft-груп. Чтобы масштабироваться по
ключевому пространству, клиент должен узнавать range по ключу, а узлы —
host-ить Raft-группы своих диапазонов.

## Требования

- MetaService: `GetRange(key)`, `ListRanges`, bootstrap карты диапазонов.
- `meta.SetRanges` с валидацией: контигуальность, уникальные ID, реплики.
- KV-узел сообщает `range_id/start/end` в Status.
- Клиент: `key → range → replicas → leader`, кэш лидера по range ID,
  refresh при not-leader, запрет cross-range транзакции в M2.
- E2E: 2 диапазона × 3 реплики, ключи в обоих, kill лидера одного range,
  новая транзакция уходит на нового лидера.

## Критерии приёмки

- [x] `GetRange/ListRanges` и bootstrap карты.
- [x] `SetRanges` валидирует карту.
- [x] Клиент маршрутизирует по range ID и обновляет лидера.
- [x] E2E 2×3: запись/чтение в оба диапазона, failover одного range.
- [ ] Split/merge с реальным переносом данных и Raft-групп.
- [ ] Multi-Raft host внутри одного процесса.

## Ссылки

- `docs/ROADMAP.md` (M2)
- `docs/CONTRACTS-V2.md`
- ADR 0003
