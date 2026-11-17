---
task_id: "0048"
type: sprint
title: Production hardening — 8 параллельных срезов
created_at: 2026-11-17T21:30:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Production hardening

1. Raft config в snapshot.
2. Split/merge freeze safety.
3. Storage lifecycle: periodic snapshot + MVCC GC.
4. Consistent backup / lock barrier.
5. SQL HTTP TLS/auth.
6. Admin-token для meta admin RPC.
7. Multi-host SLO/1 ТБ harness.
8. Helm/operator + runbooks/alerts.

## Критерии

- [ ] Каждый срез собирается и имеет тесты/документацию.
- [ ] `go build ./...`, `go vet ./...`, gofmt.
- [ ] `go test -race ./...`.
- [ ] Нет конфликтов/затирания чужих изменений.
