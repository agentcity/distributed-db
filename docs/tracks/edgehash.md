# Трек: Edgehash

> **Edgehash — интегрированный компонент репозитория, не отдельный продукт.**
> Опциональная сборка `cmd/edgehash` — convenience-идея: удобный способ
> запустить компонент отдельным процессом. Она не превращает edgehash в
> самостоятельный продукт, отдельный кластер или замену KV/SQL.

- **Статус:** alpha. Серверная часть (build/serve/lookup/sync) работает;
  mobile SDK — scaffold, не собран и не проверен на устройствах.
- **Что это:** read-only membership для больших immutable наборов SHA-256:
  Bloom для отрицательных ответов, Merkle inclusion/absence proofs, подпись
  manifest. Не транзакционная БД и не хранилище изменяемых данных.
- **Практика:** [edgehash за 10 минут](../quickstart/edgehash-10-minutes.md),
  [EDGE-HASH-CACHE.md](../EDGE-HASH-CACHE.md).
- **Не в этом треке:** KV и SQL — [kv-cluster.md](kv-cluster.md),
  [sql-newsql.md](sql-newsql.md).

## Текущий статус

| Часть | Статус | Что это значит |
|---|---|---|
| `cmd/edgehash` | alpha | build/serve/lookup/sync; отдельная сборка — convenience |
| `internal/edgeset` | alpha | сегменты, Bloom, Merkle, подпись, zstd |
| `internal/edgehttp` | alpha | HTTP API, TLS/Bearer/rate limit, Range/CDN headers |
| `internal/edgecache` | alpha | generation dirs, atomic `CURRENT`, LRU/TTL, remote fallback |
| `mobile/` | scaffold | Kotlin/Swift есть; не собраны, device-тестов нет |
| Production / SDK | нет | Нет стабильного SDK/API/format promise и CDN-развёртывания |

## Точки входа

| Что | Где |
|---|---|
| Сборка/сервер/запросы | `cmd/edgehash`: `build`, `serve`, `lookup`, `sync` |
| Формат и доказательства | `internal/edgeset` (сегменты v1/v2, Bloom, Merkle, Ed25519) |
| HTTP | `internal/edgehttp`: `/healthz`, `/v1/manifest`, `/v1/manifest/version`, `/v1/segment/{prefix}`, `/v1/lookup/{hash}` |
| Кэш | `internal/edgecache`: `Sync`, `Contains`, generation/LRU/TTL |
| Mobile scaffold | `mobile/android`, `mobile/ios`, `mobile/protocol.md`, `mobile/TESTING.md` |
| Документация | [EDGE-HASH-CACHE.md](../EDGE-HASH-CACHE.md) |

## Что поддерживается

- Вход: один 64-символьный hex SHA-256 на строку; пустые строки и `#`
  игнорируются; `-in -` читает stdin.
- Immutable сегменты по старшим байтам хеша; Bloom с target FP; точные хеши;
  Merkle inclusion и non-membership (absence) proofs.
- Manifest: Ed25519 (рекомендуется) или legacy HMAC-SHA256; подпись покрывает
  канонический JSON.
- Сегменты v2 с zstd (`auto/none/zstd`), чтение v1 для совместимости.
- HTTP: TLS, Bearer auth, per-IP rate limit, Range/206/416, strong ETag,
  immutable `Cache-Control`, CDN-friendly заголовки.
- `lookup`: проверка подписи manifest и Merkle-proof; `present: true/false`.
- `sync`: crash-atomic generation directory, atomic `CURRENT`, LRU/TTL
  eviction, remote fallback для вытесненных сегментов.
- Обновление набора — только новое поколение/manifest; удаление хеша из
  immutable-набора не поддерживается.

## Что не поддерживается или ограничено

- **Alpha:** API, формат manifest/сегментов и CLI могут меняться; стабильного
  SDK promise нет.
- **Mobile:** Kotlin/Swift — scaffold; не собран в этом репозитории, не
  проверен на устройствах; нет zstd/absence/Bearer/Range (по
  [ASSESSMENT.md](../ASSESSMENT.md)).
- **Данные:** только read-only immutable set; удаление — новое поколение;
  stale Bloom может дать false negative.
- **Ключи:** нет key id/ротации; Ed25519 private key хранится на build-хосте;
  HMAC остаётся legacy для доверенного контура.
- **Сеть/CDN:** нет prefix-range download, multi-region CDN, signed URLs и
  server-side negative cache; CDN headers/layout есть, развёртывания нет.
- **Эксплуатация:** нет production SLA, квот и полноценной observability;
  компонент не заменяет БД и не является источником истины.

## Следующие шаги

1. Ротация ключей и `key id` в manifest.
2. zstd dictionary для мелких сегментов; prefix-range download.
3. CDN multi-region, signed URLs, purge/versioning, negative cache.
4. Mobile CI (Gradle/SwiftPM), unit/device-тесты, pinning ключа, политики
   Wi-Fi/charging/quota.
5. Проверить FPR/размеры на реальных наборах и зафиксировать границы alpha.

Границы продукта: edgehash остаётся встроенным компонентом; `cmd/edgehash` —
опциональная сборка для удобства, а не отдельный продукт. Подробности и
ограничения — в [EDGE-HASH-CACHE.md](../EDGE-HASH-CACHE.md) и
[ASSESSMENT.md](../ASSESSMENT.md).
