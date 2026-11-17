---
task_id: "0049"
type: sprint
title: Safety/recovery wave — 8 срезов
created_at: 2026-11-17T23:45:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Safety/recovery wave

1. Joint consensus core (Raft).
2. Атомарное завершение split/merge.
3. 1PC apply barrier / commit lease.
4. Remote lock-scan RPC.
5. PITR archive MVP.
6. KV admin auth.
7. Decommission orchestration.
8. Release image + chart bootstrap.

## Критерии

- [ ] Каждый срез компилируется и имеет тесты.
- [ ] Нет конфликтов по файлам.
- [ ] `go build/vet/gofmt`, `go test -race ./...`.
- [ ] Оставшиеся gaps перечислены явно.
