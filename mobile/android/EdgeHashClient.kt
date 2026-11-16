@file:Suppress("unused")

/*
 * EdgeHash mobile SDK - Android/Kotlin scaffold.
 *
 * STATUS: SCAFFOLD, NOT A SHIPPED SDK. This file has not been built with the
 * Android SDK and has not run on a device. A JVM compile against minimal
 * org.json stubs and a reference-vector harness were run (see
 * mobile/TESTING.md); that is not Android validation.
 *
 * Design constraints:
 *  - No third-party runtime dependencies: java.net.HttpURLConnection, org.json,
 *    java.security and JCA only.
 *  - The manifest is authenticated with a PINNED Ed25519 public key. Legacy
 *    HMAC-SHA256 is supported only when the caller explicitly opts in, because
 *    a shared HMAC secret cannot be shipped safely in a mobile application.
 *  - Ed25519 through java.security requires Android API 33+ (or a bundled JCA
 *    provider such as Conscrypt/BouncyCastle). Older devices fail closed with
 *    EdgeHashException.CryptoUnavailable.
 *  - All network calls are blocking. Call them from a background thread
 *    (Dispatchers.IO / Executor); this file intentionally avoids coroutines.
 *
 * Protocol: mobile/protocol.md. Server: cmd/edgehash, internal/edgehttp.
 */

package dev.edgehash.mobile

import org.json.JSONArray
import org.json.JSONException
import org.json.JSONObject
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.FileOutputStream
import java.io.IOException
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.URI
import java.nio.charset.CodingErrorAction
import java.nio.charset.StandardCharsets
import java.nio.file.AtomicMoveNotSupportedException
import java.nio.file.Files
import java.nio.file.StandardCopyOption
import java.security.GeneralSecurityException
import java.security.KeyFactory
import java.security.MessageDigest
import java.security.NoSuchAlgorithmException
import java.security.Signature
import java.security.spec.X509EncodedKeySpec
import java.util.Base64
import java.util.Locale
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec

// ---------------------------------------------------------------------------
// Protocol constants
// ---------------------------------------------------------------------------

private const val MANIFEST_VERSION = 1L
private const val SEGMENT_VERSION = 1L
private const val SEGMENT_MAGIC = "EDGSEG01"
private const val SEGMENT_HEADER_BYTES = 40
private const val HASH_BYTES = 32
private const val MAX_BLOOM_BITS = 1L shl 36
private const val MAX_BLOOM_K = 64
private const val MAX_MERKLE_DEPTH = 62
private const val MAX_MANIFEST_SEGMENTS = 1 shl 16

private val LEAF_DOMAIN = "edgeset:merkle-leaf:v1\u0000".toByteArray(StandardCharsets.UTF_8)
private val NODE_DOMAIN = "edgeset:merkle-node:v1\u0000".toByteArray(StandardCharsets.UTF_8)
private val EMPTY_LEAF_DOMAIN = "edgeset:merkle-empty-leaf:v1".toByteArray(StandardCharsets.UTF_8)
private val BLOOM_DOMAIN = "edgeset:bloom:v1\u0000".toByteArray(StandardCharsets.UTF_8)

/** SHA-256("edgeset:merkle-empty-leaf:v1"). */
internal val EMPTY_LEAF: ByteArray = sha256(EMPTY_LEAF_DOMAIN)

/** DER SubjectPublicKeyInfo prefix for a raw 32-byte Ed25519 public key. */
private val ED25519_SPKI_PREFIX = byteArrayOf(
    0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00,
)

private const val HEX_DIGITS = "0123456789abcdef"

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/** All errors thrown by this SDK. No exception carries untrusted payloads. */
sealed class EdgeHashException(message: String, cause: Throwable? = null) :
    Exception(message, cause) {

    class InvalidHash(message: String) : EdgeHashException(message)

    class InvalidManifest(message: String, cause: Throwable? = null) :
        EdgeHashException("invalid manifest: $message", cause)

    class UnsupportedVersion(message: String) : EdgeHashException("unsupported protocol: $message")

    class UntrustedManifest(message: String) :
        EdgeHashException("manifest signature rejected: $message")

    class InvalidSegment(message: String, cause: Throwable? = null) :
        EdgeHashException("invalid segment: $message", cause)

    class InvalidProof(message: String) : EdgeHashException("invalid Merkle proof: $message")

    class StaleManifest(val manifestFingerprint: String, cause: Throwable? = null) :
        EdgeHashException(
            "cached manifest is stale or the server moved to another generation " +
                "(fingerprint $manifestFingerprint); sync and retry",
            cause,
        )

    class NotSynced : EdgeHashException("no verified manifest is available; call sync() first")

    class Offline : EdgeHashException("offline mode cannot satisfy this lookup")

    class PolicyDenied(val reason: String) : EdgeHashException("cache policy denied: $reason")

    class QuotaExceeded(message: String) : EdgeHashException(message)

    class HttpError(val statusCode: Int, val path: String, val detail: String = "") :
        EdgeHashException("HTTP $statusCode for $path${if (detail.isEmpty()) "" else ": $detail"}")

    class Transport(message: String, cause: Throwable? = null) : EdgeHashException(message, cause)

    class InvalidResponse(message: String) : EdgeHashException("invalid server response: $message")

    class CryptoUnavailable(message: String, cause: Throwable? = null) :
        EdgeHashException("cryptography unavailable: $message", cause)

    class Storage(message: String, cause: Throwable? = null) :
        EdgeHashException("cache storage error: $message", cause)

    class SyncInProgress : EdgeHashException("another sync is already running")
}

// ---------------------------------------------------------------------------
// Small utilities
// ---------------------------------------------------------------------------

internal fun ByteArray.toHex(): String {
    val out = CharArray(size * 2)
    for (i in indices) {
        val v = this[i].toInt() and 0xff
        out[i * 2] = HEX_DIGITS[v ushr 4]
        out[i * 2 + 1] = HEX_DIGITS[v and 0x0f]
    }
    return String(out)
}

internal fun decodeHex(hex: String): ByteArray {
    require(hex.length % 2 == 0) { "hex string must have an even length" }
    val out = ByteArray(hex.length / 2)
    for (i in out.indices) {
        val hi = Character.digit(hex[i * 2], 16)
        val lo = Character.digit(hex[i * 2 + 1], 16)
        require(hi >= 0 && lo >= 0) { "invalid hex character at offset ${i * 2}" }
        out[i] = ((hi shl 4) or lo).toByte()
    }
    return out
}

internal fun sha256(data: ByteArray): ByteArray =
    MessageDigest.getInstance("SHA-256").digest(data)

internal fun sha256(vararg parts: ByteArray): ByteArray {
    val digest = MessageDigest.getInstance("SHA-256")
    for (part in parts) digest.update(part)
    return digest.digest()
}

internal fun compareUnsigned(a: ByteArray, b: ByteArray): Int {
    require(a.size == b.size) { "byte arrays must have equal length" }
    for (i in a.indices) {
        val av = a[i].toInt() and 0xff
        val bv = b[i].toInt() and 0xff
        if (av != bv) return av - bv
    }
    return 0
}

private fun jsonString(value: String): String {
    val out = StringBuilder(value.length + 2)
    out.append('"')
    for (ch in value) {
        when (ch) {
            '"' -> out.append("\\\"")
            '\\' -> out.append("\\\\")
            '\b' -> out.append("\\b")
            '\u000c' -> out.append("\\f")
            '\n' -> out.append("\\n")
            '\r' -> out.append("\\r")
            '\t' -> out.append("\\t")
            '<' -> out.append("\\u003c")
            '>' -> out.append("\\u003e")
            '&' -> out.append("\\u0026")
            '\u2028' -> out.append("\\u2028")
            '\u2029' -> out.append("\\u2029")
            else -> {
                if (ch < ' ') {
                    out.append(String.format(Locale.ROOT, "\\u%04x", ch.code))
                } else {
                    out.append(ch)
                }
            }
        }
    }
    out.append('"')
    return out.toString()
}

private fun segmentFileName(prefix: Int): String =
    String.format(Locale.ROOT, "segments/%04x.seg", prefix)

private fun routingPrefix(hash: ByteArray, prefixBytes: Int): Int {
    require(hash.size == HASH_BYTES)
    return if (prefixBytes <= 1) {
        hash[0].toInt() and 0xff
    } else {
        ((hash[0].toInt() and 0xff) shl 8) or (hash[1].toInt() and 0xff)
    }
}

private fun readU16(data: ByteArray, offset: Int): Int =
    ((data[offset].toInt() and 0xff) shl 8) or (data[offset + 1].toInt() and 0xff)

private fun readU32(data: ByteArray, offset: Int): Long =
    ((data[offset].toLong() and 0xff) shl 24) or
        ((data[offset + 1].toLong() and 0xff) shl 16) or
        ((data[offset + 2].toLong() and 0xff) shl 8) or
        (data[offset + 3].toLong() and 0xff)

private fun readU64(data: ByteArray, offset: Int): Long {
    var value = 0L
    for (i in 0 until 8) {
        value = (value shl 8) or (data[offset + i].toLong() and 0xff)
    }
    return value
}

private fun decodeUtf8Strict(data: ByteArray): String {
    return try {
        StandardCharsets.UTF_8.newDecoder()
            .onMalformedInput(CodingErrorAction.REPORT)
            .onUnmappableCharacter(CodingErrorAction.REPORT)
            .decode(java.nio.ByteBuffer.wrap(data))
            .toString()
    } catch (e: Exception) {
        throw EdgeHashException.InvalidManifest("body is not valid UTF-8", e)
    }
}

