# Docker: образ, Compose и ограничения

Практическая записка про Docker-образ `distributed-db` и локальные
Compose-стенды. Короткий запуск — [docker-2-minutes.md](quickstart/docker-2-minutes.md).
Это **dev/test/demo**, а не production и не HA-развёртывание. Production-гейты
и Helm-базу см. в [PRODUCT-READINESS.md](PRODUCT-READINESS.md) и
[deploy/helm/distributed-db/README.md](../deploy/helm/distributed-db/README.md).

## 1. Образ: что внутри

Dockerfile собирает **один multi-binary image**; Compose v2 и Helm запускают
разные бинари из него, переопределяя `entrypoint`/`command`.

| Путь | Назначение |
|---|---|
| `/app/kv2` | v2 meta/TSO (`-mode=meta`) и Multi-Raft KV-узел (`-mode=node`) |
| `/app/sqlsrv` | SQL поверх HTTP (`POST /v1/sql`), `/health`, `/healthz`, `/metrics`, `/debug/slo` |
| `/app/pgsrv` | PostgreSQL wire gateway; опционально |
| `/app/kvctl` | терминальный клиент v2: CRUD, `ranges`, `init-ranges`, `split/merge`, backup |
| `/app/kvbackup` | логический backup/restore/verify; TLS и admin-token для backup/restore |
| `/app/edgehash` | edge hash-set builder/server/client |
| `/app/db` | legacy v1 coordinator/node; оставлен для `docker-compose.v1.yml` |
| `/bin/sh`, busybox `wget`/`nc` | healthcheck-и Compose и startup-скрипты Helm |
| CA-сертификаты | исходящий TLS к meta/KV, S3/backup, edgehash |

Runtime-стадия — Alpine 3.20, не distroless: Helm-скриптам нужен POSIX shell.
`tzdata` намеренно нет: серверы логируют UTC.

**UID/GID:** процесс идёт от `10001:10001` (`USER 10001:10001`), `WORKDIR
/app`. `/data` и `/app` принадлежат этому UID; `/tmp` доступен на запись.
Состояние БД держите только в `/data`; `/app` — код образа, не место для
данных.

**Entrypoint по умолчанию:** `/app/db`, без `CMD`. Поэтому:

```bash
# legacy v1: аргументы уходят в /app/db
docker run --rm distributed-db:local node -h

# v2: entrypoint нужно переопределить
docker run --rm --entrypoint /app/kv2 distributed-db:local -h
docker run --rm --entrypoint /app/sqlsrv distributed-db:local -h
docker run --rm --entrypoint /app/kvctl distributed-db:local help
```

`docker run <image> /app/kv2 ...` **не** запустит kv2: `/app/kv2` станет
неизвестной командой legacy `/app/db`.

`EXPOSE 7001 9090 8080 5432 9187 8001` — только документация. Compose
публикует наружу лишь часть портов:

| Порт | Компонент | В Compose |
|---|---|---|
| `8080` | sqlsrv HTTP | demo и cluster, host `DB_SQL_PORT` |
| `9090` | meta/TSO gRPC | demo `DB_META_PORT`; в cluster — только `meta-1` |
| `7001` | KV gRPC/Raft | demo `DB_KV_PORT`; в cluster — только `kv-1` |
| `5432` | pgsrv | только профиль `pgsrv` в cluster, `DB_PG_PORT` |
| `9187` | pgsrv metrics | только профиль `pgsrv`, `DB_PG_METRICS_PORT` |
| `8001` | legacy v1 node HTTP | только `docker-compose.v1.yml` |

## 2. Сборка и теги

```bash
# локальный тег, который ждёт compose по умолчанию
docker build -t distributed-db:local .

# или через Makefile: VERSION/REVISION попадут в labels
make image IMAGE_REPOSITORY=distributed-db IMAGE_TAG=local
```

Сборка требует BuildKit: в Dockerfile есть cache mounts. Docker 24+ включает
его по умолчанию; при `DOCKER_BUILDKIT=0` сборка упадёт.

`make image` без `IMAGE_TAG` берёт тег из `git describe --tags --always --dirty`.
`VERSION`/`REVISION` — только metadata (OCI labels), на код не влияют.
`-trimpath`, `-buildvcs=false` и отсутствие timestamp-ов делают повторную
сборку одного коммита воспроизводимой.

