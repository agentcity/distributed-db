---
task_id: "0021"
type: feature
title: Stage 2 — backup/restore
created_at: 2026-11-16T14:20:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 2 — backup/restore

## Проблема

Нет backup/restore; большие объёмы нельзя эксплуатировать.

## Задачи

- `internal/backup`: логический backup через range scan чанками,
  manifest с range map/TSO/version.
- Restore в пустой кластер: CREATE ranges, загрузка ключей, проверка epoch.
- CLI `cmd/kvbackup`: backup/restore/verify.
- Формат: каталог с manifest + чанки, checksum.
- Тесты: round-trip 3 узла, restore после split, corrupt chunk.

## Критерии

- [x] Backup всех range без O(range) памяти.
- [x] Restore с проверкой manifest.
- [x] Verify checksum.
- [x] E2E.

## Ограничения

- Online snapshot consistency через TSO; без PITR.

