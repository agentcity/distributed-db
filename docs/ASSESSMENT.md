# Честная оценка проекта

- Дата: 2026-11-17
- Состояние: 2026-11-17
- Область: v1 (`cmd/db`), v2 KV/SQL (`cmd/kv2`, `cmd/sqlsrv`, `cmd/pgsrv`),
  edge hash cache (`cmd/edgehash`, `internal/edgeset`, `edgehttp`, `edgecache`),
  mobile scaffold (`mobile/`).

## 1. Короткий вердикт

**Это не production-СУБД.** Это сильный инженерный прототип распределённой
транзакционной БД плюс отдельный, более зрелый для своей узкой задачи
edge-кэш хешей.

По зрелости:

| Компонент | Уровень | Честная формулировка |
|---|---|---|
| v1 `cmd/db` | demo / educational | Рабочий CP-KV для демонстрации Raft и hash-шардинга |
| v2 KV (`kv2`) | pre-alpha / internal prototype | Raft, Multi-Raft, MVCC/2PC, split/merge, meta/TSO есть; production-обвязки нет |
| v2 SQL (`sqlsrv`, `pgsrv`) | experimental | SQL P0 и pgwire для маленьких данных; аналитики и полной совместимости нет |
| edge hash cache | alpha | Узкая read-only membership-задача решена; mobile/CDN не production |
| mobile SDK | scaffold | Kotlin/Swift есть, на устройствах не проверялся |

Архитектурно v2 — mini-TiKV + mini-TiDB/CockroachDB. Продуктово — **private
single-tenant metadata/config store** для внутренних задач, а не публичный
DBaaS и не замена PostgreSQL.

## 2. Что реально реализовано

### v2 KV/data plane

- Raft: выборы, лог, group commit, snapshot, InstallSnapshot.
- Multi-Raft: несколько групп в процессе, shared Pebble, range-префиксы.
- MVCC/Percolator: locks, prewrite/commit/rollback, resolver, GC safe point.
- Single-range 1PC `CommitSingle` — один Pebble batch/fsync.
- Cross-range 2PC primary-first, параллельный commit secondary.
- Range split/merge, streaming copy, auto-split/auto-merge.
- Replicated meta/TSO (Raft 3/5), client range cache, TSO batching.
- Membership: learner → voter → remove; **single-server change, без joint
  consensus**.
- Leader transfer/rebalance; chunked/gzip/resumable snapshots.
- Read barrier/lease reads с лидера; follower reads нет.
- Logical backup/restore (`cmd/kvbackup`), без PITR.
- Batch-entry: несколько `CommitSingle` в одном Raft-entry; клиентский
  `CommitBatch` (pipelining, не atomic batch).

### v2 SQL/API

- `CREATE/DROP TABLE/INDEX`, `INSERT`, `SELECT`, `UPDATE`, `DELETE`.
- `WHERE` по PK/indexed-колонке, сравнения, `BETWEEN`, `LIMIT`.
- Агрегаты, `GROUP BY`, `INNER JOIN`.
- Индексы, базовый CBO по row count, plan cache.
- Cross-range SQL после split.
- pgwire simple + extended (Parse/Bind/Execute).
- Нет: `AND/OR`, >2 JOIN, parallel/distributed join, NULL/DECIMAL/JSON/FK,
  online DDL, pg_catalog/JDBC, TLS/auth для pgwire.

### edge hash cache

- Immutable сегменты по префиксу хеша; Bloom (target FP 0.01, false negatives 0).
- Merkle inclusion + **non-membership proofs** (before/between/after).
- Manifest: Ed25519 (рекомендуется) или legacy HMAC-SHA256.
- Сегмент v2 zstd, v1 совместим.
- HTTP: TLS, Bearer auth, per-IP rate limit, Range/206, CDN headers.
- Cache: generation dirs + atomic `CURRENT`, crash-safe sync, LRU/TTL eviction,
  remote fallback.
