# Docker за 2 минуты

Поднять локальный v2-стенд из готового образа: demo (1 meta + 1 KV + SQL) или
cluster (3 meta + 3 KV + SQL) через Docker Compose. Это **dev/demo**, не HA и
не production. Подробности об образе, volumes и TLS — в
[DOCKER.md](../DOCKER.md); production-база — Helm, но и она пока не является
production-сертификатом.

> **Проверено:** Docker 24.0.2, Docker Compose v2.18.1, macOS arm64.
> **Не проверялось:** pull из registry, публикация multi-arch в registry, TLS, `pgsrv`,
> failover и производительность на Linux/NVMe. См. таблицу в конце.

## Требования

- Docker 24+ (Engine или Docker Desktop) с BuildKit;
- Docker Compose v2 (`docker compose version`); нужен
  `service_completed_successfully` — проверено на v2.18.1;
- свободные **4 GB RAM** (для cluster удобнее 8 GB);
- свободные порты `8080` (SQL), `9090` (meta) и `7001` (KV) на host;
- `curl`; `jq` не обязателен;
- Go не нужен — всё собирается в образе.

На macOS Docker Desktop должен быть запущен (`docker info`). Windows — WSL2 с
Docker Desktop.

## 1. Собрать образ

Из корня репозитория:

```bash
docker build -t distributed-db:local .
```

`distributed-db:local` — тот тег, который Compose использует по умолчанию.
Первый build скачивает базовые образы и Go-модули, поэтому занимает заметно
дольше последующих.

Если у вас есть **свой** registry (официального published-образа в репозитории
нет):

```bash
docker pull <registry>/distributed-db:<tag>
export DB_IMAGE=<registry>/distributed-db:<tag>
```

Эта ветка в текущем checkout **не проверялась**: registry/тег не опубликованы.

## 2. Demo: 1 meta + 1 KV + SQL

```bash
docker compose --profile demo up -d --no-build
```

`--no-build` использует уже собранный в §1 образ и не запускает неявную
сборку; если образа нет, команда завершится ошибкой — сначала выполните
`docker build`. Без `--no-build` эквивалентная команда
`docker compose --profile demo up -d` сама соберёт образ при его отсутствии.
Профиль обязателен: все сервисы объявлены с `profiles`, поэтому
`docker compose up` без `--profile demo` не выберет ни одного контейнера.

Compose поднимет `meta-1` и `kv-1`, дождётся их healthcheck-ов, выполнит
одноразовый `bootstrap` и только после его успеха запустит `sqlsrv`. В конце
`docker compose --profile demo ps` показывает `bootstrap` как `Exited (0)`, а
`sqlsrv` — `healthy`.

Проверить bootstrap:

```bash
docker compose --profile demo logs bootstrap | tail -3
# ... bootstrap complete: meta=meta-1:9090 replicas=kv-1:7001
```

Health:

```bash
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok"}
```

SQL (создаём таблицу, вставляем строку, читаем):

```bash
SQL=http://127.0.0.1:8080/v1/sql

curl -sS -X POST "$SQL" -H 'Content-Type: application/json' \
  -d '{"sql":"CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"}'
# {"message":"table created"}

curl -sS -X POST "$SQL" -H 'Content-Type: application/json' \
  -d "{\"sql\":\"INSERT INTO users VALUES (1, 'alice')\"}"
# {"rows_affected":1,"message":"row inserted"}

curl -sS -X POST "$SQL" -H 'Content-Type: application/json' \
  -d '{"sql":"SELECT * FROM users ORDER BY id"}'
# {"columns":["id","name"],"rows":[[1,"alice"]]}
```

Range map:

```bash
docker compose --profile demo exec -T meta-1 \
  /app/kvctl -meta 127.0.0.1:9090 ranges
# ID  START  END  LEADER  REPLICAS  VERSION
# 1                              kv-1:7001  1
```

`LEADER` в текущей сборке остаётся пустым даже на здоровом стенде: meta
не заполняет поле leader в descriptor. Это не индикатор отказа. Проверяйте
`REPLICAS` (ожидайте `kv-1:7001` в demo и три адреса в cluster) и реальный
write/read через SQL.

