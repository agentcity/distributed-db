---
task_id: "0038"
type: sprint
title: Sprint 1 — production gates G1/G4/G5/G6
created_at: 2026-11-16T23:10:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Sprint 1

8 параллельных задач, затем сводка и проверка:

1. durability profile: sync по умолчанию, fast opt-in.
2. crash-injection harness.
3. RFC membership safety.
4. RFC PITR.
5. pgwire TLS/auth.
6. 1 ТБ benchmark harness + p99.
7. nightly chaos CI.
8. gRPC TLS/mTLS helper.

## Критерии

- [x] 8 задач реализованы/задокументированы.
- [x] `go build ./...`, `go vet ./...`, `gofmt -l .`.
- [x] `go test -race ./...`.
- [x] Изменения сведены без конфликтов.

## Результат

- durability sync default + fast opt-in;
- crash SIGKILL harness (fast/sync);
- ADR 0005 membership, ADR 0006 PITR;
- pgwire TLS/auth; grpctls helper;
- 1TB harness; nightly chaos CI;
- Jepsen PeerAddrs fix: 4/4 partition tests pass.
- Отчёт: `docs/SPRINT-1-REPORT.md`.
