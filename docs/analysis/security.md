# Безопасность: честный разбор (Sprint 2)

- Дата: 2026-11-17
- HEAD: `b588e24`
- Область: `cmd/kv2`, `cmd/sqlsrv`, `cmd/pgsrv`, `internal/{kvserver,metaserver,v2client,pgwire,grpctls,edgehttp,backup}`, `cmd/edgehash`, `mobile/`
- Метод: чтение кода и конфигов; эксплойты не запускались, это не пентест

> **Sprint 2 addendum (после аудита).** gRPC TLS/mTLS для KV, meta и Raft
> интегрирован в HEAD `843e670`: `kvserver`/`metaserver`/`raftgrpc`/`v2client`
> и CLI-флаги `-tls-*`. S-01 и S-04 закрыты **на уровне транспорта при
> включённом TLS**, но plaintext остаётся default, S-02 (admin RPC на data-
> порту, нет RBAC) и S-03 (SQL HTTP) не закрыты. Оценки ниже — на момент
> аудита; актуальный статус — в [README.md](README.md).

## 1. Вердикт

У v2 нет периметра безопасности. `internal/grpctls` (TLS 1.3, mTLS через
`ClientCAFile`, без plaintext-fallback) **не импортируется нигде за пределами
пакета**. Серверы не передают `grpc.Creds`, клиенты ходят через
`insecure.NewCredentials`. Сетевой доступ к порту 7001/9090 равен доступу
ко всем данным, TSO и карте range'ей.

Работающие контроли есть только в edgehash: TLS (опционально), Bearer-токен
(constant-time), per-IP rate limit, Ed25519-манифесты. Но TLS
выключен по умолчанию, legacy HMAC остаётся симметричным секретом, токен один,
статический и без `kid`/ротации.

`ASSESSMENT.md` §4.5 и `SPRINT-1-REPORT.md` §4 уже фиксируют «нет TLS/auth»;
ниже — разложение этого разрыва на векторы, severity и цену закрытия.

## 2. Покрытие контролей

| Слой | TLS | Аутентификация | Авторизация | Аудит | At rest |
|---|---|---|---|---|---|
| v2 KV/Raft (`:7001`) | нет | нет | нет | нет | нет |
| v2 meta/TSO (`:9090`) | нет | нет | нет | нет | нет |
| pgwire (`:5432`) | опционально, direct TLS | 1 статический пользователь | нет | нет | нет |
| sqlsrv HTTP (`:8080`) | нет | нет | нет | нет | нет |
| edgehash | опционально | Bearer (опционально) | нет (один токен) | нет | нет |
| mobile SDK | HTTPS обязателен | нет | нет | нет | зависит от ОС |
| бэкапы | нет | нет | нет | нет | нет |

## 3. Находки

