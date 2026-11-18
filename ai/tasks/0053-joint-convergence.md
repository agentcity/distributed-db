---
task_id: "0053"
type: fix
title: Joint consensus convergence
created_at: 2026-11-18T04:20:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Joint convergence

## Root causes

1. Removed voters rejected RequestVote; peers still joint could not elect the
   leader that delivers the final config. Fix: candidate must be in the local
   electorate; a removed voter may vote (candidate counts only its own config).
2. Conflict at snapshot boundary re-sent InstallSnapshot forever. Fix: probe
   AppendEntries from snapshot.Index+1 first.
3. Stale snapshot membership retried forever. Fix: acknowledge coverage when
   snapshot.Index <= local last index.

## Verification

- `RAFT_JOINT_STRESS=1 go test -race ./internal/raft -run TestJointConsensusRandomizedStress -count=3` PASS.
- `go test -race ./internal/raft/...` PASS.
- CI job `joint-stress` added.

## Gaps

- No pre-vote; stale nodes can inflate terms.
- `finishSnapshotInstall` still trusts response.LastIndex.
- Removed-as-learner joint demotion not stress-tested.
