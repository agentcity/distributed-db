# EdgeHash wire protocol (mobile view)

Status: protocol reference for the **scaffold** in `mobile/`. It describes the
immutable edge hash set served by `cmd/edgehash` / `internal/edgehttp`, with the
hardening additions in `internal/edgeset` (Ed25519 signatures, segment format
v2, optional zstd). It is not an RFC; where the mobile scaffold is narrower than
the server, that is called out explicitly.

Companion documents: [`README.md`](README.md), [`TESTING.md`](TESTING.md), and
[`tests/notes.md`](tests/notes.md) (reference vectors).

## 1. Conventions

- Hash values are SHA-256 digests: exactly 32 bytes, encoded as **64 lowercase
  hex characters** on the wire. Decoders accept uppercase hex, but emitters use
  lowercase.
- All integers are unsigned big-endian in binary structures, and JSON numbers
  in JSON structures. No floating point is used for counts or sizes.
- JSON is UTF-8. The canonical signing input is compact JSON (no whitespace)
  with a fixed field order; the HTTP body may have a trailing newline which is
  **not** signed.
- Segment ids ("prefixes") are 16-bit integers. In URLs and file names they are
  four lowercase hex characters (`%04x`), for example `1234`.
- The set is immutable. An update is a new manifest and possibly new segment
  files. There is no delete or append HTTP operation.
- The manifest `version` is the **manifest schema version** (`1`), not a content
  generation number. Use the manifest fingerprint (§5.3) to distinguish
  generations.

## 2. Endpoints

The server is read-only. `NewServerWithOptions` supports optional Bearer-token
authentication, per-IP rate limiting, TLS and a segment-size cap; the zero-value
server has none. Segment responses support byte ranges. The mobile scaffold does
not send an auth token and does not use ranges. Put the endpoint behind TLS and
a gateway; the manifest signature is what makes the data trustworthy, not the
transport alone.

| Method | Path | Response |
|---|---|---|
| `GET` | `/healthz` | `{"status":"ok"}` |
| `GET` | `/v1/manifest` | Manifest JSON, `ETag`, `Cache-Control: no-cache` |
| `GET` | `/v1/manifest/version` | `{"version":1}`; informational, unsigned |
| `GET` | `/v1/segment/{prefix}` | Raw segment bytes, `Content-Type: application/octet-stream` |
| `GET` | `/v1/lookup/{hash}` | JSON membership answer with optional inclusion proof |
| `GET` | `/v1/lookup/{hash}?absence=1` | Absent answer can include `absence_proof` |

Details:

- `{prefix}` is exactly four hex characters. A malformed prefix is `400`; an
  unknown prefix is `404`.
- `{hash}` is exactly 64 hex characters. A malformed hash is `400`.
- Segment responses carry `ETag: "<manifest sha256>"` and
  `Cache-Control: public, max-age=31536000, immutable`. Conditional
  `If-None-Match` returns `304`.
- The manifest response carries an `ETag` derived from the whole body and
  `Cache-Control: no-cache`; conditional requests return `304`.
- `/v1/manifest/version` is not signed and must not be used as a trust or
  freshness signal.
- Segment requests accept a single or multipart `Range`; ranges are over the
  stored bytes (for zstd segments, not the decoded payload). Responses may be
  `206`/`416` and carry `Accept-Ranges: bytes`. Mobile does not use ranges.
- Protected servers require `Authorization: Bearer <token>` on every endpoint
  except an exempt `/healthz`; mobile has no token configuration.
- Redirects are not part of the contract. The mobile transports do not follow
  redirects (Android) or reject a redirect that leaves HTTPS (iOS).

## 3. Manifest JSON

Example (Ed25519, format v2 uncompressed segment):

```json
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
  "signature": "<base64 or lowercase hex>",
  "signature_alg": "ed25519"
}
```

