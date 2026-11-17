# SQL за 5 минут

Короткий путь: собрать v2 KV и SQL, поднять **dev-кластер** (один meta + три
KV-узла), выполнить SQL через HTTP и, опционально, подключиться `psql`.
Это experimental P0, не production. Полный трек — [sql-newsql.md](../tracks/sql-newsql.md).

> **Dev-shortcut:** здесь один meta-узел. Он не реплицируется и держит range
> map/TSO в памяти; после рестарта TSO начинается заново. Для replicated
> meta 3/5 используйте [USER-GUIDE.md §4.2](../USER-GUIDE.md#42-metatso-3-узла).
> Не запускайте v1 (`cmd/db`) и v2 одновременно на портах 8080/9090/7001–7003.

## Требования

- Go 1.23+;
- `curl`; `jq` опционально; `psql` опционально;
- свободные порты `9090` (meta), `7001–7003` (KV), `8080` (SQL HTTP),
  `5432` (pgwire, опционально);
- macOS/Linux (Windows — WSL).

## 1. Сборка

```bash
mkdir -p bin
go build -o bin/kv2 ./cmd/kv2
go build -o bin/sqlsrv ./cmd/sqlsrv

# опционально, для psql
go build -o bin/pgsrv ./cmd/pgsrv
```

## 2. Meta (терминал 1)

```bash
./bin/kv2 -mode=meta -addr=127.0.0.1:9090 -meta-data-dir=./data/meta1
```

Ждите в логе `kv2 meta started`. Это standalone-режим без Raft-репликации.

## 3. KV-узлы (терминалы 2–4)

```bash
# терминал 2
./bin/kv2 -mode=node -id=127.0.0.1:7001 -raft-addr=127.0.0.1:7001 \
  -data-dir=./data/v2/n1 -peers=127.0.0.1:7002,127.0.0.1:7003
```

```bash
# терминал 3
./bin/kv2 -mode=node -id=127.0.0.1:7002 -raft-addr=127.0.0.1:7002 \
  -data-dir=./data/v2/n2 -peers=127.0.0.1:7001,127.0.0.1:7003
```

```bash
# терминал 4
./bin/kv2 -mode=node -id=127.0.0.1:7003 -raft-addr=127.0.0.1:7003 \
  -data-dir=./data/v2/n3 -peers=127.0.0.1:7001,127.0.0.1:7002
```

`-durability=sync` — default; не меняйте на `fast` для данных, которые нужны
после сбоя. Один из узлов напишет `raft: became leader`.

## 4. SQL HTTP-сервер (терминал 5)

```bash
./bin/sqlsrv -http-addr=:8080 -meta=127.0.0.1:9090 \
  -nodes=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003
```

Проверка:

```bash
curl -s http://127.0.0.1:8080/health
# {"status":"ok"}
```

Если `8080` занят, задайте другой порт, например `-http-addr=:8088`, и
замените порт в `curl`.

## 5. SQL через HTTP

```bash
SQL=http://127.0.0.1:8080/v1/sql

curl -s -X POST "$SQL" -H 'Content-Type: application/json' \
  -d '{"sql":"CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"}'
# {"message":"table created"}

curl -s -X POST "$SQL" -H 'Content-Type: application/json' \
  -d "{\"sql\":\"INSERT INTO users VALUES (1, 'alice')\"}"
# {"rows_affected":1,"message":"row inserted"}

curl -s -X POST "$SQL" -H 'Content-Type: application/json' \
  -d "{\"sql\":\"INSERT INTO users VALUES (2, 'bob')\"}"
# {"rows_affected":1,"message":"row inserted"}

curl -s -X POST "$SQL" -H 'Content-Type: application/json' \
  -d '{"sql":"SELECT * FROM users ORDER BY id"}'
# {"columns":["id","name"],"rows":[[1,"alice"],[2,"bob"]]}

curl -s -X POST "$SQL" -H 'Content-Type: application/json' \
  -d '{"sql":"CREATE INDEX users_name ON users(name)"}'
# {"message":"index created"}

curl -s -X POST "$SQL" -H 'Content-Type: application/json' \
  -d "{\"sql\":\"SELECT id FROM users WHERE name = 'bob'\"}"
# {"columns":["id"],"rows":[[2]]}
```

`UPDATE`/`DELETE` требуют `WHERE`; `AND/OR`, >2 JOIN, `NULL/DECIMAL/JSON`,
`pg_catalog`/JDBC не поддерживаются. Подробности — в
[sql-newsql.md](../tracks/sql-newsql.md).

## 6. psql / pgwire (опционально)

```bash
./bin/pgsrv -addr=:5432 -meta=127.0.0.1:9090 \
  -nodes=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003

psql "host=127.0.0.1 port=5432 user=postgres dbname=postgres sslmode=disable"
```

В psql:

```sql
SELECT id, name FROM users ORDER BY id;
INSERT INTO users VALUES (3, 'carol');
SELECT COUNT(*) FROM users;
\q
```

Если `5432` занят, используйте `-addr=:15432` и тот же порт в строке `psql`.
По умолчанию пароля нет. `-auth-user`/`-auth-password` требуют TLS; пароль
виден в списке процессов.

## TLS и auth: что важно

- **Defaults небезопасны:** `sqlsrv` и `pgsrv` по умолчанию plaintext и без
  auth. Не выставляйте их в сеть.
- **SQL HTTP:** `-tls-cert`/`-tls-key` включают HTTPS; `-auth-token` требует
  TLS и защищает `/v1/sql`, `/metrics`, `/debug/slo` (`/health` и `/healthz`
  публичны). Без TLS `sqlsrv` отказывается стартовать с токеном.
- **Cluster TLS:** TLS на HTTP-listener не шифрует gRPC к meta/KV. Для этого
  нужны `-tls-ca` и/или `-tls-server-name` и сертификаты кластера.
- **pgwire:** TLS — direct TLS, не PostgreSQL `SSLRequest` upgrade. Обычный
  `psql` с `sslmode=disable` работает только без пароля; `-auth-user` без TLS
  отклоняется на старте. Нет RBAC/audit.
- **Не production:** TLS/auth — опциональные механизмы, а не полная
  security-модель.

## Проверено на текущем checkout

| Проверка | Результат |
|---|---|
| `go build` для `kv2`, `sqlsrv`, `pgsrv` | ok |
| Один meta + 3 KV + `sqlsrv` | ok |
| HTTP: CREATE/INSERT/SELECT/INDEX/SELECT по индексу | ok, ответы выше |
| Canonical 3 meta + 3 KV + SQL (из USER-GUIDE) | ok, отдельный прогон |
| `psql` 13 | протокол ok через контейнерный psql на `:15432`; локального `psql` в среде проверки не было |
| TLS/Bearer на `sqlsrv` | без токена 401, с токеном 200; `/health` публичен |
| Auth без TLS | `sqlsrv` и `pgsrv` завершаются с fatal-ошибкой |

**Не проверялось:** локальный `psql`, `pgsrv` TLS, реальные CA/mTLS,
failover/restart, длительный soak, 1 ТБ и p99. Это не SLO-доказательство.

## Остановка

`Ctrl-C` в каждом терминале. Данные остаются в `./data/meta1` и
`./data/v2/n1..3`; удалить их можно вручную, когда процессы остановлены.