`.dockerignore` исключает `.git`, `data/`, `bin/`, `*.md`, `*.pem`, `*.key`,
`*.crt` и локальные кеши. Секреты и документация в build context не попадают.

### Multi-arch

Dockerfile собирает Go-бинари под `TARGETOS/TARGETARCH` (`CGO_ENABLED=0`), а
runtime-стадия материализуется под целевую платформу.

```bash
# локальная одно-платформенная сборка под архитектуру хоста
docker build -t distributed-db:local .

# явная платформа (пример)
docker build --platform linux/amd64 -t distributed-db:local-amd64 .

# multi-arch OCI-архив без registry (amd64 + arm64)
make image-multiarch \
  IMAGE_REPOSITORY=distributed-db IMAGE_TAG=v0.1.0 \
  PLATFORMS=linux/amd64,linux/arm64 \
  OCI_DEST=/tmp/distributed-db-multiarch.oci.tar

# публикация multi-platform image index; нужен docker login
make image-multiarch-push IMAGE_REPOSITORY=<registry>/distributed-db IMAGE_TAG=v0.1.0
```

`make image-push` — публикация одного platform (build + push); для multi-arch
используйте `image-multiarch-push`. Обе команды требуют предварительного
`docker login` в ваш registry.

`docker buildx` не умеет `--load` для multi-platform: скрипт требует либо
`PUSH=1`, либо `OUTPUT=oci`. OCI-архив проверяется по `index.json`
(python3/jq); после push скрипт вызывает `docker buildx imagetools inspect`.

Официального published-образа в этом репозитории **нет**. `distributed-db:local`
и Helm-дефолт `distributed-db:0.1.0` — локальные теги. Если используете свой
registry, подставляйте `IMAGE_REPOSITORY`/`IMAGE_TAG` и не применяйте
непроверенный `:latest`.

## 3. UID, volumes и данные

Compose использует **именованные volumes**, по одному на Raft-узел:

| Стенд | Volumes |
|---|---|
| demo | `demo-meta-data`, `demo-kv-data` |
| cluster | `meta1-data`, `meta2-data`, `meta3-data`, `kv1-data`, `kv2-data`, `kv3-data` |

`docker compose ... down` останавливает контейнеры и **сохраняет** volumes;
`docker compose ... down -v` удаляет их вместе с данными. Для повторяемого
demo сначала делайте `down -v`, иначе `CREATE TABLE users` вернёт
`table already exists`.

Правила:

- **Один volume — один узел.** Не монтируйте один volume в два KV/meta-процесса:
  Raft-лог и state machine должны быть локальны для узла.
- Не используйте bind mounts для `/data` в Docker Desktop: запись идёт через
  VM/virtiofs, а UID `10001` может не иметь прав на host-каталог. Именованный
  volume создаётся с нужными правами.
- Не используйте NFS/SMB/сетевые ФС для Raft-данных: `fsync` и latency ломают
  поведение и не дают честных замеров.
- `bootstrap` — одноразовый контейнер с `read_only: true` и `tmpfs /tmp`; у него
  нет volume с данными. Он только читает range map и вызывает `kvctl`.
- Штатного backup/reset Compose не делает. `down -v` — это удаление данных, а
  не бэкап.

## 4. Compose: demo, cluster и legacy

В корне:

- `docker-compose.yml` — v2 demo, профиль `demo`: 1 meta + 1 KV + sqlsrv.
- `docker-compose.cluster.yml` — v2 cluster, профиль `cluster`: 3 meta + 3 KV +
  sqlsrv; опционально `pgsrv` (профиль `pgsrv`).
- `docker-compose.v1.yml` — legacy v1: 2 шарда × 3 реплики + координатор на
  `:8080`; не смешивайте с v2 на одних портах.

Все сервисы объявлены с `profiles`, поэтому профиль обязателен. Флаг
`--no-build` означает «использовать уже собранный `distributed-db:local`»;
без него Compose может начать неявную сборку.

Demo:

```bash
docker compose --profile demo up -d --no-build
docker compose --profile demo ps
docker compose --profile demo logs bootstrap
docker compose --profile demo down -v
```