private fun normalizeBaseUrl(raw: String, allowInsecureHttp: Boolean): String {
    val value = raw.trim()
    require(value.isNotEmpty()) { "base URL must not be empty" }
    val uri = try {
        URI(value)
    } catch (e: Exception) {
        throw IllegalArgumentException("invalid base URL: $e", e)
    }
    require(uri.userInfo == null) { "base URL must not contain user info" }
    require(uri.query == null && uri.fragment == null) { "base URL must not contain query or fragment" }
    val scheme = uri.scheme?.lowercase(Locale.ROOT)
    require(scheme == "https" || (allowInsecureHttp && scheme == "http")) {
        "base URL must use https (allowInsecureHttp=true is development-only)"
    }
    require(!uri.host.isNullOrBlank()) { "base URL must have a host" }
    return value.trimEnd('/')
}

// ---------------------------------------------------------------------------
// Public model
// ---------------------------------------------------------------------------

data class SegmentMeta(
    val prefix: Int,
    val count: Long,
    val bloomBits: Long,
    val bloomK: Int,
    val root: ByteArray,
    val file: String,
    val size: Long,
    val sha256: ByteArray,
    /** null/"" for legacy v1 segments; "none" or "zstd" for format v2. */
    val compression: String? = null,
) {
    val rootHex: String get() = root.toHex()
    val sha256Hex: String get() = sha256.toHex()
}

data class Manifest(
    val version: Long,
    val prefixBytes: Int,
    val segments: List<SegmentMeta>,
    /** "ed25519", "hmac-sha256", or null for old unsigned/unlabelled HMAC servers. */
    val signatureAlg: String?,
    val signature: ByteArray?,
) {
    fun segmentForPrefix(prefix: Int): SegmentMeta? = segments.findByPrefix(prefix)

    fun segmentForHash(hash: ByteArray): SegmentMeta? =
        segmentForPrefix(routingPrefix(hash, prefixBytes))
}

data class MerkleProof(
    val hash: ByteArray,
    val index: Long,
    val path: List<ByteArray>,
    val right: List<Boolean>,
)

enum class LookupSource {
    /** No segment in the signed manifest has this routing prefix. */
    NO_SEGMENT,

    /** Local segment's Bloom filter proved absence (no false negatives for that generation). */
    LOCAL_BLOOM_NEGATIVE,

    /** Exact hit in a locally cached, checksum-verified segment. */
    LOCAL_EXACT,

    /** Bloom said maybe, exact sorted hash list said no. */
    LOCAL_BLOOM_FALSE_POSITIVE,

    /** Remote lookup returned an inclusion proof that verified against the signed root. */
    REMOTE_PROOF,

    /** Remote lookup said present=false without a verified absence proof. */
    REMOTE_ABSENT,
}

data class LookupResult(
    val present: Boolean,
    val prefix: Int,
    val prefixHex: String,
    val source: LookupSource,
    /**
     * True when the answer is derived from a verified manifest/segment/proof.
     * Remote absence without an absence proof (not implemented here) and stale
     * negative answers are not treated as verified.
     */
    val verified: Boolean,
    val staleManifest: Boolean,
    val manifestFingerprint: String,
    val proof: MerkleProof? = null,
    val rootHex: String? = null,
)

data class LookupOptions(
    /** Never touch the network. Throws Offline/StaleManifest instead of refreshing. */
    val offline: Boolean = false,
    /** Use an expired but signature-valid manifest instead of failing/syncing. */
    val allowStaleManifest: Boolean = false,
)

data class SyncReport(
    val manifestVersion: Long,
    val manifestFingerprint: String,
    val notModified: Boolean,
    val manifestBytes: Long,
    val segmentsSelected: Int,
    val segmentsDownloaded: Int,
    val segmentsAlreadyCached: Int,
    val segmentsSkipped: Int,
    val bytesDownloaded: Long,
    /** Manifest refresh was skipped by policy while a fresh cache was available. */
    val skippedReason: String? = null,
    /** Segment bulk download was deferred by Wi-Fi/charging policy. */
    val deferredReason: String? = null,
    /** Download stopped because a size/quota limit could not fit the next segment. */
    val stoppedReason: String? = null,
    val cacheBytes: Long = 0,
)

// ---------------------------------------------------------------------------
// Trust
// ---------------------------------------------------------------------------

/**
 * Manifest trust configuration.
 *
 * Production: [ed25519] with a public key pinned in the app. [hmacLegacy]
 * exists for development against the current Go server, which only knows
 * HMAC-SHA256. Never ship a shared HMAC key in a mobile binary.
 */
