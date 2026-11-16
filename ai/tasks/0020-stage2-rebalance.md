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

- [x] AutoMerge по размеру.
- [ ] Leader transfer (не сделано).
- [ ] Add/Remove replica с catch-up (не сделано).
- [x] Фоновый lifecycle loop.
- [x] E2E auto-merge/lifecycle на fake KV; реальный 3-node — в 0027.

## Ограничения

- Без полноценного capacity planner; эвристики.

