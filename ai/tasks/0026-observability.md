---
task_id: "0026"
type: feature
title: Stage 1/3 — SLO-метрики и observability
created_at: 2026-11-16T14:20:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 1/3 — SLO-метрики и observability

## Проблема

Нет метрик latency/throughput/errors/snapshot/GC; нельзя закрыть SLO Stage 1.

## Задачи

- `internal/metrics`: счётчики/histogram без внешних зависимостей.
- HTTP `/metrics` (Prometheus text) и `/debug/slo`.
- Инструментировать KV RPC, txn commit, snapshot chunks, auto-split/merge.
- SLO-отчёт: p50/p95/p99, errors, inflight.
- Тесты метрик и endpoint.

## Критерии

- [ ] Метрики RPC/txn/snapshot.
- [ ] Prometheus endpoint.
- [ ] SLO-отчёт.
- [ ] Минимальные накладные расходы.

## Ограничения

- Без OpenTelemetry.

