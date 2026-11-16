# Testing the EdgeHash mobile scaffold

Status: **this is not a validated app or SDK.** Honest inventory of what was
and was not checked:

| Check | Result |
|---|---|
| Android app build (Gradle/Android SDK) | **Not done.** No Android SDK, Gradle project, emulator, or device was available. |
| Kotlin compile | `EdgeHashClient.kt` compiled with Kotlin 2.0.21/JVM against minimal `org.json` stubs. This checks Kotlin syntax/semantics, not Android's actual `org.json` implementation or Android APIs. |
| Android vector harness | Temporary JVM harness passed canonical JSON, HMAC, Ed25519, Merkle, Bloom, and v1/v2 segment vectors. |
| iOS app build/link/run | **Not done.** No app target, no simulator run, no device. |
| iOS typecheck | `swiftc -typecheck -warnings-as-errors` against the iPhoneSimulator SDK, target `arm64-apple-ios16.0-simulator`, exit 0. |
| Swift vector harness | Compiled and run on macOS; passed the same canonical/signature/Merkle/Bloom/segment vectors, including JSON parsing. |
| HTTP end-to-end, cache, policy, concurrency | **Not run.** |
| zstd, absence proof, background sync, real devices | **Not run.** zstd/absence are not implemented in the mobile scaffold; the server has both. |

The Go tree was being changed concurrently with this scaffold (Ed25519,
segment v2/zstd, absence-proof helpers). The curl recipes describe the server
as observed at scaffold time; verify flags with `-h` and inspect the served
manifest. The temporary harnesses are not committed; the required tests below
must live in a real Android/iOS test target.

`org.json` stubs only implement the methods this file calls. Android-specific
behavior such as `JSONObject.keys()`, number coercion, and exception messages
still needs a real device/instrumentation test.

Reference values live in [`tests/notes.md`](tests/notes.md).

## 1. Build the server and a tiny fixture

From the repository root:

```sh
go build -o /tmp/edgehash ./cmd/edgehash

# Three hashes for a tiny segment. Use shasum(1) or sha256sum(1).
printf 'edgehash-mobile-vector-a\n' | shasum -a 256 | awk '{print $1}' > /tmp/hashes.txt
printf 'edgehash-mobile-vector-b\n' | shasum -a 256 | awk '{print $1}' >> /tmp/hashes.txt
printf 'edgehash-mobile-vector-c\n' | shasum -a 256 | awk '{print $1}' >> /tmp/hashes.txt
cat /tmp/hashes.txt

KEY=00112233445566778899aabbccddeeff
/tmp/edgehash build -in /tmp/hashes.txt -out /tmp/edge-store -key "$KEY"
```

The build produces a manifest signed with HMAC-SHA256 (no `signature_alg`) and
segments in the current format. A three-hash segment is well below the server's
4 KiB auto-compression threshold, so it should be v2 `"compression":"none"`.
Check:

```sh
jq '.segments[0] | {prefix,size,sha256,compression}' /tmp/edge-store/manifest.json
```

If `compression` is `"zstd"`, the mobile scaffold can read the manifest but will
skip that segment for offline caching (remote proofs still work). For a larger
offline fixture, use a server build that exposes
`BuildOptions.Compression = "none"`; do not assume the CLI has a `-compression`
flag.

## 2. Serve and inspect with curl

```sh
/tmp/edgehash serve -dir /tmp/edge-store -addr 127.0.0.1:8080 -key "$KEY"
```

In another shell:

```sh
# Health and schema version (version is unsigned/informational).
curl -sS http://127.0.0.1:8080/healthz
curl -sS http://127.0.0.1:8080/v1/manifest/version

# Manifest + headers.
curl -sS -D /tmp/manifest.headers -o /tmp/manifest.json \
  http://127.0.0.1:8080/v1/manifest
jq . /tmp/manifest.json
grep -i '^etag:' /tmp/manifest.headers

# Conditional manifest request: expect HTTP 304.
ETAG=$(awk 'tolower($1)=="etag:" {print $2}' /tmp/manifest.headers | tr -d '\r')
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H "If-None-Match: $ETAG" http://127.0.0.1:8080/v1/manifest

# Download the first segment and compare its SHA-256 with the manifest.
PREFIX=$(jq -r '.segments[0].prefix' /tmp/manifest.json)
HEX=$(printf '%04x' "$PREFIX")
curl -sS -o /tmp/segment.bin \
  "http://127.0.0.1:8080/v1/segment/$HEX"
shasum -a 256 /tmp/segment.bin
jq -r '.segments[0].sha256' /tmp/manifest.json

# Present lookup.
H1=$(sed -n '1p' /tmp/hashes.txt)
curl -sS "http://127.0.0.1:8080/v1/lookup/$H1" | tee /tmp/lookup-present.json | jq .
# Current Go encoder uses capitalized proof keys; the SDK accepts both cases.

# Absent lookup inside the same segment (prefix 1234, not one of the three).
ABSENT="123403$(printf '%058d' 0)"
curl -sS "http://127.0.0.1:8080/v1/lookup/$ABSENT" | jq .

# Ask the current server for a non-membership proof as well.
curl -sS "http://127.0.0.1:8080/v1/lookup/$ABSENT?absence=1" | jq .
# The scaffold does not request/verify absence_proof yet; inspect it manually
# and use the protocol §7.4 rules when implementing that feature.

# A hash outside the signed manifest's prefixes yields the stronger
# no-segment answer (present=false, no proof).
ZERO=$(printf '%064d' 0)
curl -sS "http://127.0.0.1:8080/v1/lookup/$ZERO" | jq .

# The CLI verifies the manifest HMAC and the returned proof.
/tmp/edgehash lookup -server http://127.0.0.1:8080 -hash "$H1" -key "$KEY"
```

Expected: present lookup includes `"present":true`, `proof`, `root`, `prefix`;
absent lookup has `"present":false` and no proof. The proof fields may be
`Hash`/`Index`/`Path`/`Right` in the current server. `size`/`sha256` cover the
stored segment bytes.

## 3. Test the mobile HMAC development path

The current server CLI exposes HMAC (`-key`) but may not expose Ed25519. Use HMAC
**only for development**:

```kotlin
// Android, dev only
val key = "00112233445566778899aabbccddeeff".chunked(2)
    .map { it.toInt(16).toByte() }.toByteArray()
val trust = TrustConfig.hmacLegacy(key)
```

```swift
// iOS, dev only
let key = Data([0x00,0x11,0x22,0x33,0x44,0x55,0x66,0x77,
                0x88,0x99,0xaa,0xbb,0xcc,0xdd,0xee,0xff])
let trust = TrustConfig.hmacLegacy(key: key)
```

Then point the client at the server with `allowInsecureHTTP = true` for local
testing and call `sync()`/`lookup()`.

This validates JSON parsing, canonical HMAC bytes (which match the historical
Go encoding), segment v1/v2 `"none"` parsing, cache install, and lookup proofs.
It does **not** validate Ed25519, because the running server is HMAC-signed.

## 4. Test Ed25519 and v2 uncompressed without an Ed25519 CLI

