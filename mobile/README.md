# EdgeHash mobile SDK scaffold

Status: **scaffold, not a shipped app or SDK**. The Android file has not been
compiled with an Android toolchain and the iOS file has not been built as an
iOS app or run on a device. The Swift file was typechecked with
`swiftc -typecheck` during authoring; that is not an iOS build, an integration
test, or a correctness proof. See [`TESTING.md`](TESTING.md).

`mobile/` contains a small, read-only membership client for the immutable edge
hash set served by `cmd/edgehash`:

| File | Contents |
|---|---|
| [`android/EdgeHashClient.kt`](android/EdgeHashClient.kt) | Android/Kotlin client: HttpURLConnection, org.json, Ed25519, SHA-256, Merkle, Bloom, cache and policy hooks |
| [`ios/EdgeHashClient.swift`](ios/EdgeHashClient.swift) | iOS/Swift client: URLSession, CryptoKit Ed25519, Codable manifest, SHA-256, Merkle, Bloom, cache and policy hooks |
| [`protocol.md`](protocol.md) | Wire format, canonical signing JSON, segment v1/v2, Merkle/Bloom algorithms |
| [`TESTING.md`](TESTING.md) | What is untested, curl/emulator recipes, required tests |
| [`tests/notes.md`](tests/notes.md) | Deterministic test vectors and implementation notes |

This is **not** a database SDK, replication client, write path, or source of
truth. It answers "is this SHA-256 hash in the set?" with a local Bloom/exact
cache and, when needed, a server lookup whose Merkle proof is verified against a
signed manifest.

## Supported platforms

- **Android:** Kotlin, `minSdk 26`, `targetSdk` whatever the host app uses.
  - Dependencies: platform `org.json`, `java.net.HttpURLConnection`,
    `java.security`, JCA. No OkHttp, Retrofit, coroutines, BouncyCastle or
    zstd dependency is required by this file.
  - Ed25519 through the platform JCA provider requires **Android 13 / API 33+**
    (or an app-provided provider such as Conscrypt/BouncyCastle, which is a
    third-party dependency and outside this scaffold). On older devices
    `EdgeHashClient` fails closed with `EdgeHashException.CryptoUnavailable`;
    do not fall back to HMAC automatically.
  - All client methods are blocking. Call them from a worker thread; the file
    deliberately does not impose a coroutine/Executor API.
- **iOS:** iOS 16+, Swift 5.7+.
  - `Foundation`, `CryptoKit`, `URLSession`. No third-party packages.
  - `CryptoKit.Curve25519.Signing.PublicKey` verifies Ed25519.
  - The client is an `actor`, so its manifest/cache state is isolated.
    Segment parsing currently runs on the actor executor; see limitations.