class TrustConfig private constructor(
    private val mode: Mode,
    private val keyMaterial: ByteArray,
) {
    private enum class Mode { ED25519, HMAC_SHA256 }

    companion object {
        fun ed25519(publicKey: ByteArray): TrustConfig {
            require(publicKey.size == 32) { "Ed25519 public key must be 32 bytes" }
            return TrustConfig(Mode.ED25519, publicKey.copyOf())
        }

        fun hmacLegacy(secretKey: ByteArray): TrustConfig {
            require(secretKey.size >= 16) { "HMAC key must be at least 16 bytes; use Ed25519 in production" }
            return TrustConfig(Mode.HMAC_SHA256, secretKey.copyOf())
        }
    }

    internal fun verify(signatureAlg: String?, canonical: ByteArray, signature: ByteArray) {
        when (mode) {
            Mode.ED25519 -> {
                if (signatureAlg != "ed25519") {
                    throw EdgeHashException.UntrustedManifest(
                        "pinned Ed25519 key requires signature_alg=ed25519, got ${signatureAlg ?: "absent"}",
                    )
                }
                if (signature.size != 64) {
                    throw EdgeHashException.UntrustedManifest(
                        "Ed25519 signature must be 64 bytes, got ${signature.size}",
                    )
                }
                if (!verifyEd25519(keyMaterial, canonical, signature)) {
                    throw EdgeHashException.UntrustedManifest("Ed25519 signature does not match the pinned key")
                }
            }

            Mode.HMAC_SHA256 -> {
                if (signatureAlg != null && signatureAlg != "hmac-sha256") {
                    throw EdgeHashException.UntrustedManifest(
                        "legacy trust only accepts hmac-sha256, got $signatureAlg",
                    )
                }
                if (signature.size != 32) {
                    throw EdgeHashException.UntrustedManifest(
                        "HMAC-SHA256 tag must be 32 bytes, got ${signature.size}",
                    )
                }
                val expected = try {
                    val mac = Mac.getInstance("HmacSHA256")
                    mac.init(SecretKeySpec(keyMaterial, "HmacSHA256"))
                    mac.doFinal(canonical)
                } catch (e: GeneralSecurityException) {
                    throw EdgeHashException.CryptoUnavailable("HmacSHA256 is unavailable", e)
                }
                if (!MessageDigest.isEqual(expected, signature)) {
                    throw EdgeHashException.UntrustedManifest("HMAC-SHA256 signature mismatch")
                }
            }
        }
    }

    private fun verifyEd25519(publicKey: ByteArray, message: ByteArray, signature: ByteArray): Boolean {
        val spki = ByteArray(ED25519_SPKI_PREFIX.size + publicKey.size)
        ED25519_SPKI_PREFIX.copyInto(spki)
        publicKey.copyInto(spki, ED25519_SPKI_PREFIX.size)
        return try {
            val keyFactory = KeyFactory.getInstance("Ed25519")
            val parsed = keyFactory.generatePublic(X509EncodedKeySpec(spki))
            val verifier = Signature.getInstance("Ed25519")
            verifier.initVerify(parsed)
            verifier.update(message)
            verifier.verify(signature)
        } catch (e: NoSuchAlgorithmException) {
            throw EdgeHashException.CryptoUnavailable(
                "Ed25519 is not provided by this Android build; require API 33+ " +
                    "or bundle a JCA provider (see mobile/README.md)",
                e,
            )
        } catch (e: GeneralSecurityException) {
            false
        }
    }
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

/**
 * Minimal HTTP abstraction so tests can replace the network. Advanced API:
 * production callers normally leave the default transport in place.
 */
data class HttpResponse(
    val statusCode: Int,
    val body: ByteArray?,
    val etag: String?,
)

interface Transport {
    fun get(path: String, accept: String, maxBytes: Long, ifNoneMatch: String? = null): HttpResponse
}

internal class HttpUrlConnectionTransport(
    private val baseUrl: String,
    private val network: NetworkConfig,
) : Transport {
    override fun get(path: String, accept: String, maxBytes: Long, ifNoneMatch: String?): HttpResponse {
        require(path.startsWith("/")) { "request path must start with /" }
        val connection = try {
            URI(baseUrl + path).toURL().openConnection() as HttpURLConnection
        } catch (e: Exception) {
            throw EdgeHashException.Transport("cannot open $path", e)
        }
        try {
            connection.requestMethod = "GET"
            connection.connectTimeout = network.connectTimeoutMillis
            connection.readTimeout = network.readTimeoutMillis
            connection.instanceFollowRedirects = false
            connection.useCaches = false
            connection.setRequestProperty("Accept", accept)
            connection.setRequestProperty("User-Agent", network.userAgent)
            connection.setRequestProperty("Accept-Encoding", "identity")
            if (ifNoneMatch != null) {
                connection.setRequestProperty("If-None-Match", ifNoneMatch)
            }

            val status = try {
                connection.responseCode
            } catch (e: IOException) {
                throw EdgeHashException.Transport("GET $path failed", e)
            }
            val etag = connection.getHeaderField("ETag")
            if (status == HttpURLConnection.HTTP_NOT_MODIFIED) {
                return HttpResponse(status, null, etag)
            }

            val successful = status in 200..299
            val limit = if (successful) maxBytes else minOf(maxBytes, 512L)
            val contentLength = connection.contentLengthLong
            if (successful && contentLength > limit) {
                throw EdgeHashException.InvalidResponse(
                    "GET $path Content-Length $contentLength exceeds $limit",
                )
            }
            val stream: InputStream? = if (successful) {
                try {
                    connection.inputStream
                } catch (e: IOException) {
                    throw EdgeHashException.Transport("GET $path: cannot read body", e)
                }
            } else {
                connection.errorStream
            }
            val body = stream?.use { readLimited(it, limit, path) } ?: ByteArray(0)
            return HttpResponse(status, body, etag)
        } finally {
            connection.disconnect()
        }
    }

    private fun readLimited(input: InputStream, maxBytes: Long, path: String): ByteArray {
        val out = ByteArrayOutputStream()
        val buffer = ByteArray(16 * 1024)
        var total = 0L
        while (true) {
            val read = input.read(buffer)
            if (read < 0) break
            total += read
            if (total > maxBytes) {
                throw EdgeHashException.InvalidResponse("GET $path exceeds $maxBytes bytes")
            }
            out.write(buffer, 0, read)
        }
        return out.toByteArray()
    }
}

data class NetworkConfig(
    val connectTimeoutMillis: Int = 5_000,
    val readTimeoutMillis: Int = 15_000,
    val maxManifestBytes: Long = 32L * 1024 * 1024,
    val maxLookupBytes: Long = 1L * 1024 * 1024,
    val maxSegmentBytes: Long = 64L * 1024 * 1024,
    val userAgent: String = "edgehash-mobile/0.1 (scaffold)",
) {
    init {
        require(connectTimeoutMillis > 0) { "connect timeout must be positive" }
        require(readTimeoutMillis > 0) { "read timeout must be positive" }
        require(maxManifestBytes > 0 && maxLookupBytes > 0 && maxSegmentBytes > 0) {
            "response limits must be positive"
        }
        require(maxSegmentBytes <= Int.MAX_VALUE) { "maxSegmentBytes must fit an Int" }
    }
}

// ---------------------------------------------------------------------------
// Device policy hooks
// ---------------------------------------------------------------------------

/**
 * Implement with Android ConnectivityManager / BatteryManager, or return
 * constants in tests. The SDK never calls platform APIs itself so it stays
 * testable on the JVM.
 */
interface DeviceConditions {
    fun isUnmeteredNetwork(): Boolean
    fun isCharging(): Boolean
}

object AlwaysAllowConditions : DeviceConditions {
    override fun isUnmeteredNetwork(): Boolean = true
    override fun isCharging(): Boolean = true
}

class StaticConditions(
    private val unmetered: Boolean,
    private val charging: Boolean,
) : DeviceConditions {
    override fun isUnmeteredNetwork(): Boolean = unmetered
    override fun isCharging(): Boolean = charging
}

data class CachePolicy(
    /** Manifest refresh is suppressed on metered networks unless force=true. */
    val requireUnmeteredForSync: Boolean = true,
    /** Bulk segment downloads require charging unless force=true. */
    val requireChargingForBulk: Boolean = true,
    /** Small JSON lookups are allowed on cellular by default. */
    val allowMeteredLookups: Boolean = true,
    val maxCacheBytes: Long = 256L * 1024 * 1024,
    val manifestTtlMillis: Long = 24L * 60 * 60 * 1000,
    val segmentTtlMillis: Long = 7L * 24 * 60 * 60 * 1000,
    /** Answer from an expired manifest instead of failing when offline. */
    val allowStaleReads: Boolean = false,
    /** Prefixes eviction must never delete (for example an offline region). */
    val pinnedPrefixes: Set<Int> = emptySet(),
) {
    init {
        require(maxCacheBytes >= 0) { "maxCacheBytes must be >= 0" }
        require(manifestTtlMillis > 0) { "manifest TTL must be positive" }
        require(segmentTtlMillis > 0) { "segment TTL must be positive" }
        require(pinnedPrefixes.all { it in 0..0xffff }) { "prefixes must be 0..0xffff" }
    }

    internal fun manifestRefreshDenied(force: Boolean, conditions: DeviceConditions): String? {
        if (force) return null
        if (requireUnmeteredForSync && !conditions.isUnmeteredNetwork()) {
            return "unmetered (Wi-Fi) network required for manifest sync"
        }
        return null
    }

    internal fun bulkDownloadDenied(force: Boolean, conditions: DeviceConditions): String? {
        if (force) return null
        if (requireUnmeteredForSync && !conditions.isUnmeteredNetwork()) {
            return "unmetered (Wi-Fi) network required for segment download"
        }
        if (requireChargingForBulk && !conditions.isCharging()) {
            return "charging required for bulk segment download"
        }
        return null
    }

    internal fun lookupDenied(conditions: DeviceConditions): String? {
        if (!allowMeteredLookups && !conditions.isUnmeteredNetwork()) {
            return "metered network lookups are disabled"
        }
        return null
    }
}

// ---------------------------------------------------------------------------
// Ed25519/HMAC manifest parsing and canonical JSON
// ---------------------------------------------------------------------------

internal class ParsedManifest(
    val manifest: Manifest,
    val canonical: ByteArray,
    val raw: ByteArray,
) {
    val fingerprint: ByteArray = sha256(canonical)
    val fingerprintHex: String get() = fingerprint.toHex()
}

internal object ManifestCodec {
    private val TOP_LEVEL_KEYS =
        setOf("version", "prefix_bytes", "segments", "signature", "signature_alg")
    private val SEGMENT_KEYS = setOf(
        "prefix", "count", "bloom_bits", "bloom_k", "root", "file", "size", "sha256",
        "compression",
    )

    fun parse(raw: ByteArray, trust: TrustConfig): ParsedManifest {
        val text = decodeUtf8Strict(raw)
        val json = try {
            JSONObject(text)
        } catch (e: JSONException) {
            throw EdgeHashException.InvalidManifest("malformed JSON", e)
        }
        requireKeys(json, TOP_LEVEL_KEYS, "manifest")

        val version = requiredLong(json, "version", "manifest")
        if (version != MANIFEST_VERSION) {
            throw EdgeHashException.UnsupportedVersion("manifest version $version (supported: $MANIFEST_VERSION)")
        }
        val prefixBytes = requiredLong(json, "prefix_bytes", "manifest").toInt()
        if (prefixBytes !in 1..4) {
            throw EdgeHashException.InvalidManifest("prefix_bytes must be 1..4, got $prefixBytes")
        }

        val array = json.opt("segments")
        if (array !is JSONArray) {
            throw EdgeHashException.InvalidManifest("segments must be an array")
        }
        if (array.length() > MAX_MANIFEST_SEGMENTS) {
            throw EdgeHashException.InvalidManifest("too many segments: ${array.length()}")
        }

        val segments = ArrayList<SegmentMeta>(array.length())
        var previousPrefix = -1
        for (i in 0 until array.length()) {
            val obj = array.opt(i)
            if (obj !is JSONObject) {
                throw EdgeHashException.InvalidManifest("segments[$i] must be an object")
            }
            val where = "segments[$i]"
            requireKeys(obj, SEGMENT_KEYS, where)
            val prefix = requiredLong(obj, "prefix", where).toInt()
            if (prefix !in 0..0xffff) {
                throw EdgeHashException.InvalidManifest("$where.prefix must be 0..65535")
            }
            if (i > 0 && prefix <= previousPrefix) {
                throw EdgeHashException.InvalidManifest("segments must be strictly sorted by prefix")
            }
            previousPrefix = prefix
            if (prefixBytes == 1 && prefix > 0xff) {
                throw EdgeHashException.InvalidManifest("$where.prefix does not fit one-byte prefixes")
            }

            val count = requiredLong(obj, "count", where)
            if (count <= 0 || count > Int.MAX_VALUE) {
                throw EdgeHashException.InvalidManifest("$where.count must be 1..Int.MAX_VALUE")
            }
            val bloomBits = requiredLong(obj, "bloom_bits", where)
            if (bloomBits <= 0 || bloomBits > MAX_BLOOM_BITS) {
                throw EdgeHashException.InvalidManifest("$where.bloom_bits must be 1..$MAX_BLOOM_BITS")
            }
            val bloomK = requiredLong(obj, "bloom_k", where).toInt()
            if (bloomK !in 1..MAX_BLOOM_K) {
                throw EdgeHashException.InvalidManifest("$where.bloom_k must be 1..$MAX_BLOOM_K")
            }
            val size = requiredLong(obj, "size", where)
            // size covers the stored file. For v2 zstd it cannot be derived
            // from count/Bloom parameters; SegmentCodec validates it exactly
            // after download. Here we only need a sane lower bound.
            if (size < SEGMENT_HEADER_BYTES) {
                throw EdgeHashException.InvalidManifest("$where.size is too small: $size")
            }
            val compression = optionalString(obj, "compression", where)
                ?.takeIf { it.isNotEmpty() }
            if (compression != null && compression != "none" && compression != "zstd") {
                throw EdgeHashException.InvalidManifest("$where.compression must be none or zstd")
            }
            val file = requiredString(obj, "file", where)
            val wantedFile = segmentFileName(prefix)
            if (file != wantedFile) {
                throw EdgeHashException.InvalidManifest(
                    "$where.file must be \"$wantedFile\", got \"$file\"",
                )
            }
            val root = requiredHash(obj, "root", where)
            val sha256 = requiredHash(obj, "sha256", where)

            segments.add(
                SegmentMeta(
                    prefix = prefix,
                    count = count,
                    bloomBits = bloomBits,
                    bloomK = bloomK,
                    root = root,
                    file = file,
                    size = size,
                    sha256 = sha256,
                    compression = compression,
                ),
            )
        }

        val signatureAlg = optionalString(json, "signature_alg", "manifest")
            ?.takeIf { it.isNotEmpty() }
        if (signatureAlg != null && signatureAlg != "ed25519" && signatureAlg != "hmac-sha256") {
            throw EdgeHashException.UnsupportedVersion("unknown signature_alg \"$signatureAlg\"")
        }
        val signatureText = optionalString(json, "signature", "manifest")
            ?: throw EdgeHashException.UntrustedManifest("manifest is unsigned")
        val signature = decodeSignature(signatureText)

        val manifest = Manifest(
            version = version,
            prefixBytes = prefixBytes,
            segments = segments,
            signatureAlg = signatureAlg,
            signature = signature,
        )
        val canonical = canonicalBytes(manifest)
        trust.verify(signatureAlg, canonical, signature)
        return ParsedManifest(manifest, canonical, raw.copyOf())
    }

    /**
     * Canonical signing input. Compact UTF-8 JSON, fields in declaration order,
     * excluding signature. signature_alg is included at the end when non-empty
     * and compression at the end of a segment when non-empty, matching
     * internal/edgeset.canonicalManifest. Legacy HMAC manifests have no
     * signature_alg and keep the old byte stream. Clients MUST pin the expected
     * algorithm; see mobile/protocol.md.
     */
    fun canonicalBytes(manifest: Manifest): ByteArray {
        val out = StringBuilder(160 + manifest.segments.size * 180)
        out.append("{\"version\":").append(manifest.version)
        out.append(",\"prefix_bytes\":").append(manifest.prefixBytes)
        out.append(",\"segments\":[")
        for ((index, segment) in manifest.segments.withIndex()) {
            if (index > 0) out.append(',')
            out.append("{\"prefix\":").append(segment.prefix)
            out.append(",\"count\":").append(segment.count)
            out.append(",\"bloom_bits\":").append(segment.bloomBits)
            out.append(",\"bloom_k\":").append(segment.bloomK)
            out.append(",\"root\":\"").append(segment.root.toHex()).append('"')
            out.append(",\"file\":").append(jsonString(segment.file))
            out.append(",\"size\":").append(segment.size)
            out.append(",\"sha256\":\"").append(segment.sha256.toHex()).append('"')
            if (!segment.compression.isNullOrEmpty()) {
                out.append(",\"compression\":").append(jsonString(segment.compression))
            }
            out.append('}')
        }
        out.append("]")
        if (!manifest.signatureAlg.isNullOrEmpty()) {
            out.append(",\"signature_alg\":").append(jsonString(manifest.signatureAlg))
        }
        out.append('}')
        return out.toString().toByteArray(StandardCharsets.UTF_8)
    }

    private fun requireKeys(obj: JSONObject, allowed: Set<String>, where: String) {
        val keys = obj.keys()
        while (keys.hasNext()) {
            val key = keys.next()
            if (key !in allowed) {
                throw EdgeHashException.InvalidManifest("$where: unknown field \"$key\"")
            }
        }
    }

    private fun requiredLong(obj: JSONObject, name: String, where: String): Long {
        if (!obj.has(name) || obj.isNull(name)) {
            throw EdgeHashException.InvalidManifest("$where.$name is required")
        }
        val value = obj.opt(name)
        if (value !is Number) {
            throw EdgeHashException.InvalidManifest("$where.$name must be an integer")
        }
        return value.toString().toLongOrNull()
            ?: throw EdgeHashException.InvalidManifest("$where.$name must be an integer")
    }

    private fun requiredString(obj: JSONObject, name: String, where: String): String =
        optionalString(obj, name, where)
            ?: throw EdgeHashException.InvalidManifest("$where.$name is required")

    private fun optionalString(obj: JSONObject, name: String, where: String): String? {
        if (!obj.has(name) || obj.isNull(name)) return null
        val value = obj.opt(name)
        if (value !is String) {
            throw EdgeHashException.InvalidManifest("$where.$name must be a string")
        }
        return value
    }

    private fun requiredHash(obj: JSONObject, name: String, where: String): ByteArray {
        val hex = requiredString(obj, name, where)
        if (hex.length != HASH_BYTES * 2) {
            throw EdgeHashException.InvalidManifest("$where.$name must be 64 hex characters")
        }
        return try {
            val bytes = decodeHex(hex)
            if (bytes.size != HASH_BYTES) {
                throw EdgeHashException.InvalidManifest("$where.$name must be 32 bytes")
            }
            bytes
        } catch (e: IllegalArgumentException) {
            throw EdgeHashException.InvalidManifest("$where.$name is not valid hex", e)
        }
    }

    /**
     * Accept lowercase/uppercase hex (the target wire form) and, for the current
     * Go server, base64 (encoding/json renders []byte as base64). Length checks
     * happen in TrustConfig.
     */
    private fun decodeSignature(value: String): ByteArray {
        if (value.isEmpty()) {
            throw EdgeHashException.UntrustedManifest("manifest signature is empty")
        }
        val isHex = value.length % 2 == 0 && value.all { Character.digit(it, 16) >= 0 }
        if (isHex) {
            return try {
                decodeHex(value)
            } catch (e: IllegalArgumentException) {
                throw EdgeHashException.UntrustedManifest("signature is not valid hex")
            }
        }
        return try {
            Base64.getDecoder().decode(value)
        } catch (e: IllegalArgumentException) {
            throw EdgeHashException.UntrustedManifest("signature is neither hex nor base64")
        }
    }
}

// ---------------------------------------------------------------------------
// Merkle tree
// ---------------------------------------------------------------------------

object EdgeHashMerkle {
    fun treeDepth(count: Long): Int {
        require(count > 0) { "count must be positive" }
        var size = 1L
        var depth = 0
        while (size < count) {
            require(depth < MAX_MERKLE_DEPTH) { "Merkle tree is too deep" }
            size = size shl 1
            depth++
        }
        return depth
    }

    /**
     * Verifies one inclusion proof. If expectedCount is supplied, the proof
     * must have exactly ceil(log2(count)) levels and index < count.
     */
    fun verify(
        root: ByteArray,
        hash: ByteArray,
        proof: MerkleProof,
        expectedCount: Long? = null,
    ): Boolean {
        if (root.size != HASH_BYTES || hash.size != HASH_BYTES) return false
        if (!proof.hash.contentEquals(hash)) return false
        if (proof.path.size != proof.right.size) return false
        val depth = proof.path.size
        if (depth > MAX_MERKLE_DEPTH) return false
        if (proof.index < 0) return false
        if (expectedCount != null) {
            if (expectedCount <= 0 || proof.index >= expectedCount) return false
            if (depth != treeDepth(expectedCount)) return false
        } else {
            if (depth == MAX_MERKLE_DEPTH && proof.index >= (1L shl MAX_MERKLE_DEPTH)) return false
            if (depth < MAX_MERKLE_DEPTH && proof.index >= (1L shl depth)) return false
        }
        for (i in 0 until depth) {
            if (proof.path[i].size != HASH_BYTES) return false
            val leftChild = ((proof.index ushr i) and 1L) == 0L
            if (proof.right[i] != leftChild) return false
        }

        var node = sha256(LEAF_DOMAIN, hash)
        for (i in 0 until depth) {
            val sibling = proof.path[i]
            node = if (proof.right[i]) {
                sha256(NODE_DOMAIN, node, sibling)
            } else {
                sha256(NODE_DOMAIN, sibling, node)
            }
        }
        return MessageDigest.isEqual(node, root)
    }

    /** Recomputes the power-of-two padded root over a sorted hash list. */
    internal fun root(hashes: Array<ByteArray>): ByteArray {
        if (hashes.isEmpty()) return EMPTY_LEAF.copyOf()
        if (hashes.size > (1 shl 24)) {
            throw EdgeHashException.InvalidSegment("hash list is too large to recompute a Merkle root")
        }
        var size = 1
        while (size < hashes.size) size = size shl 1

        var level = ByteArray(size * HASH_BYTES)
        for (i in hashes.indices) {
            val leaf = sha256(LEAF_DOMAIN, hashes[i])
            leaf.copyInto(level, i * HASH_BYTES)
        }
        for (i in hashes.size until size) {
            EMPTY_LEAF.copyInto(level, i * HASH_BYTES)
        }

        var count = size
        while (count > 1) {
            val nextCount = count / 2
            val next = ByteArray(nextCount * HASH_BYTES)
            val digest = MessageDigest.getInstance("SHA-256")
            for (i in 0 until nextCount) {
                digest.reset()
                digest.update(NODE_DOMAIN)
                digest.update(level, i * 2 * HASH_BYTES, 2 * HASH_BYTES)
                digest.digest().copyInto(next, i * HASH_BYTES)
            }
            level = next
            count = nextCount
        }
        return level.copyOfRange(0, HASH_BYTES)
    }

    internal fun leafHash(hash: ByteArray): ByteArray = sha256(LEAF_DOMAIN, hash)

    internal fun nodeHash(left: ByteArray, right: ByteArray): ByteArray =
        sha256(NODE_DOMAIN, left, right)
}

// ---------------------------------------------------------------------------
// Bloom filter
// ---------------------------------------------------------------------------

internal class BloomFilterView(
    private val bits: ByteArray,
    private val m: Long,
    private val k: Int,
) {
    init {
        require(m > 0) { "Bloom bit count must be positive" }
        require(k > 0) { "Bloom probe count must be positive" }
        require(bits.size >= (m + 7) / 8) { "Bloom bitset is too short" }
    }

    fun maybeContain(hash: ByteArray): Boolean {
        if (hash.size != HASH_BYTES) return false
        val sum = sha256(BLOOM_DOMAIN, hash)
        val h1 = readU64(sum, 0)
        val step = readU64(sum, 8) or 1L
        var x = h1
        repeat(k) {
            val position = java.lang.Long.remainderUnsigned(x, m)
            val byteIndex = (position ushr 3).toInt()
            val mask = 1 shl (position and 7L).toInt()
            if (bits[byteIndex].toInt() and mask == 0) return false
            x += step
        }
        return true
    }
}

// ---------------------------------------------------------------------------
// Segment parsing
// ---------------------------------------------------------------------------

internal class Segment(
    val meta: SegmentMeta,
    val bloom: BloomFilterView,
    val hashes: Array<ByteArray>,
    val root: ByteArray,
) {
    fun maybeContain(hash: ByteArray): Boolean = bloom.maybeContain(hash)

    fun exactIndex(hash: ByteArray): Int {
        var low = 0
        var high = hashes.size - 1
        while (low <= high) {
            val mid = (low + high) ushr 1
            val cmp = compareUnsigned(hashes[mid], hash)
            when {
                cmp < 0 -> low = mid + 1
                cmp > 0 -> high = mid - 1
                else -> return mid
            }
        }
        return -1
    }
}

internal object SegmentCodec {
    private const val FORMAT_V1 = 1L
    private const val FORMAT_V2 = 2L
    private const val HEADER_V1 = 40
    private const val HEADER_V2 = 48

    fun parse(data: ByteArray, meta: SegmentMeta, prefixBytes: Int): Segment {
        if (data.size.toLong() != meta.size) {
            throw EdgeHashException.InvalidSegment(
                "prefix ${meta.prefix.toString(16)} size ${data.size}, manifest says ${meta.size}",
            )
        }
        val digest = sha256(data)
        if (!MessageDigest.isEqual(digest, meta.sha256)) {
            throw EdgeHashException.InvalidSegment("SHA-256 mismatch for prefix ${meta.prefix.toString(16)}")
        }
        if (data.size < HEADER_V1) {
            throw EdgeHashException.InvalidSegment("file is shorter than the 40-byte header")
        }
        val magic = String(data, 0, 8, StandardCharsets.US_ASCII)
        if (magic != SEGMENT_MAGIC) {
            throw EdgeHashException.InvalidSegment("bad magic \"$magic\"")
        }
        val format = readU32(data, 8)
        val headerSize = when (format) {
            FORMAT_V1 -> HEADER_V1
            FORMAT_V2 -> HEADER_V2
            else -> throw EdgeHashException.UnsupportedVersion(
                "segment format $format (supported: 1 uncompressed, 2 uncompressed)",
            )
        }
        if (data.size < headerSize) {
            throw EdgeHashException.InvalidSegment("file is shorter than the $headerSize-byte header")
        }
        val prefix = readU16(data, 12)
        if (prefix != meta.prefix) {
            throw EdgeHashException.InvalidSegment(
                "header prefix ${prefix.toString(16)} != manifest ${meta.prefix.toString(16)}",
            )
        }
        val count = readU32(data, 16)
        if (count != meta.count) {
            throw EdgeHashException.InvalidSegment("header count $count != manifest ${meta.count}")
        }
        if (count > Int.MAX_VALUE) {
            throw EdgeHashException.InvalidSegment("hash count does not fit an Int")
        }
        val bloomBits = readU64(data, 20)
        val bloomK = readU32(data, 28)
        val bloomBytes = readU64(data, 32)
        if (bloomBits != meta.bloomBits || bloomK != meta.bloomK.toLong()) {
            throw EdgeHashException.InvalidSegment("Bloom parameters do not match the manifest")
        }
        if (bloomBits <= 0 || bloomBits > MAX_BLOOM_BITS || bloomK !in 1L..MAX_BLOOM_K.toLong()) {
            throw EdgeHashException.InvalidSegment("invalid Bloom parameters")
        }
        val wantedBloomBytes = (bloomBits + 7) / 8
        if (bloomBytes != wantedBloomBytes) {
            throw EdgeHashException.InvalidSegment("Bloom byte length $bloomBytes, want $wantedBloomBytes")
        }
        val expectedRaw = bloomBytes + count * HASH_BYTES
        if (expectedRaw < bloomBytes || expectedRaw > Int.MAX_VALUE) {
            throw EdgeHashException.InvalidSegment("segment payload is too large")
        }

        var payloadStart: Int
        when (format) {
            FORMAT_V1 -> {
                if (readU16(data, 14) != 0) {
                    throw EdgeHashException.InvalidSegment("reserved header field must be zero")
                }
                if (!meta.compression.isNullOrEmpty() && meta.compression != "none") {
                    throw EdgeHashException.InvalidSegment(
                        "v1 segment cannot carry compression ${meta.compression}",
                    )
                }
                payloadStart = HEADER_V1
                val stored = (data.size - payloadStart).toLong()
                if (stored != expectedRaw) {
                    throw EdgeHashException.InvalidSegment(
                        "payload size $stored, header implies $expectedRaw",
                    )
                }
            }

            FORMAT_V2 -> {
                val code = data[14].toInt() and 0xff
                val reserved = data[15].toInt() and 0xff
                val rawSize = readU64(data, 40)
                val headerCompression = when (code) {
                    0 -> "none"
                    1 -> "zstd"
                    else -> throw EdgeHashException.InvalidSegment("unsupported compression code $code")
                }
                val metaCompression = meta.compression
                if (metaCompression.isNullOrEmpty()) {
                    if (code != 0) {
                        throw EdgeHashException.InvalidSegment(
                            "header is $headerCompression but the manifest omits compression",
                        )
                    }
                } else if (metaCompression != headerCompression) {
                    throw EdgeHashException.InvalidSegment(
                        "header compression $headerCompression, manifest says $metaCompression",
                    )
                }
                if (reserved != 0) {
                    throw EdgeHashException.InvalidSegment("reserved header field must be zero")
                }
                if (rawSize != expectedRaw) {
                    throw EdgeHashException.InvalidSegment(
                        "raw payload size $rawSize, header implies $expectedRaw",
                    )
                }
                if (code == 1) {
                    // No zstd implementation is bundled (no third-party deps).
                    // Fail closed before allocating or decoding the payload.
                    throw EdgeHashException.UnsupportedVersion(
                        "segment ${meta.prefix.toString(16)} is zstd-compressed; " +
                            "rebuild the store with uncompressed v2 segments for this SDK",
                    )
                }
                payloadStart = HEADER_V2
                val stored = (data.size - payloadStart).toLong()
                if (stored != rawSize) {
                    throw EdgeHashException.InvalidSegment("stored payload size $stored, header says $rawSize")
                }
            }

            else -> error("unreachable")
        }

        val bloomStart = payloadStart
        val bloomEnd = bloomStart + bloomBytes.toInt()
        val bloom = BloomFilterView(
            data.copyOfRange(bloomStart, bloomEnd),
            bloomBits,
            bloomK.toInt(),
        )

        val hashStart = bloomEnd
        val hashes = Array(count.toInt()) { ByteArray(HASH_BYTES) }
        for (i in hashes.indices) {
            val offset = hashStart + i * HASH_BYTES
            data.copyInto(hashes[i], 0, offset, offset + HASH_BYTES)
            if (i > 0 && compareUnsigned(hashes[i - 1], hashes[i]) >= 0) {
                throw EdgeHashException.InvalidSegment("hashes are not strictly sorted at index $i")
            }
            val routed = routingPrefix(hashes[i], prefixBytes)
            if (routed != meta.prefix) {
                throw EdgeHashException.InvalidSegment(
                    "hash ${hashes[i].toHex()} routes to ${routed.toString(16)}, not ${meta.prefix.toString(16)}",
                )
            }
        }

        val root = EdgeHashMerkle.root(hashes)
        if (!MessageDigest.isEqual(root, meta.root)) {
            throw EdgeHashException.InvalidSegment("Merkle root mismatch")
        }
        for (hash in hashes) {
            if (!bloom.maybeContain(hash)) {
                throw EdgeHashException.InvalidSegment(
                    "Bloom filter has a false negative for ${hash.toHex()}",
                )
            }
        }
        return Segment(meta, bloom, hashes, root)
    }
}

// ---------------------------------------------------------------------------
// Local cache
// ---------------------------------------------------------------------------

internal data class CachedManifest(
    val parsed: ParsedManifest,
    val fetchedAtMillis: Long,
    val etag: String?,
    val sourceUrl: String,
) {
    val manifest: Manifest get() = parsed.manifest
    val fingerprintHex: String get() = parsed.fingerprintHex

    fun ageMillis(nowMillis: Long): Long = (nowMillis - fetchedAtMillis).coerceAtLeast(0L)

    fun isExpired(policy: CachePolicy, nowMillis: Long): Boolean =
        ageMillis(nowMillis) > policy.manifestTtlMillis
}

internal class SegmentCache(
    private val root: File,
    private val trust: TrustConfig,
    private val maxManifestBytes: Long,
) {
    private val segmentsDir = File(root, "segments")
    private val manifestFile = File(root, "manifest.json")
    private val manifestMetaFile = File(root, "manifest.meta.json")

    internal var lastLoadError: String? = null
        private set

    init {
        if (!segmentsDir.isDirectory && !segmentsDir.mkdirs()) {
            throw EdgeHashException.Storage("cannot create cache directory ${segmentsDir.path}")
        }
    }

    @Synchronized
    fun loadManifest(): CachedManifest? {
        lastLoadError = null
        if (!manifestFile.isFile) return null
        if (manifestFile.length() > maxManifestBytes) {
            lastLoadError = "cached manifest exceeds $maxManifestBytes bytes"
            return null
        }
        return try {
            val raw = manifestFile.readBytes()
            val parsed = ManifestCodec.parse(raw, trust)
            val meta = readManifestMeta()
            val metaMatches = meta?.optString("fingerprint", "") == parsed.fingerprintHex
            val fetchedAt = if (metaMatches) {
                meta?.optLong("fetched_at_ms", manifestFile.lastModified()) ?: manifestFile.lastModified()
            } else {
                manifestFile.lastModified()
            }
            val etag = if (metaMatches) {
                if (meta.has("etag") && !meta.isNull("etag")) meta.getString("etag") else null
            } else {
                null
            }
            val sourceUrl = if (metaMatches) meta.optString("source_url", "") else ""
            CachedManifest(parsed, fetchedAt, etag, sourceUrl)
        } catch (e: Exception) {
            lastLoadError = e.message ?: e.javaClass.simpleName
            null
        }
    }

    @Synchronized
    fun installManifest(
        parsed: ParsedManifest,
        etag: String?,
        sourceUrl: String,
        fetchedAtMillis: Long,
    ): CachedManifest {
        atomicWrite(manifestFile, parsed.raw)
        val meta = JSONObject()
            .put("version", parsed.manifest.version)
            .put("fingerprint", parsed.fingerprintHex)
            .put("fetched_at_ms", fetchedAtMillis)
            .put("source_url", sourceUrl)
        if (etag != null) meta.put("etag", etag)
        atomicWrite(manifestMetaFile, meta.toString().toByteArray(StandardCharsets.UTF_8))
        return CachedManifest(parsed, fetchedAtMillis, etag, sourceUrl)
    }

    @Synchronized
    fun touchManifest(cached: CachedManifest, fetchedAtMillis: Long): CachedManifest {
        val meta = JSONObject()
            .put("version", cached.manifest.version)
            .put("fingerprint", cached.fingerprintHex)
            .put("fetched_at_ms", fetchedAtMillis)
            .put("source_url", cached.sourceUrl)
        if (cached.etag != null) meta.put("etag", cached.etag)
        atomicWrite(manifestMetaFile, meta.toString().toByteArray(StandardCharsets.UTF_8))
        return cached.copy(fetchedAtMillis = fetchedAtMillis)
    }

    @Synchronized
    fun readSegment(meta: SegmentMeta, prefixBytes: Int): Segment? {
        val file = File(segmentsDir, segmentBaseName(meta.prefix))
        if (!file.isFile) return null
        if (file.length() != meta.size) {
            file.delete()
            throw EdgeHashException.InvalidSegment(
                "cached segment ${meta.prefix.toString(16)} has size ${file.length()}, want ${meta.size}",
            )
        }
        val data = try {
            file.readBytes()
        } catch (e: IOException) {
            throw EdgeHashException.Storage("cannot read cached segment", e)
        }
        return SegmentCodec.parse(data, meta, prefixBytes)
    }

    @Synchronized
    fun isSegmentValid(meta: SegmentMeta): Boolean {
        val file = File(segmentsDir, segmentBaseName(meta.prefix))
        if (!file.isFile || file.length() != meta.size) return false
        return try {
            MessageDigest.isEqual(sha256(file.readBytes()), meta.sha256)
        } catch (e: IOException) {
            false
        }
    }

    @Synchronized
    fun installSegment(meta: SegmentMeta, data: ByteArray) {
        if (data.size.toLong() != meta.size) {
            throw EdgeHashException.InvalidSegment("segment size changed before install")
        }
        if (!MessageDigest.isEqual(sha256(data), meta.sha256)) {
            throw EdgeHashException.InvalidSegment("segment checksum changed before install")
        }
        atomicWrite(File(segmentsDir, segmentBaseName(meta.prefix)), data)
    }

    @Synchronized
    fun deleteSegment(prefix: Int) {
        File(segmentsDir, segmentBaseName(prefix)).delete()
    }

    @Synchronized
    fun totalBytes(): Long {
        val files = segmentsDir.listFiles() ?: return 0L
        return files.asSequence().filter { it.isFile }.sumOf { it.length() }
    }

    /**
     * Deletes expired segments (by mtime) and then LRU segments until the total
     * is <= maxBytes. Pinned prefixes are never evicted. Returns the resulting
     * total. This is intentionally simple skeleton code: production should use
     * a persistent index and update access time on reads.
     */
    @Synchronized
    fun enforceQuota(
        maxBytes: Long,
        segmentTtlMillis: Long,
        nowMillis: Long,
        pinnedPrefixes: Set<Int>,
    ): Long {
        val files = (segmentsDir.listFiles() ?: emptyArray())
            .filter { it.isFile && it.name.endsWith(".seg") }

        var total = files.sumOf { it.length() }
        for (file in files) {
            val prefix = prefixFromFileName(file.name) ?: continue
            if (prefix in pinnedPrefixes) continue
            if (nowMillis - file.lastModified() > segmentTtlMillis) {
                val length = file.length()
                if (file.delete()) total -= length
            }
        }

        if (total > maxBytes) {
            val candidates = files
                .asSequence()
                .filter {
                    it.exists() && prefixFromFileName(it.name)?.let { p -> p !in pinnedPrefixes } == true
                }
                .sortedBy { it.lastModified() }
            for (file in candidates) {
                if (total <= maxBytes) break
                val length = file.length()
                total -= length
                file.delete()
            }
        }
        return total
    }

    @Synchronized
    fun pruneOrphans(validPrefixes: Set<Int>, pinnedPrefixes: Set<Int>) {
        val files = segmentsDir.listFiles() ?: return
        for (file in files) {
            if (!file.isFile || !file.name.endsWith(".seg")) continue
            val prefix = prefixFromFileName(file.name) ?: continue
            if (prefix !in validPrefixes && prefix !in pinnedPrefixes) {
                file.delete()
            }
        }
    }

    @Synchronized
    fun clear() {
        manifestFile.delete()
        manifestMetaFile.delete()
        segmentsDir.listFiles()?.forEach { it.delete() }
    }

    private fun readManifestMeta(): JSONObject? = try {
        if (manifestMetaFile.isFile) JSONObject(manifestMetaFile.readText()) else null
    } catch (e: Exception) {
        null
    }

    private fun segmentBaseName(prefix: Int): String =
        String.format(Locale.ROOT, "%04x.seg", prefix)

    private fun prefixFromFileName(name: String): Int? {
        if (!name.endsWith(".seg")) return null
        val base = name.removeSuffix(".seg")
        if (base.length != 4) return null
        return base.toIntOrNull(16)
    }

    private fun atomicWrite(target: File, data: ByteArray) {
        target.parentFile?.let {
            if (!it.isDirectory && !it.mkdirs()) {
                throw EdgeHashException.Storage("cannot create ${it.path}")
            }
        }
        val tmp = File(target.parentFile, ".${target.name}.tmp-${System.nanoTime()}")
        try {
            FileOutputStream(tmp).use { out ->
                out.write(data)
                out.fd.sync()
            }
            try {
                Files.move(
                    tmp.toPath(),
                    target.toPath(),
                    StandardCopyOption.REPLACE_EXISTING,
                    StandardCopyOption.ATOMIC_MOVE,
                )
            } catch (e: AtomicMoveNotSupportedException) {
                Files.move(tmp.toPath(), target.toPath(), StandardCopyOption.REPLACE_EXISTING)
            } catch (e: UnsupportedOperationException) {
                Files.move(tmp.toPath(), target.toPath(), StandardCopyOption.REPLACE_EXISTING)
            }
        } catch (e: IOException) {
            throw EdgeHashException.Storage("cannot write ${target.path}", e)
        } finally {
            if (tmp.exists()) tmp.delete()
        }
    }

}

// ---------------------------------------------------------------------------
// Configuration and client
// ---------------------------------------------------------------------------

data class EdgeHashConfig(
    val baseUrl: String,
    val trust: TrustConfig,
    val cacheDir: File,
    val network: NetworkConfig = NetworkConfig(),
    val cachePolicy: CachePolicy = CachePolicy(),
    /** Cache only selected segment prefixes; null means every segment. */
    val prefixFilter: Set<Int>? = null,
    /** Development only. Production must use https. */
    val allowInsecureHttp: Boolean = false,
) {
    val normalizedBaseUrl: String = normalizeBaseUrl(baseUrl, allowInsecureHttp)

    init {
        require(cacheDir.path.isNotBlank()) { "cacheDir must not be empty" }
        require(prefixFilter == null || prefixFilter.all { it in 0..0xffff }) {
            "prefixFilter values must be 0..0xffff"
        }
    }
}

/**
 * Blocking client for an immutable edgehash set.
 *
 * Typical use:
 * ```
 * val client = EdgeHashClient(config, AndroidConditions(context))
 * client.sync()
 * val result = client.lookup(hashHex)
 * ```
 * Every public method performs disk and/or network I/O; call from a worker
 * thread. `sync` is serialized; `lookup` may run concurrently with itself.
 */
class EdgeHashClient(
    private val config: EdgeHashConfig,
    private val conditions: DeviceConditions = AlwaysAllowConditions,
    private val clock: () -> Long = System::currentTimeMillis,
    transport: Transport? = null,
) {
    private val http: Transport =
        transport ?: HttpUrlConnectionTransport(config.normalizedBaseUrl, config.network)

    private val cache = SegmentCache(
        config.cacheDir,
        config.trust,
        config.network.maxManifestBytes,
    )

    @Volatile
    private var cachedManifest: CachedManifest? = cache.loadManifest()

    private val segmentMemo = object : LinkedHashMap<String, Segment>(8, 0.75f, true) {
        override fun removeEldestEntry(eldest: MutableMap.MutableEntry<String, Segment>): Boolean =
            size > 4
    }

    val lastCacheLoadError: String?
        get() = cache.lastLoadError

    fun manifest(): Manifest? = cachedManifest?.manifest

    fun manifestFingerprint(): String? = cachedManifest?.fingerprintHex

    fun cacheBytes(): Long = cache.totalBytes()

    /** Refreshes the manifest and downloads missing segments. Blocking. */
    @Synchronized
    fun sync(force: Boolean = false): SyncReport {
        val now = clock()
        val policy = config.cachePolicy
        val previous = cachedManifest

        val manifestDenied = policy.manifestRefreshDenied(force, conditions)
        if (manifestDenied != null) {
            if (previous != null && !previous.isExpired(policy, now)) {
                return SyncReport(
                    manifestVersion = previous.manifest.version,
                    manifestFingerprint = previous.fingerprintHex,
                    notModified = false,
                    manifestBytes = 0,
                    segmentsSelected = 0,
                    segmentsDownloaded = 0,
                    segmentsAlreadyCached = 0,
                    segmentsSkipped = 0,
                    bytesDownloaded = 0,
                    skippedReason = manifestDenied,
                    cacheBytes = cache.totalBytes(),
                )
            }
            throw EdgeHashException.PolicyDenied(manifestDenied)
        }

        val ifNoneMatch = if (!force) previous?.etag else null
        val response = http.get(
            "/v1/manifest",
            "application/json",
            config.network.maxManifestBytes,
            ifNoneMatch,
        )
        var manifestBytes = 0L
        var notModified = false
        val current: CachedManifest
        if (response.statusCode == HttpURLConnection.HTTP_NOT_MODIFIED) {
            if (previous == null) {
                throw EdgeHashException.HttpError(304, "/v1/manifest", "304 without a cached manifest")
            }
            current = cache.touchManifest(previous, now)
            cachedManifest = current
            notModified = true
        } else if (response.statusCode == HttpURLConnection.HTTP_OK) {
            val body = response.body
                ?: throw EdgeHashException.InvalidResponse("manifest response has no body")
            manifestBytes = body.size.toLong()
            val parsed = ManifestCodec.parse(body, config.trust)
            current = cache.installManifest(
                parsed,
                response.etag,
                config.normalizedBaseUrl,
                now,
            )
            cachedManifest = current
            val validPrefixes = parsed.manifest.segments.mapTo(HashSet()) { it.prefix }
            cache.pruneOrphans(validPrefixes, policy.pinnedPrefixes)
            synchronized(segmentMemo) { segmentMemo.clear() }
        } else {
            throw EdgeHashException.HttpError(
                response.statusCode,
                "/v1/manifest",
                errorDetail(response.body),
            )
        }

        val selected = selectSegments(current.manifest)
        val bulkDenied = if (selected.isEmpty()) {
            null
        } else {
            policy.bulkDownloadDenied(force, conditions)
        }
        if (bulkDenied != null) {
            return SyncReport(
                manifestVersion = current.manifest.version,
                manifestFingerprint = current.fingerprintHex,
                notModified = notModified,
                manifestBytes = manifestBytes,
                segmentsSelected = selected.size,
                segmentsDownloaded = 0,
                segmentsAlreadyCached = 0,
                segmentsSkipped = 0,
                bytesDownloaded = manifestBytes,
                deferredReason = bulkDenied,
                cacheBytes = cache.totalBytes(),
            )
        }

        var downloaded = 0
        var alreadyCached = 0
        var skipped = 0
        var segmentsBytes = 0L
        var stoppedReason: String? = null
        val prefixBytes = current.manifest.prefixBytes

        for (meta in selected) {
            if (cache.isSegmentValid(meta)) {
                alreadyCached++
                continue
            }
            if (meta.compression == "zstd") {
                skipped++
                stoppedReason = "segment ${meta.prefix.toString(16)} is zstd-compressed; " +
                    "this SDK can only cache uncompressed v2 segments (lookups still use remote proofs)"
                continue
            }
            if (meta.size > config.network.maxSegmentBytes) {
                skipped++
                stoppedReason = "segment ${meta.prefix.toString(16)} is larger than maxSegmentBytes"
                continue
            }
            if (meta.size > policy.maxCacheBytes) {
                stoppedReason = "segment ${meta.prefix.toString(16)} alone exceeds maxCacheBytes"
                break
            }
            val retained = cache.enforceQuota(
                policy.maxCacheBytes,
                policy.segmentTtlMillis,
                now,
                policy.pinnedPrefixes,
            )
            if (retained + meta.size > policy.maxCacheBytes) {
                stoppedReason = "cache quota ${policy.maxCacheBytes} cannot fit segment ${meta.prefix.toString(16)}"
                break
            }

            val path = String.format(Locale.ROOT, "/v1/segment/%04x", meta.prefix)
            val segmentResponse = http.get(
                path,
                "application/octet-stream",
                meta.size,
                null,
            )
            if (segmentResponse.statusCode != HttpURLConnection.HTTP_OK) {
                throw EdgeHashException.HttpError(
                    segmentResponse.statusCode,
                    path,
                    errorDetail(segmentResponse.body),
                )
            }
            val data = segmentResponse.body
                ?: throw EdgeHashException.InvalidResponse("segment response has no body")
            // Fully validate before the bytes become visible to lookup().
            SegmentCodec.parse(data, meta, prefixBytes)
            cache.installSegment(meta, data)
            downloaded++
            segmentsBytes += data.size
        }

        val finalRetained = cache.enforceQuota(
            policy.maxCacheBytes,
            policy.segmentTtlMillis,
            clock(),
            policy.pinnedPrefixes,
        )
        return SyncReport(
            manifestVersion = current.manifest.version,
            manifestFingerprint = current.fingerprintHex,
            notModified = notModified,
            manifestBytes = manifestBytes,
            segmentsSelected = selected.size,
            segmentsDownloaded = downloaded,
            segmentsAlreadyCached = alreadyCached,
            segmentsSkipped = skipped,
            bytesDownloaded = manifestBytes + segmentsBytes,
            stoppedReason = stoppedReason,
            cacheBytes = finalRetained,
        )
    }

    /** Blocking lookup. See LookupOptions for offline/stale behavior. */
    fun lookup(hash: ByteArray, options: LookupOptions = LookupOptions()): LookupResult {
        if (hash.size != HASH_BYTES) {
            throw EdgeHashException.InvalidHash("hash must be exactly 32 bytes, got ${hash.size}")
        }
        val query = hash.copyOf()
        val current = ensureManifest(options)
        val now = clock()
        val stale = current.isExpired(config.cachePolicy, now)
        val prefix = routingPrefix(query, current.manifest.prefixBytes)
        val prefixHex = String.format(Locale.ROOT, "%04x", prefix)
        val meta = current.manifest.segmentForPrefix(prefix)
            ?: return LookupResult(
                present = false,
                prefix = prefix,
                prefixHex = prefixHex,
                source = LookupSource.NO_SEGMENT,
                verified = true,
                staleManifest = stale,
                manifestFingerprint = current.fingerprintHex,
            )

        var segment: Segment? = null
        try {
            segment = loadSegment(meta, current.fingerprintHex, current.manifest.prefixBytes)
        } catch (e: EdgeHashException.InvalidSegment) {
            cache.deleteSegment(meta.prefix)
        } catch (e: EdgeHashException.UnsupportedVersion) {
            // A zstd/unknown-format file must not poison local lookups;
            // resolve through a remote proof instead.
            cache.deleteSegment(meta.prefix)
        }
        if (segment != null) {
            if (!segment.maybeContain(query)) {
                return LookupResult(
                    present = false,
                    prefix = prefix,
                    prefixHex = prefixHex,
                    source = LookupSource.LOCAL_BLOOM_NEGATIVE,
                    verified = true,
                    staleManifest = stale,
                    manifestFingerprint = current.fingerprintHex,
                )
            }
            val index = segment.exactIndex(query)
            if (index >= 0) {
                return LookupResult(
                    present = true,
                    prefix = prefix,
                    prefixHex = prefixHex,
                    source = LookupSource.LOCAL_EXACT,
                    verified = true,
                    staleManifest = stale,
                    manifestFingerprint = current.fingerprintHex,
                    rootHex = segment.root.toHex(),
                )
            }
            return LookupResult(
                present = false,
                prefix = prefix,
                prefixHex = prefixHex,
                source = LookupSource.LOCAL_BLOOM_FALSE_POSITIVE,
                verified = true,
                staleManifest = stale,
                manifestFingerprint = current.fingerprintHex,
            )
        }

        if (options.offline) {
            throw EdgeHashException.Offline()
        }
        config.cachePolicy.lookupDenied(conditions)?.let {
            throw EdgeHashException.PolicyDenied(it)
        }
        return remoteLookup(current, meta, query, prefix, prefixHex, stale)
    }

    fun lookup(hashHex: String, options: LookupOptions = LookupOptions()): LookupResult {
        val bytes = try {
            decodeHex(hashHex)
        } catch (e: IllegalArgumentException) {
            throw EdgeHashException.InvalidHash(e.message ?: "invalid hex")
        }
        if (bytes.size != HASH_BYTES) {
            throw EdgeHashException.InvalidHash("hash must be 64 hex characters")
        }
        return lookup(bytes, options)
    }

    /** Applies TTL + LRU quota eviction. Blocking. */
    fun trimCache(nowMillis: Long = clock()): Long = cache.enforceQuota(
        config.cachePolicy.maxCacheBytes,
        config.cachePolicy.segmentTtlMillis,
        nowMillis,
        config.cachePolicy.pinnedPrefixes,
    )

    /** Deletes the local manifest and all cached segments. Blocking. */
    @Synchronized
    fun clearCache() {
        cache.clear()
        cachedManifest = null
        synchronized(segmentMemo) { segmentMemo.clear() }
    }

    private fun ensureManifest(options: LookupOptions): CachedManifest {
        val policy = config.cachePolicy
        val allowStale = options.allowStaleManifest || policy.allowStaleReads
        var current = cachedManifest
        if (current == null) {
            if (options.offline) throw EdgeHashException.NotSynced()
            sync(force = false)
            current = cachedManifest ?: throw EdgeHashException.NotSynced()
            return current
        }
        if (!current.isExpired(policy, clock())) return current

        if (options.offline) {
            if (allowStale) return current
            throw EdgeHashException.StaleManifest(current.fingerprintHex)
        }
        return try {
            sync(force = false)
            cachedManifest ?: current
        } catch (e: EdgeHashException) {
            if (allowStale) {
                current
            } else {
                throw EdgeHashException.StaleManifest(current.fingerprintHex, e)
            }
        }
    }

    private fun loadSegment(
        meta: SegmentMeta,
        manifestFingerprint: String,
        prefixBytes: Int,
    ): Segment? {
        val key = "$manifestFingerprint:${meta.prefix}:${meta.sha256.toHex()}"
        synchronized(segmentMemo) {
            segmentMemo[key]?.let { return it }
        }
        val segment = cache.readSegment(meta, prefixBytes) ?: return null
        synchronized(segmentMemo) { segmentMemo[key] = segment }
        return segment
    }

    private fun remoteLookup(
        current: CachedManifest,
        meta: SegmentMeta,
        query: ByteArray,
        prefix: Int,
        prefixHex: String,
        stale: Boolean,
    ): LookupResult {
        val path = "/v1/lookup/${query.toHex()}"
        val response = http.get(
            path,
            "application/json",
            config.network.maxLookupBytes,
            null,
        )
        if (response.statusCode == HttpURLConnection.HTTP_NOT_FOUND) {
            // Compatibility with older servers; absence is unverified here.
            return LookupResult(
                present = false,
                prefix = prefix,
                prefixHex = prefixHex,
                source = LookupSource.REMOTE_ABSENT,
                verified = false,
                staleManifest = stale,
                manifestFingerprint = current.fingerprintHex,
            )
        }
        if (response.statusCode != HttpURLConnection.HTTP_OK) {
            throw EdgeHashException.HttpError(response.statusCode, path, errorDetail(response.body))
        }
        val body = response.body
            ?: throw EdgeHashException.InvalidResponse("lookup response has no body")
        val json = try {
            JSONObject(decodeUtf8Strict(body))
        } catch (e: JSONException) {
            throw EdgeHashException.InvalidResponse("lookup response is not valid JSON")
        } catch (e: EdgeHashException.InvalidManifest) {
            throw EdgeHashException.InvalidResponse("lookup response is not valid UTF-8")
        }
        val present = try {
            json.getBoolean("present")
        } catch (e: JSONException) {
            throw EdgeHashException.InvalidResponse("lookup response lacks boolean present")
        }

        optionalString(json, "prefix")?.let { remotePrefix ->
            val parsedPrefix = parsePrefix(remotePrefix)
            if (parsedPrefix != prefix) {
                throw EdgeHashException.InvalidProof(
                    "server prefix $remotePrefix != expected $prefixHex",
                )
            }
        }
        if (!present) {
            return LookupResult(
                present = false,
                prefix = prefix,
                prefixHex = prefixHex,
                source = LookupSource.REMOTE_ABSENT,
                verified = false,
                staleManifest = stale,
                manifestFingerprint = current.fingerprintHex,
            )
        }

        val proofJson = json.opt("proof")
        if (proofJson !is JSONObject) {
            throw EdgeHashException.InvalidProof("present=true without a proof object")
        }
        val proof = parseProof(proofJson)
        if (!proof.hash.contentEquals(query)) {
            throw EdgeHashException.InvalidProof("proof is for a different hash")
        }
        if (proof.index < 0 || proof.index >= meta.count) {
            throw EdgeHashException.InvalidProof("proof index ${proof.index} is outside segment count ${meta.count}")
        }
        val depth = EdgeHashMerkle.treeDepth(meta.count)
        if (proof.path.size != depth) {
            throw EdgeHashException.InvalidProof(
                "proof has ${proof.path.size} levels, expected $depth for ${meta.count} hashes",
            )
        }
        if (!EdgeHashMerkle.verify(meta.root, query, proof, meta.count)) {
            throw EdgeHashException.InvalidProof("proof does not verify against the signed root")
        }

        optionalString(json, "root")?.let { rootHex ->
            val root = try {
                decodeHex(rootHex)
            } catch (e: IllegalArgumentException) {
                throw EdgeHashException.InvalidProof("server root is not valid hex")
            }
            if (root.size != HASH_BYTES) {
                throw EdgeHashException.InvalidProof("server root must be 32 bytes")
            }
            if (!MessageDigest.isEqual(root, meta.root)) {
                throw EdgeHashException.StaleManifest(current.fingerprintHex)
            }
        }

        return LookupResult(
            present = true,
            prefix = prefix,
            prefixHex = prefixHex,
            source = LookupSource.REMOTE_PROOF,
            verified = true,
            staleManifest = stale,
            manifestFingerprint = current.fingerprintHex,
            proof = proof,
            rootHex = meta.root.toHex(),
        )
    }

    private fun parseProof(obj: JSONObject): MerkleProof {
        val hashText = firstString(obj, "hash", "Hash")
            ?: throw EdgeHashException.InvalidProof("proof.hash is required")
        val indexText = firstNumber(obj, "index", "Index")
            ?: throw EdgeHashException.InvalidProof("proof.index is required")
        val pathValue = firstValue(obj, "path", "Path")
            ?: throw EdgeHashException.InvalidProof("proof.path is required")
        val rightValue = firstValue(obj, "right", "Right")
            ?: throw EdgeHashException.InvalidProof("proof.right is required")
        if (pathValue !is JSONArray || rightValue !is JSONArray) {
            throw EdgeHashException.InvalidProof("proof.path and proof.right must be arrays")
        }
        if (pathValue.length() != rightValue.length()) {
            throw EdgeHashException.InvalidProof("proof.path and proof.right lengths differ")
        }
        if (pathValue.length() > MAX_MERKLE_DEPTH) {
            throw EdgeHashException.InvalidProof("proof is too deep")
        }
        val hash = try {
            decodeHex(hashText)
        } catch (e: IllegalArgumentException) {
            throw EdgeHashException.InvalidProof("proof.hash is not valid hex")
        }
        if (hash.size != HASH_BYTES) {
            throw EdgeHashException.InvalidProof("proof.hash must be 32 bytes")
        }
        val index = indexText.toString().toLongOrNull()
            ?: throw EdgeHashException.InvalidProof("proof.index must be an integer")
        val path = ArrayList<ByteArray>(pathValue.length())
        for (i in 0 until pathValue.length()) {
            val item = pathValue.opt(i)
            if (item !is String) {
                throw EdgeHashException.InvalidProof("proof.path[$i] must be a hex string")
            }
            val sibling = try {
                decodeHex(item)
            } catch (e: IllegalArgumentException) {
                throw EdgeHashException.InvalidProof("proof.path[$i] is not valid hex")
            }
            if (sibling.size != HASH_BYTES) {
                throw EdgeHashException.InvalidProof("proof.path[$i] must be 32 bytes")
            }
            path.add(sibling)
        }
        val right = ArrayList<Boolean>(rightValue.length())
        for (i in 0 until rightValue.length()) {
            val item = rightValue.opt(i)
            if (item !is Boolean) {
                throw EdgeHashException.InvalidProof("proof.right[$i] must be boolean")
            }
            right.add(item)
        }
        return MerkleProof(hash, index, path, right)
    }

    private fun parsePrefix(value: String): Int {
        if (value.length != 4 || !value.all { Character.digit(it, 16) >= 0 }) {
            throw EdgeHashException.InvalidProof("server prefix must be 4 hex characters")
        }
        return value.toInt(16)
    }

    private fun firstValue(obj: JSONObject, vararg names: String): Any? {
        for (name in names) {
            if (obj.has(name) && !obj.isNull(name)) return obj.opt(name)
        }
        return null
    }

    private fun firstString(obj: JSONObject, vararg names: String): String? {
        val value = firstValue(obj, *names) ?: return null
        return value as? String
    }

    private fun firstNumber(obj: JSONObject, vararg names: String): Number? {
        val value = firstValue(obj, *names) ?: return null
        return value as? Number
    }

    private fun optionalString(obj: JSONObject, name: String): String? {
        if (!obj.has(name) || obj.isNull(name)) return null
        val value = obj.opt(name)
        return value as? String
    }

    private fun selectSegments(manifest: Manifest): List<SegmentMeta> {
        val filter = config.prefixFilter ?: return manifest.segments
        return manifest.segments.filter { it.prefix in filter }
    }

    private fun errorDetail(body: ByteArray?): String {
        if (body == null || body.isEmpty()) return ""
        return String(body, 0, minOf(body.size, 512), StandardCharsets.UTF_8)
    }
}

// ---------------------------------------------------------------------------
// Small collection helpers
// ---------------------------------------------------------------------------

internal fun List<SegmentMeta>.findByPrefix(prefix: Int): SegmentMeta? {
    var low = 0
    var high = size - 1
    while (low <= high) {
        val mid = (low + high) ushr 1
        val value = this[mid].prefix
        when {
            prefix < value -> high = mid - 1
            prefix > value -> low = mid + 1
            else -> return this[mid]
        }
    }
    return null
}
