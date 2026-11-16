---
task_id: "0012"
type: feature
title: M2.2/M2.3 — RangeHost, range_id RPC и split/merge с переносом данных
created_at: 2026-11-16T07:00:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# M2.2/M2.3 — RangeHost и split/merge

## Проблема

Один процесс держал одну Raft-группу; split/merge не могли создать новую
группу и перенести данные.

## Решение

### M2.2

- `Config.Ranges []RangeConfig`; один процесс host-ит N групп.
- У каждой группы свой Pebble (`data/db/range-<id>`), Raft log
  (`data/raft/range-<id>`), MVCC, state machine, snapshot и transport.
- gRPC слушатель один; KV-запросы маршрутизируются по ключу, Raft-запросы —
  по `range_id`; `Status(range_id)` возвращает конкретный range.
- Cross-range Prewrite/Commit/Rollback/ResolveLock/Scan отклоняются
  `range mismatch` ещё до Raft.
- Legacy `RangeID/RangeStart/RangeEnd` работает как один range.

### M2.3 (host-local MVP)

- `mvcc.ExportRange/ImportRecords/DeleteRange` копируют raw MVCC-записи по
  user-key диапазону, включая locks/versions.
- `Server.SplitRange`: freeze записи, export → новый Raft group + import →
  delete right → snapshot old; новый group получает своего лидера.
- `Server.MergeRanges`: export right → import left → stop right group →
  расширить левый range.
- `MetaService.SetRanges` обновляет карту после переноса.
- E2E: 3 ключа → split → чтение обеих половин → запись в новый range →
  merge → чтение всех ключей.

## Критерии приёмки

- [x] N групп в одном процессе, range_id в Raft/Status.
- [x] Cross-range операции отклоняются.
- [x] Реальный перенос MVCC-записей при split/merge.
- [x] Meta-карта обновляется через SetRanges.
- [x] E2E split/merge на одном RangeHost.
- [ ] Распределённый split: один и тот же split на всех репликах + epoch/fencing.
- [ ] Persist range map RangeHost между рестартами.

## Ссылки

- `docs/ARCHITECTURE-TARGET.md`
- `docs/ROADMAP.md` (M2)