- Mobile: Kotlin/Swift scaffold с Ed25519/Merkle/Bloom/cache policy; **не
  собран и не проверен на устройствах**, без zstd/absence/Bearer/Range.

## 3. Чем это подтверждено

- `go build ./...`, `go vet ./...`, `gofmt -l .` — чисто.
- `go test -race ./...` — все пакеты зелёные.
- Тесты: Raft/failover, snapshot catch-up, split/merge, 2PC recovery,
  Jepsen-подобные partitions/clock skew, SQL P0, pgwire, backup/restore,
  batch, edgehash FPR/proofs/tamper/cache.
- Небольшие бенчмарки на M1 Pro/loopback:
  - fast: single txn ~11.6 мс (~86 tx/s);
  - fast: parallel 10 клиентов ~1.2–2.1 мс/op (~480–820 tx/s);
  - fast: batch 256 ~0.14–0.20 мс/txn (~5–7k tx/s амортизированно,
    batch latency 36–51 мс);
  - production `sync`: ~22.6 мс single (~44 tx/s), ~5.4 мс/op
    (~185 tx/s) на 10 клиентах;
  - PostgreSQL 13 на том же железе: ~0.68 мс / 1 469 tx/s (1 клиент),
    ~1.47 мс / 6 835 tx/s (10 клиентов).

Это **не** SLO-доказательство: loopback, маленькие данные, нет p99-прогона,
нет 1 ТБ soak, нет реальной сети.

## 4. Что не доказано

1. Поведение на 1–10 ТБ: compaction, split/move, meta/TSO на десятках тысяч
   range’ей.
2. p99 и tail latency на реальной сети и NVMe.
3. Crash-consistency `SyncWrites=false`: durability state machine опирается на
   replay Raft-лога; нужен crash-тест/soak.
4. Restore drills и RPO/RTO; PITR отсутствует; remote backup lock-scan RPC
   не реализован.
5. Безопасность v2: gRPC TLS/mTLS **есть опционально** (KV/meta/Raft,
   v2client, CLI), но по умолчанию plaintext; нет RBAC/audit/encryption at
   rest; SQL HTTP TLS/Bearer и meta admin-token **есть опционально**;
   KV admin RPC без auth; pgwire — один статический пользователь;
   v1 HTTP имеет Basic Auth/TLS, edgehash — TLS/Bearer.
6. Membership safety: config в state-machine snapshot есть, но нет joint
   consensus и decommission.
7. Rolling upgrade/rollback и совместимость протоколов.
8. Multi-AZ/geo placement и capacity-aware rebalance.

## 5. Где это можно использовать

### Можно уже сейчас

1. **Учебный/исследовательский стенд** для Raft, Multi-Raft, MVCC, 2PC,
   split/merge, snapshot catch-up. Ценность — реальный код и тесты, не
   production SLA.
2. **Dev/test/CI-хранилище** с реальными сценариями failover, split/merge,
   membership changes.
3. **Внутренний metadata/config store** в закрытом контуре:
   single-region, 3 реплики, низкий QPS, некритичные данные, есть бэкап и
   on-call. Только после приёмки P0-рисков.
4. **SQL-песочница** для маленьких таблиц (10⁴–10⁵ строк), psql simple/extended,
   демонстрация SQL поверх KV.
5. **Edge hash membership** для read-only наборов:
   дедупликация, blocklists, content-addressed проверки, «видели ли объект»;
   серверы + edge/CDN + Bloom; Ed25519 обязателен для недоверенных клиентов.
6. **Embedded single-node KV** для Go-сервиса, если нужен ordered KV и
   транзакции, а не распределённый кластер (с оговорками по durability).

### Можно условно

- **Private single-tenant production** для внутренних low-criticality данных:
  только после закрытия P0 (PITR, TLS/auth, membership safety, rolling
  upgrade, SLO-прогон). Не для regulated/financial.
- **Mobile edge cache**: после device-тестов, zstd/absence/Bearer/Range,
  key pinning и background policy. Сейчас — только scaffold.

### Нельзя