Cluster (файл самодостаточен):

```bash
docker compose -f docker-compose.cluster.yml --profile cluster up -d --no-build
docker compose -f docker-compose.cluster.yml --profile cluster logs bootstrap
docker compose -f docker-compose.cluster.yml --profile cluster down -v
```

Эквивалентная форма с двумя файлами (cluster перекрывает demo):

```bash
docker compose -f docker-compose.yml -f docker-compose.cluster.yml \
  --profile cluster up -d --no-build
```

`sqlsrv` стартует только после успешного завершения `bootstrap`
(`depends_on: condition: service_completed_successfully`). `up -d` ждёт этот
шаг; если bootstrap упал, SQL-порт не поднимется.

`pgsrv` в cluster включается отдельно:

```bash
docker compose -f docker-compose.cluster.yml \
  --profile cluster --profile pgsrv up -d --no-build
```

Это plaintext-слушатель без auth; только для локальной проверки протокола.

### Переменные Compose

Шаблон — [.env.compose.example](../.env.compose.example); скопируйте в `.env`
рядом с compose-файлом. Основные:

| Переменная | Назначение | Default |
|---|---|---|
| `DB_IMAGE` | тег образа | `distributed-db:local` |
| `DB_LOG_LEVEL` | `debug/info/warn/error` | `info` |
| `DB_DURABILITY` | `sync` или экспериментальный `fast` | `sync` |
| `DB_ADMIN_TOKEN` | общий admin-token для meta/KV (пусто = выключен) | пусто |
| `DB_SQL_PORT`, `DB_META_PORT`, `DB_KV_PORT` | host-порты demo/cluster | `8080`, `9090`, `7001` |
| `DB_PG_PORT`, `DB_PG_METRICS_PORT` | порты pgsrv | `5432`, `9187` |
| `DB_BOOTSTRAP_*` | таймауты/попытки bootstrap | см. шаблон |

`DB_ADMIN_TOKEN` передаётся `kv2` **аргументом командной строки** (у бинаря нет
env-формы), поэтому виден в `docker inspect`/`ps` на хосте. Для локального demo
это приемлемо; для чего-то общего — только TLS/Helm/Secret.

## 5. TLS, token и auth

Образ **не читает** TLS/token из environment: это флаги бинарей. Helm-скрипты
читают env и транслируют их во флаги; Compose оставляет listeners plaintext.

| Компонент | Флаги | Замечания |
|---|---|---|
| `kv2` meta/node | `-tls-cert`, `-tls-key`, `-tls-client-ca`, `-tls-ca`, `-tls-server-name`, `-admin-token` | `-tls-cert/-tls-key` включают TLS listener; `-tls-client-ca` включает mTLS |
| `sqlsrv` | `-tls-cert`, `-tls-key`, `-tls-ca`, `-tls-server-name`, `-auth-token` | listener TLS и cluster TLS — разные вещи; `-auth-token` без TLS отклоняется на старте |
| `pgsrv` | `-tls-cert`, `-tls-key`, `-tls-ca`, `-tls-server-name`, `-auth-user`, `-auth-password` | password-auth требует TLS; это direct TLS, не PostgreSQL `SSLRequest` |
| `kvctl` | `-tls-ca`, `-tls-cert`, `-tls-key`, `-tls-server-name`, `-admin-token` | token также читается из `KVCTL_ADMIN_TOKEN`; в argv не обязателен |
| `kvbackup` | `-tls-ca`, `-tls-cert`, `-tls-key`, `-tls-server-name`, `-admin-token` | `backup`/`restore`; token без TLS отклоняется, кроме `-allow-plaintext-token` (dev) |

`sqlsrv` оставляет `/health` и `/healthz` публичными; `/v1/sql`, `/metrics` и
`/debug/slo` требуют Bearer при `-auth-token`. Проверить флаги в конкретном
образе:

```bash
docker run --rm --entrypoint /app/sqlsrv distributed-db:local -h
docker run --rm --entrypoint /app/kv2 distributed-db:local -h
```

В Compose TLS **не настроен**: `sqlsrv` слушает plaintext, auth выключен,
`DB_ADMIN_TOKEN` опционален. Не публикуйте эти порты за пределы localhost.
Helm-чарт, наоборот, по умолчанию требует TLS/mTLS и Secrets; значения
секретов в `values.yaml` не хранятся.

