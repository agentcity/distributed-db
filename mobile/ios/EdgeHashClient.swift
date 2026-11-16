//
//  EdgeHashClient.swift
//  EdgeHash mobile SDK - iOS/Swift scaffold.
//
//  STATUS: SCAFFOLD, NOT A SHIPPED SDK. This file has not been built/linked as
//  an iOS application and has not run on a device. It was typechecked against
//  the iOS 16 simulator SDK and a reference-vector harness was run on macOS;
//  see mobile/TESTING.md. That is not device validation.
//
//  Design constraints:
//   - Foundation + CryptoKit + URLSession only; no third-party dependencies.
//   - Pinned Ed25519 manifest verification. Legacy HMAC-SHA256 is available only
//     for development against the current Go server; never ship a shared key.
//   - The client is an actor, so manifest/segment cache state is isolated.
//     Network and file I/O still block that actor while running; production
//     should move large segment parsing off the actor executor.
//   - iOS 16+ / Swift 5.7+ (async URLSession, actors, CryptoKit Ed25519).
//
//  Protocol: mobile/protocol.md. Server: cmd/edgehash, internal/edgehttp.
//

import Foundation
import CryptoKit

// ---------------------------------------------------------------------------
// Protocol constants
// ---------------------------------------------------------------------------

private let manifestSchemaVersion: UInt64 = 1
private let segmentMagic = Data("EDGSEG01".utf8)
private let segmentHeaderSize = 40
private let hashByteCount = 32
private let maxBloomBits: UInt64 = 1 << 36
private let maxBloomK = 64
private let maxMerkleDepth = 62
private let maxManifestSegments = 1 << 16
private let maxMerkleLeaves = 1 << 24

private let leafDomain = Data("edgeset:merkle-leaf:v1\0".utf8)
private let nodeDomain = Data("edgeset:merkle-node:v1\0".utf8)
private let emptyLeafDomain = Data("edgeset:merkle-empty-leaf:v1".utf8)
private let bloomDomain = Data("edgeset:bloom:v1\0".utf8)

private let hexDigits = Array("0123456789abcdef".utf8)

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

public enum EdgeHashError: Error, LocalizedError, Sendable {
    case invalidHash(String)
    case invalidManifest(String)
    case unsupportedVersion(String)
    case untrustedManifest(String)
    case invalidSegment(String)
    case invalidProof(String)
    case staleManifest(fingerprint: String)
    case notSynced
    case offline
    case policyDenied(String)
    case quotaExceeded(String)
    case httpStatus(code: Int, path: String, detail: String)
    case transport(String)
    case invalidResponse(String)
    case syncInProgress
    case storage(String)
    case cryptoUnavailable(String)

    public var errorDescription: String? {
        switch self {
        case .invalidHash(let message):
            return "invalid hash: \(message)"
        case .invalidManifest(let message):
            return "invalid manifest: \(message)"
        case .unsupportedVersion(let message):
            return "unsupported protocol: \(message)"
        case .untrustedManifest(let message):
            return "manifest signature rejected: \(message)"
        case .invalidSegment(let message):
            return "invalid segment: \(message)"
        case .invalidProof(let message):
            return "invalid Merkle proof: \(message)"
        case .staleManifest(let fingerprint):
            return "cached manifest is stale or the server moved to another generation " +
                "(fingerprint \(fingerprint)); sync and retry"
        case .notSynced:
            return "no verified manifest is available; call sync() first"
        case .offline:
            return "offline mode cannot satisfy this lookup"
        case .policyDenied(let reason):
            return "cache policy denied: \(reason)"
        case .quotaExceeded(let message):
            return "cache quota exceeded: \(message)"
        case .httpStatus(let code, let path, let detail):
            return "HTTP \(code) for \(path)\(detail.isEmpty ? "" : ": \(detail)")"
        case .transport(let message):
            return "transport error: \(message)"
        case .invalidResponse(let message):
            return "invalid server response: \(message)"
        case .syncInProgress:
            return "another sync is already running"
        case .storage(let message):
            return "cache storage error: \(message)"
        case .cryptoUnavailable(let message):
            return "cryptography unavailable: \(message)"
        }
    }
}

// ---------------------------------------------------------------------------
// Byte/hex helpers
// ---------------------------------------------------------------------------

extension Data {
    /// Lowercase hexadecimal, the wire form for hashes and signatures.
    var edgeHashHex: String {
        var output = String()
        output.reserveCapacity(count * 2)
        for byte in self {
            output.append(Character(UnicodeScalar(hexDigits[Int(byte >> 4)])))
            output.append(Character(UnicodeScalar(hexDigits[Int(byte & 0x0f)])))
        }
        return output
    }

    /// Strict even-length hex decoder. Accepts upper and lower case.
    init?(edgeHashHex: String) {
        let utf8 = Array(edgeHashHex.utf8)
        guard utf8.count % 2 == 0 else { return nil }
        var bytes = [UInt8]()
        bytes.reserveCapacity(utf8.count / 2)
        var index = 0
        while index < utf8.count {
            guard let high = Data.hexNibble(utf8[index]),
                  let low = Data.hexNibble(utf8[index + 1]) else {
                return nil
            }
            bytes.append((high << 4) | low)
            index += 2
        }
        self = Data(bytes)
    }

    private static func hexNibble(_ value: UInt8) -> UInt8? {
        switch value {
        case 48...57: return value - 48          // 0-9
        case 65...70: return value - 65 + 10     // A-F
        case 97...102: return value - 97 + 10    // a-f
        default: return nil
        }
    }
}

private func sha256(_ data: Data) -> Data {
    Data(SHA256.hash(data: data))
}

private func sha256(_ parts: [Data]) -> Data {
    var hasher = SHA256()
    for part in parts {
        hasher.update(data: part)
    }
    return Data(hasher.finalize())
}

private func compareBytes(_ left: Data, _ right: Data) -> Int {
    let count = min(left.count, right.count)
    for index in 0..<count {
        if left[index] != right[index] {
            return Int(left[index]) - Int(right[index])
        }
    }
    return left.count - right.count
}

private func constantTimeEqual(_ left: Data, _ right: Data) -> Bool {
    guard left.count == right.count else { return false }
    var difference: UInt8 = 0
    for index in 0..<left.count {
        difference |= left[index] ^ right[index]
    }
    return difference == 0
}

private func routingPrefix(_ hash: Data, prefixBytes: Int) -> UInt16 {
    precondition(hash.count == hashByteCount)
    let first = UInt16(hash[hash.startIndex])
    if prefixBytes <= 1 {
        return first
    }
    let second = UInt16(hash[hash.startIndex + 1])
    return (first << 8) | second
}

private func readU16(_ bytes: [UInt8], _ offset: Int) -> UInt16 {
    (UInt16(bytes[offset]) << 8) | UInt16(bytes[offset + 1])
}

private func readU32(_ bytes: [UInt8], _ offset: Int) -> UInt32 {
    (UInt32(bytes[offset]) << 24) |
        (UInt32(bytes[offset + 1]) << 16) |
        (UInt32(bytes[offset + 2]) << 8) |
        UInt32(bytes[offset + 3])
}

private func readU64(_ bytes: [UInt8], _ offset: Int) -> UInt64 {
    var value: UInt64 = 0
    for index in 0..<8 {
        value = (value << 8) | UInt64(bytes[offset + index])
    }
    return value
}

private func segmentPath(prefix: UInt16) -> String {
    String(format: "segments/%04x.seg", prefix)
}

private func jsonString(_ value: String) -> String {
    var output = "\""
    for character in value {
        switch character {
        case "\"": output += "\\\""
        case "\\": output += "\\\\"
        case "\u{08}": output += "\\b"
        case "\u{0c}": output += "\\f"
        case "\n": output += "\\n"
        case "\r": output += "\\r"
        case "\t": output += "\\t"
        case "<": output += "\\u003c"
        case ">": output += "\\u003e"
        case "&": output += "\\u0026"
        case "\u{2028}": output += "\\u2028"
        case "\u{2029}": output += "\\u2029"
        default:
            if let ascii = character.asciiValue, ascii < 0x20 {
                output += String(format: "\\u%04x", Int(ascii))
            } else {
                output.append(character)
            }
        }
    }
    output += "\""
    return output
}

// ---------------------------------------------------------------------------
// Public model
// ---------------------------------------------------------------------------

public struct SegmentMeta: Sendable {
    public let prefix: UInt16
    public let count: Int
    public let bloomBits: UInt64
    public let bloomK: Int
    public let root: Data
    public let file: String
    public let size: Int64
    public let sha256: Data
    /// nil/"" for legacy v1; "none" or "zstd" for format v2.
    public let compression: String?

    public var rootHex: String { root.edgeHashHex }
    public var sha256Hex: String { sha256.edgeHashHex }

    public init(
        prefix: UInt16,
        count: Int,
        bloomBits: UInt64,
        bloomK: Int,
        root: Data,
        file: String,
        size: Int64,
        sha256: Data,
        compression: String? = nil
    ) {
        self.prefix = prefix
        self.count = count
        self.bloomBits = bloomBits
        self.bloomK = bloomK
        self.root = root
        self.file = file
        self.size = size
        self.sha256 = sha256
        self.compression = compression
    }
}

public struct Manifest: Sendable {
    public let version: UInt64
    public let prefixBytes: Int
    public let segments: [SegmentMeta]
    /// "ed25519", "hmac-sha256", or nil for the legacy Go server.
    public let signatureAlg: String?
    public let signature: Data?
    public let fingerprint: Data

    public init(
        version: UInt64,
        prefixBytes: Int,
        segments: [SegmentMeta],
        signatureAlg: String?,
        signature: Data?,
        fingerprint: Data
    ) {
        self.version = version
        self.prefixBytes = prefixBytes
        self.segments = segments
        self.signatureAlg = signatureAlg
        self.signature = signature
        self.fingerprint = fingerprint
    }

