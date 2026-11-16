---
task_id: "0028"
type: feature
title: Stage 2 — rebalancing и leader transfer
created_at: 2026-11-16T14:30:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Stage 2 — rebalancing и leader transfer

## Осталось

- `KVService.TransferLeader(range_id, target)` + Raft leadership transfer.
- AddReplica/RemoveReplica через Raft config change.
- Meta placement loop: capacity/AZ-aware выбор, перенос реплики,
  catch-up через snapshot.
- Decommission узла.
- E2E: kill/remove node, add replica, transfer leader.

## Критерии

- [x] Leader transfer (KVService.TransferLeader + TimeoutNow).
- [ ] Add/Remove replica (не сделано).
- [x] Leader rebalance loop (RebalanceLeaders/StartLeaderRebalance).
- [x] E2E transfer/leader rebalance на 3 узлах.
