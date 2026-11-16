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

The Go server now exposes Ed25519, segment v2/zstd, absence proofs, and
auth/rate/TLS/range options. The recipes below target that CLI; if your checkout
differs, check `-h` and inspect the served manifest. The temporary harnesses are
not committed; the required tests below must live in a real Android/iOS test
target.

`org.json` stubs only implement the methods this file calls. Android-specific
behavior such as `JSONObject.keys()`, number coercion, and exception messages
still needs a real device/instrumentation test.

Reference values live in [`tests/notes.md`](tests/notes.md).

## 1. Build the server and an Ed25519 fixture

From the repository root:

```sh
go build -o /tmp/edgehash ./cmd/edgehash

# Three hashes. Use shasum(1) or sha256sum(1).
printf 'edgehash-mobile-vector-a\n' | shasum -a 256 | awk '{print $1}' > /tmp/hashes.txt
printf 'edgehash-mobile-vector-b\n' | shasum -a 256 | awk '{print $1}' >> /tmp/hashes.txt
printf 'edgehash-mobile-vector-c\n' | shasum -a 256 | awk '{print $1}' >> /tmp/hashes.txt
cat /tmp/hashes.txt

# Test-only Ed25519 key. Do not reuse it for anything real.
openssl genpkey -algorithm ED25519 -out /tmp/edge-ed25519.pem
openssl pkey -in /tmp/edge-ed25519.pem -pubout -out /tmp/edge-ed25519.pub.pem

# The mobile SDK pins the raw 32-byte key, not the PEM file.
openssl pkey -pubin -in /tmp/edge-ed25519.pub.pem -outform DER \
  | tail -c 32 | xxd -p -c 32
```

Build a store with uncompressed v2 segments and an Ed25519 manifest:

```sh
/tmp/edgehash build -in /tmp/hashes.txt -out /tmp/edge-store \
  -ed25519-key /tmp/edge-ed25519.pem -compression none
```

`-compression none` is required for offline mobile caching in this scaffold;
`auto`/`zstd` may produce segments the mobile client skips. Check the manifest:

```sh
jq '{version, signature_alg, segment: (.segments[0] | {prefix,size,sha256,compression})}' \
  /tmp/edge-store/manifest.json
```

Expected: `signature_alg` is `"ed25519"`, `compression` is `"none"` (unless
your build/service changed defaults). The signature is base64 in the current Go
JSON; the mobile decoder accepts base64 or hex.

For a larger store, `-prefix-bytes`, `-fp`, `-compression none|zstd|auto` and
`-key` are available. HMAC remains supported for compatibility.

## 2. Serve and inspect with curl

```sh
/tmp/edgehash serve -dir /tmp/edge-store -addr 127.0.0.1:8080 \
  -verify-ed25519 /tmp/edge-ed25519.pub.pem
```

Do not pass `-key`; `-key` and `-verify-ed25519` are mutually exclusive. In
another shell:

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

# Absent lookup inside the same segment: same routing prefix, different tail.
ABSENT="${HEX}$(printf '%060d' 0)"
curl -sS "http://127.0.0.1:8080/v1/lookup/$ABSENT" | jq .

# Ask the current server for a non-membership proof.
curl -sS "http://127.0.0.1:8080/v1/lookup/$ABSENT?absence=1" | jq .
# The mobile scaffold does not request/verify absence_proof yet; inspect it
# manually and use protocol §7.4 when implementing that feature.

# A hash outside the signed manifest's prefixes yields the stronger
# no-segment answer (present=false, no proof).
ZERO=$(printf '%064d' 0)
curl -sS "http://127.0.0.1:8080/v1/lookup/$ZERO" | jq .

# The CLI verifies the Ed25519 manifest and any Merkle/absence proof.
/tmp/edgehash lookup -server http://127.0.0.1:8080 -hash "$H1" \
  -verify-ed25519 /tmp/edge-ed25519.pub.pem
```

Expected: present lookup includes `"present":true`, `proof`, `root`, `prefix`;
absent lookup has `"present":false` and no proof unless `?absence=1` was asked.
The proof fields may be `Hash`/`Index`/`Path`/`Right` in the current server.
`size`/`sha256` cover the stored segment bytes.

For local HTTP, the mobile client needs an explicit development cleartext/ATS
exception (see §4). Do not add `-auth-token` yet: the scaffold has no Bearer
token configuration. If you test auth, use curl/`httpie` and add the header to
the transport first.

## 3. HMAC development path

Build and serve a separate HMAC store on port 8081:

```sh
KEY=00112233445566778899aabbccddeeff
/tmp/edgehash build -in /tmp/hashes.txt -out /tmp/edge-store-hmac \
  -key "$KEY" -compression none
/tmp/edgehash serve -dir /tmp/edge-store-hmac -addr 127.0.0.1:8081 -key "$KEY"
```

The HMAC manifest has no `signature_alg` and a base64 tag. Configure mobile
trust explicitly:

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

Then point the client at `http://127.0.0.1:8081` with
`allowInsecureHTTP = true` for local testing and call `sync()`/`lookup()`.

This validates JSON parsing, canonical HMAC bytes (matching the historical Go
encoding), segment v1/v2 `"none"` parsing, cache install, and lookup proofs. It
does **not** validate Ed25519. Use the Ed25519 store in §1 for that; never ship
a shared HMAC key in a mobile binary.

## 4. Emulator / simulator / device networking

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

## 5. Required tests before shipping

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
31. Absent: local `NO_SEGMENT` from the signed manifest is exact; remote
    `present=false` without a verified proof is `verified=false`; if
    `absence_proof` support is added, verify the boundary/neighbour rules from
    protocol §7.4 before marking it verified.
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

## 6. What remains untested in this repository

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