| Field | Type | Meaning / constraint |
|---|---|---|
| `version` | integer | Must be `1`. Schema version, not a generation counter. |
| `prefix_bytes` | integer | `1..4`. Routing uses 1 byte when `1`, otherwise the first 2 bytes (current server caps segment identity at `uint16`). |
| `segments` | array | Strictly increasing by `prefix`. Empty is valid. At most 65536 entries. |
| `segments[].prefix` | integer | `0..65535`; with `prefix_bytes=1`, `0..255`. |
| `segments[].count` | integer | Positive number of hashes in the segment. |
| `segments[].bloom_bits` | integer | `m`, `1..2^36`. Must equal `ceil(-count*ln(p)/(ln 2)^2)` for the build's target `p` (default `0.01`), but clients treat it as authoritative. |
| `segments[].bloom_k` | integer | `k`, `1..64`. |
| `segments[].root` | hex | 32-byte Merkle root over the sorted hash list (§7). |
| `segments[].file` | string | Exactly `segments/%04x.seg` for that prefix (lowercase). Reject traversal/other names. |
| `segments[].size` | integer | Byte length of the **stored** segment file (possibly compressed). Positive; at least the header size. |
| `segments[].sha256` | hex | SHA-256 of the complete stored segment file. |
| `segments[].compression` | string, optional | `"none"` or `"zstd"` for segment format v2; absent/empty for legacy v1. |
| `signature` | string | Required by mobile. Current Go emits standard base64 because `[]byte`; the mobile scaffold also accepts hex. Length depends on algorithm. |
| `signature_alg` | string, optional | `"ed25519"` or `"hmac-sha256"`. Absent/empty means legacy HMAC-SHA256. |

The mobile parsers reject unknown manifest and segment fields, unknown
`signature_alg`, non-integer numbers, malformed hex, non-canonical file names,
and segment lists that are not strictly sorted. This is deliberate: silently
ignoring new unsigned fields is how protocol confusion starts.

`size` is not derived from `count`/Bloom parameters for v2: a zstd payload's
stored size is only known from the file. The server validates `size` against
the bytes it loads; mobile validates it after download.

## 4. Signatures

Two algorithms are defined:

| `signature_alg` | Signature | Trust |
|---|---|---|
| `ed25519` | 64 bytes, RFC 8032 Ed25519 over the canonical bytes | Public key pinned in the app |
| `hmac-sha256` | 32 bytes, HMAC-SHA256 over the canonical bytes | Shared secret; development only |

Absent/empty `signature_alg` is legacy HMAC-SHA256 (the current Go default).
The manifest is never used unsigned by the mobile scaffold.

Signature encoding on the wire:

- Current Go `encoding/json` encodes `Manifest.Signature []byte` as **standard
  base64 with padding** (`+`, `/`, `=`).
- The mobile decoders accept standard base64 and lowercase/uppercase hex. A
  future wire revision should choose one encoding; hex is recommended for
  consistency with hashes.
- Do not guess the algorithm from the signature length. Read
  `signature_alg`. A pinned Ed25519 verifier must reject a manifest whose
  `signature_alg` is absent or not `ed25519`; this prevents downgrade to HMAC.

## 5. Canonical JSON and manifest fingerprint

### 5.1 Exact canonical bytes

The signed payload is compact UTF-8 JSON with this exact structure and field
order (no spaces, no newline):

```text
{"version":<int>,"prefix_bytes":<int>,"segments":[
  {"prefix":<int>,"count":<int>,"bloom_bits":<int>,"bloom_k":<int>,
   "root":"<64 hex>","file":"<string>","size":<int>,"sha256":"<64 hex>"
   [,"compression":"none"|"zstd"]}
][,"signature_alg":"ed25519"|"hmac-sha256"]}
```

- `segments` preserves manifest order (which is strictly increasing).
- `compression` is included **only when non-empty**, at the end of the segment
  object, after `sha256`.
- `signature_alg` is included **only when non-empty**, at the end of the
  top-level object, after the `segments` array.
- `signature` is always excluded.
- Strings use JSON escaping. In practice `file` and `compression` are
  constrained ASCII; Go also escapes `<`, `>`, `&`, U+2028 and U+2029.
- Numbers are decimal integers without leading `+`, zero padding, exponent or
  fractional part.

This matches `internal/edgeset.canonicalManifest` (a struct encoding with
`signature_alg` and `compression` `omitempty`). Legacy HMAC manifests have no
`signature_alg` and therefore keep the byte stream signed by older versions.

