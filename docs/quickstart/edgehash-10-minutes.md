# Edgehash за 10 минут

Собрать `cmd/edgehash`, загрузить маленький набор SHA-256, поднять сервер и
проверить membership: есть хеш или нет. Edgehash — **alpha** и
**встроенный компонент**, а не отдельный продукт; стабильного SDK/API promise
нет. Обзор — [edgehash.md](../tracks/edgehash.md), формат и ограничения —
[EDGE-HASH-CACHE.md](../EDGE-HASH-CACHE.md).

## Требования

- Go 1.23+;
- `openssl` для Ed25519-ключей;
- `shasum` (macOS) или `sha256sum` (Linux); `awk`;
- свободный порт `8081` (в примере; `8080` часто занят SQL HTTP);
- `curl` опционально.

## 1. Сборка

```bash
mkdir -p bin
go build -o bin/edgehash ./cmd/edgehash
```

## 2. Ключ подписи

```bash
openssl genpkey -algorithm ED25519 -out priv.pem
openssl pkey -in priv.pem -pubout -out pub.pem
```

`priv.pem` остаётся на build-хосте; серверу и клиенту нужен только `pub.pem`.
HMAC (`-key`) существует как legacy, но здесь используется Ed25519.

## 3. Маленький набор хешей

```bash
: > hashes.txt
for s in alice bob carol dave erin; do
  printf '%s' "$s" | shasum -a 256 | awk '{print $1}' >> hashes.txt
done

wc -l hashes.txt
# 5
```

На Linux замените `shasum -a 256` на `sha256sum`. Формат: один
64-символьный hex SHA-256 на строку; пустые строки и `#` игнорируются.

## 4. Сборка store

```bash
./bin/edgehash build -in hashes.txt -out ./edge-store \
  -ed25519-key priv.pem -compression auto
```

Ожидаемо:

```text
built 5 hashes in 5 segments at edge-store (manifest v1, signed (ed25519))
```

`edge-store/` immutable: обновление набора — новое поколение/новый store,
а не правка файлов на месте.

## 5. Сервер

```bash
./bin/edgehash serve -dir ./edge-store -addr=127.0.0.1:8081 \
  -verify-ed25519 pub.pem
```

Ожидаемо:

```text
edgehash: serving edge-store on 127.0.0.1:8081
```

## 6. Запрос: хеш есть

В другом терминале:

```bash
H=$(printf '%s' alice | shasum -a 256 | awk '{print $1}')
./bin/edgehash lookup -server http://127.0.0.1:8081 \
  -hash "$H" -verify-ed25519 pub.pem
```

Ожидаемо:

```text
present: true
prefix: 2bd8
root: 541be...
proof: verified
```

`lookup` проверяет подпись manifest и Merkle inclusion proof. `root` и
`prefix` зависят от набора.

## 7. Запрос: хеша нет

```bash
H2=$(printf '%s' mallory | shasum -a 256 | awk '{print $1}')
./bin/edgehash lookup -server http://127.0.0.1:8081 \
  -hash "$H2" -verify-ed25519 pub.pem
```

Ожидаемо:

```text
present: false
```

Для отсутствующего хеша в непустом сегменте сервер умеет отдавать absence
proof; этот пример с `mallory`, скорее всего, попадёт в пустой prefix и
вернёт только `present: false`. Это нормально для проверки membership.

## 8. Опционально: offline-кэш

```bash
./bin/edgehash sync -server http://127.0.0.1:8081 -dir ./edge-cache \
  -verify-ed25519 pub.pem -max-bytes 1048576 -max-segments 64
```

Ожидаемо:

```text
synced 5/5 segments (5 hashes) into edge-cache
cache: bytes=... segments=5 evictions=0 generations=1 generation=...
```

`sync` скачивает manifest и сегменты в crash-atomic generation directory;
`CURRENT` переключается атомарно. Это кэш, не источник истины.

## Границы alpha

- **Не отдельный продукт.** `cmd/edgehash` — опциональная сборка для удобства;
  компонент живёт в репозитории и не является заменой KV/SQL.
- **Нет стабильного SDK.** Формат manifest/сегментов, HTTP API и CLI могут
  меняться. Mobile Kotlin/Swift — scaffold, не собран и не проверен на
  устройствах.
- **Только read-only.** Удаление хеша — новое поколение; stale Bloom может
  дать false negative.
- **Ключи:** нет key id/ротации; private key хранится на build-хосте.
- **CDN:** заголовки/layout есть, multi-region CDN и signed URLs не
  развёрнуты; prefix-range download нет.
- **Не production:** нет SLA, квот и полной observability. HMAC — legacy.

## Проверено на текущем checkout

| Проверка | Результат |
|---|---|
| `go build -o bin/edgehash ./cmd/edgehash` | ok |
| `openssl genpkey`/`pkey` для Ed25519 | ok |
| `build` с `-ed25519-key` и `-compression auto` | ok, 5 сегментов |
| `serve` + `lookup` для `alice` | `present: true`, `proof: verified` |
| `lookup` для отсутствующего `mallory` | `present: false` |
| `sync` | 5/5 сегментов, `CURRENT` создан |

**Не проверялось:** TLS/Bearer/rate limit на edgehash-сервере, absence proof
для непустого сегмента, мобильные клиенты, ротация ключей, CDN и большие
наборы. Это alpha, а не production-приёмка.

## Остановка

`Ctrl-C` в терминале с `serve`. Store и ключи остаются в текущей директории;
удаляйте их вручную, когда сервер остановлен.
