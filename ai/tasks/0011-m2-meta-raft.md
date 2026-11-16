---
task_id: "0011"
type: feature
title: M2.1 — replicated meta и TSO через Raft
created_at: 2026-11-16T06:30:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# M2.1 — replicated meta и TSO через Raft

## Проблема

Meta/TSO были in-memory на одном узле: падение meta останавливало TSO,
range map и split/merge. Нужен маленький Raft-кластер управления.

## Решение

- `metaserver.Config{Replicated,ID,Peers,DataDir,...}` — 3/5-node Raft.
- `MetaState` (ranges + TSO high-water + nextID) в state machine и snapshot.
- `AllocTS` — replicated command: лидер резервирует, все реплики двигают
  high-water; TSO монотонен после failover.
- `MetaService.Leader` — follower отдаёт адрес лидера.
- `v2client` редиректится на лидера и повторяет `AllocTS` после election.
- gRPC `RaftService`/InstallSnapshot для meta через `internal/raftgrpc`.

## Критерии приёмки

- [x] 3-node meta Raft, лидер и follower.
- [x] `AllocTS` после kill лидера > предыдущего значения.
- [x] Клиент, стартовавший на follower, находит лидера.
- [x] Клиент переживает meta failover.
- [x] Restart meta с тем же data dir не откатывает TSO.
- [ ] Multi-seed meta addresses в конфиге клиента.
- [ ] split/merge commands через meta.

## Ссылки

- `docs/ARCHITECTURE-TARGET.md`
- `docs/ROADMAP.md` (M2)