    public var fingerprintHex: String { fingerprint.edgeHashHex }

    public func segment(forPrefix prefix: UInt16) -> SegmentMeta? {
        var low = 0
        var high = segments.count - 1
        while low <= high {
            let middle = (low + high) / 2
            let value = segments[middle].prefix
            if prefix < value {
                high = middle - 1
            } else if prefix > value {
                low = middle + 1
            } else {
                return segments[middle]
            }
        }
        return nil
    }

    public func segment(forHash hash: Data) -> SegmentMeta? {
        guard hash.count == hashByteCount else { return nil }
        return segment(forPrefix: routingPrefix(hash, prefixBytes: prefixBytes))
    }
}

public struct MerkleProof: Sendable {
    public let hash: Data
    public let index: UInt64
    public let path: [Data]
    public let right: [Bool]

    public init(hash: Data, index: UInt64, path: [Data], right: [Bool]) {
        self.hash = hash
        self.index = index
        self.path = path
        self.right = right
    }
}

public enum LookupSource: String, Sendable, CaseIterable {
    /// No segment in the signed manifest has this routing prefix.
    case noSegment = "no_segment"
    /// Local segment's Bloom filter proved absence for this manifest generation.
    case localBloomNegative = "local_bloom_negative"
    /// Exact hit in a locally cached, checksum-verified segment.
    case localExact = "local_exact"
    /// Bloom said maybe, the exact sorted hash list said no.
    case localBloomFalsePositive = "local_bloom_false_positive"
    /// Remote inclusion proof verified against the signed Merkle root.
    case remoteProof = "remote_proof"
    /// Remote server answered present=false without a verified absence proof.
    case remoteAbsent = "remote_absent"
}

public struct LookupResult: Sendable {
    public let present: Bool
    public let prefix: UInt16
    public let prefixHex: String
    public let source: LookupSource
    /// True when derived from a signature-verified manifest/segment/proof.
    public let verified: Bool
    public let staleManifest: Bool
    public let manifestFingerprint: String
    public let proof: MerkleProof?
    public let rootHex: String?

    public init(
        present: Bool,
        prefix: UInt16,
        prefixHex: String,
        source: LookupSource,
        verified: Bool,
        staleManifest: Bool,
        manifestFingerprint: String,
        proof: MerkleProof? = nil,
        rootHex: String? = nil
    ) {
        self.present = present
        self.prefix = prefix
        self.prefixHex = prefixHex
        self.source = source
        self.verified = verified
        self.staleManifest = staleManifest
        self.manifestFingerprint = manifestFingerprint
        self.proof = proof
        self.rootHex = rootHex
    }
}

public struct LookupOptions: Sendable {
    /// Never touch the network. Throws .offline/.staleManifest instead of syncing.
    public let offline: Bool
    /// Use an expired but signature-valid manifest instead of failing.
    public let allowStaleManifest: Bool

    public init(offline: Bool = false, allowStaleManifest: Bool = false) {
        self.offline = offline
        self.allowStaleManifest = allowStaleManifest
    }
}

public struct SyncReport: Sendable {
    public let manifestVersion: UInt64
    public let manifestFingerprint: String
    public let notModified: Bool
    public let manifestBytes: Int
    public let segmentsSelected: Int
    public let segmentsDownloaded: Int
    public let segmentsAlreadyCached: Int
    public let segmentsSkipped: Int
    public let bytesDownloaded: Int64
    public let skippedReason: String?
    public let deferredReason: String?
    public let stoppedReason: String?
    public let cacheBytes: Int64

    public init(
        manifestVersion: UInt64,
        manifestFingerprint: String,
        notModified: Bool,
        manifestBytes: Int,
        segmentsSelected: Int,
        segmentsDownloaded: Int,
        segmentsAlreadyCached: Int,
        segmentsSkipped: Int,
        bytesDownloaded: Int64,
        skippedReason: String? = nil,
        deferredReason: String? = nil,
        stoppedReason: String? = nil,
        cacheBytes: Int64
    ) {
        self.manifestVersion = manifestVersion
        self.manifestFingerprint = manifestFingerprint
        self.notModified = notModified
        self.manifestBytes = manifestBytes
        self.segmentsSelected = segmentsSelected
        self.segmentsDownloaded = segmentsDownloaded
        self.segmentsAlreadyCached = segmentsAlreadyCached
        self.segmentsSkipped = segmentsSkipped
        self.bytesDownloaded = bytesDownloaded
        self.skippedReason = skippedReason
        self.deferredReason = deferredReason
        self.stoppedReason = stoppedReason
        self.cacheBytes = cacheBytes
    }
}

// ---------------------------------------------------------------------------
// Trust
// ---------------------------------------------------------------------------

public enum TrustConfig: Sendable {
    /// Production mode: a 32-byte Ed25519 public key pinned in the app.
    case ed25519(publicKey: Data)
    /// Development only: shared HMAC key for the current Go server.
    case hmacLegacy(key: Data)
}

// ---------------------------------------------------------------------------
// Manifest parsing / canonical signing input
// ---------------------------------------------------------------------------

internal struct ParsedManifest: Sendable {
    let manifest: Manifest
    let canonical: Data
    let raw: Data
    let fingerprint: Data

    var fingerprintHex: String { fingerprint.edgeHashHex }
}

private struct ManifestWire: Decodable {
    let version: UInt64
    let prefixBytes: Int
    let segments: [SegmentWire]
    let signature: String?
    let signatureAlg: String?

    enum CodingKeys: String, CodingKey {
        case version
        case prefixBytes = "prefix_bytes"
        case segments
        case signature
        case signatureAlg = "signature_alg"
    }
}

private struct SegmentWire: Decodable {
    let prefix: UInt16
    let count: Int
    let bloomBits: UInt64
    let bloomK: Int
    let root: String
    let file: String
    let size: Int64
    let sha256: String
    let compression: String?

    enum CodingKeys: String, CodingKey {
        case prefix
        case count
        case bloomBits = "bloom_bits"
        case bloomK = "bloom_k"
        case root
        case file
        case size
        case sha256
        case compression
    }
}

internal enum ManifestCodec {
    private static let topLevelKeys: Set<String> = [
        "version", "prefix_bytes", "segments", "signature", "signature_alg",
    ]
    private static let segmentKeys: Set<String> = [
        "prefix", "count", "bloom_bits", "bloom_k", "root", "file", "size", "sha256",
        "compression",
    ]

    static func parse(raw: Data, trust: TrustConfig) throws -> ParsedManifest {
        try validateKeys(raw)

        let wire: ManifestWire
        do {
            wire = try JSONDecoder().decode(ManifestWire.self, from: raw)
        } catch let error as EdgeHashError {
            throw error
        } catch {
            throw EdgeHashError.invalidManifest("malformed JSON: \(error.localizedDescription)")
        }

        guard wire.version == manifestSchemaVersion else {
            throw EdgeHashError.unsupportedVersion(
                "manifest version \(wire.version) (supported: \(manifestSchemaVersion))"
            )
        }
        guard (1...4).contains(wire.prefixBytes) else {
            throw EdgeHashError.invalidManifest(
                "prefix_bytes must be 1..4, got \(wire.prefixBytes)"
            )
        }
        guard wire.segments.count <= maxManifestSegments else {
            throw EdgeHashError.invalidManifest("too many segments: \(wire.segments.count)")
        }

        var segments = [SegmentMeta]()
        segments.reserveCapacity(wire.segments.count)
        var previousPrefix: UInt16?
        for (index, item) in wire.segments.enumerated() {
            let whereText = "segments[\(index)]"
            if let previous = previousPrefix, item.prefix <= previous {
                throw EdgeHashError.invalidManifest("segments must be strictly sorted by prefix")
            }
            previousPrefix = item.prefix
            if wire.prefixBytes == 1 && item.prefix > 0xff {
                throw EdgeHashError.invalidManifest("\(whereText).prefix does not fit one-byte prefixes")
            }
            guard item.count > 0, item.count <= Int.max / hashByteCount else {
                throw EdgeHashError.invalidManifest("\(whereText).count is out of range")
            }
            guard item.bloomBits > 0, item.bloomBits <= maxBloomBits else {
                throw EdgeHashError.invalidManifest(
                    "\(whereText).bloom_bits must be 1..\(maxBloomBits)"
                )
            }
            guard item.bloomK > 0, item.bloomK <= maxBloomK else {
                throw EdgeHashError.invalidManifest(
                    "\(whereText).bloom_k must be 1..\(maxBloomK)"
                )
            }
            guard item.size >= Int64(segmentHeaderSize) else {
                throw EdgeHashError.invalidManifest("\(whereText).size is too small: \(item.size)")
            }
            let compression = item.compression?.isEmpty == true ? nil : item.compression
            if let compression, compression != "none", compression != "zstd" {
                throw EdgeHashError.invalidManifest("\(whereText).compression must be none or zstd")
            }
            let wantedFile = segmentPath(prefix: item.prefix)
            guard item.file == wantedFile else {
                throw EdgeHashError.invalidManifest(
                    "\(whereText).file must be \"\(wantedFile)\", got \"\(item.file)\""
                )
            }
            guard let root = Data(edgeHashHex: item.root), root.count == hashByteCount else {
                throw EdgeHashError.invalidManifest("\(whereText).root must be 64 hex characters")
            }
            guard let checksum = Data(edgeHashHex: item.sha256), checksum.count == hashByteCount else {
                throw EdgeHashError.invalidManifest("\(whereText).sha256 must be 64 hex characters")
            }
            segments.append(
                SegmentMeta(
                    prefix: item.prefix,
                    count: item.count,
                    bloomBits: item.bloomBits,
                    bloomK: item.bloomK,
                    root: root,
                    file: item.file,
                    size: item.size,
                    sha256: checksum,
                    compression: compression
                )
            )
        }

        let signatureAlg = wire.signatureAlg?.isEmpty == true ? nil : wire.signatureAlg
        if let algorithm = signatureAlg,
           algorithm != "ed25519" && algorithm != "hmac-sha256" {
            throw EdgeHashError.unsupportedVersion("unknown signature_alg \"\(algorithm)\"")
        }
        guard let signatureText = wire.signature else {
            throw EdgeHashError.untrustedManifest("manifest is unsigned")
        }
        let signature = try decodeSignature(signatureText)

        let canonical = canonicalBytes(
            version: wire.version,
            prefixBytes: wire.prefixBytes,
            segments: segments,
            signatureAlg: signatureAlg
        )
        let fingerprint = sha256(canonical)
        let manifest = Manifest(
            version: wire.version,
            prefixBytes: wire.prefixBytes,
            segments: segments,
            signatureAlg: signatureAlg,
            signature: signature,
            fingerprint: fingerprint
        )
        try verifySignature(
            trust: trust,
            algorithm: signatureAlg,
            signature: signature,
            canonical: canonical
        )
        return ParsedManifest(
            manifest: manifest,
            canonical: canonical,
            raw: raw,
            fingerprint: fingerprint
        )
    }

