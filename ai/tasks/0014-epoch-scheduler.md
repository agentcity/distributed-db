---
task_id: "0014"
type: feature
title: M2.5 — epoch/fencing и meta scheduler
created_at: 2026-11-16T08:00:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# M2.5 — epoch/fencing и meta scheduler

## Проблема

Клиент со старой картой мог обратиться к range после split/merge; split/merge
запускались тестовым кодом, а не meta.

## Решение

- `RangeConfig.Epoch`; Status отдаёт epoch; split/merge инкрементируют epoch.
- Клиент отправляет `x-range-epoch` из `RangeInfo.Version`; сервер отклоняет
  stale epoch.
- `MetaService.SplitRange/MergeRanges` — scheduler на meta-лидере:
  - читает range map, выбирает детерминированный newID;
  - параллельно вызывает все реплики KV `SplitRange`/`MergeRanges`;
  - только после успеха всех делает `ApplyRanges`;
  - версия карты = epoch+1.
- E2E: 3 meta + 3 KV, split и merge через meta RPC; stale client получает
  `stale epoch`.

## Критерии приёмки

- [x] epoch в RangeConfig/Status/админ-RPC.
- [x] stale epoch отклоняется на KV-запросах.
- [x] meta scheduler split/merge на всех репликах.
- [x] meta map обновляется только после успеха.
- [x] e2e 3 meta + 3 KV.
- [x] Retry fan-out и partial-merge idempotency.
- [ ] Durable scheduler queue/leader lease.

## Ссылки

- `docs/ARCHITECTURE-TARGET.md`
- `docs/ROADMAP.md` (M2)