## 6. Производительность: где Docker врёт

- **Docker Desktop на macOS** запускает Linux VM. Даже named volume лежит в
  виртуальном диске; `fsync` и мелкая запись идут через VM. Замеры latency и
  throughput здесь не репрезентативны для Linux/NVMe и не являются SLO.
- **Linux + локальный NVMe** — минимальная честная конфигурация для I/O-
  замеров. Всё равно проверяйте `-durability=sync` и отсутствие сетевых ФС.
- **Не используйте bind mounts** для `/data`: на macOS это virtiofs/9p и
  медленный fsync; на Linux — риск UID/прав и случайного изменения файлов
  с host. Именованный volume на узел — правильный default.
- **Один volume на узел.** Общий volume для нескольких Raft-групп/узлов — это
  не «экономия», а corruption.
- `-durability=sync` — default и единственный production-safe профиль.
  `fast` существует для экспериментов; он не дает durability и не должен
  использоваться для данных, которые нужны после сбоя.
- Compose не задаёт CPU/memory limits. Для demo достаточно ~4 GB RAM; для
  cluster с 3 meta + 3 KV + SQL удобнее 8 GB. Это рекомендация, не гарантия.
- Один Docker host — одна точка отказа. `restart: unless-stopped` не является
  failover и не перезапускает «unhealthy» контейнер сам по себе.

## 7. Demo vs cluster vs production Helm

| | demo Compose | cluster Compose | Helm chart |
|---|---|---|---|
| Топология | 1 meta + 1 KV + sqlsrv | 3 meta + 3 KV + sqlsrv (+pgsrv) | 3/5 meta, 3–5 KV, 2 sqlsrv, 2 pgsrv |
| Репликация | нет | 3 | 3/5 |
| Bootstrap range map | сервис `bootstrap` | сервис `bootstrap` | опциональный Job `bootstrap.enabled` |
| TLS / auth | нет, plaintext | нет, plaintext | TLS/mTLS + Secrets по умолчанию |
| Persistence | named volumes | named volumes | PVC |
| Host/HA | один host, без HA | один host, без HA | multi-node baseline, но не production-сертификация |
| Назначение | demo/туториал | dev/test репликации | reference baseline для private cluster |

Compose **не** предназначен для production: нет TLS, backup, anti-affinity,
нескольких hostов и операторских процедур. Cluster Compose не даёт HA: один
Docker host, потеря hostа теряет весь стенд; перезапуск и failover в этой
проверке не тестировались. Helm-чарт — тоже reference baseline:
он не устанавливался в рамках этой проверки и не является production-
сертификатом; см. [deploy/helm/distributed-db/README.md](../deploy/helm/distributed-db/README.md).

## 8. Проверено на текущем checkout

Окружение: Docker 24.0.2, Docker Compose v2.18.1, macOS arm64.

| Проверка | Результат |
|---|---|
| `docker build -t distributed-db:local .` | ok |
| `docker compose --profile demo build` | ok |
| `docker compose --profile demo config` | ok |
| demo: `up -d`, bootstrap, `/healthz`, CREATE/INSERT/SELECT, `kvctl ranges`, `down -v` | ok |
| cluster: `config`, `up -d` (3 meta + 3 KV), bootstrap, SQL, `kvctl ranges`, `down -v` | ok |
| `make image IMAGE_REPOSITORY=distributed-db IMAGE_TAG=...` | ok |
| `make image-multiarch PLATFORMS=linux/arm64` (OCI archive) | ok |
| `make image-multiarch` для `linux/amd64,linux/arm64` | ok, OCI index содержит `linux/amd64` и `linux/arm64` (проверено по `index.json`) |
| `docker compose --profile demo` + cluster одновременно | не проверялось; порты пересекаются |

**Не проверялось:** pull из registry, `image-push`/`image-multiarch-push`,
публикация manifest-а, TLS/mTLS end-to-end, `pgsrv`/`psql`, failover,
restart-политики, backup/restore, Docker Desktop vs Linux performance,
production Helm install. Это не SLO-доказательство и не production-приёмка.
