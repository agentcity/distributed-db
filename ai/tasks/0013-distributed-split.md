---
task_id: "0013"
type: feature
title: M2.4 — распределённые split и merge
created_at: 2026-11-16T07:30:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# M2.4 — распределённый split

## Проблема

M2.3 split/merge были host-local: данные и новая Raft-группа создавались
только в одном процессе, мета-карта не отражала остальные реплики.

## Решение

- `KVService.SplitRange` admin-RPC.
- Лидер range реплицирует `CommandSplitPrepare` с split key; все реплики
  замораживают записи и ждут применения команды.
- Каждая реплика локально копирует правую половину в новую Raft-группу;
  старый range получает `End=splitKey`.
- Координатор обновляет meta через `SetRanges` только после успеха на всех
  репликах.
- E2E: 3 узла, prewrite/commit, split на всех, meta SetRanges, чтение обеих
  половин и запись в новый range.

## Критерии приёмки

- [x] prepare-команда через Raft, freeze на всех репликах.
- [x] новая Raft-группа на каждой реплике.
- [x] meta обновляется после всех реплик.
- [x] e2e 3 узла: чтение a/m/z и запись zz в range 2.
- [x] Распределённый merge.
- [ ] Epoch/fencing и persist range map RangeHost.

## Ссылки

- `docs/ARCHITECTURE-TARGET.md`
- `docs/ROADMAP.md` (M2)
