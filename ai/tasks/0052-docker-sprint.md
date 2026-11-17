---
task_id: "0052"
type: sprint
title: Sprint 2 — Docker multi-arch и Compose quickstart
created_at: 2026-11-18T02:20:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Sprint 2 — Docker

1. Multi-arch publish `linux/amd64,linux/arm64`.
2. `docker-compose.yml`: demo + 3-node cluster.
3. Bootstrap range-map в Compose.
4. Docs «поднял за 2 минуты».

## Критерии

- [ ] Образ собирается/публикуется multi-arch (или dry-run при отсутствии registry).
- [ ] `docker compose config` для demo/cluster.
- [ ] Demo SQL запрос работает.
- [ ] Cluster: 3 meta + 3 KV, bootstrap, write/read.
- [ ] Docs проверены по фактическим флагам.
