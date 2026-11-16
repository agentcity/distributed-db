---
task_id: "0031"
type: feature
title: Stage 2 — Add/Remove replica и membership
created_at: 2026-11-16T15:00:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Stage 2 — membership

## Проблема

Replica sets статические; leader transfer есть, Add/Remove replica нет.

## MVP

- Raft learner: репликация без голоса, catch-up.
- Promote learner в voter (single-server change, без joint consensus).
- Remove voter.
- KV RPC AddReplica/RemoveReplica + meta orchestration.
- Персист membership (snapshot/hard state).

## Критерии

- [x] Add learner + catch-up.
- [x] Promote voter.
- [x] Remove voter.
- [x] Meta обновляет replicas только после успеха.
- [x] E2E 4 узла: add, remove, kill.
- [ ] Joint consensus — нет (single-server change, отдельная задача).

## Ограничения

- Один server change за раз, без joint consensus.
- Без decommission/placement capacity.