Reference canonical payloads (also in [`tests/notes.md`](tests/notes.md)):

```text
C1 (legacy HMAC, segment format v1):
{"version":1,"prefix_bytes":2,"segments":[{"prefix":4660,"count":3,"bloom_bits":29,"bloom_k":7,"root":"f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61","file":"segments/1234.seg","size":140,"sha256":"badf8a2d07565eb7a69863b4e852c48710f1cd9e56b5c9bb0f4ecbdcdc4c11d7"}]}
```

```text
C2 (Ed25519, same v1 segment):
{"version":1,"prefix_bytes":2,"segments":[{"prefix":4660,"count":3,"bloom_bits":29,"bloom_k":7,"root":"f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61","file":"segments/1234.seg","size":140,"sha256":"badf8a2d07565eb7a69863b4e852c48710f1cd9e56b5c9bb0f4ecbdcdc4c11d7"}],"signature_alg":"ed25519"}
```

```text
C3 (Ed25519, v2 uncompressed segment):
{"version":1,"prefix_bytes":2,"segments":[{"prefix":4660,"count":3,"bloom_bits":29,"bloom_k":7,"root":"f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61","file":"segments/1234.seg","size":148,"sha256":"5b7316849c9966c7ad4d5d1b9fb30d056403b8774a7171e9cb890e7271e2cc33","compression":"none"}],"signature_alg":"ed25519"}
```

### 5.2 Verification procedure (mobile)

1. Parse JSON and validate structure/constraints.
2. Re-encode the canonical bytes from the parsed values (never reserialize the
   raw JSON object; key order must not depend on a JSON library).
3. Verify the signature over those bytes with the pinned key/algorithm.
4. Only then use any segment metadata.

Verification failures are fatal for the whole manifest. There is no "ignore bad
signature and continue with cached segments" path.

### 5.3 Fingerprint

The mobile cache computes:

```text
fingerprint = SHA-256(canonical_bytes)
```

It is not transmitted as a separate field; the app stores it with the cached
manifest. It identifies a manifest generation for cache keys and staleness
checks, unlike `version` (always 1). It does not prevent rollback by itself: an
old validly signed manifest has a valid fingerprint. See §11.

## 6. Segment file format

All integers are big-endian. `size`/`sha256` in the manifest cover the complete
stored file, including the header, and are checked **before** parsing or
decompressing.

### 6.1 Format v1 (legacy, uncompressed)

| Offset | Size | Field |
|---:|---:|---|
| 0 | 8 | magic `"EDGSEG01"` |
| 8 | 4 | format version `1` |
| 12 | 2 | segment prefix (`uint16`) |
| 14 | 2 | reserved, must be `0` |
| 16 | 4 | hash count |
| 20 | 8 | Bloom bit count `m` |
| 28 | 4 | Bloom probe count `k` |
| 32 | 8 | Bloom bitset byte length `(m+7)/8` |
| 40 | ... | Bloom bitset, then `count * 32` sorted hashes |

The manifest has no `compression` field for v1 (or may say `"none"`; the server
also tolerates that). The payload length is exactly `40 + bloom_bytes +
count*32`.

### 6.2 Format v2 (compression-capable)

| Offset | Size | Field |
|---:|---:|---|
| 0 | 8 | magic `"EDGSEG01"` |
| 8 | 4 | format version `2` |
| 12 | 2 | segment prefix |
| 14 | 1 | compression code: `0` none, `1` zstd |
| 15 | 1 | reserved, must be `0` |
| 16 | 4 | hash count |
| 20 | 8 | Bloom bit count `m` |
| 28 | 4 | Bloom probe count `k` |
| 32 | 8 | Bloom bitset byte length `(m+7)/8` (uncompressed) |
| 40 | 8 | raw payload size: `bloom_bytes + count*32` |
| 48 | ... | stored payload: Bloom bitset + hashes, raw or zstd frame |

For `compression:"none"`, stored payload length is `raw_size` and the file size
is `48 + raw_size`. For `compression:"zstd"`, the payload is one zstd frame that
decodes to exactly `raw_size` bytes.

