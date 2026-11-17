---
task_id: "0046"
type: perf
title: Multi-range fan-out benchmark
created_at: 2026-11-17T03:15:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Multi-range fan-out

## Сделано

- [x] `newSpeedMultiRangeCluster` (3 nodes × N ranges).
- [x] `BenchmarkV2MultiRangeWorkstations` ranges 1/4/8, pipeline 1/64/256.
- [x] Замеры на одном M1-диске: fan-out не быстрее одного range.
- [x] Документация: PERFORMANCE/analysis/speed/README.

## Вывод

Fan-out требует разных узлов/дисков и достаточной нагрузки на range.
На одном диске каждый range делает свой Raft-fsync; 4/8 ranges делят один
IOPS-бюджет. Один in-flight становится медленнее из-за дробления батча.
