---
task_id: "0001"
type: feature
title: Рабочее Raft-ядро и KV-хранилище
created_at: 2026-11-16T00:50:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Рабочее Raft-ядро и KV-хранилище

## Проблема

Текущий HEAD не собирается: два конкурирующих entrypoint, импортные циклы
`node` ↔ `node/grpc|http|raft`, отсутствует сгенерированный protobuf-код.
Raft не реплицирует записи (пустой `AppendEntries`), коммитит без кворума,
хранит только ключи и не персистит `term`/`votedFor`/лог.

## Требования

- Единый контракт `Entry`/`CommandType` и один `proto/db.proto` с генерацией Go-кода.
- Raft-ядро без импорта `node`: выборы, репликация, кворумный коммит, no-op
  лидера, персистентный WAL, восстановление после рестарта.
- `ReadBarrier`: лидер + lease кворума + ожидание apply до commitIndex/no-op,
  чтобы чтение сразу после failover не было устаревшим.
- KV state machine: put/delete/noop, WAL с fsync, torn-tail recovery, replay,
  идемпотентный к повторному применению.
- Тесты: выборы, репликация, failover, потеря кворума, конфликт логов,
  рестарт из WAL, ожидание `ReadBarrier`.

## Критерии приёмки

- [ ] `go build ./...` и `go vet ./...` зелёные.
- [ ] `go test -race ./internal/raft/... ./internal/kv/...` зелёные.
- [ ] После падения лидера подтверждённое значение читается с нового лидера.
- [ ] Рестарт узла сохраняет term/vote/лог; KV переживает рестарт.
- [ ] Значения и delete реплицируются, а не только ключи.

## Ссылки

- Контракт: `docs/ARCHITECTURE.md`
- План: `docs/ARCHITECTURE.md`, раздел «Correctness requirements»