| ID | Уровень | Находка и доказательство | Сценарий | Исправление | Оценка |
|---|---|---|---|---|---|
| S-01 | Critical | gRPC KV/Raft — plaintext без аутентификации: `internal/kvserver/server.go:256-265` (`grpc.NewServer` без `Creds`), `internal/kvserver/transport.go:59-65` и `internal/raftgrpc/transport.go:38` (`insecure`), `internal/v2client/client.go:287,363`, `cmd/kv2/main.go:24-37` (TLS-флагов нет) | Любой в сети: `Scan`/`Get` вытягивает весь range; `AppendEntries` с term+1 принимается как лог «лидера» (`service.go:703`) → раскол/подмена данных; трафик читается пассивно | Подключить `grpctls`: mTLS, сертификат на узел, SAN=node ID; перевести `peerTransport`, `raftgrpc`, `v2client` на `ClientCredentials`; тест mixed plaintext/TLS | 8–12 ч/д |
| S-02 | Critical | Админ- и membership-RPC на том же порту без auth: `proto/v2.proto:25-41`, `internal/metaserver/server.go:155-159,384-400`, `internal/metaserver/scheduler.go:58,169` | `SetRanges` переписывает карту range'ей, `AddReplica` создаёт range на произвольном узле, `RemoveVoter`/`RemoveReplica` ломает кворум; `kvbackup restore` перезаписывает ключи | Отдельный admin-listener, mTLS+RBAC, подпись/токен админ-запросов, аудит; запрет admin RPC на data-порту | 10–15 ч/д |
| S-03 | Critical | SQL HTTP без TLS и аутентификации: `internal/sqlhttp/http.go:51-68`, `cmd/sqlsrv/main.go:44-52` (bind `:8080`, все интерфейсы) | `POST /v1/sql` = полное чтение/запись БД без креденшелов | TLS + аутентификация (mTLS или токен), авторизация по statement, лимиты; по умолчанию loopback и выключенный endpoint | 5–8 ч/д |
| S-04 | High | Meta/TSO plaintext, без auth и без лимитов: `internal/metaserver/server.go:149-159` (`grpc.NewServer()` без опций), `352-380` (`AllocTS`) | Массовые `AllocTS` жгут TSO → отказ записи во всём кластере; MITM подменяет leader/range map | `grpctls` + квоты на TSO + rate limit + интерсепторы размера сообщений | 3–5 ч/д |
| S-05 | High | pgwire: один статический пользователь, cleartext-пароль, нет RBAC/tenant, пароль в argv: `cmd/pgsrv/main.go:31-34`, `internal/pgwire/config.go:40-49,152-159`, `server.go:469-522` | Пароль = суперпользователь; `ps`/history раскрывают его; подбор без lockout; нельзя выдать read-only | SCRAM-SHA-256, хэш в файле 0600/секрете, роли и гранты, lockout, rate limit, пароль не через argv | 10–15 ч/д |
| S-06 | High | Нет шифрования at rest и в бэкапах: `internal/kvserver/server.go:209-226` (Pebble без encryption), `backup/chunk.go:69` (`0644`), `backup/backup.go:73` (`0755`), `backup.go:303` (манифест `0644`), `server.go:522` (`ranges.json 0644`) | Кража диска/снапшота/бэкапа отдаёт все данные и метаданные; бэкапы часто лежат в менее защищённых местах | Envelope-encryption в storage, root key в KMS, шифрование бэкапов (age/AES-GCM) вне хоста, `0600` | 15–25 ч/д |
| S-07 | High | Аудита и identity нет: `pgwire/server.go:304,321` — только Debug; `sqlhttp` не логирует запросы; в KV/meta нет principal в контексте | Нельзя установить, кто читал/писал/менял membership; злоумышленник не оставляет следа; regulated не выполняется | Identity из mTLS SAN, структурный audit log (кто/что/когда/результат), append-only/WORM, экспорт в SIEM | 10–15 ч/д |
| S-08 | High | DoS и exfiltration через unbounded `Scan`: `kvserver/service.go:232-256`, `mvcc/read.go:114` (`limit <= 0` = unlimited); 16 MiB сообщения (`server.go:256-259`); нет лимитов соединений в pgwire (`server.go:251-290`) и meta | `Scan{start:"",end:"\xff",limit:0}` вытягивает весь range и память узла; pgwire/SQL без `LIMIT` дают OOM/CPU; rate limit есть только у edgehash | Серверный max scan limit + стриминг, квоты per-identity, connection limits, rate limit, таймауты | 6–10 ч/д |
| S-09 | Medium | Сетевая экспозиция: `docker-compose.yml:30-33` и далее публикуют Raft 7001–7006 и HTTP 8001–8006 на `0.0.0.0`; `Dockerfile:4,21` собирает только `cmd/db`, образов для v2 нет | Скан портов → plaintext-доступ к Raft в обход координатора; v2 ставят вручную, конфиг дрейфует | Bind на внутренний интерфейс, network policy, отдельные non-root образы v2, запрет публикации 7001 | 2–4 ч/д |
| S-10 | Medium | edgehash: legacy HMAC (общий секрет), статический Bearer в argv, TLS опционален, без `-key` подпись вообще не проверяется: `cmd/edgehash/main.go:209-215,461-477`, `internal/edgeset/edgeset.go:104`, `manifest.go:95`, `edgehttp/server.go:426-444` | Утечка HMAC-ключа = подделка манифеста всем клиентам; токен без scopes/ротации; без TLS всё открыто; `serve` без ключей принимает unsigned-store | Ed25519 обязательным, HMAC только с явным `-allow-legacy-hmac`; `kid`+ротация; токен из файла/env; mTLS; требовать TLS при токене | 5–8 ч/д |
| S-11 | Medium | Mobile trust: HTTPS обязателен, но TLS pinning нет; Bearer не поддерживается: `mobile/android/EdgeHashClient.kt:253-265,1509-1510`, `mobile/ios/EdgeHashClient.swift:1271-1272,1730-1732`; Ed25519 pin — `mobile/README.md:55-60` | MITM/корпоративный CA читает трафик (целостность защищена, конфиденциальность — нет); с `-auth-token` клиент не работает → auth выключают; ротация ключа = релиз приложения | SPKI-pinning с fallback, поддержка Bearer, `key id` и ротация | 5–10 ч/д |
| S-12 | Medium | Нет tenant isolation и RBAC: ключ в `proto/v2.proto` — просто `bytes`, `meta.Range` без tenant, `sqlengine` не знает пользователя/БД | Любой клиент читает все таблицы/ключи; доступа «только к своему префиксу» нет; multi-tenant исключён | Tenant-id в ключе/метаданных + проверка префикса на сервере, роли и политики | 15–25 ч/д |
| S-13 | Medium | Supply chain: `.github/workflows/ci.yml` не запускает `govulncheck`/SCA/SBOM; базовые образы по тегам, не digest (`Dockerfile:4,21`); `go.sum` фиксирует версии, но CVE не отслеживаются | CVE в зависимости или подмена базового образа не детектируются | `govulncheck`+trivy в CI, digest-pin, dependabot, SBOM | 2–3 ч/д |
| S-14 | Medium | Секреты и ключи: `docker-compose.yml:9-10` — `admin/admin` по умолчанию; пароли/ключи в argv (`cmd/pgsrv/main.go:34`, `cmd/edgehash/main.go:211-214`); KMS и ротации нет; `ranges.json` `0644` | Дефолтные креды + публикация портов = вход; argv видят другие пользователи; ключ не отозвать | Обязательные секреты без дефолтов, файлы `0600`/env, Vault/KMS, ротация, аудит доступа к ключам | 4–6 ч/д |

