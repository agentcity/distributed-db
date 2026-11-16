---
task_id: "0029"
type: feature
title: Stage 2 — snapshot resume
created_at: 2026-11-16T14:30:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Stage 2 — snapshot resume

## Осталось

- upload id + offset negotiation в InstallSnapshotChunk.
- Продолжение с подтверждённого offset после обрыва.
- Per-chunk checksum (сейчас whole-payload CRC + entry CRC).
- Cleanup/лимиты для resume.
- E2E: обрыв на середине, продолжение без повторной передачи.

## Критерии

- [ ] Resume с offset.
- [ ] Per-chunk CRC.
- [ ] E2E обрыва.
