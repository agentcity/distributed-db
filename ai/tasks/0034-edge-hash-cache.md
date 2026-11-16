---
task_id: "0034"
type: feature
title: Edge hash cache — Bloom + Merkle + manifest
created_at: 2026-11-16T21:00:00Z
author: saron
scope: {backend: true, console: false, mobile: false}
---

# Edge hash cache

## Цель

Авторитетные серверы хранят точный набор хешей; телефоны/edge-узлы
кэшируют immutable Bloom-сегменты и отвечают локально:
- Bloom negative → «нет» без сети;
- Bloom maybe → запрос на сервер + Merkle-proof.

## Компоненты

- immutable segment: prefix, sorted hashes, Bloom, Merkle root;
- manifest: version, segments, HMAC signature;
- Store: exact lookup + proof;
- Cache: manifest/segment download, verify, local bloom;
- HTTP: /manifest, /segment/{prefix}, /lookup/{hash};
- CLI: build/serve/query.

## Критерии

- [ ] Bloom false-positive rate в допуске, false-negative = 0.
- [ ] Merkle-proof проверяется.
- [ ] Подпись manifest.
- [ ] Кэш: отрицательный локально, положительный через сервер.
- [ ] HTTP/CLI e2e.
- [ ] Тесты.
