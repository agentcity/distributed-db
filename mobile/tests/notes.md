# EdgeHash mobile test vectors and implementation notes

These vectors are **test-only**. The private seed and HMAC key below are
published on purpose; never use them in a server or app.

They are designed to catch the mistakes mobile ports make most often:

- hashing the canonical JSON with a different key order or whitespace;
- omitting `signature_alg` or `compression` from the canonical payload;
- signing the pretty-printed manifest instead of the compact canonical bytes;
- computing Merkle leaves without domain separation, or padding with a zero
  hash instead of `emptyLeaf`;
- getting the `right` flag backwards, or using the wrong proof depth;
- Bloom double-hashing with little-endian words, a non-odd step, or MSB-first
  bit order;
- confusing the v1 40-byte header with the v2 48-byte header, or treating
  `size`/`sha256` as covering the uncompressed payload.

## 0. Reference Python for the primitive calculations

```python
import hashlib
H = lambda b: hashlib.sha256(b).digest()
leaf = lambda h: H(b"edgeset:merkle-leaf:v1\x00" + h)
node = lambda l, r: H(b"edgeset:merkle-node:v1\x00" + l + r)
empty = H(b"edgeset:merkle-empty-leaf:v1")
```

All signatures below are over the exact UTF-8 strings `C1`, `C2`, `C3` in
§4 — no trailing newline, no spaces. The HTTP body may add a newline; that
newline is not signed.

## 1. Synthetic hashes

The three hashes deliberately share prefix `0x1234` so they belong to one
segment. They are not SHA-256 outputs of anything; they are bit patterns:

```text
H1 = 1234000000000000000000000000000000000000000000000000000000000000
H2 = 1234010000000000000000000000000000000000000000000000000000000000
H3 = 1234020000000000000000000000000000000000000000000000000000000000
```

Routing prefix with `prefix_bytes=2`: `0x1234`; segment file
`segments/1234.seg`; URL `/v1/segment/1234`.

An absent hash in the same segment (used by tests):

```text
A  = 1234030000000000000000000000000000000000000000000000000000000000
```

## 2. Merkle values

```text
emptyLeaf = db679495c4d18288f0cc6db743e825ce158e9acc3b52d09e7b8167639b04b410

leaf(H1)  = 78021b29bca43f84ffcf3c0d5eecfc8f7caa4495d4cf336697fc5dfc6121f0d7
leaf(H2)  = 0f825feb8034d72a2da4cec7a3bdf3bfe3ed3339922cc104c2dc9ae78c0b8a2a
leaf(H3)  = 34381bf9001fb3738adeafe65e1ce90a92d31fbd33f9eda364dbb28867c105fa

node(leaf(H1), leaf(H2)) = c2a9408d1ec920e571bda1bdb4d7f7f18979b2e344bf56abe6b5e2995289b00d
node(leaf(H3), emptyLeaf) = d1d591d672dc3edba2c17d00368057638f1f5c2b1762fe78d68fcbab943c2cd3

root = node(c2a9408d…b00d, d1d591d6…2cd3)
     = f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61
```

Tree size is 4 (next power of two >= 3), leaves `[L1,L2,L3,emptyLeaf]`.

Inclusion proof for `H2` at index `1`:

```json
{
  "hash": "1234010000000000000000000000000000000000000000000000000000000000",
  "index": 1,
  "path": [
    "78021b29bca43f84ffcf3c0d5eecfc8f7caa4495d4cf336697fc5dfc6121f0d7",
    "d1d591d672dc3edba2c17d00368057638f1f5c2b1762fe78d68fcbab943c2cd3"
  ],
  "right": [false, true]
}
```

Proof for `H3` at index `2` (path `[emptyLeaf, node(L1,L2)]`, `right=[true,false]`):

```text
path[0] = db679495c4d18288f0cc6db743e825ce158e9acc3b52d09e7b8167639b04b410
path[1] = c2a9408d1ec920e571bda1bdb4d7f7f18979b2e344bf56abe6b5e2995289b00d
right   = [true, false]
```

Single-hash root: `leaf(H1)` (no levels). Empty set root: `emptyLeaf`.

## 3. Bloom filter

`n=3`, `p=0.01`:

```text
m = ceil(-3 * ln(0.01) / (ln 2)^2) = 29
k = round((29/3) * ln 2)          = 7
bitset bytes = (29+7)/8           = 4
```

Bitset (hex): `c78d6d04`; bytes are LSB-first.

Position sequences (`h1`, then `pos_i = (h1 + i*step) mod 29`):

```text
H1: h1=2030586405230101635, step=15357222266843674783
    positions = [19, 7, 0, 22, 15, 8, 1]

H2: h1=428644447244930759,  step=902918631119815417
    positions = [7, 2, 26, 21, 16, 11, 6]

H3: h1=5676160706173053478, step=6577201639289129535
    positions = [18, 26, 10, 18, 26, 10, 18]
```

For H1 the byte/bit writes are:

```text
19 -> byte 2, bit 3
 7 -> byte 0, bit 7
 0 -> byte 0, bit 0
22 -> byte 2, bit 6
15 -> byte 1, bit 7
 8 -> byte 1, bit 0
 1 -> byte 0, bit 1
```

`maybeContain(H1/H2/H3)` must be true. `maybeContain(A)` is normally false for
this fixture (if it is a Bloom false positive, the exact sorted list still
answers false).

## 4. Canonical manifest payloads

### C1 — legacy HMAC, v1 segment, no `signature_alg`/`compression`

```text
{"version":1,"prefix_bytes":2,"segments":[{"prefix":4660,"count":3,"bloom_bits":29,"bloom_k":7,"root":"f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61","file":"segments/1234.seg","size":140,"sha256":"badf8a2d07565eb7a69863b4e852c48710f1cd9e56b5c9bb0f4ecbdcdc4c11d7"}]}
```

SHA-256 of C1:
`7b6d8f2c1e29bb8b9b0049e5a1f2acbe94ca1f1bc674b92029e5d5c32cdbf92e`

HMAC-SHA256 key (test only):
`00112233445566778899aabbccddeeff`

HMAC tag:

```text
hex:  efb11ccade70ae1974abbdaed0cae04432ae0a517b3155f9b555a3d09098d6e0
b64:  77Ecyt5wrhl0q72u0MrgRDKuClF7MVX5tVWj0JCY1uA=
```

This is the vector for the current Go server's HMAC mode. `signature_alg` is
absent and `signature` is base64 in its JSON.

### C2 — Ed25519, same v1 segment

```text
{"version":1,"prefix_bytes":2,"segments":[{"prefix":4660,"count":3,"bloom_bits":29,"bloom_k":7,"root":"f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61","file":"segments/1234.seg","size":140,"sha256":"badf8a2d07565eb7a69863b4e852c48710f1cd9e56b5c9bb0f4ecbdcdc4c11d7"}],"signature_alg":"ed25519"}
```

SHA-256: `0f16f3d7cd1e8b1c5cca95a14033345a1721939ee68eb957d0dd5ada846092c2`

### C3 — Ed25519, v2 uncompressed segment

```text
{"version":1,"prefix_bytes":2,"segments":[{"prefix":4660,"count":3,"bloom_bits":29,"bloom_k":7,"root":"f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61","file":"segments/1234.seg","size":148,"sha256":"5b7316849c9966c7ad4d5d1b9fb30d056403b8774a7171e9cb890e7271e2cc33","compression":"none"}],"signature_alg":"ed25519"}
```

SHA-256: `5d7e5f0c276d28cd39a42e1f36d11ca85b2a7a2b148a8ba23a94405eb60b4725`

## 5. Ed25519 test key and signatures

Test-only seed (32 bytes):
`000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f`

Public key (32 bytes):
`03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8`

Signature over **C2**:

```text
hex: bceea9617ef3cb25f6f66cab83072cda550056e8d640da5cd75f3feb8d387f1c7f5ed97f38c1465d03d1e903d0b6e714794334862e5c7a15d6a84e6984ee7d08
b64: vO6pYX7zyyX29myrgwcs2lUAVujWQNpc118/6404fxx/Xtl/OMFGXQPR6QPQtucUeUM0hi5cehXWqE5phO59CA==
```

Signature over **C3**:

```text
hex: 1b646c9dc39a1b57d5e8283b045ac01528b25cac75f9855cd9fc424f30b6f5c1dfd0a8216d8b626b850e22ca8f80d9b0cd48fccf65eb889a2e4e6693e8a6ce0e
b64: G2RsncOaG1fV6Cg7BFrAFSiyXKx1+YVc2fxCTzC29cHf0KghbYtia4UOIsqPgNmwzUj8z2XriJouTmaT6KbODg==
```

Both signatures were generated with Go's `crypto/ed25519` over the exact C2/C3
strings. Verify with `Curve25519.Signing.PublicKey` (Swift) or the Android
SPKI/Ed25519 path. A signature that verifies with a different canonicalization
(e.g. omitting `signature_alg`) is a bug.

## 6. Segment fixture v1 (legacy, 140 bytes)

Header hex (40 bytes):

```text
4544475345473031000000011234000000000003000000000000001d000000070000000000000004
```

Fields:

```text
magic        EDGSEG01
version      1
prefix       1234
reserved     0000
count        00000003
bloom_bits   000000000000001d (29)
bloom_k      00000007
bloom_bytes  0000000000000004
payload      c78d6d04 + H1 + H2 + H3
```

Full file hex:

```text
4544475345473031000000011234000000000003000000000000001d000000070000000000000004c78d6d04123400000000000000000000000000000000000000000000000000000000000012340100000000000000000000000000000000000000000000000000000000001234020000000000000000000000000000000000000000000000000000000000
```

Size `140`; SHA-256:
`badf8a2d07565eb7a69863b4e852c48710f1cd9e56b5c9bb0f4ecbdcdc4c11d7`.

Manifest segment entry:

```json
{"prefix":4660,"count":3,"bloom_bits":29,"bloom_k":7,"root":"f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61","file":"segments/1234.seg","size":140,"sha256":"badf8a2d07565eb7a69863b4e852c48710f1cd9e56b5c9bb0f4ecbdcdc4c11d7"}
```

