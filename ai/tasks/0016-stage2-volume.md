---
task_id: "0016"
type: feature
title: Stage 2 — потоковый split и авто-split
created_at: 2026-11-16T13:20:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 2 — объём без загрузки range в память

## Проблема

`ExportRange` собирает весь range в `[]Record`, split/merge копируют его
целиком через RAM. Поэтому безопасный range — сотни МБ, а не гигабайты.

## Решение (инкремент 1)

- `mvcc.Store.StreamRange`: батчевый экспорт из Pebble snapshot, без
  материализации всего range.
- `splitLocalLocked`/`mergeLocalLocked` импортируют батчами.
- `KVService.RangeStats`: размер, число ключей, медианный split key.
- `metaserver.AutoSplit`: по размеру range вызывает существующий
  SplitRange; опциональный фоновый цикл на meta-лидере.

## Критерии приёмки

- [x] StreamRange с батчами и тестами.
- [x] Split/merge используют потоковый импорт.
- [x] RangeStats (size/keys/split_key).
- [x] AutoSplit по порогу.
- [x] Тесты зелёные.

## Не входит в инкремент

- shared Pebble на узел;
- потоковый Raft snapshot/InstallSnapshot;
- rebalancing реплик;
- backup/restore.

Эти пункты требуют отдельной переделки интерфейсов и остаются на Stage 2.2+.