### Если demo уже запускался

`users` хранится в volume `demo-kv-data`. Повторный `up` без очистки даст
`{"error":"sqlengine: table already exists"}`. Для чистого demo:

```bash
docker compose --profile demo down -v
docker compose --profile demo up -d --no-build
```

`down -v` **удаляет данные**. Без `-v` volumes сохраняются.

## 3. Cluster: 3 meta + 3 KV + SQL

```bash
docker compose -f docker-compose.yml -f docker-compose.cluster.yml \
  --profile cluster up -d --no-build
```

Здесь также нужен `--profile cluster`: без него ни один cluster-сервис не
выбирается.

Ту же топологию можно поднять одним cluster-файлом (он самодостаточен):

```bash
docker compose -f docker-compose.cluster.yml --profile cluster up -d --no-build
```

Без `--no-build` обе cluster-команды тоже работают, но могут запустить
неявную сборку образа.

Дальше `bootstrap` сам находит лидера meta, вызывает
`kvctl init-ranges` с тремя KV-репликами и проверяет range map. `sqlsrv`
стартует только после `Exited (0)` у bootstrap:

```bash
docker compose -f docker-compose.yml -f docker-compose.cluster.yml \
  --profile cluster logs bootstrap | tail -5
# ... init-ranges: {"status":"ok","range_id":1,...,"already_initialized":false}
# ... bootstrap complete: meta=meta-3:9090 \
#     replicas=kv-1:7001,kv-2:7001,kv-3:7001
```

Лидер meta может быть `meta-1`, `meta-2` или `meta-3`; это нормально.

Range map:

```bash
docker compose -f docker-compose.yml -f docker-compose.cluster.yml \
  --profile cluster exec -T meta-1 \
  /app/kvctl -meta 127.0.0.1:9090 ranges
# ID  START  END  LEADER  REPLICAS  VERSION
# 1                              kv-1:7001,kv-2:7001,kv-3:7001  1
```

`LEADER` в текущей сборке остаётся пустым даже после bootstrap и успешных
SQL-запросов: meta descriptor не заполняет это поле. Ориентируйтесь на
`REPLICAS` (три адреса) и на write/read через SQL, а не на `LEADER`.

Health и SQL:

```bash
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok"}

SQL=http://127.0.0.1:8080/v1/sql

curl -sS -X POST "$SQL" -H 'Content-Type: application/json' \
  -d '{"sql":"CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"}'
# {"message":"table created"}

curl -sS -X POST "$SQL" -H 'Content-Type: application/json' \
  -d "{\"sql\":\"INSERT INTO users VALUES (1, 'alice')\"}"
# {"rows_affected":1,"message":"row inserted"}

curl -sS -X POST "$SQL" -H 'Content-Type: application/json' \
  -d '{"sql":"SELECT * FROM users ORDER BY id"}'
# {"columns":["id","name"],"rows":[[1,"alice"]]}
```

Опционально PostgreSQL wire (plaintext, без auth, только для проверки
протокола):

```bash
docker compose -f docker-compose.cluster.yml \
  --profile cluster --profile pgsrv up -d --no-build
psql "host=127.0.0.1 port=5432 dbname=demo"
```

Этот путь в текущей проверке **не тестировался**.

## 4. Остановка

```bash
# demo
docker compose --profile demo down -v

# cluster (та же форма -f, с которой запускали)
docker compose -f docker-compose.yml -f docker-compose.cluster.yml \
  --profile cluster down -v
```

`down` без `-v` сохраняет данные; `down -v` удаляет и volumes, и сеть.
Не запускайте demo и cluster одновременно: у них совпадают host-порты
`8080`, `9090`, `7001`.

## 5. Если что-то не поднялось

### Порты заняты

Симптом: `Bind for 0.0.0.0:8080 failed: port is already allocated`.

Скопируйте шаблон и поменяйте **host**-порты (внутренние порты контейнеров не
меняются):