The manifest `compression` must match the header code. As a compatibility
allowance, an uncompressed v2 segment may omit `compression` in the manifest,
but a zstd header may **not** be treated as absent. v1 with `"zstd"` is
corrupt. Unknown format versions and compression codes are rejected.

Validation order in the server (and the mobile parser):

1. manifest `size` equals actual file length;
2. manifest `sha256` equals SHA-256 of the stored file;
3. format/header fields, prefix, count, Bloom parameters;
4. raw size consistency;
5. decompression (if any), bounded by the authenticated raw size;
6. payload length, sorted order, routing prefix;
7. Merkle root equals the manifest root;
8. every present hash passes the Bloom filter (detects Bloom corruption).

### 6.3 Mobile limitation

The Android/iOS scaffolds have **no zstd decompressor** (no third-party
dependencies). They:

- verify and use v1 and v2 `"none"` segments;
- skip `"zstd"` segments during `sync()` and continue to serve their hashes
  through remote inclusion proofs;
- reject a directly parsed zstd segment with an explicit
  "unsupported version" error.

For offline coverage, build the store with uncompressed segments
(`BuildOptions.Compression = "none"`; CLI flag availability depends on the
server build). Zstd support is a production item, not silently ignored.

## 7. Hashes, routing and Merkle tree

### 7.1 Hashes and routing

- A hash is 32 bytes. `prefix(hash) = hash[0]` when `prefix_bytes == 1`,
  otherwise `be16(hash[0:2])`. Current Go also caps `prefix_bytes` 3/4 at two
  bytes.
- Segment file `segments/%04x.seg`; URL `/v1/segment/%04x`.
- Hashes inside a segment are sorted by unsigned lexicographic byte order and
  must be strictly increasing. All hashes must route to the segment prefix.

### 7.2 Domain-separated tree

```text
leaf  = SHA-256("edgeset:merkle-leaf:v1"  || 0x00 || hash)
node  = SHA-256("edgeset:merkle-node:v1"  || 0x00 || left || right)
empty = SHA-256("edgeset:merkle-empty-leaf:v1")
```

The domains include the literal NUL byte except `empty`, which is the SHA-256
of the ASCII string with no NUL. `left`/`right`/`empty` are 32 bytes.

For `n > 0`, let `size` be the smallest power of two `>= n`. Build a complete
tree with the `n` leaves followed by `size-n` copies of `empty`; repeatedly pair
adjacent nodes. The root is the single node at the top. For an empty set (no
segments), the root is `empty`; a segment file itself always has `count > 0`.

### 7.3 Inclusion proof

```json
{
  "hash": "<32-byte hash>",
  "index": 1,
  "path": ["<leaf0>", "<node(leaf2,empty)>"],
  "right": [false, true],
  "root": "<segment root>"
}
```

- `path[i]` is the sibling at level `i`, from the leaf level upward.
- `right[i] == true` means the sibling is the right child, i.e. bit `i` of the
  leaf index is `0`.
- `index` is the position in the sorted hash list.

Verification, given the authenticated `root` and queried `hash`:

```text
reject if proof.hash != hash
depth = path.length
require depth == right.length
require depth <= 62
require depth == ceil(log2(segment.count))
require 0 <= index < segment.count
for i in 0..depth-1:
    require right[i] == ((index >> i) & 1 == 0)
node = leaf(hash)
for i in 0..depth-1:
    node = right[i] ? node(node, path[i]) : node(path[i], node)
require node == root
```

The mobile SDK additionally requires the proof length to equal the tree depth
derived from the signed segment `count`; a proof with extra levels is rejected
even if it hashes to the root. The root is always taken from the manifest; a
server-provided `root` is only an extra equality check (a mismatch means the
server is on another manifest generation).

The Go server's inclusion proof JSON currently uses exported field names
(`Hash`, `Index`, `Path`, `Right`) because the Go type has no JSON tags. The
mobile parsers accept both those names and the canonical lower-case names.
The canonical contract for future servers is lower-case; do not make security
depend on JSON key case.

Worked example: for the three-hash segment in `tests/notes.md`, the root is
`f99ca0d8…fb61`; the proof for index 1 is path `[leaf0, node(leaf2, empty)]`
with `right = [false, true]`.

### 7.4 Absence

