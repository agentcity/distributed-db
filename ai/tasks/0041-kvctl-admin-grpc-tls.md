---
task_id: "0041"
type: feature
title: kvctl admin + gRPC TLS
created_at: 2026-11-17T01:40:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# kvctl admin + gRPC TLS

## Сделано

- [x] kvctl ranges/status/split/merge/backup.
- [x] v2client ListRanges/SplitRange/MergeRanges/MetaLeader, NewMetaOnly.
- [x] gRPC TLS/mTLS для KV/meta/Raft и v2client.
- [x] CLI TLS-флаги kv2/sqlsrv/pgsrv/kvctl.
- [x] Тесты: admin unit + e2e split/merge/backup; mTLS 3-node Raft.
- [x] USER-GUIDE/README обновлены.

## Gaps

- TLS opt-in, plaintext default.
- kvbackup plaintext.
- admin RPC на data-порту, нет RBAC/audit.