```bash
cp .env.compose.example .env
# в .env:
# DB_SQL_PORT=18080
# DB_META_PORT=19090
# DB_KV_PORT=17001
docker compose --profile demo up -d --no-build
```

Проверить итоговые публикации: `docker compose --profile demo config` или
`docker compose --profile demo ps`. Если занят `5432`/`9187`, это профиль
`pgsrv`; либо не включайте его, либо задайте `DB_PG_PORT`/`DB_PG_METRICS_PORT`.

### Docker daemon недоступен

`Cannot connect to the Docker daemon` — запустите Docker Desktop/engine и
проверьте `docker info`. На macOS: `open -a Docker`, затем повторите.

### Bootstrap не завершился

`sqlsrv` не стартует, пока `bootstrap` не завершится успешно. Смотрите:

```bash
docker compose --profile demo logs bootstrap
docker compose --profile demo ps -a
```

Частые причины:

- meta не выбрал лидера (cluster): подождите/посмотрите логи `meta-*`;
- не совпали адреса/порты или `DB_ADMIN_TOKEN` у meta/KV и bootstrap;
- range map уже изменён (split/старые данные) — `init-ranges` откажется
  перезаписывать непустой map.

Увеличить окно ожидания можно в `.env`:
`DB_BOOTSTRAP_WAIT_TIMEOUT`, `DB_BOOTSTRAP_INIT_ATTEMPTS`,
`DB_BOOTSTRAP_VERIFY_ATTEMPTS`. Не удаляйте volumes «на всякий случай»: если
там уже есть данные, `down -v` их уничтожит. Сначала прочитайте
`bootstrap`/`meta` логи.

### `table already exists` / старые данные

Это не ошибка SQL, а сохранённый volume. Либо используйте новое имя таблицы,
либо для чистого demo сделайте `down -v` (удалит данные).

### Healthcheck зелёный, а SQL не работает

Healthcheck-и `meta-1`/`kv-1` в Compose — только TCP (`nc -z`), они не
доказывают наличие лидера Raft или корректный range map. Проверяйте:

```bash
docker compose --profile demo ps
docker compose --profile demo logs --tail=50 meta-1 kv-1 sqlsrv
docker compose --profile demo exec -T meta-1 \
  /app/kvctl -meta 127.0.0.1:9090 status
docker compose --profile demo exec -T meta-1 \
  /app/kvctl -meta 127.0.0.1:9090 ranges
curl -s -i http://127.0.0.1:8080/healthz
```

`/health` и `/healthz` у `sqlsrv` публичны; `/v1/sql` в Compose без auth.
Если включили `DB_ADMIN_TOKEN`, помните: в Compose meta/KV получают его
аргументом, а bootstrap — через env; значение должно совпадать.

### Образ не найден

`docker compose ... up -d` с `build:` соберёт образ, если тега
`distributed-db:local` нет. Если меняли Dockerfile/код, пересоберите явно:

```bash
docker build -t distributed-db:local .
# или
docker compose --profile demo build
```

## 6. Что проверено

| Проверка | Результат |
|---|---|
| `docker build -t distributed-db:local .` | ok |
| demo: `docker compose --profile demo config` | ok |
| demo: `up -d`, bootstrap, `/healthz`, CREATE/INSERT/SELECT, `ranges` | ok |
| demo: `down -v` и повторный чистый `up` | ok |
| cluster: `config` (обе формы `-f`) | ok |
| cluster: 3 meta + 3 KV (обе формы `-f`: два файла и один cluster-файл), bootstrap, `/healthz`, CREATE/INSERT/SELECT, `ranges` | ok |
| cluster: `down -v` | ok |
| `kvctl` внутри `meta-1` | ok |

**Не проверялось:** опубликованный registry-образ, `docker pull`, TLS/mTLS,
`DB_ADMIN_TOKEN` end-to-end, `pgsrv`/`psql`, перезапуск и failover, backup,
Linux/NVMe performance. Это dev/demo-проверка, не SLO и не production-приёмка.
