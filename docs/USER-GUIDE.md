# Практическое руководство: как запускать и работать

- Дата: 2026-11-17
- Состояние: experimental / internal prototype; см. [ASSESSMENT.md](ASSESSMENT.md)
- Быстрый выбор: [какой интерфейс нужен](#1-какой-интерфейс-выбрать)

Это руководство для терминала. Оно показывает реальные команды, порты,
файлы данных и ожидаемые ответы. Если что-то не запускается — сначала
смотрите [Troubleshooting](#12-troubleshooting).

## 1. Какой интерфейс выбрать

| Что нужно | Что использовать | Раздел |
|---|---|---|
| Простой KV по HTTP, быстро попробовать | v1: `cmd/db` + coordinator | [v1](#3-v1-демо-kv-за-5-минут) |
| SQL-таблица, `INSERT/SELECT` | v2: `sqlsrv` HTTP | [SQL HTTP](#6-sql-через-http) |
| `psql` / PostgreSQL-клиент | v2: `pgsrv` pgwire | [psql](#7-psql-pgwire) |
| Сырой KV v2 из терминала | `kvctl` | [kvctl](#8-kvctl-терминальный-клиент) |
| Сырой KV v2 из Go | `internal/v2client` | [Go SDK](#8a-сырой-kv-v2-из-go-advanced) |
| Бэкап/восстановление | `cmd/kvbackup` | [backup](#9-backup-и-restore) |
| Проверка огромного набора хешей | `cmd/edgehash` | [edgehash](#10-edgehash) |

**Важно:** v1 и v2 — разные кластеры и разные бинари. Не запускайте их
одновременно на одних портах (8080/9090/7001–7003).

## 2. Требования

- Go 1.23+;
- свободные порты:
  - v1: `8080` HTTP, `9090` gRPC, `7001–7003` Raft, `8001–8003` HTTP узлов;
  - v2: `9090` meta, `7001–7003` KV, `8080` SQL HTTP, `5432` pgwire;
- `curl`, `jq` (опционально), `psql` (для pgwire);
- Docker — только для compose-демо;
- macOS/Linux; Windows — через WSL.

Проверить порт:

```bash
lsof -nP -iTCP:8080 -sTCP:LISTEN
lsof -nP -iTCP:9090 -sTCP:LISTEN
```

## 3. v1 демо KV за 5 минут

v1 — учебный CP-KV: 3 Raft-узла + stateless coordinator. Хорош для того,
чтобы потрогать KV и failover; не production.

### 3.1 Запуск

```bash
cd /path/to/distributed-db
make build

# foreground: Ctrl-C остановит все процессы
./scripts/dev-cluster.sh
```

Скрипт поднимает:

- 3 узла: `127.0.0.1:7001–7003` (gRPC), HTTP `:8001–8003`;
- coordinator: HTTP `:8080`, gRPC `:9090`;
- Basic Auth `admin/admin`;
- логи в `data/logs/*.log`, PID в `data/pids/`.

Ожидаемо:

```text
ready: http://127.0.0.1:8080  (admin/admin)
logs: data/logs/*.log
```

В другом терминале:

```bash
./scripts/smoke.sh
# PASS
```

### 3.2 PUT / GET / DELETE через curl

```bash
# записать сырые байты
curl -i -u admin:admin -X PUT \
  --data-binary 'hello raft' \
  http://127.0.0.1:8080/v1/kv/user:1

# прочитать
curl -i -u admin:admin http://127.0.0.1:8080/v1/kv/user:1
# hello raft

# удалить
curl -i -u admin:admin -X DELETE http://127.0.0.1:8080/v1/kv/user:1
# 204 No Content

# после удаления
curl -i -u admin:admin http://127.0.0.1:8080/v1/kv/user:1
# 404
```

Особенности:

- `PUT` принимает **сырое тело**, не JSON;
- `GET` возвращает сырое значение;
- ключ — часть URL path; спецсимволы кодируйте (`%2F`, `%20`);
- Basic Auth защищает только `/v1/kv/*`;
- при failover возможен `503` + `X-Leader`; повторите запрос.

### 3.3 Статус и метрики

```bash
curl -s http://127.0.0.1:8080/v1/status | jq .
curl -s http://127.0.0.1:8080/metrics | head
```

`/v1/status` показывает карту шардов, лидеров и нагрузку. `/health` — liveness.

### 3.4 Docker-демо

```bash
make compose-up     # 2 шарда × 3 реплики
make smoke
make compose-down   # удалит volume'ы
```

Логин по умолчанию `admin/admin`; переопределяется `DB_AUTH_USER/DB_AUTH_PASS`.

### 3.5 Остановка

- `dev-cluster.sh`: `Ctrl-C`;
- Docker: `make compose-down`;
- вручную: kill PID из `data/pids/*.pid`.

## 4. v2 кластер: meta + KV + SQL

v2 — настоящий Multi-Raft KV с range split/merge, MVCC/2PC и SQL сверху.
Production-профиль по умолчанию `-durability=sync`.

### 4.1 Сборка

```bash
go build -o bin/kv2 ./cmd/kv2
go build -o bin/sqlsrv ./cmd/sqlsrv
go build -o bin/pgsrv ./cmd/pgsrv
go build -o bin/kvbackup ./cmd/kvbackup
go build -o bin/kvctl ./cmd/kvctl
```

### 4.2 Meta/TSO: 3 узла

Нужно 3 терминала. Данные — `./data/meta1..3`.

```bash
# терминал 1
./bin/kv2 -mode=meta -addr=127.0.0.1:9090 \
  -meta-id=127.0.0.1:9090 \
  -meta-peers=127.0.0.1:9091,127.0.0.1:9092 \
  -meta-data-dir=./data/meta1
```

```bash
# терминал 2
./bin/kv2 -mode=meta -addr=127.0.0.1:9091 \
  -meta-id=127.0.0.1:9091 \
  -meta-peers=127.0.0.1:9090,127.0.0.1:9092 \
  -meta-data-dir=./data/meta2
```

```bash
# терминал 3
./bin/kv2 -mode=meta -addr=127.0.0.1:9092 \
  -meta-id=127.0.0.1:9092 \
  -meta-peers=127.0.0.1:9090,127.0.0.1:9091 \
  -meta-data-dir=./data/meta3
```

Ждите в логах `kv2 meta started`; лидер появится после выборов.

### 4.3 KV: 3 узла

Ещё 3 терминала. Данные — `./data/v2/n1..3`.

```bash
# терминал 4
./bin/kv2 -mode=node -id=127.0.0.1:7001 \
  -raft-addr=127.0.0.1:7001 -data-dir=./data/v2/n1 \
  -peers=127.0.0.1:7002,127.0.0.1:7003
```

```bash
# терминал 5
./bin/kv2 -mode=node -id=127.0.0.1:7002 \
  -raft-addr=127.0.0.1:7002 -data-dir=./data/v2/n2 \
  -peers=127.0.0.1:7001,127.0.0.1:7003
```

```bash
# терминал 6
./bin/kv2 -mode=node -id=127.0.0.1:7003 \
  -raft-addr=127.0.0.1:7003 -data-dir=./data/v2/n3 \
  -peers=127.0.0.1:7001,127.0.0.1:7002
```

Проверка, что лидер есть:

```bash
# Status доступен через gRPC; быстрый способ — SQL ниже.
# Если используете grpcurl:
grpcurl -plaintext -d '{"range_id":1}' 127.0.0.1:7001 \
  dbv2.KVService/Status
```

> `grpcurl` не обязателен; SQL/pgwire сами редиректят к лидеру.

Флаг `-durability`:

- `sync` (по умолчанию) — fsync state machine, production;
- `fast` — быстрее, но durability только через replay Raft-лога;
  не production до отдельного soak.

### 4.4 SQL HTTP-сервер

```bash
./bin/sqlsrv -http-addr=:8080 \
  -meta=127.0.0.1:9090 \
  -nodes=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003
```

Проверка:

```bash
curl -s http://127.0.0.1:8080/health
# {"status":"ok"}
```

## 5. SQL: первые команды

SQL P0: `INT`/`TEXT`, PK обязателен, вторичные индексы, `SELECT` с
`WHERE/ORDER BY/LIMIT/GROUP BY/INNER JOIN`, `UPDATE/DELETE`.

### 5.1 HTTP

```bash
SQL='http://127.0.0.1:8080/v1/sql'

curl -s -X POST "$SQL" -H 'Content-Type: application/json' \
  -d '{"sql":"CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"}'

curl -s -X POST "$SQL" -H 'Content-Type: application/json' \
  -d "{\"sql\":\"INSERT INTO users VALUES (1, 'alice')\"}"

curl -s -X POST "$SQL" -H 'Content-Type: application/json' \
  -d "{\"sql\":\"INSERT INTO users VALUES (2, 'bob')\"}"

curl -s -X POST "$SQL" -H 'Content-Type: application/json' \
  -d '{"sql":"SELECT * FROM users ORDER BY id"}'
```

Пример ответа:

```json
{"columns":["id","name"],"rows":[[1,"alice"],[2,"bob"]]}
```

`INSERT/UPDATE/DELETE` возвращают `rows_affected`/`message`.

### 5.2 Индекс и выборка

```bash
curl -s -X POST "$SQL" -d '{"sql":"CREATE INDEX users_name ON users(name)"}'
curl -s -X POST "$SQL" -d "{\"sql\":\"SELECT id FROM users WHERE name = 'bob'\"}"
```

### 5.3 UPDATE / DELETE

```bash
curl -s -X POST "$SQL" -d \
  "{\"sql\":\"UPDATE users SET name = 'bobby' WHERE id = 2\"}"

curl -s -X POST "$SQL" -d \
  '{"sql":"DELETE FROM users WHERE id = 1"}'
```

### 5.4 Ограничения SQL

- один statement на HTTP-запрос;
- нет `AND/OR`, >2 JOIN, LEFT/RIGHT JOIN, NULL/DECIMAL/JSON/FK;
- нет online DDL;
- pg_catalog/JDBC — нет;
- сложные планы на больших данных не гарантированы.

## 6. SQL через HTTP: шпаргалка

```bash
# health
curl -s http://127.0.0.1:8080/health

# любой SQL
curl -s -X POST http://127.0.0.1:8080/v1/sql \
  -H 'Content-Type: application/json' \
  -d '{"sql":"SELECT COUNT(*) FROM users"}'

# метрики
curl -s http://127.0.0.1:8080/metrics | head
curl -s http://127.0.0.1:8080/debug/slo | jq .
```

## 7. psql (pgwire)

```bash
./bin/pgsrv -addr=:5432 \
  -meta=127.0.0.1:9090 \
  -nodes=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 \
  -metrics-addr=:9091
```

Подключение:

```bash
psql "host=127.0.0.1 port=5432 user=postgres dbname=postgres sslmode=disable"
```

Пример сессии:

```sql
CREATE TABLE users (id INT PRIMARY KEY, name TEXT);
INSERT INTO users VALUES (1, 'alice');
INSERT INTO users VALUES (2, 'bob');
SELECT * FROM users ORDER BY id;
\q
```

Поддерживается simple + extended protocol (Parse/Bind/Execute), INT/TEXT
параметры. Пароля по умолчанию нет.

### TLS в pgwire

Сейчас TLS — **direct TLS** (`tls.NewListener`), а не PostgreSQL `SSLRequest`
upgrade. Поэтому:

- обычный `psql` с `sslmode=disable` работает;
- `sslmode=require` с libpq может не сработать: libpq сначала шлёт SSLRequest;
- нужен клиент с direct TLS или TLS-терминатор перед pgsrv.

Это известное ограничение; см. [PRODUCTION-PLAN.md](PRODUCTION-PLAN.md).

## 8. kvctl: терминальный клиент

`kvctl` — полноценный CLI для v2 KV: CRUD, scan, атомарные транзакции,
interactive shell, JSON/hex/raw-вывод.

### 8.1 Сборка и конфиг

```bash
go build -o bin/kvctl ./cmd/kvctl

# адреса можно задать флагами
./bin/kvctl -meta 127.0.0.1:9090 \
  -nodes 127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 get user:1

# или через env
export KVCTL_META=127.0.0.1:9090
export KVCTL_NODES=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003
./bin/kvctl get user:1
```

Глобальные флаги ставятся **до** команды:

```text
-meta ADDR                meta/TSO gRPC
-nodes A,B                KV node gRPC
-timeout 10s              таймаут команды
-format table|json|raw    вывод
-hex                      ключи/значения в hex
-quiet                    без OK
```

### 8.2 CRUD

```bash
./bin/kvctl put user:1 alice
# OK

./bin/kvctl get user:1
# user:1	alice

./bin/kvctl -format raw get user:1
# alice

./bin/kvctl delete user:1
# OK
```

Значение с пробелами — в кавычках:

```bash
./bin/kvctl put greeting "hello world"
```

Значение из stdin или файла:

```bash
echo -n "hello" | ./bin/kvctl put greeting -
./bin/kvctl put config @./config.json
```

### 8.3 Scan

```bash
# все ключи с префиксом
./bin/kvctl scan -prefix user:

# диапазон [start, end), лимит
./bin/kvctl scan -start user: -end "user;" -limit 100

# JSON для скриптов
./bin/kvctl -format json scan -prefix user:
# [{"key":"user:1","value":"alice"}, ...]
```

`-prefix` нельзя смешивать с `-start/-end`; `-limit` 1..10000.

### 8.4 Атомарная транзакция

`txn` читает операции со stdin или `-file` и коммитит их одной транзакцией:

```bash
printf 'put a 1\nput b 2\ndelete old\n' | ./bin/kvctl txn
# committed 3 operations
```

Файл:

```bash
cat > ops.txt <<'OPS'
put user:1 alice
put user:2 bob
delete user:3
OPS
./bin/kvctl txn -file ops.txt
```

Ошибка в любой строке → транзакция не коммитится. Поддерживаются `put`/`set`
и `delete`/`del`; комментарии `#`.

### 8.5 Shell

```bash
./bin/kvctl shell
kvctl> put a 1
OK
kvctl> get a
a	1
kvctl> scan -prefix ""
kvctl> quit
```

В shell команды те же (`put/get/delete/scan`), но без глобальных флагов.

### 8.6 Hex и бинарные данные

```bash
./bin/kvctl -hex put 6b 76        # key "k", value "v"
./bin/kvctl -hex -format raw get 6b
# 76
```

Без `-hex` CLI печатает текст; управляющие байты escaped как `\xNN`.

### 8.7 Ping и коды выхода

```bash
./bin/kvctl ping
# pong
```

- `0` — успех;
- `1` — операционная ошибка, ключ не найден;
- `2` — ошибка использования/флагов.

`get` отсутствующего ключа печатает `key ... not found` в stderr и возвращает 1.

### 8.8 Админ-команды

Эти команды работают **только с meta**, `-nodes` не нужен:

```bash
export KVCTL_META=127.0.0.1:9090

# список range'ей
./bin/kvctl ranges
# ID  START  END  LEADER  REPLICAS  VERSION

# JSON
./bin/kvctl -format json ranges

# состояние meta/range map
./bin/kvctl status
# META_LEADER ... TERM ... RANGES ... LEADER ...
```

`split` и `merge` меняют range map — это админские операции:

```bash
# разделить range 1 по ключу m
./bin/kvctl split 1 m
# new range id: 2

# слить соседние range 1 и 2
./bin/kvctl merge 1 2
# OK
```

Сервер проверяет границы, соседство и реплики; ошибки выводятся как есть.
Перед `split/merge` сделайте backup и не выполняйте их под активной записью
без понимания последствий.

Backup:

```bash
./bin/kvctl backup -out ./backup   -chunk-keys 1000 -chunk-bytes 1048576
# backup complete: ./backup
# snapshot_ts=... keys=... bytes=... ranges=...
```

`-out` должен не существовать; backup логический, без PITR. Для verify/restore
используйте `cmd/kvbackup` (раздел 9).

`ranges/status/split/merge` не требуют `-nodes`; `backup` требует, потому что
читает данные.

### 8.9 TLS для gRPC

TLS/mTLS для KV/meta/Raft включается флагами; без них — plaintext (dev).

Клиент `kvctl`:

```bash
./bin/kvctl   -tls-ca ca.pem   -tls-cert client.pem -tls-key client.key   -tls-server-name kv-node   -meta 127.0.0.1:9090   -nodes 127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003   get user:1
```

Сервер `kv2` (meta или node):

```bash
./bin/kv2 -mode=node ...   -tls-cert node.crt -tls-key node.key   -tls-client-ca ca.pem   -tls-ca ca.pem -tls-server-name kv-node
```

- `-tls-cert/-tls-key` — identity узла; для mTLS сертификат должен иметь
  `ServerAuth` и `ClientAuth` EKU (он же используется для peer dials).
- `-tls-client-ca` — включает требование клиентского сертификата.
- `-tls-ca`/`-tls-server-name` — проверка серверов при исходящих dials.
- `sqlsrv`/`pgsrv`/`kvctl` поддерживают `-tls-ca`, `-tls-cert`, `-tls-key`,
  `-tls-server-name` для cluster gRPC.
- `kvbackup` пока ходит plaintext — известный gap.

## 8a. Сырой KV v2 из Go (advanced)

Если нужен доступ из кода, используйте `internal/v2client`. Программа должна
лежать внутри репозитория (пакет internal).

```go
package main

import (
	"context"
	"fmt"
	"log"

	"distributed-db/internal/v2client"
)

func main() {
	ctx := context.Background()
	c, err := v2client.New(
		"127.0.0.1:9090",
		[]string{"127.0.0.1:7001", "127.0.0.1:7002", "127.0.0.1:7003"},
	)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	tx, err := c.Begin(ctx)
	if err != nil {
		log.Fatal(err)
	}
	tx.Set([]byte("user:1"), []byte("alice"))
	if err := tx.Commit(ctx); err != nil {
		log.Fatal(err)
	}

	read, _ := c.Begin(ctx)
	v, err := read.Get(ctx, []byte("user:1"))
	if err != nil {
		log.Fatal(err)
	}
	_ = read.Rollback(ctx)
	fmt.Printf("user:1 = %s\n", v)
}
```

```bash
go run ./cmd/kvdemo
# user:1 = alice
```

## 9. Backup и restore

```bash
./bin/kvbackup backup \
  -meta=127.0.0.1:9090 \
  -nodes=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 \
  -out=./backup

./bin/kvbackup verify -in=./backup

# restore в тот же/другой кластер
./bin/kvbackup restore \
  -meta=127.0.0.1:9090 \
  -nodes=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 \
  -in=./backup
```

Ограничения:

- backup **логический**, point-in-time на одном TSO;
- PITR/непрерывного архива нет;
- restore перезаписывает ключи из backup, но **не удаляет** лишние;
- останавливайте писателей перед restore;
- backup не заменяет restore drill.

## 10. Edgehash

Read-only набор хешей: Bloom + Merkle + Ed25519.

### 10.1 Сборка и ключи

```bash
go build -o bin/edgehash ./cmd/edgehash

# Ed25519 ключи (PEM PKCS#8 / SPKI)
openssl genpkey -algorithm ED25519 -out priv.pem
openssl pkey -in priv.pem -pubout -out pub.pem

# файл хешей: один SHA-256 hex на строку
# Linux
sha256sum /path/to/file | awk '{print $1}' > hashes.txt
# macOS
shasum -a 256 /path/to/file | awk '{print $1}' > hashes.txt
```

### 10.2 Build / serve / lookup

```bash
./bin/edgehash build -in hashes.txt -out ./edge-store \
  -ed25519-key priv.pem -compression auto

./bin/edgehash serve -dir ./edge-store -addr :8080 \
  -verify-ed25519 pub.pem

# в другом терминале
H=$(sha256sum /path/to/file | awk '{print $1}')
./bin/edgehash lookup -server http://127.0.0.1:8080 \
  -hash "$H" -verify-ed25519 pub.pem
# present: true / proof: verified
```

### 10.3 Edge cache

```bash
./bin/edgehash sync -server http://127.0.0.1:8080 \
  -dir ./edge-cache -verify-ed25519 pub.pem \
  -max-bytes 268435456 -ttl 168h
```

Опции production:

```bash
./bin/edgehash serve -dir ./edge-store -addr :8443 \
  -verify-ed25519 pub.pem \
  -tls-cert server.crt -tls-key server.key \
  -auth-token "$TOKEN" -rate-limit 100 -rate-burst 200
```

Ограничения: mobile SDK — scaffold; CDN headers есть, geo-CDN не развёрнут;
stale Bloom может дать false negative; удаление — только новое поколение.

## 11. Где данные, логи и метрики

| Что | Где |
|---|---|
| v1 узлы | `./data/node1..3` |
| v1 логи | `./data/logs/*.log` |
| v1 PID | `./data/pids/*.pid` |
| v2 meta | `./data/meta1..3` |
| v2 KV | `./data/v2/n1..3` |
| v2 SQL/pgwire | данных нет, только клиент к KV |
| backup | `./backup` |
| edgehash store | `./edge-store` |
| edgehash cache | `./edge-cache` |

Метрики:

```bash
curl -s http://127.0.0.1:8080/metrics | head
curl -s http://127.0.0.1:8080/debug/slo | jq .
curl -s http://127.0.0.1:9091/metrics | head   # pgsrv -metrics-addr
```

## 12. Troubleshooting

### `bind: address already in use`

```bash
lsof -nP -iTCP:8080 -sTCP:LISTEN
lsof -nP -iTCP:9090 -sTCP:LISTEN
kill <PID>
```

### `no leader` / запросы висят

- все 3 узла запущены?
- `-peers` у каждого узла содержит **других**, не себя;
- data dirs разные;
- подождите 1–3 секунды после старта;
- проверьте логи.

### `401 Unauthorized`

v1: передайте `-u admin:admin` (или свои `-auth-user/-auth-pass`).
v2 SQL HTTP auth нет; pgwire — только если задан `-auth-user` + TLS.

### `503` + `X-Leader`

Лидер сменился. Повторите запрос; клиент/coordinator обычно ретраит сам.

### SQL `400`

- один statement на запрос;
- проверьте синтаксис P0;
- `AND/OR` и сложные JOIN не поддерживаются.

### `psql` не подключается

- `pgsrv` слушает `:5432`?
- `-nodes`/`-meta` корректны?
- для локального plain: `sslmode=disable`;
- TLS direct не совместим с libpq SSLRequest upgrade.

### Старые данные после смены layout

v2 shared Pebble не читает старый per-range layout `db/range-<id>`.
Для dev удалите `./data` (это удалит данные).

### Edgehash `signature mismatch`

- `-verify-ed25519` должен соответствовать ключу, которым делали build;
- HMAC legacy `-key` и Ed25519 не смешиваются;
- проверьте, что serve смотрит в тот же store.

## 13. Остановка и очистка

```bash
# v1 foreground
Ctrl-C

# Docker
make compose-down

# v2: Ctrl-C в каждом терминале
# или найдите PID:
pgrep -af 'bin/(kv2|sqlsrv|pgsrv)'
```

Dev-очистка (удалит данные!):

```bash
rm -rf ./data ./bin ./backup ./edge-store ./edge-cache
```

## 14. Ограничения, о которых надо знать

- v2 — pre-alpha/internal prototype, не production;
- v2 gRPC/meta без TLS; pgwire TLS — direct, не libpq upgrade;
- нет PITR, joint consensus, RBAC/audit/encryption;
- SQL P0, не аналитика;
- 1 ТБ SLO не подтверждён;
- edgehash mobile — scaffold, CDN не развёрнут;
- не используйте для regulated/financial данных.

Полная оценка: [ASSESSMENT.md](ASSESSMENT.md).
Что нужно до production: [PRODUCTION-PLAN.md](PRODUCTION-PLAN.md).

## 15. Шпаргалка команд

```bash
# v1
make build
./scripts/dev-cluster.sh
./scripts/smoke.sh
curl -u admin:admin -X PUT --data-binary 'v' localhost:8080/v1/kv/k
curl -u admin:admin localhost:8080/v1/kv/k
curl -u admin:admin -X DELETE localhost:8080/v1/kv/k
curl -s localhost:8080/v1/status | jq .

# v2 build
go build -o bin/kv2 ./cmd/kv2
go build -o bin/kvctl ./cmd/kvctl
go build -o bin/sqlsrv ./cmd/sqlsrv
go build -o bin/pgsrv ./cmd/pgsrv
go build -o bin/kvbackup ./cmd/kvbackup
go build -o bin/kvctl ./cmd/kvctl

# kvctl
./bin/kvctl -nodes 127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 put a 1
./bin/kvctl -nodes 127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 get a

# SQL HTTP (таблица users должна существовать)
curl -s -X POST localhost:8080/v1/sql \
  -H 'Content-Type: application/json' \
  -d '{"sql":"SELECT COUNT(*) FROM users"}'

# psql
psql "host=127.0.0.1 port=5432 user=postgres dbname=postgres sslmode=disable"

# backup
./bin/kvbackup backup -meta=127.0.0.1:9090 \
  -nodes=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 \
  -out=./backup

# edgehash
./bin/edgehash build -in hashes.txt -out ./edge-store -ed25519-key priv.pem
./bin/edgehash serve -dir ./edge-store -addr :8080 -verify-ed25519 pub.pem
./bin/edgehash lookup -server http://127.0.0.1:8080 -hash "$H" \
  -verify-ed25519 pub.pem
```