    /// Compact JSON over version/prefix_bytes/segments, excluding signature.
    /// signature_alg is appended when non-empty; compression is appended at
    /// the end of each segment when non-empty. This matches
    /// internal/edgeset.canonicalManifest.
    static func canonicalBytes(
        version: UInt64,
        prefixBytes: Int,
        segments: [SegmentMeta],
        signatureAlg: String?
    ) -> Data {
        var output = "{\"version\":\(version)"
        output += ",\"prefix_bytes\":\(prefixBytes)"
        output += ",\"segments\":["
        for (index, segment) in segments.enumerated() {
            if index > 0 { output += "," }
            output += "{\"prefix\":\(segment.prefix)"
            output += ",\"count\":\(segment.count)"
            output += ",\"bloom_bits\":\(segment.bloomBits)"
            output += ",\"bloom_k\":\(segment.bloomK)"
            output += ",\"root\":\"\(segment.root.edgeHashHex)\""
            output += ",\"file\":\(jsonString(segment.file))"
            output += ",\"size\":\(segment.size)"
            output += ",\"sha256\":\"\(segment.sha256.edgeHashHex)\""
            if let compression = segment.compression, !compression.isEmpty {
                output += ",\"compression\":\(jsonString(compression))"
            }
            output += "}"
        }
        output += "]"
        if let signatureAlg, !signatureAlg.isEmpty {
            output += ",\"signature_alg\":\(jsonString(signatureAlg))"
        }
        output += "}"
        return Data(output.utf8)
    }

    private static func validateKeys(_ raw: Data) throws {
        let object: Any
        do {
            object = try JSONSerialization.jsonObject(with: raw, options: [])
        } catch {
            throw EdgeHashError.invalidManifest("malformed JSON")
        }
        guard let top = object as? [String: Any] else {
            throw EdgeHashError.invalidManifest("top-level JSON value must be an object")
        }
        for key in top.keys where !topLevelKeys.contains(key) {
            throw EdgeHashError.invalidManifest("unknown field \"\(key)\"")
        }
        guard let segments = top["segments"] as? [[String: Any]] else {
            throw EdgeHashError.invalidManifest("segments must be an array of objects")
        }
        for (index, segment) in segments.enumerated() {
            for key in segment.keys where !segmentKeys.contains(key) {
                throw EdgeHashError.invalidManifest("segments[\(index)]: unknown field \"\(key)\"")
            }
        }
    }

    private static func decodeSignature(_ value: String) throws -> Data {
        guard !value.isEmpty else {
            throw EdgeHashError.untrustedManifest("manifest signature is empty")
        }
        let isHex = value.count % 2 == 0 && value.unicodeScalars.allSatisfy {
            CharacterSet(charactersIn: "0123456789abcdefABCDEF").contains($0)
        }
        if isHex, let signature = Data(edgeHashHex: value) {
            return signature
        }
        if let signature = Data(base64Encoded: value) {
            return signature
        }
        throw EdgeHashError.untrustedManifest("signature is neither hex nor base64")
    }

