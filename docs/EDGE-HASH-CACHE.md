# Edge hash cache: Bloom + Merkle + manifest

- Дата: 2026-11-16
- Задача: 0034
- Код: `internal/edgeset`, `internal/edgehttp`, `internal/edgecache`, `cmd/edgehash`

## 1. Зачем

Проверять «есть ли такой хеш в огромном наборе» на телефонах/edge-узлах,
не отправляя каждый запрос на сервер.

Идея:

- **авторитетные серверы** хранят точный набор и immutable сегменты;
- **телефон/edge** кэширует Bloom-сегменты и отвечает локально:
  - Bloom negative → «нет» без сети;
  - Bloom maybe → запрос на сервер + Merkle-proof.

Это не транзакционная БД и не распределённая память. Это read-only
(immutable) set с версионированием.

## 2. Формат

### Сегмент

Набор разбит по старшим байтам хеша:

- `PrefixBytes=2` → 65536 сегментов;
- `prefix = be16(hash[0:2])`.

Файл `segments/%04x.seg`:

```
magic "EDGSEG01"
version u32
prefix u16
reserved u16
count u32
bloom_bits u64
bloom_k u32
bloom_bytes u64
bloom bitset
sorted hashes [32]byte * count
```

Merkle root не дублируется в файле — он в manifest и пересчитывается при
загрузке. SHA-256 файла тоже в manifest; при загрузке проверяются size и
SHA-256.

### Bloom

- целевой false-positive rate по умолчанию 0.01;
- `m = ceil(-n ln p / (ln 2)^2)`, `k = round(m/n ln 2)`;
- double hashing от SHA-256;
- **false negatives = 0** (проверяется тестами).

### Merkle

- листья: `SHA256("edgeset:merkle-leaf:v1\0" || hash)`;
- узлы: `SHA256("edgeset:merkle-node:v1\0" || left || right)`;
- дерево дополняется `emptyLeaf` до степени двойки;
- proof: `Path`, `Right`, `Index`; verifier проверяет и путь, и Index.

### Manifest

```json
{
  "version": 1,
  "prefix_bytes": 2,
  "segments": [
    {"prefix": 4660, "count": 1234, "bloom_bits": 12340, "bloom_k": 7,
     "root": "hex", "file": "segments/1234.seg", "size": 45678,
     "sha256": "hex"}
  ],
  "signature": "hex"
}
```

`signature` — HMAC-SHA256 по каноническому JSON всех полей кроме signature.
Ключ задаётся при build/serve/cache. HMAC — не публичная подпись; для
недоверенных клиентов нужен Ed25519/публичный ключ (не в этом срезе).

## 3. Сервер

`cmd/edgehash serve -dir DIR -addr :8080 -key HEX`

- `GET /healthz`
- `GET /v1/manifest`
- `GET /v1/segment/{prefix}` — raw segment bytes
- `GET /v1/lookup/{hash}`:
  - `{"present":false}` — Bloom negative или нет в сегменте;
  - `{"present":true,"proof":{...},"root":"hex","prefix":"hex"}`.

Сервер загружает immutable store, проверяет manifest/segments на старте.

## 4. Клиент/телефон

`internal/edgecache`:

- `Sync(ctx)` — скачать manifest, проверить HMAC, скачать недостающие сегменты
  атомарно;
- `Contains(ctx,h)`:
  1. локальный `MayContain` (Bloom) — false → ответ без сети;
  2. локальный `Contains` — true → ответ;
  3. иначе remote lookup + проверка Merkle-proof по root из manifest.

Кэш можно ограничить набором prefix’ов (телефон не тянет все сегменты).

## 5. Версионирование и false negatives

Bloom нельзя удалять. Старый фильтр может дать **false negative** для хеша,
добавленного после версии.

Правила:

- store immutable; обновление = новый manifest/сегменты (новая версия);
- клиент проверяет версию manifest;
- для append-only набора можно использовать несколько поколений сегментов;
- при сомнении — remote lookup;
- удаление из Bloom невозможно: нужен counting Bloom/Cuckoo или новое
  поколение сегментов.

## 6. Оценки

Для 1 млрд хешей:

| Что | Размер |
|---|---:|
| Точные хеши (32B) | ~32 ГБ + overhead |
| Bloom 1% | ~1.2 ГБ |
| Bloom 0.1% | ~1.7 ГБ |
| Один сегмент при 2B prefix | в среднем ~15k хешей |

Для 10 млрд: Bloom 1% ~12 ГБ, точные хеши — единицы ТБ.

## 7. Ограничения

- immutable только; удаление — новое поколение;
- HMAC, не публичная подпись;
- нет сжатия сегментов;
- нет partial download по диапазону хешей (только prefix);
- нет rate limiting/auth на HTTP;
- телефон — только кэш, не источник истины;
- stale Bloom → возможен false negative; версия обязательна.

## 8. Что дальше

- Ed25519-подпись manifest для недоверенных клиентов;
- сжатие сегментов (zstd);
- range download `prefix_start..prefix_end`;
- Bloom для отрицательных кэшей на сервере;
- quota/rate limit;
- интеграция с мобильным клиентом (Wi-Fi/charging policy).