## 4. Что не является находкой

- `grpctls` корректен: TLS 1.3 минимум, `RequireAndVerifyClientCert`, отказ на TLS < 1.2, нет plaintext-fallback (`grpctls.go:127-153,219-231`). Проблема — интеграция, а не helper.
- `pgwire.Config.Validate` запрещает пароль без TLS (`config.go:78-84`) — правильный контроль; опасно лишь то, что нулевой конфиг по умолчанию даёт plaintext без auth.
- `edgehttp` сравнивает токен constant-time и берёт IP из `RemoteAddr`, не доверяя `X-Forwarded-For` (`server.go:437,522`).
- Бэкапы имеют checksums и атомарный staging — целостность, но не конфиденциальность.
- `Dockerfile:31` — non-root `10001`, но только для v1 `cmd/db`; v1 имеет Basic Auth/TLS (README:210,246), на v2 это не переносится.
- Mobile требует HTTPS и пинит Ed25519-манифест; HMAC там недоступен в production-режиме.

## 5. Приоритетный план

- **P0 (4–6 недель):** S-01, S-02, S-03 — без них сетевой доступ = владение кластером. Сначала `grpctls` в KV/Raft/meta (S-01+S-04), затем отдельный admin-listener (S-02), затем sqlsrv (S-03).
- **P1 (8–10 недель):** S-05…S-08 — аутентификация и роли, шифрование at rest/бэкапов, аудит, лимиты.
- **P2 (4–6 недель):** S-09…S-14 — экспозиция, edgehash-ключи, mobile pinning, tenant-модель, SCA, секреты.

## 6. Границы анализа

Это review кода, не пентест: эксплойты не запускались, нагрузка на реальной сети не измерялась, мобильный код не собирался, CI не прогонялся. Оценки — человеко-дни для 1–2 инженеров, без миграции развёртываний.

## 7. Итог

По безопасности v2 — **прототип в доверенной сети**: TLS/mTLS/аутентификация/авторизация/аудит/шифрование отсутствуют, единственный готовый блок (`grpctls`) не подключён. edgehash зрелее, но и там остаются legacy-HMAC, статический токен и необязательный TLS. До private single-tenant нужен P0; до regulated — весь список.