If the CLI cannot sign with Ed25519 yet, serve the deterministic fixture from
[`tests/notes.md`](tests/notes.md) with the current server (it accepts an
unsigned/Ed25519 manifest when started without `-key`, but verifies every
segment's size/checksum/root). This exercises the mobile Ed25519 verifier and
v2 parser against real HTTP.

```sh
mkdir -p /tmp/edstore/segments

# v2 segment hex from tests/notes.md (size 148, sha256 5b73...).
printf '%s' '4544475345473031000000021234000000000003000000000000001d0000000700000000000000040000000000000064c78d6d04123400000000000000000000000000000000000000000000000000000000000012340100000000000000000000000000000000000000000000000000000000001234020000000000000000000000000000000000000000000000000000000000' | xxd -r -p > /tmp/edstore/segments/1234.seg
shasum -a 256 /tmp/edstore/segments/1234.seg  # must be 5b731684...e2cc33

cat > /tmp/edstore/manifest.json <<'EOF'
{
  "version": 1,
  "prefix_bytes": 2,
  "segments": [
    {
      "prefix": 4660,
      "count": 3,
      "bloom_bits": 29,
      "bloom_k": 7,
      "root": "f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61",
      "file": "segments/1234.seg",
      "size": 148,
      "sha256": "5b7316849c9966c7ad4d5d1b9fb30d056403b8774a7171e9cb890e7271e2cc33",
      "compression": "none"
    }
  ],
  "signature": "G2RsncOaG1fV6Cg7BFrAFSiyXKx1+YVc2fxCTzC29cHf0KghbYtia4UOIsqPgNmwzUj8z2XriJouTmaT6KbODg==",
  "signature_alg": "ed25519"
}
EOF

/tmp/edgehash serve -dir /tmp/edstore -addr 127.0.0.1:8080
```

Serve **without** `-key`. The server loads with the legacy verifier disabled:
it still validates the manifest structure and all segment bytes, and serves the
Ed25519 manifest. (If your server build rejects this, use a mock HTTP server; do
not weaken the client.)

App configuration:

- Ed25519 public key (test only):
  `03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8`
- Queries:
  - present: `1234000000000000000000000000000000000000000000000000000000000000`
  - present: `12340100…` (second hash)
  - absent: `12340000…0003` (same prefix, not in the list)
  - absent/no-segment: a hash with a different prefix
- The server's lookup response will have capitalized proof keys and base64
  signature; the mobile decoder handles both.

This is an integration fixture, not a substitute for server-side Ed25519
signing/verification tests.

## 5. Emulator / simulator / device networking

- **Android emulator:** host loopback is `10.0.2.2`. Use
  `http://10.0.2.2:8080` (development cleartext exception required). Or run
  `adb reverse tcp:8080 tcp:8080` and use `http://127.0.0.1:8080`.
- **iOS Simulator:** shares the host loopback; use
  `http://127.0.0.1:8080`. For local HTTP, add a scoped ATS exception in the
  test app's `Info.plist`.
- **Physical device:** the server must listen on a reachable interface
  (`-addr :8080`), and the device must be on the same network. Prefer HTTPS with
  a trusted test certificate; do not ship ATS/cleartext exceptions.
- Test on both a modern device (API 33+ for Android Ed25519) and the oldest
  supported OS. On API < 33, assert `CryptoUnavailable` rather than silently
  using HMAC.
- Test network transitions: Wi-Fi → cellular, charging → battery, airplane
  mode, and process death. The scaffold's `DeviceConditions` are supplied by
  the app, so these are app-integration tests.

## 6. Required tests before shipping

At minimum, add these to a real Android test source set and an iOS XCTest target
(and, where possible, a JVM/SwiftPM shared test harness). Use the vectors in
[`tests/notes.md`](tests/notes.md).

### Manifest and signatures

1. Canonical bytes exactly equal C1 (HMAC legacy), C2 (Ed25519 v1 segment), and
   C3 (Ed25519 v2 `"none"` segment) byte for byte.
2. HMAC vector verifies with the 16-byte test key; any changed byte fails.
3. Ed25519 vector verifies with the pinned test key; a changed signature,
   message, key, or `signature_alg` fails.
4. Ed25519 trust rejects: absent/empty `signature_alg`, `hmac-sha256`,
   unknown algorithm, 32-byte HMAC signature, 64-byte garbage.
5. HMAC trust rejects Ed25519 signatures and unknown algorithms.
6. Signature hex and base64 encodings both work for the same bytes.
7. Manifest rejects: unsorted/duplicate prefixes, prefix > 255 with
   `prefix_bytes=1`, count <= 0, bloom bits/k out of range, unknown
   `compression`, non-canonical `file`, bad root/sha256 hex, wrong size type,
   unknown top-level/segment field, unsupported schema version.
8. Manifest accepts an empty `segments` array and a `"compression":"none"`
   v2 segment; accepts absent compression for legacy v1/v2-none.
9. Duplicate JSON keys behavior is specified/tested (reject if possible; at
   least ensure the canonical re-encoding and signature decide the truth).
10. Fingerprint equals SHA-256 of canonical bytes and changes when
    `signature_alg`, compression, root, size, or sha changes.

### Segments

11. Parse the 140-byte v1 fixture and the 148-byte v2 `"none"` fixture; check
    size, SHA-256, Bloom, sorted hashes, routing, root.
12. Corrupt each header field (magic, version, prefix, reserved/compression
    code, count, bloom bits/k/bytes, raw size) and assert rejection.
13. Truncate/append bytes, flip a hash byte, reorder two hashes, change a
    routing prefix, change the Merkle root: reject.
14. Check size/SHA-256 before parsing/decompression; a valid header with a bad
    checksum must fail without allocating the payload.
15. v2 zstd fixture: `sync` skips it with a clear reason; direct parse throws
    `unsupportedVersion`; a remote lookup for its hashes still verifies a proof.
16. v1 with `"zstd"` metadata and v2 zstd with missing metadata: reject.
17. Empty segment / count mismatch / raw size overflow: reject without huge
    allocation.
18. Cache file with wrong size is deleted; corrupt segment is deleted and
    re-fetched; a good file survives process restart.

### Merkle

19. Root for one leaf = leaf hash; root for three leaves uses padding with
    `empty`; root for an empty set = `empty`.
20. Proof for index 0/1/2 verifies; tampered index, `right`, path element,
    root, or proof hash fails.
21. Proof depth mismatch (extra/missing level), index >= count, path/right
    length mismatch, and depth > 62 fail.
22. Domain separation: a 64-byte node interpreted as two hashes must not
    verify; empty leaves cannot be used as membership proofs.

### Bloom

23. `m=29,k=7` fixture positions and bitset (`c78d6d04`) match.
24. `maybeContain` true for all inserted hashes; false for the known absent
    hashes (unless a legitimate false positive—then assert exact lookup says
    absent).
25. Bit order is LSB-first; `h2` is forced odd; unsigned 64-bit wraparound and
    modulo match Go.
26. A filter with a false negative is rejected when parsing a segment.
27. All positions stay in range for `m` not divisible by 8; trailing bits are
    ignored.

### Lookup

28. Present proof verifies against the signed root; result source
    `REMOTE_PROOF`, `verified=true`.
29. Proof for another hash, prefix mismatch, root mismatch (`staleManifest`),
    index outside count, depth mismatch, malformed path/right: reject.
30. Lower-case and capitalized proof keys both parse; unknown fields do not
    become security-relevant.
31. Absent: local `NO_SEGMENT` from signed manifest is exact; remote
    `present=false` is `verified=false`; a future absence field is not treated
    as verified.
32. 404 treated as absent (compatibility) with `verified=false`; 5xx and
    malformed JSON produce explicit errors.

### Cache / policy

33. Atomic install: simulate write/rename failure; never expose partial bytes.
34. Quota: LRU eviction order, pinned prefixes survive, a segment larger than
    quota is skipped with a reason, total stays <= quota.
35. TTL: expired manifest triggers refresh; expired segments are evicted;
    offline + `allowStale` returns `staleManifest=true`; offline without stale
    fails.
36. `force=true` bypasses Wi-Fi/charging policy but not signature/checksum/
    quota.
37. Wi-Fi/cellular and charging/battery decisions produce the expected
    `skippedReason`/`deferredReason`; metered lookup policy is enforced.
38. Restart with a corrupt/unsigned manifest, a stale fingerprint, a missing
    segment, and a manifest whose segments changed; no old Bloom answer is
    used for the new generation.
39. Concurrent `sync`/`lookup`: Android serialized sync; iOS
    `syncInProgress`; no torn reads; a proof racing a manifest change surfaces
    stale rather than accepting a wrong root.
40. Cache path is app-private; file names are derived from validated prefixes
    (no traversal); symlink behavior is defined.

### Transport and robustness

41. `If-None-Match` 304 path; ETag missing; oversized Content-Length; chunked
    response exceeding `maxBytes`; non-2xx bodies bounded.
42. Redirect to a different host/scheme rejected; HTTPS-only enforcement;
    timeout/connect errors map to `Transport`.
43. Fuzz manifest JSON and segment bytes; assert bounded memory and no crashes.
44. Cross-language: compare canonical bytes, roots, Bloom bitsets, segment
    SHA-256, and proof verification with Go/Python reference vectors.
45. Key rotation/unknown key: fail closed, no fallback to HMAC.
46. Device tests: API 26/33+, iOS 16/current; airplane mode; low storage;
    clock skew; OS cache purge; process death during sync.

## 7. What remains untested in this repository

- Android compile, lint, unit tests, instrumentation, API 26 vs 33 behavior.
- iOS build/link, simulator/device run, CryptoKit behavior, actor concurrency
  under load.
- Real HTTP/2, proxies, CDNs, TLS, ATS/cleartext configuration.
- zstd segments and decompression limits.
- Absence proofs: the server supports `?absence=1` and `absence_proof`; the
  mobile scaffold neither requests nor verifies them.
- Background sync, WorkManager/BGTaskScheduler, battery/network observers.
- Key rotation, multi-key keyrings, rollback prevention.
- Memory/CPU/disk benchmarks and large-store behavior.
- Security review and penetration testing.

A green Swift typecheck or a successful Android compile would still not be
enough: the trust chain and cache staleness behavior need the tests above plus
an end-to-end Ed25519 server.
