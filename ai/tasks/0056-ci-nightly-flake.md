---
task_id: "0056"
type: fix
title: Nightly chaos meta-scheduler flake
created_at: 2026-11-19T20:45:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Nightly chaos CI flake

Symptom: `TestV2MetaSchedulerSplitMerge` failed under 2 CPUs with
`not meta leader` between split and ApplyRanges, then merge timed out on one
replica.

Fix:
- test meta/KV election timeouts raised for this 3-node integration test;
- retry idempotent split/merge after a meta leader change;
- local `GOMAXPROCS=2` 10x test and 3 chaos rounds PASS.

No production semantics changed.
