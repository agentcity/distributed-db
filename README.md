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
- **Чтение** — только с лидера и только после `ReadBarrier` (проверка lease:
  лидер слышал кворум не дольше election timeout). Это даёт
  линеаризуемые чтения без отдельного ReadIndex-раунда.
- **Запись** — `Propose` возвращается после коммита на кворуме и применения к
  state machine; значения и delete едут в логе (delete — tombstone-команда).

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
Узлы: HTTP `8001..8006`, Raft gRPC `7001..7006`.
Логин/пароль по умолчанию `admin/admin`; переопределяются переменными
`DB_AUTH_USER` / `DB_AUTH_PASS` (только для демо).

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
| PUT | `/v1/kv/{key}` | сырые байты значения | `200/201`, либо `503` + `X-Leader` |
| GET | `/v1/kv/{key}` | — | `200` + значение, `404`, `503` |
| DELETE | `/v1/kv/{key}` | — | `200/204`, `503` |
| GET | `/health` | — | `200` |
| GET | `/v1/status` | — | JSON: роль, терм, лидер, индексы |
| GET | `/metrics` | — | Prometheus |

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
- линеаризуемые чтения с лидера через lease-barrier;
- KV и Raft-лог переживают рестарт процесса; WAL KV компактится.

**Не реализовано (сознательно, для следующей итерации):**
- снапшоты/компакция **Raft-лога** — лог растёт, для долгой эксплуатации нужен
  `InstallSnapshot`;
- динамическая карта шардов и ребалансировка — карта задаётся флагом;
- mTLS и проверка клиентских сертификатов; при `-tls-cert` без `-tls-ca`
  peer-клиенты используют `InsecureSkipVerify`;
- HTTP-серверы узла и координатора поднимаются по TLS, если заданы
  `-tls-cert`/`-tls-key` (клиент должен использовать `https://`);
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