The current Go server returns a Merkle non-membership proof when the client asks
for it:

```http
GET /v1/lookup/<hash>?absence=1
```

If the hash is absent and its routing prefix has a segment, the response is:

```json
{
  "present": false,
  "absence_proof": {
    "Prefix": 4660,
    "Root": "<64 hex>",
    "Left": {
      "Hash": "<64 hex>",
      "Index": 2,
      "Path": ["<emptyLeaf>", "<node>"],
      "Right": [true, false]
    },
    "Right": null
  },
  "root": "<64 hex>",
  "prefix": "1234"
}
```

`AbsenceProof` has no JSON tags in the Go type, so its field names are
capitalized (`Prefix`, `Root`, `Left`, `Right`), like the nested `Proof` fields.
`Left`/`Right` are null when that neighbour does not exist.

Verification, against the authenticated segment root:

- `proof.Root` must equal the root from the signed segment metadata, and
  `proof.Prefix` must be the prefix that routes the queried hash.
- If both `Left` and `Right` are present: each is a normal inclusion proof
  against the root, `Right.Index == Left.Index + 1`, and
  `Left.Hash < hash < Right.Hash`.
- If only `Right` is present: it proves index 0 and `hash < Right.Hash`.
- If only `Left` is present: it proves the last leaf and `Left.Hash < hash`.
  "Last leaf" is checked by requiring every right-hand sibling on the path to be
  the canonical root of an all-empty subtree at that level:
  `emptyRoot[0] = emptyLeaf`, `emptyRoot[i] = node(emptyRoot[i-1], emptyRoot[i-1])`.
  This prevents using an arbitrary leaf as the "last" leaf.
- The server returns no absence proof when the hash's prefix has no segment;
  the signed manifest itself is the evidence for that case.

**The mobile scaffold does not implement this verification or request
`?absence=1`.** A plain `present=false` must be reported as unverified. The
server can be used for presence-only applications, or absence verification can
be added as a production item. The proof is only as fresh as the manifest root
it verifies against; a later generation may contain the hash.

## 8. Bloom filter

Parameters for `n = count` items at target false-positive rate `p` (default
`0.01`):

```text
m = ceil(-n * ln(p) / (ln 2)^2)
k = round((m / n) * ln 2)
```

`m` is clamped to at least 1 and at most `2^36`; `k` to `1..64`. The server
computes these at build time and records them in the manifest and segment
header.

Hashing and bit order:

```text
digest = SHA-256("edgeset:bloom:v1" || 0x00 || hash)
h1 = big-endian uint64(digest[0:8])
h2 = big-endian uint64(digest[8:16]) | 1
x = h1
for i in 0..k-1:
    pos = x mod m
    byte = pos >> 3
    bit  = 1 << (pos & 7)      # least-significant bit first
    require bits[byte] & bit != 0      # for maybeContain
    x = (x + h2) mod 2^64
```

- A negative answer means "definitely not in this manifest generation" (no
  false negatives for correctly built filters).
- A positive answer means "maybe"; resolve with the exact sorted hash list or a
  remote proof.
- Bloom filters cannot be updated in place. Removing hashes or changing the set
  requires a new generation and new filters.
- The server verifies every hash against the Bloom filter at segment load, so a
  corrupt filter is rejected rather than producing a false negative.

## 9. Lookup response

Present:

```json
{
  "present": true,
  "proof": {
    "hash": "<64 hex>",
    "index": 1,
    "path": ["<64 hex>", "<64 hex>"],
    "right": [false, true]
  },
  "root": "<64 hex>",
  "prefix": "1234"
}
```

Absent without `?absence=1` (current server):

```json
{"present":false,"root":"","prefix":"1234"}
```

Absent with `?absence=1` and an existing segment includes `absence_proof` and
the segment root (see §7.4).

Current Go emits `proof` with capitalized keys as noted in §7.3 and may omit
`root` for absent lookup. The mobile client:

1. validates the response prefix if present;
2. requires a proof when `present=true`;
3. requires `proof.hash == queried hash`;
4. requires `index <` the signed segment `count`;
5. requires the proof depth to match the signed `count`;
6. verifies against the signed segment root;
7. if the server supplies `root`, requires it to equal the signed root; a
   mismatch is reported as `staleManifest` so the caller can re-sync rather than
   accepting a proof from a newer generation.