## 7. Segment fixture v2 (uncompressed, 148 bytes)

Header hex (48 bytes):

```text
4544475345473031000000021234000000000003000000000000001d0000000700000000000000040000000000000064
```

Fields:

```text
magic          EDGSEG01
version        2
prefix         1234
compression    00 (none)
reserved       00
count          00000003
bloom_bits     000000000000001d (29)
bloom_k        00000007
bloom_bytes    0000000000000004
raw_size       0000000000000064 (100 = 4 + 3*32)
payload        c78d6d04 + H1 + H2 + H3
```

Full file hex:

```text
4544475345473031000000021234000000000003000000000000001d0000000700000000000000040000000000000064c78d6d04123400000000000000000000000000000000000000000000000000000000000012340100000000000000000000000000000000000000000000000000000000001234020000000000000000000000000000000000000000000000000000000000
```

Size `148`; SHA-256:
`5b7316849c9966c7ad4d5d1b9fb30d056403b8774a7171e9cb890e7271e2cc33`.

Manifest segment entry (note `compression`):

```json
{"prefix":4660,"count":3,"bloom_bits":29,"bloom_k":7,"root":"f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61","file":"segments/1234.seg","size":148,"sha256":"5b7316849c9966c7ad4d5d1b9fb30d056403b8774a7171e9cb890e7271e2cc33","compression":"none"}
```

This fixture was assembled from the documented v2 layout, not produced by
`edgehash build`; it matches the Go v2 none encoding. The Ed25519 signature in
C3 is a valid signed manifest for it.

## 8. Lookup fixtures

Manifest root: `f99ca0d8c620026f1e279fa659c10eb5d15cbddc7e58fada9d876d962467fb61`.

| Query | `present` | Source | Notes |
|---|---|---|---|
| `H1` | true | local exact / remote proof | index 0 |
| `H2` | true | local exact / remote proof | index 1; proof in §2 |
| `H3` | true | local exact / remote proof | index 2 |
| `A` (`123403…`) | false | local Bloom-negative or Bloom false positive | same segment |
| a hash with prefix != `1234` | false | no segment | signed manifest proves no segment |

The proof JSON may use `hash/index/path/right` (target) or
`Hash/Index/Path/Right` (current Go encoder). Verify both cases with the same
bytes.

## 9. Notes on the current server/client gap

At scaffold time:

- `internal/edgeset` had Ed25519, v2 compression, and absence-proof helpers,
  but `cmd/edgehash` exposed only HMAC `-key` and `internal/edgehttp` loaded
  stores with the legacy HMAC verifier. The mobile default (pinned Ed25519) can
  only be tested end to end once the HTTP server serves an Ed25519-signed
  manifest, or with the manual fixture in `TESTING.md`.
- `internal/edgeset.Proof` has no JSON tags, so the server currently emits
  `Hash`, `Index`, `Path`, `Right`. The mobile decoders accept both cases; new
  servers should emit lower-case. `absence_proof` uses `Prefix`, `Root`,
  `Left`, `Right` for the same reason.
- The current server supports `GET /v1/lookup/{hash}?absence=1` with a Merkle
  non-membership proof. The mobile scaffold does not request or verify it; a
  plain `present=false` remains unverified. The fixture in §8 can be used to
  test an absence proof once implemented.
- `Manifest.Signature []byte` is base64 in JSON. The mobile decoders accept
  base64 and hex.
- `Manifest.Version` is schema version 1, not a content generation. Cache keys
  must use the canonical fingerprint.
- The mobile scaffold has no zstd decoder. It skips zstd segments during sync
  and can still answer online via remote proofs. Use v2 `"none"` for offline
  tests.
- Absence proofs are available from the current server via `?absence=1` but
  are not consumed by the scaffold. `present=false` without a verified
  `absence_proof` is not a client-verifiable answer; a missing segment in the
  signed manifest is the stronger answer.

## 10. Reproducing the vectors

The binary/JSON values were generated with Python's `hashlib` and Go's
`crypto/ed25519` from a fixed seed. To re-derive the Merkle/Bloom/segment bytes,
use the formulas in `protocol.md` and the exact byte strings above. To check a
signature:

```sh
# Save C2 without a trailing newline, then verify a detached signature with
# OpenSSL 3 (Ed25519 is pure Ed25519; no prehash).
printf '%s' '<C2 exact string>' > /tmp/c2.msg
printf '%s' 'bceea961…7d08' | xxd -r -p > /tmp/c2.sig
# Public key DER (SPKI) prefix 302a300506032b6570032100:
printf '302a300506032b657003210003a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8' \
  | xxd -r -p > /tmp/c2.pub.der
openssl pkeyutl -verify -pubin -inkey /tmp/c2.pub.der -rawin \
  -in /tmp/c2.msg -sigfile /tmp/c2.sig
```

Or use the Go test key seed above with `ed25519.NewKeyFromSeed` and
`ed25519.Sign`. Do not use these keys outside tests.