    private static func verifySignature(
        trust: TrustConfig,
        algorithm: String?,
        signature: Data,
        canonical: Data
    ) throws {
        switch trust {
        case .ed25519(let publicKey):
            guard algorithm == "ed25519" else {
                throw EdgeHashError.untrustedManifest(
                    "pinned Ed25519 key requires signature_alg=ed25519, got \(algorithm ?? "absent")"
                )
            }
            guard publicKey.count == 32 else {
                throw EdgeHashError.cryptoUnavailable("pinned Ed25519 public key must be 32 bytes")
            }
            guard signature.count == 64 else {
                throw EdgeHashError.untrustedManifest(
                    "Ed25519 signature must be 64 bytes, got \(signature.count)"
                )
            }
            let key: Curve25519.Signing.PublicKey
            do {
                key = try Curve25519.Signing.PublicKey(rawRepresentation: publicKey)
            } catch {
                throw EdgeHashError.cryptoUnavailable("invalid pinned Ed25519 public key")
            }
            guard key.isValidSignature(signature, for: canonical) else {
                throw EdgeHashError.untrustedManifest("Ed25519 signature does not match the pinned key")
            }

        case .hmacLegacy(let key):
            guard algorithm == nil || algorithm == "hmac-sha256" else {
                throw EdgeHashError.untrustedManifest(
                    "legacy trust only accepts hmac-sha256, got \(algorithm ?? "absent")"
                )
            }
            guard key.count >= 16 else {
                throw EdgeHashError.cryptoUnavailable(
                    "HMAC key must be at least 16 bytes; use Ed25519 in production"
                )
            }
            guard signature.count == 32 else {
                throw EdgeHashError.untrustedManifest(
                    "HMAC-SHA256 tag must be 32 bytes, got \(signature.count)"
                )
            }
            let symmetricKey = SymmetricKey(data: key)
            guard HMAC<SHA256>.isValidAuthenticationCode(
                signature,
                authenticating: canonical,
                using: symmetricKey
            ) else {
                throw EdgeHashError.untrustedManifest("HMAC-SHA256 signature mismatch")
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Merkle tree
// ---------------------------------------------------------------------------

public enum EdgeHashMerkle {
    static let emptyLeaf = sha256(emptyLeafDomain)

    static func leafHash(_ hash: Data) -> Data {
        sha256([leafDomain, hash])
    }

    static func nodeHash(_ left: Data, _ right: Data) -> Data {
        sha256([nodeDomain, left, right])
    }

    public static func depth(forCount count: UInt64) -> Int? {
        guard count > 0 else { return nil }
        var size: UInt64 = 1
        var depth = 0
        while size < count {
            guard depth < maxMerkleDepth else { return nil }
            size <<= 1
            depth += 1
        }
        return depth
    }

    /// Verifies one inclusion proof. If expectedCount is supplied, the proof
    /// must have exactly ceil(log2(count)) levels and index < count.
    public static func verify(
        root: Data,
        hash: Data,
        proof: MerkleProof,
        expectedCount: UInt64? = nil
    ) -> Bool {
        guard root.count == hashByteCount, hash.count == hashByteCount else { return false }
        guard constantTimeEqual(proof.hash, hash) else { return false }
        guard proof.path.count == proof.right.count else { return false }
        let depth = proof.path.count
        guard depth <= maxMerkleDepth else { return false }

        if let expectedCount {
            guard expectedCount > 0, proof.index < expectedCount else { return false }
            guard let expectedDepth = EdgeHashMerkle.depth(forCount: expectedCount),
                  expectedDepth == depth else {
                return false
            }
        } else {
            guard proof.index < (UInt64(1) << UInt64(depth)) else { return false }
        }

        for level in 0..<depth {
            guard proof.path[level].count == hashByteCount else { return false }
            let leftChild = ((proof.index >> UInt64(level)) & 1) == 0
            guard proof.right[level] == leftChild else { return false }
        }

        var node = leafHash(hash)
        for level in 0..<depth {
            let sibling = proof.path[level]
            if proof.right[level] {
                node = nodeHash(node, sibling)
            } else {
                node = nodeHash(sibling, node)
            }
        }
        return constantTimeEqual(node, root)
    }

    /// Recomputes the power-of-two padded root over a sorted hash list.
    static func root(hashes: [Data]) throws -> Data {
        if hashes.isEmpty {
            return emptyLeaf
        }
        guard hashes.count <= maxMerkleLeaves else {
            throw EdgeHashError.invalidSegment("hash list is too large to recompute a Merkle root")
        }
        var size = 1
        while size < hashes.count {
            size <<= 1
        }
        var level = [UInt8](repeating: 0, count: size * hashByteCount)
        for (index, hash) in hashes.enumerated() {
            writeHash(leafHash(hash), into: &level, offset: index * hashByteCount)
        }
        for index in hashes.count..<size {
            writeHash(emptyLeaf, into: &level, offset: index * hashByteCount)
        }

        var count = size
        while count > 1 {
            let nextCount = count / 2
            var next = [UInt8](repeating: 0, count: nextCount * hashByteCount)
            for index in 0..<nextCount {
                let leftStart = index * 2 * hashByteCount
                let rightStart = leftStart + hashByteCount
                let left = Data(level[leftStart..<(leftStart + hashByteCount)])
                let right = Data(level[rightStart..<(rightStart + hashByteCount)])
                writeHash(nodeHash(left, right), into: &next, offset: index * hashByteCount)
            }
            level = next
            count = nextCount
        }
        return Data(level[0..<hashByteCount])
    }

    private static func writeHash(_ hash: Data, into buffer: inout [UInt8], offset: Int) {
        for index in 0..<hash.count {
            buffer[offset + index] = hash[hash.startIndex + index]
        }
    }
}

// ---------------------------------------------------------------------------
// Bloom filter
// ---------------------------------------------------------------------------

internal struct BloomFilterView: Sendable {
    let bits: Data
    let m: UInt64
    let k: Int

    init(bits: Data, m: UInt64, k: Int) throws {
        guard m > 0, m <= maxBloomBits, k > 0, k <= maxBloomK else {
            throw EdgeHashError.invalidSegment("invalid Bloom parameters")
        }
        guard (m + 7) / 8 <= UInt64(bits.count) else {
            throw EdgeHashError.invalidSegment("Bloom bitset is too short")
        }
        self.bits = bits
        self.m = m
        self.k = k
    }

    /// False positives are possible; false negatives are not for a correctly
    /// built, same-generation filter.
    func maybeContain(_ hash: Data) -> Bool {
        guard hash.count == hashByteCount else { return false }
        var input = Data()
        input.reserveCapacity(bloomDomain.count + hash.count)
        input.append(bloomDomain)
        input.append(hash)
        let digest = Array(SHA256.hash(data: input))
        let h1 = readU64(digest, 0)
        let step = readU64(digest, 8) | 1
        var x = h1
        for _ in 0..<k {
            let position = x % m
            let byteIndex = Int(position >> 3)
            let mask = UInt8(1) << Int(position & 7)
            if (bits[bits.startIndex + byteIndex] & mask) == 0 {
                return false
            }
            x = x &+ step
        }
        return true
    }
}

// ---------------------------------------------------------------------------
// Segment parsing
// ---------------------------------------------------------------------------

internal struct Segment: Sendable {
    let meta: SegmentMeta
    let bloom: BloomFilterView
    let hashes: [Data]
    let root: Data

    func maybeContain(_ hash: Data) -> Bool {
        bloom.maybeContain(hash)
    }

    func exactIndex(_ hash: Data) -> Int? {
        var low = 0
        var high = hashes.count - 1
        while low <= high {
            let middle = (low + high) / 2
            let comparison = compareBytes(hashes[middle], hash)
            if comparison < 0 {
                low = middle + 1
            } else if comparison > 0 {
                high = middle - 1
            } else {
                return middle
            }
        }
        return nil
    }
}

internal enum SegmentCodec {
    private static let headerV1 = 40
    private static let headerV2 = 48
    private static let formatV1: UInt32 = 1
    private static let formatV2: UInt32 = 2

    static func parse(_ data: Data, meta: SegmentMeta, prefixBytes: Int) throws -> Segment {
        guard Int64(data.count) == meta.size else {
            throw EdgeHashError.invalidSegment(
                "prefix \(String(format: "%04x", meta.prefix)) size \(data.count), manifest says \(meta.size)"
            )
        }
        guard constantTimeEqual(sha256(data), meta.sha256) else {
            throw EdgeHashError.invalidSegment(
                "SHA-256 mismatch for prefix \(String(format: "%04x", meta.prefix))"
            )
        }
        guard data.count >= headerV1 else {
            throw EdgeHashError.invalidSegment("file is shorter than the 40-byte header")
        }
        let bytes = Array(data)
        guard Data(bytes[0..<8]) == segmentMagic else {
            throw EdgeHashError.invalidSegment("bad magic")
        }
        let format = readU32(bytes, 8)
        let headerSize: Int
        switch format {
        case formatV1:
            headerSize = headerV1
        case formatV2:
            headerSize = headerV2
        default:
            throw EdgeHashError.unsupportedVersion(
                "segment format \(format) (supported: 1 uncompressed, 2 uncompressed)"
            )
        }
        guard data.count >= headerSize else {
            throw EdgeHashError.invalidSegment(
                "file is shorter than the \(headerSize)-byte header"
            )
        }
        let prefix = readU16(bytes, 12)
        guard prefix == meta.prefix else {
            throw EdgeHashError.invalidSegment(
                "header prefix \(String(format: "%04x", prefix)) != manifest " +
                    "\(String(format: "%04x", meta.prefix))"
            )
        }
        let count = readU32(bytes, 16)
        guard meta.count <= Int(UInt32.max) else {
            throw EdgeHashError.invalidSegment("manifest hash count does not fit a segment header")
        }
        guard count == UInt32(meta.count) else {
            throw EdgeHashError.invalidSegment("header count \(count) != manifest \(meta.count)")
        }
        let bloomBits = readU64(bytes, 20)
        let bloomK = readU32(bytes, 28)
        let bloomBytes = readU64(bytes, 32)
        guard bloomBits == meta.bloomBits, bloomK == UInt32(meta.bloomK) else {
            throw EdgeHashError.invalidSegment("Bloom parameters do not match the manifest")
        }
        guard bloomBits > 0, bloomBits <= maxBloomBits, bloomK >= 1, bloomK <= UInt32(maxBloomK) else {
            throw EdgeHashError.invalidSegment("invalid Bloom parameters")
        }
        let wantedBloomBytes = (bloomBits + 7) / 8
        guard bloomBytes == wantedBloomBytes else {
            throw EdgeHashError.invalidSegment(
                "Bloom byte length \(bloomBytes), want \(wantedBloomBytes)"
            )
        }
        let expectedRaw = bloomBytes + UInt64(count) * UInt64(hashByteCount)
        guard expectedRaw >= bloomBytes, expectedRaw <= UInt64(Int.max) else {
            throw EdgeHashError.invalidSegment("segment payload is too large")
        }

        let payloadStart: Int
        switch format {
        case formatV1:
            guard readU16(bytes, 14) == 0 else {
                throw EdgeHashError.invalidSegment("reserved header field must be zero")
            }
            if let compression = meta.compression, !compression.isEmpty, compression != "none" {
                throw EdgeHashError.invalidSegment(
                    "v1 segment cannot carry compression \(compression)"
                )
            }
            payloadStart = headerV1
            let stored = UInt64(data.count - payloadStart)
            guard stored == expectedRaw else {
                throw EdgeHashError.invalidSegment(
                    "payload size \(stored), header implies \(expectedRaw)"
                )
            }

        case formatV2:
            let code = bytes[14]
            let reserved = bytes[15]
            let rawSize = readU64(bytes, 40)
            let headerCompression: String
            switch code {
            case 0: headerCompression = "none"
            case 1: headerCompression = "zstd"
            default:
                throw EdgeHashError.invalidSegment("unsupported compression code \(code)")
            }
            if let compression = meta.compression, !compression.isEmpty {
                guard compression == headerCompression else {
                    throw EdgeHashError.invalidSegment(
                        "header compression \(headerCompression), manifest says \(compression)"
                    )
                }
            } else if code != 0 {
                throw EdgeHashError.invalidSegment(
                    "header is \(headerCompression) but the manifest omits compression"
                )
            }
            guard reserved == 0 else {
                throw EdgeHashError.invalidSegment("reserved header field must be zero")
            }
            guard rawSize == expectedRaw else {
                throw EdgeHashError.invalidSegment(
                    "raw payload size \(rawSize), header implies \(expectedRaw)"
                )
            }
            if code == 1 {
                // No zstd implementation is bundled. Fail closed before
                // allocating or decoding the payload.
                throw EdgeHashError.unsupportedVersion(
                    "segment \(String(format: "%04x", meta.prefix)) is zstd-compressed; " +
                        "rebuild the store with uncompressed v2 segments for this SDK"
                )
            }
            payloadStart = headerV2
            let stored = UInt64(data.count - payloadStart)
            guard stored == rawSize else {
                throw EdgeHashError.invalidSegment(
                    "stored payload size \(stored), header says \(rawSize)"
                )
            }

        default:
            throw EdgeHashError.unsupportedVersion("segment format \(format)")
        }

        let bloomStart = payloadStart
        let bloomEnd = bloomStart + Int(bloomBytes)
        let bloom = try BloomFilterView(
            bits: Data(bytes[bloomStart..<bloomEnd]),
            m: bloomBits,
            k: Int(bloomK)
        )

        let hashStart = bloomEnd
        var hashes = [Data]()
        hashes.reserveCapacity(Int(count))
        for index in 0..<Int(count) {
            let offset = hashStart + index * hashByteCount
            let hash = Data(bytes[offset..<(offset + hashByteCount)])
            if index > 0, compareBytes(hashes[index - 1], hash) >= 0 {
                throw EdgeHashError.invalidSegment(
                    "hashes are not strictly sorted at index \(index)"
                )
            }
            let routed = routingPrefix(hash, prefixBytes: prefixBytes)
            guard routed == meta.prefix else {
                throw EdgeHashError.invalidSegment(
                    "hash \(hash.edgeHashHex) routes to \(String(format: "%04x", routed)), " +
                        "not \(String(format: "%04x", meta.prefix))"
                )
            }
            hashes.append(hash)
        }

        let root = try EdgeHashMerkle.root(hashes: hashes)
        guard constantTimeEqual(root, meta.root) else {
            throw EdgeHashError.invalidSegment("Merkle root mismatch")
        }
        for hash in hashes {
            guard bloom.maybeContain(hash) else {
                throw EdgeHashError.invalidSegment(
                    "Bloom filter has a false negative for \(hash.edgeHashHex)"
                )
            }
        }
        return Segment(meta: meta, bloom: bloom, hashes: hashes, root: root)
    }
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

public struct HTTPResponse: Sendable {
    public let statusCode: Int
    public let body: Data?
    public let etag: String?

    public init(statusCode: Int, body: Data?, etag: String?) {
        self.statusCode = statusCode
        self.body = body
        self.etag = etag
    }
}

/// Advanced/testing seam. Production callers normally use URLSessionTransport.
public protocol HTTPTransport: Sendable {
    func get(
        path: String,
        accept: String,
        maxBytes: Int,
        ifNoneMatch: String?
    ) async throws -> HTTPResponse
}

public struct NetworkConfig: Sendable {
    public let timeout: TimeInterval
    public let maxManifestBytes: Int
    public let maxLookupBytes: Int
    public let maxSegmentBytes: Int
    public let userAgent: String

    public init(
        timeout: TimeInterval = 15,
        maxManifestBytes: Int = 32 << 20,
        maxLookupBytes: Int = 1 << 20,
        maxSegmentBytes: Int = 64 << 20,
        userAgent: String = "edgehash-mobile/0.1 (scaffold)"
    ) {
        self.timeout = timeout
        self.maxManifestBytes = maxManifestBytes
        self.maxLookupBytes = maxLookupBytes
        self.maxSegmentBytes = maxSegmentBytes
        self.userAgent = userAgent
    }
}

public final class URLSessionTransport: HTTPTransport, @unchecked Sendable {
    private let baseURLString: String
    private let network: NetworkConfig
    private let allowInsecureHTTP: Bool
    private let session: URLSession

    public init(baseURL: URL, network: NetworkConfig, allowInsecureHTTP: Bool) {
        var base = baseURL.absoluteString
        while base.hasSuffix("/") {
            base.removeLast()
        }
        self.baseURLString = base
        self.network = network
        self.allowInsecureHTTP = allowInsecureHTTP

        let configuration = URLSessionConfiguration.ephemeral
        configuration.timeoutIntervalForRequest = network.timeout
        configuration.timeoutIntervalForResource = network.timeout * 6
        configuration.httpShouldSetCookies = false
        configuration.httpCookieAcceptPolicy = .never
        configuration.urlCache = nil
        configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
        self.session = URLSession(configuration: configuration)
    }

    public func get(
        path: String,
        accept: String,
        maxBytes: Int,
        ifNoneMatch: String?
    ) async throws -> HTTPResponse {
        guard path.hasPrefix("/") else {
            throw EdgeHashError.invalidResponse("request path must start with /")
        }
        guard let url = URL(string: baseURLString + path) else {
            throw EdgeHashError.invalidResponse("cannot build URL for \(path)")
        }
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        request.timeoutInterval = network.timeout
        request.setValue(accept, forHTTPHeaderField: "Accept")
        request.setValue(network.userAgent, forHTTPHeaderField: "User-Agent")
        request.setValue("identity", forHTTPHeaderField: "Accept-Encoding")
        if let ifNoneMatch {
            request.setValue(ifNoneMatch, forHTTPHeaderField: "If-None-Match")
        }

        do {
            let (data, response) = try await session.data(for: request)
            guard let http = response as? HTTPURLResponse else {
                throw EdgeHashError.invalidResponse("\(path) returned a non-HTTP response")
            }
            if let finalURL = http.url,
               finalURL.scheme?.lowercased() != "https",
               !allowInsecureHTTP {
                throw EdgeHashError.invalidResponse("\(path) redirected to a non-HTTPS URL")
            }
            if http.expectedContentLength > 0,
               http.expectedContentLength > Int64(maxBytes) {
                throw EdgeHashError.invalidResponse(
                    "\(path) Content-Length \(http.expectedContentLength) exceeds \(maxBytes)"
                )
            }
            guard data.count <= maxBytes else {
                throw EdgeHashError.invalidResponse(
                    "\(path) response exceeds \(maxBytes) bytes"
                )
            }
            return HTTPResponse(
                statusCode: http.statusCode,
                body: http.statusCode == 304 ? nil : data,
                etag: http.value(forHTTPHeaderField: "ETag")
            )
        } catch let error as EdgeHashError {
            throw error
        } catch {
            throw EdgeHashError.transport("GET \(path) failed: \(error.localizedDescription)")
        }
    }
}

// ---------------------------------------------------------------------------
// Device policy hooks
// ---------------------------------------------------------------------------

public protocol DeviceConditions: Sendable {
    var isUnmeteredNetwork: Bool { get }
    var isCharging: Bool { get }
}

public struct AlwaysConnectedConditions: DeviceConditions, Sendable {
    public init() {}
    public var isUnmeteredNetwork: Bool { true }
    public var isCharging: Bool { true }
}

public struct StaticConditions: DeviceConditions, Sendable {
    public let isUnmeteredNetwork: Bool
    public let isCharging: Bool

    public init(isUnmeteredNetwork: Bool, isCharging: Bool) {
        self.isUnmeteredNetwork = isUnmeteredNetwork
        self.isCharging = isCharging
    }
}

public struct CachePolicy: Sendable {
    public let requireUnmeteredForSync: Bool
    public let requireChargingForBulk: Bool
    public let allowMeteredLookups: Bool
    public let maxCacheBytes: Int64
    public let manifestTTL: TimeInterval
    public let segmentTTL: TimeInterval
    public let allowStaleReads: Bool
    public let pinnedPrefixes: Set<UInt16>

    public init(
        requireUnmeteredForSync: Bool = true,
        requireChargingForBulk: Bool = true,
        allowMeteredLookups: Bool = true,
        maxCacheBytes: Int64 = 256 << 20,
        manifestTTL: TimeInterval = 24 * 60 * 60,
        segmentTTL: TimeInterval = 7 * 24 * 60 * 60,
        allowStaleReads: Bool = false,
        pinnedPrefixes: Set<UInt16> = []
    ) {
        self.requireUnmeteredForSync = requireUnmeteredForSync
        self.requireChargingForBulk = requireChargingForBulk
        self.allowMeteredLookups = allowMeteredLookups
        self.maxCacheBytes = maxCacheBytes
        self.manifestTTL = manifestTTL
        self.segmentTTL = segmentTTL
        self.allowStaleReads = allowStaleReads
        self.pinnedPrefixes = pinnedPrefixes
    }

    func manifestRefreshDenied(force: Bool, conditions: DeviceConditions) -> String? {
        if force { return nil }
        if requireUnmeteredForSync && !conditions.isUnmeteredNetwork {
            return "unmetered (Wi-Fi) network required for manifest sync"
        }
        return nil
    }

    func bulkDownloadDenied(force: Bool, conditions: DeviceConditions) -> String? {
        if force { return nil }
        if requireUnmeteredForSync && !conditions.isUnmeteredNetwork {
            return "unmetered (Wi-Fi) network required for segment download"
        }
        if requireChargingForBulk && !conditions.isCharging {
            return "charging required for bulk segment download"
        }
        return nil
    }

    func lookupDenied(conditions: DeviceConditions) -> String? {
        if !allowMeteredLookups && !conditions.isUnmeteredNetwork {
            return "metered network lookups are disabled"
        }
        return nil
    }
}

// ---------------------------------------------------------------------------
// Local cache
// ---------------------------------------------------------------------------

internal struct CachedManifest: Sendable {
    let parsed: ParsedManifest
    let fetchedAt: Date
    let etag: String?
    let sourceURL: String

    var manifest: Manifest { parsed.manifest }
    var fingerprintHex: String { parsed.fingerprintHex }

    func isExpired(policy: CachePolicy, now: Date) -> Bool {
        now.timeIntervalSince(fetchedAt) > policy.manifestTTL
    }
}

internal final class SegmentCache {
    private struct ManifestMeta: Codable {
        let fingerprint: String
        let fetchedAtMillis: Int64
        let sourceURL: String
        let etag: String?
    }

    private let fileManager = FileManager.default
    private let root: URL
    private let segmentsDirectory: URL
    private let manifestFile: URL
    private let manifestMetaFile: URL
    private let trust: TrustConfig
    private let maxManifestBytes: Int

    private(set) var lastLoadError: String?

    init(root: URL, trust: TrustConfig, maxManifestBytes: Int) throws {
        let rootURL = root.standardizedFileURL
        self.root = rootURL
        self.segmentsDirectory = rootURL.appendingPathComponent("segments", isDirectory: true)
        self.manifestFile = rootURL.appendingPathComponent("manifest.json", isDirectory: false)
        self.manifestMetaFile = rootURL.appendingPathComponent("manifest.meta.json", isDirectory: false)
        self.trust = trust
        self.maxManifestBytes = maxManifestBytes

        do {
            try fileManager.createDirectory(
                at: self.segmentsDirectory,
                withIntermediateDirectories: true
            )
        } catch {
            throw EdgeHashError.storage("cannot create cache directory: \(error.localizedDescription)")
        }
        var resourceURL = rootURL
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        try? resourceURL.setResourceValues(values)
    }

    func loadManifest() -> CachedManifest? {
        lastLoadError = nil
        guard fileManager.fileExists(atPath: manifestFile.path) else { return nil }
        do {
            let attributes = try fileManager.attributesOfItem(atPath: manifestFile.path)
            if let size = attributes[.size] as? NSNumber, size.intValue > maxManifestBytes {
                lastLoadError = "cached manifest exceeds \(maxManifestBytes) bytes"
                return nil
            }
            let raw = try Data(contentsOf: manifestFile)
            let parsed = try ManifestCodec.parse(raw: raw, trust: trust)
            let meta = readManifestMeta()
            let matches = meta?.fingerprint == parsed.fingerprintHex
            let modification = fileModificationDate(manifestFile) ?? Date(timeIntervalSince1970: 0)
            let fetchedAt: Date
            let etag: String?
            let sourceURL: String
            if matches, let meta {
                fetchedAt = Date(timeIntervalSince1970: Double(meta.fetchedAtMillis) / 1000)
                etag = meta.etag
                sourceURL = meta.sourceURL
            } else {
                fetchedAt = modification
                etag = nil
                sourceURL = ""
            }
            return CachedManifest(
                parsed: parsed,
                fetchedAt: fetchedAt,
                etag: etag,
                sourceURL: sourceURL
            )
        } catch {
            lastLoadError = (error as? EdgeHashError)?.localizedDescription ?? error.localizedDescription
            return nil
        }
    }

    func installManifest(
        _ parsed: ParsedManifest,
        etag: String?,
        sourceURL: String,
        fetchedAt: Date
    ) throws -> CachedManifest {
        let meta = ManifestMeta(
            fingerprint: parsed.fingerprintHex,
            fetchedAtMillis: Int64(fetchedAt.timeIntervalSince1970 * 1000),
            sourceURL: sourceURL,
            etag: etag
        )
        let metaData: Data
        do {
            metaData = try JSONEncoder().encode(meta)
        } catch {
            throw EdgeHashError.storage("cannot encode manifest metadata")
        }
        try writeAtomic(parsed.raw, to: manifestFile)
        try writeAtomic(metaData, to: manifestMetaFile)
        return CachedManifest(
            parsed: parsed,
            fetchedAt: fetchedAt,
            etag: etag,
            sourceURL: sourceURL
        )
    }

    func touchManifest(_ cached: CachedManifest, fetchedAt: Date) throws -> CachedManifest {
        let meta = ManifestMeta(
            fingerprint: cached.fingerprintHex,
            fetchedAtMillis: Int64(fetchedAt.timeIntervalSince1970 * 1000),
            sourceURL: cached.sourceURL,
            etag: cached.etag
        )
        let data: Data
        do {
            data = try JSONEncoder().encode(meta)
        } catch {
            throw EdgeHashError.storage("cannot encode manifest metadata")
        }
        try writeAtomic(data, to: manifestMetaFile)
        return CachedManifest(
            parsed: cached.parsed,
            fetchedAt: fetchedAt,
            etag: cached.etag,
            sourceURL: cached.sourceURL
        )
    }

    func readSegment(meta: SegmentMeta, prefixBytes: Int) throws -> Segment? {
        let url = segmentURL(prefix: meta.prefix)
        guard fileManager.fileExists(atPath: url.path) else { return nil }
        guard let attributes = try? fileManager.attributesOfItem(atPath: url.path),
              let size = attributes[.size] as? NSNumber,
              size.int64Value == meta.size else {
            try? fileManager.removeItem(at: url)
            return nil
        }
        let data: Data
        do {
            data = try Data(contentsOf: url)
        } catch {
            throw EdgeHashError.storage("cannot read cached segment: \(error.localizedDescription)")
        }
        do {
            return try SegmentCodec.parse(data, meta: meta, prefixBytes: prefixBytes)
        } catch EdgeHashError.invalidSegment(_) {
            try? fileManager.removeItem(at: url)
            return nil
        } catch EdgeHashError.unsupportedVersion(_) {
            // A zstd/unknown-format file must not poison local lookups;
            // resolve through a remote proof instead.
            try? fileManager.removeItem(at: url)
            return nil
        }
    }

    func isSegmentValid(_ meta: SegmentMeta) -> Bool {
        let url = segmentURL(prefix: meta.prefix)
        guard fileManager.fileExists(atPath: url.path) else { return false }
        guard let attributes = try? fileManager.attributesOfItem(atPath: url.path),
              let size = attributes[.size] as? NSNumber,
              size.int64Value == meta.size else {
            return false
        }
        guard let data = try? Data(contentsOf: url) else { return false }
        return constantTimeEqual(sha256(data), meta.sha256)
    }

    func installSegment(meta: SegmentMeta, data: Data) throws {
        guard Int64(data.count) == meta.size else {
            throw EdgeHashError.invalidSegment("segment size changed before install")
        }
        guard constantTimeEqual(sha256(data), meta.sha256) else {
            throw EdgeHashError.invalidSegment("segment checksum changed before install")
        }
        try writeAtomic(data, to: segmentURL(prefix: meta.prefix))
    }

    func deleteSegment(prefix: UInt16) {
        try? fileManager.removeItem(at: segmentURL(prefix: prefix))
    }

    func totalBytes() -> Int64 {
        guard let urls = try? fileManager.contentsOfDirectory(
            at: segmentsDirectory,
            includingPropertiesForKeys: [.fileSizeKey],
            options: [.skipsHiddenFiles]
        ) else {
            return 0
        }
        return urls.reduce(Int64(0)) { partial, url in
            let size = (try? url.resourceValues(forKeys: [.fileSizeKey]).fileSize) ?? 0
            return partial + Int64(size)
        }
    }

    /// Deletes expired segments, then LRU segments until total <= maxBytes.
    /// Pinned prefixes are never evicted. Simple skeleton: no persistent index,
    /// no access-time update on reads.
    @discardableResult
    func enforceQuota(
        maxBytes: Int64,
        segmentTTL: TimeInterval,
        now: Date,
        pinnedPrefixes: Set<UInt16>
    ) throws -> Int64 {
        let urls = try fileManager.contentsOfDirectory(
            at: segmentsDirectory,
            includingPropertiesForKeys: [.fileSizeKey, .contentModificationDateKey],
            options: [.skipsHiddenFiles]
        )
        var entries = [(url: URL, size: Int64, date: Date, prefix: UInt16?)]()
        var total: Int64 = 0
        for url in urls where url.pathExtension == "seg" {
            let values = try? url.resourceValues(
                forKeys: [.fileSizeKey, .contentModificationDateKey]
            )
            let size = Int64(values?.fileSize ?? 0)
            let date = values?.contentModificationDate ?? Date(timeIntervalSince1970: 0)
            let prefix = UInt16(url.deletingPathExtension().lastPathComponent, radix: 16)
            entries.append((url, size, date, prefix))
            total += size
        }

        for entry in entries {
            guard let prefix = entry.prefix, !pinnedPrefixes.contains(prefix) else { continue }
            guard now.timeIntervalSince(entry.date) > segmentTTL else { continue }
            try? fileManager.removeItem(at: entry.url)
            total -= entry.size
        }

        if total > maxBytes {
            let candidates = entries
                .filter { entry in
                    guard let prefix = entry.prefix else { return false }
                    return !pinnedPrefixes.contains(prefix)
                }
                .sorted { $0.date < $1.date }
            for entry in candidates {
                if total <= maxBytes { break }
                try? fileManager.removeItem(at: entry.url)
                total -= entry.size
            }
        }
        return total
    }

    func pruneOrphans(validPrefixes: Set<UInt16>, pinnedPrefixes: Set<UInt16>) {
        guard let urls = try? fileManager.contentsOfDirectory(
            at: segmentsDirectory,
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        ) else {
            return
        }
        for url in urls where url.pathExtension == "seg" {
            guard let prefix = UInt16(url.deletingPathExtension().lastPathComponent, radix: 16) else {
                continue
            }
            if !validPrefixes.contains(prefix) && !pinnedPrefixes.contains(prefix) {
                try? fileManager.removeItem(at: url)
            }
        }
    }

    func clear() {
        try? fileManager.removeItem(at: manifestFile)
        try? fileManager.removeItem(at: manifestMetaFile)
        if let urls = try? fileManager.contentsOfDirectory(
            at: segmentsDirectory,
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        ) {
            for url in urls {
                try? fileManager.removeItem(at: url)
            }
        }
    }

    private func segmentURL(prefix: UInt16) -> URL {
        segmentsDirectory.appendingPathComponent(String(format: "%04x.seg", prefix))
    }

    private func readManifestMeta() -> ManifestMeta? {
        guard let data = try? Data(contentsOf: manifestMetaFile) else { return nil }
        return try? JSONDecoder().decode(ManifestMeta.self, from: data)
    }

    private func fileModificationDate(_ url: URL) -> Date? {
        guard let attributes = try? fileManager.attributesOfItem(atPath: url.path),
              let date = attributes[.modificationDate] as? Date else {
            return nil
        }
        return date
    }

    private func writeAtomic(_ data: Data, to url: URL) throws {
        do {
            try data.write(to: url, options: [.atomic])
        } catch {
            throw EdgeHashError.storage("cannot write \(url.lastPathComponent): \(error.localizedDescription)")
        }
    }
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

public struct EdgeHashConfig: Sendable {
    public let baseURL: URL
    public let trust: TrustConfig
    public let cacheDirectory: URL
    public let network: NetworkConfig
    public let policy: CachePolicy
    /// Cache only selected segment prefixes; nil means every segment.
    public let prefixFilter: Set<UInt16>?
    /// Development only. Production must use https.
    public let allowInsecureHTTP: Bool

    public init(
        baseURL: URL,
        trust: TrustConfig,
        cacheDirectory: URL,
        network: NetworkConfig = NetworkConfig(),
        policy: CachePolicy = CachePolicy(),
        prefixFilter: Set<UInt16>? = nil,
        allowInsecureHTTP: Bool = false
    ) throws {
        let scheme = baseURL.scheme?.lowercased()
        guard scheme == "https" || (allowInsecureHTTP && scheme == "http") else {
            throw EdgeHashError.invalidResponse(
                "base URL must use https (allowInsecureHTTP=true is development-only)"
            )
        }
        guard baseURL.host != nil else {
            throw EdgeHashError.invalidResponse("base URL must have a host")
        }
        guard baseURL.user == nil, baseURL.password == nil else {
            throw EdgeHashError.invalidResponse("base URL must not contain user info")
        }
        guard baseURL.query == nil, baseURL.fragment == nil else {
            throw EdgeHashError.invalidResponse("base URL must not contain query or fragment")
        }
        guard !cacheDirectory.path.isEmpty else {
            throw EdgeHashError.storage("cache directory must not be empty")
        }
        guard network.timeout > 0,
              network.maxManifestBytes > 0,
              network.maxLookupBytes > 0,
              network.maxSegmentBytes > 0 else {
            throw EdgeHashError.invalidResponse("network limits and timeout must be positive")
        }
        guard policy.maxCacheBytes >= 0,
              policy.manifestTTL > 0,
              policy.segmentTTL > 0 else {
            throw EdgeHashError.storage("cache quota/TTL values are invalid")
        }

        self.baseURL = baseURL
        self.trust = trust
        self.cacheDirectory = cacheDirectory
        self.network = network
        self.policy = policy
        self.prefixFilter = prefixFilter
        self.allowInsecureHTTP = allowInsecureHTTP
    }
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

/// Blocking-I/O-under-an-actor client for an immutable edgehash set.
///
/// ```
/// let client = try EdgeHashClient(config: config)
/// _ = try await client.sync()
/// let result = try await client.lookup(hashHex: hash)
/// ```
///
/// `sync()` is serialized; concurrent calls fail with `.syncInProgress` rather
/// than downloading in parallel. Large segment parsing currently runs on the
/// actor executor and should be moved to a dedicated queue in production.
public actor EdgeHashClient {
    private let config: EdgeHashConfig
    private let conditions: DeviceConditions
    private let transport: HTTPTransport
    private let cache: SegmentCache
    private let clock: @Sendable () -> Date

    private var cachedManifest: CachedManifest?
    private var segmentMemo: [String: Segment] = [:]
    private var syncing = false

    public private(set) var lastCacheLoadError: String?

    public init(
        config: EdgeHashConfig,
        conditions: DeviceConditions = AlwaysConnectedConditions(),
        transport: HTTPTransport? = nil,
        clock: @escaping @Sendable () -> Date = { Date() }
    ) throws {
        self.config = config
        self.conditions = conditions
        self.transport = transport ?? URLSessionTransport(
            baseURL: config.baseURL,
            network: config.network,
            allowInsecureHTTP: config.allowInsecureHTTP
        )
        self.clock = clock
        let cache = try SegmentCache(
            root: config.cacheDirectory,
            trust: config.trust,
            maxManifestBytes: config.network.maxManifestBytes
        )
        self.cache = cache
        self.cachedManifest = cache.loadManifest()
        self.lastCacheLoadError = cache.lastLoadError
    }

    public var manifest: Manifest? {
        cachedManifest?.manifest
    }

    public var manifestFingerprint: String? {
        cachedManifest?.fingerprintHex
    }

    public func cacheBytes() -> Int64 {
        cache.totalBytes()
    }

    /// Refreshes the manifest and downloads missing segments.
    public func sync(force: Bool = false) async throws -> SyncReport {
        if syncing {
            throw EdgeHashError.syncInProgress
        }
        syncing = true
        defer { syncing = false }

        let now = clock()
        let policy = config.policy
        let previous = cachedManifest

        if let reason = policy.manifestRefreshDenied(force: force, conditions: conditions) {
            if let previous, !previous.isExpired(policy: policy, now: now) {
                return SyncReport(
                    manifestVersion: previous.manifest.version,
                    manifestFingerprint: previous.fingerprintHex,
                    notModified: false,
                    manifestBytes: 0,
                    segmentsSelected: 0,
                    segmentsDownloaded: 0,
                    segmentsAlreadyCached: 0,
                    segmentsSkipped: 0,
                    bytesDownloaded: 0,
                    skippedReason: reason,
                    cacheBytes: cache.totalBytes()
                )
            }
            throw EdgeHashError.policyDenied(reason)
        }

        let response = try await transport.get(
            path: "/v1/manifest",
            accept: "application/json",
            maxBytes: config.network.maxManifestBytes,
            ifNoneMatch: force ? nil : previous?.etag
        )

        var manifestBytes = 0
        var notModified = false
        let current: CachedManifest
        switch response.statusCode {
        case 304:
            guard let previous else {
                throw EdgeHashError.httpStatus(
                    code: 304,
                    path: "/v1/manifest",
                    detail: "304 without a cached manifest"
                )
            }
            current = try cache.touchManifest(previous, fetchedAt: now)
            notModified = true
        case 200:
            guard let body = response.body else {
                throw EdgeHashError.invalidResponse("manifest response has no body")
            }
            manifestBytes = body.count
            let parsed = try ManifestCodec.parse(raw: body, trust: config.trust)
            current = try cache.installManifest(
                parsed,
                etag: response.etag,
                sourceURL: config.baseURL.absoluteString,
                fetchedAt: now
            )
            let validPrefixes = Set(parsed.manifest.segments.map { $0.prefix })
            cache.pruneOrphans(
                validPrefixes: validPrefixes,
                pinnedPrefixes: policy.pinnedPrefixes
            )
            segmentMemo.removeAll(keepingCapacity: true)
        default:
            throw EdgeHashError.httpStatus(
                code: response.statusCode,
                path: "/v1/manifest",
                detail: errorDetail(response.body)
            )
        }
        cachedManifest = current

        let selected = selectSegments(current.manifest)
        let bulkDenied = selected.isEmpty
            ? nil
            : policy.bulkDownloadDenied(force: force, conditions: conditions)
        if let bulkDenied {
            return SyncReport(
                manifestVersion: current.manifest.version,
                manifestFingerprint: current.fingerprintHex,
                notModified: notModified,
                manifestBytes: manifestBytes,
                segmentsSelected: selected.count,
                segmentsDownloaded: 0,
                segmentsAlreadyCached: 0,
                segmentsSkipped: 0,
                bytesDownloaded: Int64(manifestBytes),
                deferredReason: bulkDenied,
                cacheBytes: cache.totalBytes()
            )
        }

        var downloaded = 0
        var alreadyCached = 0
        var skipped = 0
        var segmentsBytes: Int64 = 0
        var stoppedReason: String?
        let prefixBytes = current.manifest.prefixBytes

        for meta in selected {
            if cache.isSegmentValid(meta) {
                alreadyCached += 1
                continue
            }
            if meta.compression == "zstd" {
                skipped += 1
                stoppedReason = "segment \(String(format: "%04x", meta.prefix)) is zstd-compressed; " +
                    "this SDK can only cache uncompressed v2 segments (lookups still use remote proofs)"
                continue
            }
            if meta.size > Int64(config.network.maxSegmentBytes) {
                skipped += 1
                stoppedReason = "segment \(String(format: "%04x", meta.prefix)) is larger than maxSegmentBytes"
                continue
            }
            if meta.size > policy.maxCacheBytes {
                stoppedReason = "segment \(String(format: "%04x", meta.prefix)) alone exceeds maxCacheBytes"
                break
            }
            let retained = try cache.enforceQuota(
                maxBytes: policy.maxCacheBytes,
                segmentTTL: policy.segmentTTL,
                now: now,
                pinnedPrefixes: policy.pinnedPrefixes
            )
            if retained + meta.size > policy.maxCacheBytes {
                stoppedReason = "cache quota \(policy.maxCacheBytes) cannot fit segment " +
                    String(format: "%04x", meta.prefix)
                break
            }

            let path = String(format: "/v1/segment/%04x", meta.prefix)
            let segmentResponse = try await transport.get(
                path: path,
                accept: "application/octet-stream",
                maxBytes: Int(meta.size),
                ifNoneMatch: nil
            )
            guard segmentResponse.statusCode == 200 else {
                throw EdgeHashError.httpStatus(
                    code: segmentResponse.statusCode,
                    path: path,
                    detail: errorDetail(segmentResponse.body)
                )
            }
            guard let data = segmentResponse.body else {
                throw EdgeHashError.invalidResponse("segment response has no body")
            }
            _ = try SegmentCodec.parse(data, meta: meta, prefixBytes: prefixBytes)
            try cache.installSegment(meta: meta, data: data)
            downloaded += 1
            segmentsBytes += Int64(data.count)
        }

        let finalRetained = try cache.enforceQuota(
            maxBytes: policy.maxCacheBytes,
            segmentTTL: policy.segmentTTL,
            now: clock(),
            pinnedPrefixes: policy.pinnedPrefixes
        )
        return SyncReport(
            manifestVersion: current.manifest.version,
            manifestFingerprint: current.fingerprintHex,
            notModified: notModified,
            manifestBytes: manifestBytes,
            segmentsSelected: selected.count,
            segmentsDownloaded: downloaded,
            segmentsAlreadyCached: alreadyCached,
            segmentsSkipped: skipped,
            bytesDownloaded: Int64(manifestBytes) + segmentsBytes,
            stoppedReason: stoppedReason,
            cacheBytes: finalRetained
        )
    }

    public func lookup(hash: Data, options: LookupOptions = LookupOptions()) async throws -> LookupResult {
        guard hash.count == hashByteCount else {
            throw EdgeHashError.invalidHash("hash must be exactly 32 bytes, got \(hash.count)")
        }
        let query = hash
        let current = try await ensureManifest(options: options)
        let stale = current.isExpired(policy: config.policy, now: clock())
        let prefix = routingPrefix(query, prefixBytes: current.manifest.prefixBytes)
        let prefixHex = String(format: "%04x", prefix)

        guard let meta = current.manifest.segment(forPrefix: prefix) else {
            return LookupResult(
                present: false,
                prefix: prefix,
                prefixHex: prefixHex,
                source: .noSegment,
                verified: true,
                staleManifest: stale,
                manifestFingerprint: current.fingerprintHex
            )
        }

        if let segment = try loadSegment(
            meta,
            manifestFingerprint: current.fingerprintHex,
            prefixBytes: current.manifest.prefixBytes
        ) {
            if !segment.maybeContain(query) {
                return LookupResult(
                    present: false,
                    prefix: prefix,
                    prefixHex: prefixHex,
                    source: .localBloomNegative,
                    verified: true,
                    staleManifest: stale,
                    manifestFingerprint: current.fingerprintHex
                )
            }
            if segment.exactIndex(query) != nil {
                return LookupResult(
                    present: true,
                    prefix: prefix,
                    prefixHex: prefixHex,
                    source: .localExact,
                    verified: true,
                    staleManifest: stale,
                    manifestFingerprint: current.fingerprintHex,
                    rootHex: segment.root.edgeHashHex
                )
            }
            return LookupResult(
                present: false,
                prefix: prefix,
                prefixHex: prefixHex,
                source: .localBloomFalsePositive,
                verified: true,
                staleManifest: stale,
                manifestFingerprint: current.fingerprintHex
            )
        }

        if options.offline {
            throw EdgeHashError.offline
        }
        if let reason = config.policy.lookupDenied(conditions: conditions) {
            throw EdgeHashError.policyDenied(reason)
        }
        return try await remoteLookup(
            current: current,
            meta: meta,
            query: query,
            prefix: prefix,
            prefixHex: prefixHex,
            stale: stale
        )
    }

    public func lookup(hashHex: String, options: LookupOptions = LookupOptions()) async throws -> LookupResult {
        guard let hash = Data(edgeHashHex: hashHex), hash.count == hashByteCount else {
            throw EdgeHashError.invalidHash("hash must be 64 hex characters")
        }
        return try await lookup(hash: hash, options: options)
    }

    /// Applies TTL + LRU quota eviction.
    @discardableResult
    public func trimCache(now: Date? = nil) throws -> Int64 {
        try cache.enforceQuota(
            maxBytes: config.policy.maxCacheBytes,
            segmentTTL: config.policy.segmentTTL,
            now: now ?? clock(),
            pinnedPrefixes: config.policy.pinnedPrefixes
        )
    }

    /// Deletes the local manifest and all cached segments.
    public func clearCache() {
        cache.clear()
        cachedManifest = nil
        segmentMemo.removeAll()
    }

    private func ensureManifest(options: LookupOptions) async throws -> CachedManifest {
        let policy = config.policy
        let allowStale = options.allowStaleManifest || policy.allowStaleReads
        guard var current = cachedManifest else {
            if options.offline {
                throw EdgeHashError.notSynced
            }
            _ = try await sync(force: false)
            guard let synced = cachedManifest else {
                throw EdgeHashError.notSynced
            }
            return synced
        }

        if !current.isExpired(policy: policy, now: clock()) {
            return current
        }
        if options.offline {
            if allowStale { return current }
            throw EdgeHashError.staleManifest(fingerprint: current.fingerprintHex)
        }

        do {
            _ = try await sync(force: false)
            current = cachedManifest ?? current
            if !current.isExpired(policy: policy, now: clock()) || allowStale {
                return current
            }
            throw EdgeHashError.staleManifest(fingerprint: current.fingerprintHex)
        } catch {
            if allowStale { return current }
            if let edge = error as? EdgeHashError,
               case .staleManifest(_) = edge {
                throw edge
            }
            throw EdgeHashError.staleManifest(fingerprint: current.fingerprintHex)
        }
    }

    private func loadSegment(
        _ meta: SegmentMeta,
        manifestFingerprint: String,
        prefixBytes: Int
    ) throws -> Segment? {
        let key = "\(manifestFingerprint):\(meta.prefix):\(meta.sha256Hex)"
        if let memoized = segmentMemo[key] {
            return memoized
        }
        guard let segment = try cache.readSegment(meta: meta, prefixBytes: prefixBytes) else {
            return nil
        }
        if segmentMemo.count >= 4 {
            segmentMemo.removeAll(keepingCapacity: true)
        }
        segmentMemo[key] = segment
        return segment
    }

    private func remoteLookup(
        current: CachedManifest,
        meta: SegmentMeta,
        query: Data,
        prefix: UInt16,
        prefixHex: String,
        stale: Bool
    ) async throws -> LookupResult {
        let path = "/v1/lookup/\(query.edgeHashHex)"
        let response = try await transport.get(
            path: path,
            accept: "application/json",
            maxBytes: config.network.maxLookupBytes,
            ifNoneMatch: nil
        )
        if response.statusCode == 404 {
            // Compatibility with older servers; absence is unverified here.
            return LookupResult(
                present: false,
                prefix: prefix,
                prefixHex: prefixHex,
                source: .remoteAbsent,
                verified: false,
                staleManifest: stale,
                manifestFingerprint: current.fingerprintHex
            )
        }
        guard response.statusCode == 200 else {
            throw EdgeHashError.httpStatus(
                code: response.statusCode,
                path: path,
                detail: errorDetail(response.body)
            )
        }
        guard let body = response.body else {
            throw EdgeHashError.invalidResponse("lookup response has no body")
        }

        let wire: LookupWire
        do {
            wire = try JSONDecoder().decode(LookupWire.self, from: body)
        } catch let error as EdgeHashError {
            throw error
        } catch {
            throw EdgeHashError.invalidResponse(
                "lookup response is not valid JSON: \(error.localizedDescription)"
            )
        }

        if let remotePrefix = wire.prefix {
            guard remotePrefix.count == 4,
                  let parsed = UInt16(remotePrefix, radix: 16),
                  parsed == prefix else {
                throw EdgeHashError.invalidProof(
                    "server prefix \(remotePrefix) != expected \(prefixHex)"
                )
            }
        }
        guard wire.present else {
            return LookupResult(
                present: false,
                prefix: prefix,
                prefixHex: prefixHex,
                source: .remoteAbsent,
                verified: false,
                staleManifest: stale,
                manifestFingerprint: current.fingerprintHex
            )
        }
        guard let proof = wire.proof?.toProof() else {
            throw EdgeHashError.invalidProof("present=true without a proof object")
        }
        guard proof.hash == query else {
            throw EdgeHashError.invalidProof("proof is for a different hash")
        }
        guard proof.index < UInt64(meta.count) else {
            throw EdgeHashError.invalidProof(
                "proof index \(proof.index) is outside segment count \(meta.count)"
            )
        }
        guard let depth = EdgeHashMerkle.depth(forCount: UInt64(meta.count)),
              proof.path.count == depth else {
            throw EdgeHashError.invalidProof(
                "proof depth does not match segment count \(meta.count)"
            )
        }
        guard EdgeHashMerkle.verify(
            root: meta.root,
            hash: query,
            proof: proof,
            expectedCount: UInt64(meta.count)
        ) else {
            throw EdgeHashError.invalidProof("proof does not verify against the signed root")
        }

        if let rootHex = wire.root, !rootHex.isEmpty {
            guard let remoteRoot = Data(edgeHashHex: rootHex), remoteRoot.count == hashByteCount else {
                throw EdgeHashError.invalidProof("server root must be 32 bytes of hex")
            }
            guard constantTimeEqual(remoteRoot, meta.root) else {
                throw EdgeHashError.staleManifest(fingerprint: current.fingerprintHex)
            }
        }

        return LookupResult(
            present: true,
            prefix: prefix,
            prefixHex: prefixHex,
            source: .remoteProof,
            verified: true,
            staleManifest: stale,
            manifestFingerprint: current.fingerprintHex,
            proof: proof,
            rootHex: meta.root.edgeHashHex
        )
    }

    private func selectSegments(_ manifest: Manifest) -> [SegmentMeta] {
        guard let filter = config.prefixFilter else {
            return manifest.segments
        }
        return manifest.segments.filter { filter.contains($0.prefix) }
    }

    private func errorDetail(_ body: Data?) -> String {
        guard let body, !body.isEmpty else { return "" }
        return String(decoding: body.prefix(512), as: UTF8.self)
    }
}

// ---------------------------------------------------------------------------
// Lookup JSON (accept lower-case target keys and the current Go encoder's
// capitalized struct-field names)
// ---------------------------------------------------------------------------

private struct LookupWire: Decodable {
    let present: Bool
    let proof: ProofWire?
    let root: String?
    let prefix: String?
}

private struct ProofWire: Decodable {
    let hash: Data
    let index: UInt64
    let path: [Data]
    let right: [Bool]

    enum CodingKeys: String, CodingKey {
        case hashLower = "hash"
        case hashUpper = "Hash"
        case indexLower = "index"
        case indexUpper = "Index"
        case pathLower = "path"
        case pathUpper = "Path"
        case rightLower = "right"
        case rightUpper = "Right"
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let hashLower = try container.decodeIfPresent(String.self, forKey: .hashLower)
        let hashUpper = try container.decodeIfPresent(String.self, forKey: .hashUpper)
        let indexLower = try container.decodeIfPresent(UInt64.self, forKey: .indexLower)
        let indexUpper = try container.decodeIfPresent(UInt64.self, forKey: .indexUpper)
        let pathLower = try container.decodeIfPresent([String].self, forKey: .pathLower)
        let pathUpper = try container.decodeIfPresent([String].self, forKey: .pathUpper)
        let rightLower = try container.decodeIfPresent([Bool].self, forKey: .rightLower)
        let rightUpper = try container.decodeIfPresent([Bool].self, forKey: .rightUpper)

        guard let hashText = hashLower ?? hashUpper else {
            throw EdgeHashError.invalidProof("proof.hash is required")
        }
        guard let proofIndex = indexLower ?? indexUpper else {
            throw EdgeHashError.invalidProof("proof.index is required")
        }
        guard let pathText = pathLower ?? pathUpper,
              let rightFlags = rightLower ?? rightUpper else {
            throw EdgeHashError.invalidProof("proof.path and proof.right are required")
        }
        guard let hash = Data(edgeHashHex: hashText), hash.count == hashByteCount else {
            throw EdgeHashError.invalidProof("proof.hash must be 32 bytes of hex")
        }
        guard pathText.count == rightFlags.count else {
            throw EdgeHashError.invalidProof("proof.path and proof.right lengths differ")
        }
        guard pathText.count <= maxMerkleDepth else {
            throw EdgeHashError.invalidProof("proof is too deep")
        }
        var path = [Data]()
        path.reserveCapacity(pathText.count)
        for (level, value) in pathText.enumerated() {
            guard let sibling = Data(edgeHashHex: value), sibling.count == hashByteCount else {
                throw EdgeHashError.invalidProof("proof.path[\(level)] must be 32 bytes of hex")
            }
            path.append(sibling)
        }
        self.hash = hash
        self.index = proofIndex
        self.path = path
        self.right = rightFlags
    }

    func toProof() -> MerkleProof {
        MerkleProof(hash: hash, index: index, path: path, right: right)
    }
}