`404` is treated as absent for compatibility with older servers. Without
`?absence=1` (and without client-side verification), absence is not verified. A
local `NO_SEGMENT` answer (no segment for the routing prefix in the signed
manifest) is stronger: it is evidence from the signed manifest itself.

## 10. Cache semantics (why Bloom can lie)

1. The app downloads and verifies a manifest with its pinned key.
2. It stores the raw manifest, its fingerprint, its fetch time and ETag.
3. It downloads selected segment files, verifies `size`, `sha256`, header,
   sorted order, routing, root and Bloom, then installs atomically.
4. Local lookup:
   - no segment for the prefix -> absent (from signed manifest);
   - Bloom negative -> absent for that generation;
   - Bloom maybe + exact hit -> present;
   - Bloom maybe + exact miss -> absent (false positive resolved locally);
   - no cached segment -> remote lookup + proof.
5. Local answers are only reliable for the manifest generation they were
   verified against. An old Bloom filter can produce a **false negative** for a
   hash added in a newer generation.

Consequences:

- Never merge segments or Bloom filters from different manifests.
- Never use a local negative after the manifest TTL without either refreshing
  or surfacing the staleness to the caller.
- `manifest.version` is not a generation counter; compare the fingerprint.
- A stale manifest is not detected by the signature alone. The mobile cache
  stores `fetchedAt` and enforces a TTL; this is a local availability/freshness
  heuristic, not a cryptographic guarantee.
- If a remote proof fails against the local signed root, treat it as a stale
  generation and re-sync; do not retry the same proof.
- Deletion requires a new generation. Bloom filters do not support deletion.

## 11. Versioning and rollback

- Schema version is `1`. New optional manifest fields must be covered by the
  signature (as `signature_alg` and `compression` are) and are rejected by
  clients that do not know them.
- Segment format version is independent: v1 and v2 are distinguished by the
  file header, and `compression` in the manifest describes v2. A future v3 must
  bump the header version and be negotiated; clients reject unknown versions.
- The protocol currently has no signed generation number, timestamp, expiry or
  transparency log. A party that can serve HTTP can replay an older valid
  manifest and segments. Mitigations are app/server policy: pin a minimum
  fingerprint/generation, add a signed `generation`/`not_after` field in a
  future version, or distribute a checkpoint. The scaffold does not pretend TTL
  solves rollback.
- Key rotation is not implemented. Rotating the Ed25519 key requires an app
  update (or a future multi-key keyring with an overlap window).

## 12. Compatibility matrix

| Server output | Mobile scaffold | Notes |
|---|---|---|
| Manifest v1 HMAC-SHA256, absent `signature_alg` | Supported with explicit `hmacLegacy` trust | Current Go default; development only |
| Manifest v1 Ed25519, `signature_alg:"ed25519"` | Supported with pinned Ed25519 key | Go library/HTTP options support verify; CLI signing flags may lag |
| Segment format v1 | Supported | Legacy stores |
| Segment format v2, `compression:"none"` | Supported | Recommended for mobile offline cache |
| Segment format v2, `compression:"zstd"` | Manifest accepted; segment skipped; remote proofs still work | No zstd dependency in mobile scaffold |
| `present=false` without absence proof | Accepted as unverified absence | Mobile does not request `?absence=1` yet |
| `absence_proof` | Not consumed by mobile | Server supports `?absence=1`; production gap |
| Lower-case proof keys | Supported | Canonical target |
| Capitalized proof keys | Supported | Current Go encoder |
| Unknown manifest field/version | Rejected | Fail closed |

At the time this scaffold was written, `cmd/edgehash` exposed HMAC `-key` but
not Ed25519/compression flags, while `internal/edgehttp` already accepted a
custom `Verifier`, optional auth/rate/TLS/range, and `?absence=1`. Check
`edgehash build -h` and the served manifest: if it has no
`signature_alg:"ed25519"`, do not ship the Ed25519 trust mode against that
server. This is a server integration gap, not something the mobile client can
fix.
