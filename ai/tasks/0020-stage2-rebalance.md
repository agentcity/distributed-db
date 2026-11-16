---
task_id: "0020"
type: feature
title: Stage 2 — auto-merge и rebalancing
created_at: 2026-11-16T14:20:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 2 — auto-merge и rebalancing

## Проблема

AutoSplit есть, но нет auto-merge, переноса реплик и балансировки лидеров.

## Задачи

- AutoMerge: порог маленького range, поиск соседа, вызов MergeRanges.
- Leader transfer: RPC/метод перевода лидера range на выбранную реплику.
- Placement: meta scheduler переносит реплику (AddReplica/RemoveReplica),
  capacity/AZ-aware выбор; snapshot catch-up при добавлении.
- Фоновый lifecycle loop на meta-лидере с остановкой.
- Тесты: merge пары, перенос лидера, add/remove replica, отказ реплики.

## Критерии

- [ ] AutoMerge по размеру.
- [ ] Leader transfer.
- [ ] Add/Remove replica с catch-up.
- [ ] Фоновый lifecycle loop.
- [ ] E2E на 3 KV.

## Ограничения

- Без полноценного capacity planner; эвристики.

