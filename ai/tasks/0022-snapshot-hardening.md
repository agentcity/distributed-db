---
task_id: "0022"
type: feature
title: Stage 2 — snapshot compression и resume
created_at: 2026-11-16T14:20:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Stage 2 — snapshot compression и resume

## Проблема

Chunked snapshot без сжатия и resume; обрыв начинает заново.

## Задачи

- Сжатие snapshot-потока (zstd/snappy) с флагом в header.
- Resume: upload id + offset, `Begin/Chunk/Finish` семантика; follower
  продолжает с подтверждённого offset.
- Per-chunk CRC и общий CRC.
- Ограничение размера/диска, cleanup.
- Тесты: обрыв на середине, повтор с offset, сжатый round-trip.

## Критерии

- [x] Compression (gzip, auto >1 MiB).
- [ ] Resume после обрыва (не сделано).
- [x] Per-chunk? нет: whole-payload CRC + entry CRC; per-chunk не сделан.
- [x] E2E catch-up (компрессия + chunked).

## Ограничения

- Без TLS; CRC не криптографический.

