---
task_id: "0027"
type: feature
title: Stage 3 — Jepsen partitions и clock skew
created_at: 2026-11-16T14:20:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 3 — Jepsen partitions и clock skew

## Проблема

Jepsen проверяет только graceful kill/restart.

## Задачи

- Network partition между KV-узлами (proxy/filter) на уровне test.
- Clock skew: управляемая подмена TSO/wall-clock в тесте.
- Invariants: acknowledged writes, snapshot isolation, cross-range atomicity.
- Opt-in `JEPSEN=1`, seed, отчёт.
- Тесты: minority partition, heal, leader isolation, skew.

## Критерии

- [ ] Partition majority/minority.
- [ ] Heal и catch-up.
- [ ] Clock skew сценарий.
- [ ] Cross-range invariant.

## Ограничения

- Внутрипроцессный proxy, не реальная сеть.

