---
task_id: "0035"
type: feature
title: Edge hash cache — production hardening
created_at: 2026-11-16T22:00:00Z
author: saron
scope: {backend: true, console: false, mobile: true}
---

# Edge hash cache hardening

- Ed25519 manifest signatures (public key for phones).
- TLS/auth/rate limit on HTTP.
- zstd segment compression + HTTP range.
- Crash-atomic cache sync.
- Cache eviction/TTL.
- Non-membership proofs.
- Mobile SDK scaffold.
- CDN-friendly headers/layout.

## Критерии

- [ ] Ed25519 verify, HMAC legacy.
- [ ] Absence proof verifies.
- [ ] zstd segment + legacy v1.
- [ ] TLS/auth/rate/range/CDN headers.
- [ ] Atomic sync + TTL/LRU.
- [ ] Mobile protocol scaffold.
- [ ] Тесты.