- публичный DBaaS и multi-tenant;
- финансовый/медицинский/регулируемый контур;
- замена PostgreSQL «из коробки»;
- OLAP/аналитика, большие JOIN/aggregations;
- WAN/geo-distributed OLTP;
- нагрузка 10k–30k ops/s с p99 < 20 мс без отдельного SLO-прогона;
- хранение весов DeepSeek/инференс из БД;
- телефоны как узлы консенсуса или source of truth;
- динамический hash-set с удалениями без нового поколения сегментов.

## 6. С чем сравнивать

| Задача | Разумный выбор | Наш проект |
|---|---|---|
| Обычная реляционная БД | PostgreSQL | Не конкурент по SQL/аналитике |
| Распределённый транзакционный KV | TiKV/TiDB, CockroachDB | Учебный/внутренний вариант, меньше зрелости |
| AP KV с высокой записью | Cassandra/Scylla | Другая модель консистентности; не наш путь |
| Кэш/сессии | Redis | Не наш слой |
| Read-only membership на edge | Bloom + object store/CDN или edgehash | Узкая ниша, где мы полезны |
| Хранение весов модели | Object storage + local NVMe + inference engine | Не наша задача |

## 7. Главные риски

| Риск | Последствие |
|---|---|
| Нет joint consensus | смена состава при сбое может нарушить кворум |
| Membership не в snapshot | stale voter set после компакции |
| `SyncWrites=false` без crash-soak | потеря/несогласованность state machine после сбоя |
| Нет PITR | потеря данных между бэкапами |
| Нет TLS/auth в v2 | доступ к сети = доступ к данным |
| Cross-range 2PC без wait-for графа | долгие блокировки вместо диагностируемого deadlock |
| SQL planner без гистограмм | плохие планы на больших таблицах |
| Mobile scaffold | нельзя считать SDK готовым |
| CDN headers ≠ CDN | geo/edge deployment не проверен |

## 8. Что делать дальше

Детальный backlog с воротами G1–G8, порядком фаз и оценками:
[PRODUCTION-PLAN.md](PRODUCTION-PLAN.md).

### P0 для v2 (до private alpha)

1. Crash-consistency тест для `SyncWrites=false` + решение по durability.
2. Membership: joint consensus или формальный one-change протокол +
   membership snapshot + decommission.
3. PITR/continuous backup + restore drills + RPO/RTO.
4. TLS/mTLS, authn/authz, audit, encryption at rest для v2/pgwire.
5. Rolling upgrade/rollback + protocol versioning.
6. SLO-прогон 3 узла, YCSB/TPC-C-подобный, 1 ТБ soak, p99.
7. Jepsen real-network partitions/clock skew, длинный soak.
8. Helm/operator, alerts/runbooks.

### Edgehash до production

1. Mobile device/integration tests; zstd/absence/Bearer/Range в SDK.
2. Ed25519 key rotation/key id.
3. CDN multi-region, purge/versioning, signed URLs.
4. Security review TLS pinning/mobile storage.
5. Prefix-range download и zstd dictionary.

## 9. Как проверить за день

```bash
go build ./...
go vet ./...
go test -race ./...

# 3-node KV/SQL e2e
go test -race ./test -run 'TestV2|TestSQLP0' -v

# benchmarks
go test -run '^$' -bench 'BenchmarkV2Txn3Node|BenchmarkV2TxnBatch3Node' ./test

# edgehash e2e
edgehash build -in hashes.txt -out store -ed25519-key priv.pem
edgehash serve -dir store -verify-ed25519 pub.pem
edgehash lookup -server http://127.0.0.1:8080 -hash <hex> -verify-ed25519 pub.pem
```

## 10. Итог

- **Как продукт для внешних клиентов — нет.**
- **Как инженерный прототип распределённой БД — да, сильный.**
- **Как private single-tenant metadata store — только после P0.**
- **Как edge hash membership — самая готовая часть, но mobile/CDN ещё не production.**
- **Как замена PostgreSQL или хранилище весов модели — нет.**
