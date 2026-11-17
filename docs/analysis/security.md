# Безопасность: текущее состояние

- Дата: 2026-11-17
- Метод: чтение кода и конфигов; не пентест.

## 1. Вердикт

gRPC TLS/mTLS, SQL HTTP TLS/Bearer и admin-token для meta реализованы, но
**все три выключены по умолчанию**. Периметр безопасности ещё не production:
KVService admin RPC не защищены, нет отдельного admin-listener, RBAC, audit и
encryption at rest. Безопасность допустима только в доверенной сети с
явно включёнными TLS/token.

## 2. Покрытие

| Слой | TLS | Аутентификация | Авторизация | Аудит | At rest |
|---|---|---|---|---|---|
| v2 KV/Raft (`:7001`) | опционально, TLS 1.3/mTLS | mTLS при включении | нет | нет | нет |
| v2 meta/TSO (`:9090`) | опционально | mTLS при включении | нет | нет | нет |
| pgwire (`:5432`) | опционально, direct TLS | один статический пользователь | нет | нет | нет |
| sqlsrv HTTP (`:8080`) | нет | нет | нет | нет | нет |
| edgehash | опционально | Bearer | нет | нет | нет |
| backup | нет | нет | нет | нет | нет |

## 3. Текущие находки

| ID | Уровень | Находка | Исправление |
|---|---|---|---|
| S-01 | High | SQL HTTP TLS/Bearer реализованы, но default open; без флагов — полный доступ | fail-closed профиль, auth обязателен |
| S-02 | High | Meta admin RPC защищены токеном, но KVService admin RPC — нет; нет отдельного admin-listener/RBAC | отдельный listener, mTLS/token на KV admin, RBAC, audit |
| S-03 | High | TLS opt-in, plaintext default; нет требования TLS в production | fail-closed профиль, TLS обязателен |
| S-04 | High | Нет RBAC/identity на уровне запросов | principal из mTLS, роли/гранты |
| S-05 | High | Нет audit log | структурный audit, export |
| S-06 | High | Нет encryption at rest и шифрования backup | KMS/envelope, encrypted backup |
| S-07 | High | pgwire: cleartext-over-TLS, один пользователь, пароль в argv | SCRAM, роли, secrets |
| S-08 | Medium | `kvbackup` ходит plaintext | TLS-опции и mTLS |
| S-09 | Medium | Unbounded Scan/лимиты, 16 MiB RPC | серверный max scan, квоты |
| S-10 | Medium | edgehash: legacy HMAC, статический Bearer, TLS опционален | Ed25519 обязателен, kid/rotation |
| S-11 | Medium | mobile: no TLS pinning, no Bearer | SPKI pinning, Bearer |
| S-12 | Medium | Нет tenant isolation | tenant-id/префиксы, RBAC |
| S-13 | Medium | Нет SCA/SBOM/digest pinning | govulncheck/trivy/SBOM |
| S-14 | Medium | Дефолтные admin/admin, секреты в argv | обязательные секреты, KMS |

## 4. Что закрыто

- gRPC TLS 1.3/mTLS для KV/meta/Raft, v2client, CLI (`kv2/sqlsrv/pgsrv/kvctl`).
- SQL HTTP TLS + Bearer (`sqlsrv -tls-cert/-tls-key/-auth-token`).
- Admin-token на meta `SetRanges/SplitRange/MergeRanges`; kvctl/kv2 флаги.
- mTLS 3-node Raft, отказ plaintext/certless клиентов.
- pgwire TLS + password (cleartext внутри TLS), auth без TLS запрещён.
- edgehash TLS/Bearer/rate limit, Ed25519-манифесты.

## 5. Приоритет

1. Fail-closed профиль: TLS/auth обязательны в production.
2. Admin-listener + токен/mTLS на KV admin RPC + RBAC + audit.
3. SQL HTTP default-open убрать.
4. Encryption at rest/backups.
5. pgwire SCRAM/роли.
6. kvbackup TLS, edgehash key rotation, mobile pinning, SCA.

## 6. Ограничения

Это review кода, не пентест: эксплойты не запускались, нагрузка на реальной
сети не измерялась, mobile не собирался.
