---
task_id: "0018"
type: feature
title: Stage 2.2b — потоковый и chunked Raft snapshot
created_at: 2026-11-16T14:00:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 2.2b — snapshot без whole-range памяти

## Проблема

State machine собирал все записи range в `[]struct`, JSON-ил весь snapshot,
FileStore писал один `data-*.bin`, InstallSnapshot передавал весь payload
одним unary RPC.

## Решение

- `stateMachine.StreamSnapshot`/`RestoreSnapshotStream`: NDJSON, батчи
  1024/4 MiB, CRC/count, restore удаляет и вставляет батчами.
- `FileStore.SaveSnapshotFromReader`/`OpenSnapshot`: потоковая запись/чтение,
  CRC/size, atomic meta.
- `Node.SnapshotToStore`/`RestoreSnapshotStream`.
- `InstallSnapshotChunk`: чанки 1 MiB, offset/size/CRC, temp-файл,
  cleanup; legacy unary остаётся.
- E2E: остановленная/отставшая реплика догоняет через chunked snapshot.

## Критерии

- [x] Потоковый state-machine snapshot.
- [x] Потоковый FileStore.
- [x] Chunked gRPC InstallSnapshot.
- [x] E2E catch-up.
- [x] Все тесты зелёные.
- [ ] Resume/compression/snapshot streaming для backup.

## Ограничения

- Restore неатомарен после валидного header: ошибка может оставить смесь;
  повторный полный stream идемпотентен.
- Chunked install без resume: обрыв начинает передачу заново.
- CRC-32 не криптографический.
