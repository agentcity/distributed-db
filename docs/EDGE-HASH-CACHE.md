# Edge hash cache: Bloom + Merkle + manifest

- Дата: 2026-11-16
- Задачи: 0034, 0035
- Код: `internal/edgeset`, `internal/edgehttp`, `internal/edgecache`, `cmd/edgehash`, `mobile/`

## 1. Зачем

Проверять «есть ли такой хеш в огромном наборе» на телефонах/edge-узлах,
не отправляя каждый запрос на сервер.

- **авторитетные серверы** хранят точный набор и immutable сегменты;
- **телефон/edge** кэширует Bloom-сегменты:
  - Bloom negative → «нет» без сети;
  - Bloom maybe → exact lookup; положительный ответ проверяется Merkle-proof.

Это read-only immutable set с версионированием, не транзакционная БД.

## 2. Формат

### Сегмент v2

Набор разбит по старшим байтам хеша:

- `PrefixBytes=2` → 65536 сегментов;
- `prefix = be16(hash[0:2])`.

Файл `segments/%04x.seg`:

```
magic "EDGSEG01"
version u32 = 2
prefix u16
compression u8     # 0 none, 1 zstd
reserved u8
count u32
bloom_bits u64
bloom_k u32
bloom_bytes u64
raw_size u64
payload            # Bloom bitset + sorted hashes, maybe zstd
```

v1 (40-byte header, без compression/raw_size) читается для совместимости.
Новые сборки пишут v2. SHA-256 и size в manifest покрывают **stored** файл;
сжатие проверяется после size/SHA.

Merkle root не дублируется в файле — он в manifest и пересчитывается при
загрузке.

### Bloom

- target FP 0.01 по умолчанию;
- `m = ceil(-n ln p / (ln 2)^2)`, `k = round(m/n ln 2)`;
- double hashing от SHA-256;
- **false negatives = 0**.

### Merkle и absence

- leaf: `SHA256("edgeset:merkle-leaf:v1\0" || hash)`;
- node: `SHA256("edgeset:merkle-node:v1\0" || left || right)`;
- padding: domain-separated `emptyLeaf`.

Inclusion proof: `Path`, `Right`, `Index`.

Non-membership proof (`AbsenceProof`):

- `h < first` → proof первого листа;
- `h` между → proof двух **соседних** листьев `left < h < right`;
- `h > last` → proof последнего листа (пустые правые siblings).
- Проверяются root, prefix, индексы и строгий порядок; non-adjacent
  индексы отвергаются.

Absence proof актуален только для версии manifest; stale Bloom может дать
false negative.

### Manifest

```json
{
  "version": 1,
  "prefix_bytes": 2,
  "segments": [
    {"prefix": 4660, "count": 1234, "bloom_bits": 12340, "bloom_k": 7,
     "root": "hex", "file": "segments/1234.seg", "size": 45678,
     "sha256": "hex", "compression": "zstd"}
  ],
  "signature": "hex",
  "signature_alg": "ed25519"
}
```

Подпись покрывает канонический JSON всех полей кроме `signature`.

- `signature_alg: "ed25519"` — публичная подпись; телефону нужен только
  public key. Рекомендуемый режим.
- `signature_alg` пустой/`"hmac-sha256"` — legacy HMAC; ключ симметричный,
  подходит только для доверенного контура.

## 3. Сервер

```bash
edgehash serve -dir DIR -addr :8080 \
  -verify-ed25519 pubkey.hex \
  -tls-cert server.crt -tls-key server.key \
  -auth-token TOKEN -rate-limit 100 -rate-burst 200
```

- `GET /healthz`
- `GET /v1/manifest` — no-cache + ETag/304
- `GET /v1/manifest/version`
- `GET /v1/segment/{prefix}`:
  - raw stored bytes (zstd — как есть);
  - `Range`/206/416, `Accept-Ranges: bytes`;
  - `Cache-Control: public, max-age=31536000, immutable`;
  - strong ETag по SHA-256.
- `GET /v1/lookup/{hash}?absence=1`:
  - `present:true` + inclusion proof;
  - `present:false` + `absence_proof` (если запрошено);
  - `root`, `prefix`.

TLS, Bearer auth и per-IP rate limit включаются опциями/флагами.
`/healthz` можно оставить без auth (LB health check).

## 4. Клиент/телефон

`internal/edgecache`:

- `Sync(ctx)`:
  - скачивает manifest, проверяет Ed25519/HMAC;
  - пишет **новую generation directory**;
  - проверяет все сегменты, fsync, atomic rename;
  - атомарно переключает `CURRENT`;
  - старая generation остаётся fallback; при битом `CURRENT` выбирается
    последняя валидная.
- `Contains(ctx,h)`:
  1. локальный `MayContain` false → ответ без сети;
  2. локальный exact hit → true;
  3. иначе remote lookup + проверка proof (inclusion/absence).
- Eviction/TTL:
  - `MaxCacheBytes`, `MaxSegments`, `SegmentTTL`, `EvictInterval`;
  - LRU-ish; evicted segment → remote fallback;
  - `Stats` содержит bytes/segments/evictions.

Кэш можно ограничить prefix’ами: телефон не тянет все сегменты.

## 5. Mobile SDK

`mobile/` — **scaffold**, не production SDK:

- `android/EdgeHashClient.kt` — HttpURLConnection, org.json, Ed25519 (API 33+),
  SHA-256, Merkle, Bloom, cache/policy hooks;
- `ios/EdgeHashClient.swift` — URLSession, CryptoKit Ed25519, Codable manifest,
  SHA-256, Merkle, Bloom;
- `protocol.md`, `TESTING.md`, `tests/notes.md`.

Ни Android, ни iOS не собраны в этом репозитории. Перед релизом нужны
unit-тесты, device-тесты, pinning ключа, Wi-Fi/charging/quota policy.

## 6. Версионирование и false negatives

- store immutable; обновление = новый manifest/сегменты;
- клиент проверяет версию manifest;
- stale Bloom может дать **false negative** для добавленного позже хеша;
- при сомнении — remote lookup;
- удаление из Bloom невозможно: новое поколение или counting/Cuckoo.

## 7. Оценки

Для 1 млрд хешей:

| Что | Размер |
|---|---:|
| Точные хеши (32B) | ~32 ГБ + overhead |
| Bloom 1% | ~1.2 ГБ |
| Bloom 0.1% | ~1.7 ГБ |
| Один сегмент при 2B prefix | в среднем ~15k хешей |

Для 10 млрд: Bloom 1% ~12 ГБ, точные хеши — единицы ТБ.
zstd обычно даёт 10–40% на Bloom/хешах, но зависит от энтропии.

## 8. Ограничения

- immutable; удаление — новое поколение;
- HMAC legacy остаётся для доверенного контура;
- Ed25519 private key хранится на build-хосте; ротация ключей требует
  нового manifest/version;
- нет prefix-range download (только HTTP byte range);
- mobile SDK — scaffold;
- stale Bloom → false negative;
- телефон — кэш, не источник истины;
- CDN: сегменты immutable, manifest — small/no-cache; нужен purge/versioning
  на стороне CDN.

## 9. Что дальше

- Ed25519 key rotation/key id;
- zstd dictionary для мелких сегментов;
- prefix-range download;
- CDN multi-region + signed URLs;
- mobile CI (Gradle/SwiftPM) и device-тесты;
- server-side negative cache.
