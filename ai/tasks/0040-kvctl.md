---
task_id: "0040"
type: feature
title: kvctl — полноценный терминальный CLI
created_at: 2026-11-17T01:10:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# kvctl

## Цель

Убрать необходимость писать Go-код/curl для v2 KV: CRUD, scan, атомарные
транзакции, shell, JSON/hex/raw.

## Сделано

- [x] `cmd/kvctl` + `internal/kvctl` (backend interface, fake tests).
- [x] `get/put/delete/scan/txn/shell/ping/version/help`.
- [x] `-format table|json|raw`, `-hex`, stdin/@file, env config, timeout.
- [x] prefix scan, exit codes, atomic txn parser.
- [x] e2e на 3-node кластере: put/get/scan/txn.
- [x] `make build-cli`, USER-GUIDE обновлён.
