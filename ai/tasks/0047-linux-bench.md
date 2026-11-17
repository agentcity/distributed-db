---
task_id: "0047"
type: bench
title: Linux fsync comparison
created_at: 2026-11-17T03:25:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Linux benchmark

- [x] `scripts/bench-linux.sh` (golang container, mounted repo).
- [x] fast/single ~6.8ms, sync/single ~6.9ms; 10 clients ~677/800 tx/s.
- [x] Docs: Linux container != NVMe/multi-host, but macOS fsync not portable.
