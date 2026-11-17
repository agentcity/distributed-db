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

## Результат

- [x] Multi-arch: OCI `linux/amd64,linux/arm64`, 85 MB; publish dry-run.
- [x] `docker-compose.yml` demo + `docker-compose.cluster.yml` 3 meta/3 KV.
- [x] Bootstrap `kvctl init-ranges` + meta leader wait + idempotency.
- [x] Demo SQL и cluster 3/3 + 2/3 SQL write/read.
- [x] Docs: DOCKER.md, docker-2-minutes.md.
- [x] Plain `docker build` works; compose smoke pass.

## Gaps

- Registry publish не выполнен (нет auth/registry); multi-arch OCI проверен.
- `kvctl ranges` LEADER пуст: meta descriptor не заполняет leader.
- TLS/mTLS, pgsrv, failover, Linux/NVMe performance не в этом прогоне.
- v1 compose сохранён как docker-compose.v1.yml.
