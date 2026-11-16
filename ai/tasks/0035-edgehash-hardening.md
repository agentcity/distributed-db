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

- [x] Ed25519 verify, HMAC legacy.
- [x] Absence proof verifies.
- [x] zstd segment + legacy v1.
- [x] TLS/auth/rate/range/CDN headers.
- [x] Atomic sync + TTL/LRU.
- [x] Mobile protocol scaffold.
- [x] Тесты.

## Результат

- Ed25519 manifest + Verifier; HMAC legacy.
- AbsenceProof для before/between/after.
- Segment v2 zstd, v1 совместим.
- TLS, Bearer auth, per-IP rate limit, Range/206, CDN headers.
- generation dirs + CURRENT, crash-atomic sync.
- MaxCacheBytes/MaxSegments/SegmentTTL + LRU.
- mobile scaffold (Kotlin/Swift, untested on device).
