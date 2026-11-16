---
task_id: "0017"
type: feature
title: Stage 2.2a — shared Pebble и prefix-диапазоны
created_at: 2026-11-16T13:40:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 2.2a — один Pebble на узел

## Проблема

Каждый range открывал собственную Pebble-базу. При тысячах range’ей это
тысячи WAL/DB и файловых дескрипторов.

## Решение

- `storage.Prefix(engine, prefix)` — Engine/Batch/Iterator/Snapshot,
  прозрачно добавляющий префикс `'R' | rangeID | 0x00`.
- `kvserver.Server` открывает один `<DataDir>/db/shared`; группа получает
  prefix-wrapper, реальный engine закрывает только Server.
- `Stats` wrapper’а считает размер только своего range (полный scan,
  задокументировано).
- Split/merge/restart работают на общем engine; старый per-range layout
  не мигрируется.

## Критерии

- [x] Prefix wrapper и тесты изоляции/границ/snapshot/Stats.
- [x] Один shared engine на процесс, корректный Stop.
- [x] Split/merge/restart двух range.
- [x] Все тесты зелёные.
- [ ] Потоковый Raft snapshot/InstallSnapshot — не сделано.

## Ссылки

- `internal/storage/prefix.go`
- `internal/kvserver/shared_engine_test.go`
