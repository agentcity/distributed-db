# distributed-db

Шардированное key/value хранилище на Raft: несколько независимых Raft-групп
(по одной на шард), координатор как единая точка входа для клиентов,
персистентный лог и WAL состояния.

> Это учебно-демонстрационный проект, а не production-СУБД. Ограничения честно
> перечислены в конце; для реальной эксплуатации нужны снапшоты, динамическая
> карта шардов и нормальная TLS-инфраструктура.

## Что внутри

```
клиент ──HTTP──▶ координатор ──gRPC──▶ лидер шарда
                                         │
                                         ├── Raft: выборы, лог, кворум
                                         └── KV: map + WAL (fsync)
```

- **Узел** — член ровно одного шарда. Держит Raft-лог и состояние KV на диске,
  принимает Raft-RPC (`RequestVote`, `AppendEntries`) и клиентские
  `Get/Put/Delete/Status` по gRPC, плюс HTTP API для отладки.
- **Шард** — независимая Raft-группа из N реплик (в демо N=3). Лидера выбирает
  сам Raft; координатор только узнаёт его через `Status`.
- **Координатор** — stateless-роутер: `xxhash(key) % len(shards)` выбирает шард,
  затем запрос проксируется лидеру с retry при смене лидера.
- **Чтение** — только с лидера и только после `ReadBarrier`: проверка lease
  (лидер слышал кворум не дольше election timeout) плюс ожидание, пока
  state machine применит записи до commitIndex. Это даёт линеаризуемые чтения
  без отдельного ReadIndex-раунда и без устаревших ответов сразу после failover.
- **Запись** — только через лидера: `Propose` возвращается после коммита на
  кворуме и применения к state machine; значения и delete едут в логе
  (delete — tombstone-команда). Follower отвечает `503` + `X-Leader`,
  координатор повторяет запрос у лидера. Прямой записи в локальный KV в обход
  Raft нет.

## Куда развивается проект (v2)

Текущий кластер — **v1**: рабочий CP-KV для демо и проверки Raft. Рядом
строится **v2**: Pebble LSM, MVCC/2PC, Multi-Raft, range-шардирование,
metadata/TSO и smart client; поверх него — SQL-слой. План, SLO и границы
этапов: [docs/ROADMAP.md](docs/ROADMAP.md), контракты пакетов:
[docs/CONTRACTS-V2.md](docs/CONTRACTS-V2.md), решения:
[docs/adr/](docs/adr/README.md).

### v2: single-range (experimental)

v2 уже умеет одну Raft-группу с MVCC и транзакциями. Отдельный бинарь:

```bash
go build -o bin/kv2 ./cmd/kv2

# meta/TSO: один узел
./bin/kv2 -mode=meta -addr=:9090

# meta/TSO: replicated Raft-кластер (3 узла, каждый в своём терминале)
./bin/kv2 -mode=meta -addr=127.0.0.1:9090 -meta-id=127.0.0.1:9090 \
  -meta-peers=127.0.0.1:9091,127.0.0.1:9092 -meta-data-dir=./data/meta1
./bin/kv2 -mode=meta -addr=127.0.0.1:9091 -meta-id=127.0.0.1:9091 \
  -meta-peers=127.0.0.1:9090,127.0.0.1:9092 -meta-data-dir=./data/meta2
./bin/kv2 -mode=meta -addr=127.0.0.1:9092 -meta-id=127.0.0.1:9092 \
  -meta-peers=127.0.0.1:9090,127.0.0.1:9091 -meta-data-dir=./data/meta3

# три узла (в разных терминалах)
./bin/kv2 -mode=node -id=127.0.0.1:7001 -raft-addr=127.0.0.1:7001 \
  -data-dir=./data/v2/n1 -peers=127.0.0.1:7002,127.0.0.1:7003
./bin/kv2 -mode=node -id=127.0.0.1:7002 -raft-addr=127.0.0.1:7002 \
  -data-dir=./data/v2/n2 -peers=127.0.0.1:7001,127.0.0.1:7003
./bin/kv2 -mode=node -id=127.0.0.1:7003 -raft-addr=127.0.0.1:7003 \
  -data-dir=./data/v2/n3 -peers=127.0.0.1:7001,127.0.0.1:7002
```

Клиент v2 — Go-пакет `internal/v2client`; e2e-проверка:

```bash
go test -race ./test -run TestV2SingleRangeCluster -v
```

Есть 2 диапазона × 3 реплики: meta отдаёт `GetRange/ListRanges`, клиент
маршрутизирует ключ по range ID и обновляет лидера при failover. Meta/TSO
теперь можно запустить как Raft-кластер из 3 узлов (`-mode=meta` + peers).
`kvserver.Config.Ranges` host-ит несколько Raft-групп в одном процессе; для
одного host доступны `SplitRange`/`MergeRanges` с реальным переносом MVCC-
записей (`TestV2RangeHostSplitMerge`).

Split теперь умеет распределённый режим: `KVService.SplitRange` реплицирует
prepare, замораживает все реплики и создаёт новую Raft-группу на каждой;
meta обновляется после успеха. Merge пока host-local, epoch/fencing — дальше. Транзакции
поддерживают `CheckTxnStatus/ResolveLock`, а `Get` сам разрешает
committed/rolled-back/expired locks.

Пока нет: Multi-Raft host внутри одного процесса, split/merge с переносом
данных и SQL-слой. Snapshot catch-up, restart из snapshot и gRPC-бенчмарк
уже есть.

### SQL P0 поверх KV (experimental)

Минимальный SQL-слой: `CREATE/DROP TABLE`, `CREATE/DROP INDEX`, `INSERT`,
`SELECT` (projection, `WHERE` по PK или indexed-колонке, `LIMIT`), `UPDATE`,
`DELETE` (включая multi-row по indexed-колонке). Каталог, строки и индексы
лежат в KV, каждая команда — транзакция.

```bash
go build -o bin/sqlsrv ./cmd/sqlsrv
./bin/sqlsrv -http-addr=:8080 -meta=127.0.0.1:9090 \
  -nodes=127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003

curl -s -X POST localhost:8080/v1/sql \
  -d '{"sql":"CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"}'
curl -s -X POST localhost:8080/v1/sql \
  -d "{\"sql\":\"INSERT INTO users VALUES (1, 'alice')\"}"
curl -s -X POST localhost:8080/v1/sql \
  -d '{"sql":"SELECT * FROM users"}'
```

P0 не умеет JOIN, aggregates и pgwire; это следующие фазы ADR 0004. E2E: `go test -race ./test -run TestSQLP0OnKV -v`.

## Быстрый старт (Docker, 2 шарда × 3 реплики)

```bash
make compose-up          # docker compose up -d --build
make smoke               # PUT/GET/DELETE через координатор
# или:
curl -u admin:admin -X PUT --data-binary 'hello' http://127.0.0.1:8080/v1/kv/foo
curl -u admin:admin http://127.0.0.1:8080/v1/kv/foo
curl -u admin:admin -X DELETE http://127.0.0.1:8080/v1/kv/foo
curl -s http://127.0.0.1:8080/v1/status | jq .
```

Координатор: `http://127.0.0.1:8080` (HTTP) и `:9090` (gRPC).
Узлы: HTTP `8001..8006`, Raft gRPC `7001..7006` (наружу опубликованы для отладки).
В compose логин/пароль по умолчанию `admin/admin` и переопределяются
переменными `DB_AUTH_USER` / `DB_AUTH_PASS`; в `make run-*` они захардкожены.
При пустых `-auth-user`/`-auth-pass` Basic Auth выключен — так делать можно
только локально.

## Локально без Docker (3 узла + координатор)

```bash
make build
./scripts/dev-cluster.sh        # поднимет 3 узла и координатор, Ctrl-C остановит
# в другом терминале:
./scripts/smoke.sh
```

Или вручную, в четырёх терминалах:

```bash
make run-node1
make run-node2
make run-node3
make run-coordinator
```

Данные лежат в `./data/node*`, логи — в `./data/logs/`.

## HTTP API

Все KV-запросы требуют Basic Auth, если задан `-auth-user`.

| Метод | Путь | Тело | Ответ |
|---|---|---|---|
| PUT | `/v1/kv/{key}` | сырые байты значения | `200`, `413`, `429`, `503` + `X-Leader` |
| GET | `/v1/kv/{key}` | — | `200` + значение, `404`, `429`, `503` |
| DELETE | `/v1/kv/{key}` | — | `200` у узла / `204` у координатора, `503` |
| GET | `/health` | — | `200` + JSON `{"status":"ok"}` |
| GET | `/v1/status` | — | у узла: роль/терм/лидер/индексы; у координатора: карта шардов, лидеры, нагрузки |
| GET | `/metrics` | — | Prometheus |