- **Server:** `cmd/edgehash` / `internal/edgehttp`. The mobile scaffold targets
  manifest v1 and segment formats v1/v2; see the compatibility matrix in
  [`protocol.md`](protocol.md#12-compatibility-matrix).
- **Not supported:** web, desktop, Linux, Rust, Flutter, React Native wrappers.
  No background scheduler, UI, telemetry, key rotation or remote config is
  included.

## Trust model

Production mode pins a **32-byte Ed25519 public key** in the app binary (or a
build-time configuration value that is itself integrity-protected):

```kotlin
// Android
val trust = TrustConfig.ed25519(pinnedPublicKey)
```

```swift
// iOS
let trust: TrustConfig = .ed25519(publicKey: pinnedPublicKey)
```

The chain of trust is:

1. The manifest signature is verified over the canonical JSON described in
   [`protocol.md` §5](protocol.md#5-canonical-json-and-manifest-fingerprint).
   The key is pinned, not discovered from the server or TLS certificate.
2. The manifest lists each segment's SHA-256 and Merkle root.
3. Each segment downloaded by the client is checked against the manifest
   `size` and `sha256`, then fully parsed: header, prefix routing, sorted
   hashes, Bloom parameters, and recomputed Merkle root.
4. A remote `present=true` answer is accepted only if its inclusion proof
   verifies against the manifest's segment root, the queried hash, the
   segment's signed `count`, and a server root equal to the signed root (if
   supplied).
5. TLS is still required in production, but transport security is not the root
   of trust. A compromised CDN or reverse proxy cannot forge a manifest,
   segment, or proof without the Ed25519 private key.

The signed payload includes `signature_alg` when non-empty. A pinned Ed25519
verifier rejects a manifest with no algorithm or with `hmac-sha256`, preventing a
downgrade. HMAC-SHA256 is implemented only for development against the current
Go server:

```kotlin
val devTrust = TrustConfig.hmacLegacy(devKeyBytes) // never ship a shared key
```

**What the signature does not provide:** freshness/rollback protection. There
is no signed generation counter, expiry, or transparency log. An attacker who
can serve HTTP can replay an older validly signed manifest. The cache TTL is a
local heuristic. Before production, add a signed monotonic generation or pin a
minimum fingerprint/checkpoint. See
[`protocol.md` §11](protocol.md#11-versioning-and-rollback).

Absence is also weaker than presence: v1 has no verifiable absence proof. A
remote `present=false` is a server assertion. A `NO_SEGMENT` result (the signed
manifest has no segment for the hash's routing prefix) is evidence from the
signed manifest itself. Bloom-negative answers are exact for the manifest
generation they were verified against, but can become false negatives if the
cached manifest is stale.

## Cache policy

Both implementations default to a conservative mobile policy:

| Policy | Default | Hook |
|---|---|---|
| Manifest refresh | Unmetered (Wi-Fi) network | `requireUnmeteredForSync` |
| Bulk segment download | Unmetered + charging | `requireUnmeteredForSync`, `requireChargingForBulk` |
| Small JSON lookup | Metered allowed | `allowMeteredLookups` |
| Cache quota | 256 MiB | `maxCacheBytes` |
| Manifest TTL | 24 h | `manifestTTL` / `manifestTtlMillis` |
| Segment idle TTL | 7 days | `segmentTTL` / `segmentTtlMillis` |
| Stale reads | Off | `allowStaleReads` |
| Pinned prefixes | none | `pinnedPrefixes` |
| Partition filter | none (all segments) | `prefixFilter` |

Behavior:

- `sync()` refreshes the manifest, then downloads only missing/invalid selected
  segments. It writes files to a temp file and atomically replaces the target;
  the manifest is installed only after signature verification. A corrupt local
  file is deleted and refetched.
- `sync(force = true)` bypasses the Wi-Fi/charging gates (for an explicit user
  "download now"), but never bypasses signature, checksum, size or quota checks.
- If the network/power gate blocks a refresh and the cached manifest is still
  fresh, `sync()` returns a report with `skippedReason` instead of failing. If
  it is expired, it throws `PolicyDenied`.
- Bulk download can be deferred (`deferredReason`) while the manifest refresh
  still succeeds.
- LRU + TTL eviction deletes non-pinned segments when the quota is exceeded.
  `pinnedPrefixes` protects an offline region. Quota is measured over segment
  files; the manifest is small and not counted.
- A local lookup uses the cached segment first. If the segment is absent (or
  zstd-compressed), it falls back to a remote lookup + proof. It does not
  download a segment during `lookup()`.
- Missing segment for a routing prefix is verified absence from the signed
  manifest.
- `allowStaleReads` / `LookupOptions.allowStaleManifest` permit answering from
  an expired manifest when offline; the result sets `staleManifest=true`.
  Production should treat stale local negatives with suspicion.

Platform conditions are intentionally **hooks**, not baked-in platform calls, so
the core is testable. Supply an implementation:

Android:

```kotlin
class AndroidConditions(context: Context) : DeviceConditions {
    private val cm = context.getSystemService(ConnectivityManager::class.java)
    private val bm = context.getSystemService(BatteryManager::class.java)

    override fun isUnmeteredNetwork(): Boolean {
        val network = cm.activeNetwork ?: return false
        val caps = cm.getNetworkCapabilities(network) ?: return false
        return caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) &&
            caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED)
    }

    override fun isCharging(): Boolean =
        bm.isCharging  // BatteryManager.isCharging, API 23+
}
```

Add `INTERNET` and `ACCESS_NETWORK_STATE` permissions. Do not use
`isActiveNetworkMetered` alone if you need validated internet; check
`NET_CAPABILITY_VALIDATED` as appropriate for your product.

iOS:

```swift
final class DeviceConditionsAdapter: DeviceConditions {
    private let monitor = NWPathMonitor()
    private var path: NWPath?

    init() {
        monitor.pathUpdateHandler = { [weak self] path in self?.path = path }
        monitor.start(queue: DispatchQueue(label: "edgehash.network"))
    }

    var isUnmeteredNetwork: Bool {
        guard let path else { return false }
        return path.status == .satisfied && !path.isExpensive && !path.isConstrained
    }

    var isCharging: Bool {
        UIDevice.current.batteryState == .charging || UIDevice.current.batteryState == .full
    }
}
```

Enable battery monitoring if you need `UIDevice.batteryState`; consider
`ProcessInfo` low-power mode and platform background restrictions in a real
integration. The scaffold does not schedule background sync (WorkManager /
BGTaskScheduler).

## Public API surface

### Android (`dev.edgehash.mobile`)

```kotlin
val config = EdgeHashConfig(
    baseUrl = "https://edge.example",
    trust = TrustConfig.ed25519(pinnedPublicKey),
    cacheDir = File(context.cacheDir, "edgehash"),
    prefixFilter = setOf(0x1234), // optional
    cachePolicy = CachePolicy(),
)
val client = EdgeHashClient(config, conditions = AndroidConditions(context))

val report: SyncReport = client.sync()
val result: LookupResult = client.lookup(hashHex)
if (result.present && result.verified) { /* ... */ }

client.trimCache()
client.clearCache()
```

Key types: `EdgeHashClient`, `EdgeHashConfig`, `TrustConfig`, `CachePolicy`,
`DeviceConditions`, `NetworkConfig`, `Manifest`, `SegmentMeta`, `MerkleProof`,
`LookupResult`, `LookupSource`, `SyncReport`, `EdgeHashException`,
`EdgeHashMerkle`.

`lookup` is overloaded for `ByteArray` (exactly 32 bytes) and 64-char hex.
`lookup` is blocking; `sync` is serialized and logs a policy skip in its report.

### iOS (`EdgeHashClient.swift`)

```swift
let config = try EdgeHashConfig(
    baseURL: URL(string: "https://edge.example")!,
    trust: .ed25519(publicKey: pinnedPublicKey),
    cacheDirectory: cacheDir,
    policy: CachePolicy()
)
let client = try EdgeHashClient(config: config, conditions: DeviceConditionsAdapter())

let report: SyncReport = try await client.sync()
let result: LookupResult = try await client.lookup(hashHex: hash)
await client.cacheBytes()
try await client.trimCache()
await client.clearCache()
```

Key types: `EdgeHashClient` (actor), `EdgeHashConfig`, `TrustConfig`,
`CachePolicy`, `DeviceConditions`, `NetworkConfig`, `Manifest`, `SegmentMeta`,
`MerkleProof`, `LookupResult`, `LookupSource`, `SyncReport`, `EdgeHashError`,
`EdgeHashMerkle`.

`sync()` throws `.syncInProgress` if called concurrently. `lookup` may fall
back to a remote proof when a segment is not cached.

## Build instructions (scaffolding only)

There is deliberately **no** Gradle project, Android manifest, Xcode project,
`Package.swift`, sample app, or signing configuration in this directory. Adding
the file to an app is the integration step; treat the snippets below as
scaffolding to adapt, not as a build verified in this repository.

### Android (Gradle)

1. Create an app/library module with Kotlin and `minSdk 26` (Ed25519 requires
   API 33+ or a bundled provider).
2. Copy [`android/EdgeHashClient.kt`](android/EdgeHashClient.kt) to
   `app/src/main/java/dev/edgehash/mobile/EdgeHashClient.kt` (the package
   declaration is `dev.edgehash.mobile`; rename consistently if desired).
3. Do **not** add OkHttp/Retrofit/coroutines for this file. It uses platform
   APIs only. `org.json` is part of Android.
4. Add to `AndroidManifest.xml`:
   ```xml
   <uses-permission android:name="android.permission.INTERNET" />
   <uses-permission android:name="android.permission.ACCESS_NETWORK_STATE" />
   ```
   For local HTTP development only, an explicit network-security-config
   exception is required; production should use HTTPS.
5. Wire `EdgeHashClient` into a worker (for example
   `Executors.newSingleThreadExecutor()` or `Dispatchers.IO` around the
   blocking calls). Do not call `sync`/`lookup` on the main thread.
6. Ed25519 on API < 33: either declare the feature unsupported for those
   devices, ship a JCA provider (new dependency; re-evaluate the no-third-party
   constraint), or keep the app in HMAC development mode with a non-shipped key.

### iOS (SwiftPM or Xcode)

1. Create an iOS 16+ app or a SwiftPM library target. Add
   [`ios/EdgeHashClient.swift`](ios/EdgeHashClient.swift) to the target.
2. Link `Foundation` and `CryptoKit` (both system frameworks).
3. If you add a `Package.swift`, the target needs no external dependencies:
   ```swift
   // Scaffold only: create and validate this in your own package.
   let package = Package(
       name: "EdgeHashMobile",
       platforms: [.iOS(.v16)],
       products: [.library(name: "EdgeHashMobile", targets: ["EdgeHashMobile"])],
       targets: [.target(name: "EdgeHashMobile")]
   )
   ```
4. For local HTTP development, add an ATS exception in `Info.plist`
   (`NSAppTransportSecurity` → `NSAllowsLocalNetworking` or a scoped exception).
   Do not weaken ATS for production. The manifest signature still protects data
   integrity, but the transport policy is part of defense in depth.
5. Provide network/power conditions via `NWPathMonitor` / `UIDevice` as above.
6. Store `cacheDirectory` under `Application Support` or `Caches`; the client
   excludes it from iCloud backup. The OS may purge caches; re-sync after a
   purge.

### Server-side prerequisite

Serve an HTTPS endpoint whose manifest has `signature_alg:"ed25519"` and whose
segments are v1 or v2 `"none"` if you want an offline mobile cache. The current
Go tree has Ed25519 and v2 compression support in `internal/edgeset`, but the
`edgehash` CLI/HTTP wiring may still expose only the HMAC path. Check
`edgehash build -h`, `edgehash serve -h`, and the actual `/v1/manifest` body
before choosing `TrustConfig.ed25519`. For development, use `hmacLegacy` and a
tiny 3-hash store (small segments auto-select uncompressed v2).

See [`TESTING.md`](TESTING.md) for `curl` commands and emulator/simulator host
mapping.

## Limitations and production gaps

Protocol / server integration:

- Ed25519 signing exists in the Go library but the HTTP/CLI path is not
  guaranteed to expose it yet. Do not ship until `signature_alg:"ed25519"` is
  actually served and tested end to end.
- The current Go proof JSON uses capitalized keys; the SDK accepts both. New
  servers should emit lower-case keys.
- `signature` is base64 in the current Go JSON; the SDK accepts base64 and hex.
  Pick one in the next wire revision.
- No absence proof over HTTP yet. `present=false` is unverified.
- No signed generation/timestamp/expiry/rollback protection; no key rotation.
- No TLS certificate pinning, auth, rate limiting or server-side quota.
- No zstd decoder in the mobile scaffold. zstd segments are skipped for local
  caching and resolved remotely. This means offline coverage is limited to v1
  or v2 `"none"` segments.
- `version` is schema version, not a data generation. Use the fingerprint.
- No multi-server failover, CDN discovery, or range requests.

Client / app integration:

- No Gradle/Xcode/SPM project, sample app, UI, permissions declaration, or
  signing config is included.
- Whole segment is held in memory (`ByteArray`/`Data`) for parsing. Large
  segments should be streamed/mmap'd and Merkle-root computation moved to a
  flat buffer or native code. `maxSegmentBytes` defaults to 64 MiB and caps
  allocations.
- Android `lookup`/`sync` are blocking; iOS parsing runs on the client actor.
  Production should move large file parsing off the actor and use a dedicated
  I/O executor.
- Cache is app-private plain files, not encrypted at rest. Hashes may be
  sensitive; use platform file protection/encryption if the threat model needs
  it.
- LRU uses file mtime; reads do not update access time. A production cache needs
  a persistent index and read-through accounting.
- Eviction is best-effort; a crash may leave temp files (they are ignored/
  cleaned on subsequent writes in practice, but no startup sweep is included).
- No telemetry, metrics, logging, or diagnostics; errors do not include
  untrusted response bodies beyond bounded error text.
- No background scheduling, retry/backoff, or connectivity-change listener.
- No battery/charging observation is built in; the app supplies
  `DeviceConditions`.
- No tests, fuzzing, benchmarks, or device validation were run here (Swift
  typecheck only; no Android compile).

Before production:

1. Implement the required unit/integration tests in
   [`TESTING.md`](TESTING.md), including cross-language vectors.
2. Make the server emit and verify Ed25519, and test the exact canonical bytes
   with the vectors.
3. Decide zstd support (platform library or server `"none"` policy) and prove
   it with decompression-bomb and size-limit tests.
4. Add freshness/rollback protection and key rotation.
5. Harden file I/O, quota accounting, and background/network transitions on
   real devices.
6. Perform a security review of trust, replay, absence, and cache poisoning.