HTTP-серверы узла и координатора поднимаются по TLS, если заданы
`-tls-cert`/`-tls-key`; клиент тогда должен использовать `https://`.

Ошибки — JSON `{"error":"...","leader":"..."}`. При `429` сработал rate limit.
Полная схема координатора: [`docs/openapi.yaml`](docs/openapi.yaml).

## CLI

```text
db node -id <addr> -shard <name> -raft-addr :7001 -http-addr :8001 \
        -peers addr2,addr3 -data-dir ./data/node1 \
        [-coordinator addr] [-auth-user u -auth-pass p] \
        [-tls-cert f -tls-key f -tls-ca f] \
        [-rate-limit 100 -rate-burst 200] [-log-level info] [-log-format json]

db coordinator -http-addr :8080 -grpc-addr :9090 \
        -shards "shard-1=n1:7001,n2:7001;shard-2=n3:7001,n4:7001" \
        [-auth-user u -auth-pass p] [-tls-cert f -tls-key f -tls-ca f] \
        [-rate-limit 1000 -rate-burst 2000] [-log-level info] [-log-format json]
```

`-id` по умолчанию равен `-raft-addr`; адреса в `-peers` и `-shards` — это
именно id/адреса Raft-узлов. Порт `-raft-addr` должен совпадать с адресом,
который видят другие узлы (в compose это `node1:7001`).

## Гарантии и ограничения

**Гарантируется:**
- выборы лидера по строгому большинству, один лидер на терм;
- `term`/`votedFor` и записи лога персистятся до подтверждения;
- подтверждённая запись переживает падение лидера (коммит только по кворуму);
- delete реплицируется как команда, а не как локальное удаление;
- линеаризуемые чтения с лидера через lease-barrier + ожидание apply;
- KV и Raft-лог переживают рестарт процесса; WAL KV компактится;
- HTTP и gRPC могут работать по TLS; Basic Auth на `/v1/kv/*`,
  глобальный rate limit на все HTTP-запросы.

**Не реализовано (сознательно, для следующей итерации):**
- снапшоты/компакция **Raft-лога** — лог растёт, для долгой эксплуатации нужен
  `InstallSnapshot`;
- динамическая карта шардов и ребалансировка — карта задаётся флагом;
- mTLS и проверка клиентских сертификатов; при `-tls-cert` без `-tls-ca`
  peer-клиенты используют `InsecureSkipVerify`;
- транзакции, TTL, вторичные индексы;
- автоматическое восстановление кворума шарда после потери большинства.

## Тесты

```bash
make test        # go test -race ./...
make vet
make test-short  # без интеграционных тестов
```

- `internal/raft` — выборы, репликация, failover, потеря кворума, конфликт
  логов, рестарт из WAL; тесты используют in-memory транспорт.
- `internal/kv` — WAL, рестарт, torn tail, конкурентный доступ.
- `test/` — реальные gRPC/HTTP-серверы: 2 шарда × 3 реплики, маршрутизация
  координатора, убийство лидера и сохранность подтверждённой записи.

## Структура

```
cmd/db/               единый бинарь (node | coordinator)
internal/model/       Entry / CommandType
internal/raft/        консенсус-ядро, файловый WAL, тесты
internal/rafttest/    in-memory сеть для тестов
internal/kv/          state machine + WAL
internal/pb/          сгенерированный gRPC-код
internal/node/        gRPC/HTTP-адаптеры узла
internal/coordinator/ роутер, карта шардов, gRPC/HTTP
test/                 интеграционные тесты
docs/                 архитектура и OpenAPI
ai/tasks/             карточки задач 0001–0003
scripts/              dev-cluster.sh и smoke.sh
```

Проектные контракты и инварианты: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## Разработка

```bash
make proto       # требует protoc + protoc-gen-go + protoc-gen-go-grpc
make fmt
make build
```

CI (`.github/workflows/ci.yml`) гоняет `go vet`, `go test -race`, `go build`
и сборку Docker-образа.

Задачи проекта лежат в `ai/tasks/NNNN-*.md`; один коммит закрывает одну задачу
и ссылается на неё строкой `Задача: NNNN`.
