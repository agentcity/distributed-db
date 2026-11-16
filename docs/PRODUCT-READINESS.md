# Продуктовая оценка и путь к production

- Дата: 2026-11-16
- Состояние кода: HEAD `13c03c2`
- Область: v1 (`cmd/db`), v2 (`cmd/kv2`, `cmd/sqlsrv`, `cmd/pgsrv`,
  `cmd/kvbackup`, `cmd/metricsrv`), edge hash cache (`cmd/edgehash`) и
  mobile scaffold.
- Полная оценка и применимость: [ASSESSMENT.md](ASSESSMENT.md).
- Что реализовать до production: [PRODUCTION-PLAN.md](PRODUCTION-PLAN.md).

## 1. Короткий вердикт

Это **не production-СУБД** и пока не продукт для внешних клиентов. Это
**сильный инженерный прототип распределённой транзакционной БД**: Raft,
Multi-Raft, MVCC/Percolator, range split/merge, replicated meta/TSO,
dynamic membership, chunked snapshots, SQL P0 и pgwire.

По зрелости — **pre-alpha / internal prototype**. По архитектуре — mini-TiKV +
mini-TiDB/CockroachDB. По применимости — private single-tenant хранилище для
внутренних задач с низкими требованиями к SLA, а не публичный DBaaS.

Отдельно: **edge hash cache** (`cmd/edgehash`) — более узкий, но более
готовый продукт для read-only membership (Bloom + Merkle + Ed25519). Mobile
SDK — scaffold, не production.

## 2. Что уже есть

### KV/data plane

| Возможность | Статус | Комментарий |
|---|---|---|
| Raft: выборы, лог, snapshot, InstallSnapshot | есть | group commit, chunked snapshot |
| Multi-Raft: N групп в процессе | есть | shared Pebble, range-префиксы |
| MVCC/Percolator 2PC | есть | locks, resolver, GC safe point |
| Single-range 1PC | есть | `CommitSingle` |
| Cross-range 2PC | есть, MVP | primary-first, recovery, без wait-for графа |
| Range split/merge | есть | streaming copy, auto-split/auto-merge |
| Replicated meta/TSO | есть | Raft 3/5, TSO batch на клиенте |
| Dynamic membership | есть, MVP | learner→voter→remove; **без joint consensus** |
| Leader transfer/rebalance | есть | TransferLeader/TimeoutNow |
| Snapshots | есть | chunked, gzip, resume по offset |
| Backup/restore | есть, логический | `cmd/kvbackup`, без PITR |
| Read barrier / lease reads | есть | линеаризуемые чтения с лидера |
| Follower reads | нет | |
| Batch entry / `CommitBatch` | есть | несколько txn в одном Raft-entry; не atomic batch |
| TLS/mTLS, auth, audit (v2) | **нет** | pgwire без TLS/паролей |

### SQL/API

| Возможность | Статус |
|---|---|
| CREATE/DROP TABLE/INDEX, INSERT/SELECT/UPDATE/DELETE | есть |
| WHERE: `= != < <= > >= BETWEEN` | есть |
| GROUP BY, aggregates, INNER JOIN | есть |
| ORDER BY/LIMIT, index selection, CBO по row count, plan cache | есть, базовый |
| Cross-range SQL после split | есть |
| pgwire simple + extended (Parse/Bind/Execute) | есть |
| JDBC/pgx, TLS/auth, pg_catalog | нет |
| AND/OR, >2 JOIN, parallel/distributed join | нет |
| NULL/DECIMAL/JSON, FK, online DDL | нет |

### Эксплуатация

| Возможность | Статус |
|---|---|
| Prometheus-метрики KV/SQL/pgwire, SLO p50/p95/p99 | есть |
| `/metrics`, `/debug/slo` в sqlsrv/pgsrv | есть |
| Jepsen kill/restart, partitions, clock skew (client-side) | есть, opt-in |
| TLS/mTLS, authn/authz, audit | **нет** (v1 HTTP — Basic/TLS; v2 — нет; edgehash — TLS/Bearer) |
| Multi-tenancy, quotas, rate limits | **нет** (edgehash — per-IP rate limit) |
| PITR/continuous backup, restore drills | **нет** |
| Helm/operator, rolling upgrade/rollback | **нет** |
| Encryption at rest, secrets management | **нет** |
| Multi-AZ/geo placement policy | **нет** (ручной placement) |
| Capacity planning/decommission | частично, без политики |

## 3. Где проект может использоваться уже сейчас

### Подходит

1. **Внутренний metadata/config store** для сервисов: небольшой объём,
   single-region, 3 реплики, low QPS; ценность — транзакции и range scans.
2. **Feature flags / service registry / coordination data** внутри закрытого
   контура, где нет требований к публичному SLA.
3. **Dev/test/CI-хранилище** с реальными Raft-сценариями: split/merge,
   failover, snapshot catch-up, membership changes.
4. **Учебный/исследовательский стенд** для Raft, MVCC, 2PC, range scheduling.
5. **Embedded single-node KV** для Go-сервиса, если нужен ordered KV и
   транзакции, а не распределённый кластер.
6. **SQL-песочница** для небольших таблиц (10⁴–10⁵ строк), psql simple/extended,
   аналитика на маленьких данных.

### Edge hash cache

Подходит для read-only membership: дедупликация, blocklists, проверка
«видели ли объект», content-addressed наборы. Есть Bloom, Merkle,
non-membership proofs, Ed25519, zstd, TLS/Bearer/rate limit, CDN headers,
crash-atomic cache. Mobile SDK — scaffold: не собран и не проверен на
устройствах; нет zstd/absence/Bearer/Range.

### Не подходит

- публичный DBaaS и multi-tenant;
- финансовый/медицинский/регулируемый контур без audit/encryption/PITR;
- аналитика и OLAP, большие JOIN/aggregations;
- WAN/geo-распределённый OLTP;
- нагрузка «10k–30k ops/s, p99 < 20 ms» без отдельного SLO-прогона;
- данные, где потеря/частичная видимость недопустима: current membership и
  cross-range 2PC ещё MVP;
- замена PostgreSQL/TiDB «из коробки»;
- хранение весов DeepSeek и инференс из БД;
- телефоны как узлы консенсуса или source of truth;
- динамический hash-set с удалениями без нового поколения сегментов.

## 4. Что мешает назвать это production

### P0 — блокеры

1. **Membership safety**
   - нет joint consensus, изменения по одному серверу;
   - membership не входит в state-machine snapshot: после компакции возможен
     stale voter set;
   - нет decommission и политики замены узла;
   - удалённый узел может хранить stale state.

2. **Данные и восстановление**
   - backup логический, без PITR/CDC;
   - restore неатомарен и не изолирован от писателей;
   - нет регулярных restore-учений и заявленных RPO/RTO;
   - нет проверки backup на больших объёмах.

3. **Безопасность**
   - нет TLS/mTLS, authn/authz, RBAC;
   - нет audit log;
   - нет encryption at rest;
   - pgwire без TLS/паролей;
   - нет tenant isolation и квот.

4. **Lifecycle**
   - нет версионирования протокола/совместимости;
   - нет rolling upgrade/rollback;
   - нет миграций схемы/данных;
   - нет Helm/operator и формальной топологии.

5. **Reliability evidence**
   - Jepsen opt-in и внутрипроцессный, не реальная сеть;
   - нет длинного soak (дни/недели);
   - нет проверки линейuizability на всём наборе сценариев;
   - cross-range 2PC без distributed wait-for графа;
   - membership/2PC recovery покрыты тестами, но не production-инцидентами.

6. **SLO/performance**
   - нет воспроизводимого p99-прогона на 3 узлах и 1 ТБ;
   - fast-профиль (M1 Pro/loopback): ~11.6 мс single (~86 tx/s),
     ~1.2–2.1 мс/op parallel (~480–820 tx/s), batch 256 ~0.14–0.20 мс/txn
     (~5–7k tx/s, batch latency 36–51 мс); production `sync` — ~40–70 tx/s;
     PostgreSQL 13 рядом — ~6.8k tx/s на 10 клиентах;
   - это не SLO: нет реальной сети, p99, 1 ТБ soak;
   - нет follower reads и parallel query.

### P1 — продуктовая зрелость

- multi-tenancy, quotas, rate limits, billing;
- PITR/CDC, incremental backup;
- cost-based planner с гистограммами, AND/OR, >2 JOIN, parallel join;
- pgwire catalog/JDBC/TLS, SQL auth;
- capacity-aware placement, AZ/rack awareness, decommission;
- Prometheus alerts, dashboards, on-call runbooks;
- Kubernetes operator, upgrade automation;
- license/security review зависимостей.

### P2 — масштаб

- geo-replication, multi-region;
- tiered storage, columnar/OLAP;
- change data capture, streaming export;
- multi-version protocol compatibility.

## 5. Целевой продукт

Рекомендую **не публичный DBaaS**, а:

> **Private single-tenant transactional KV + SQL для внутренних сервисов**
> в одном регионе, 3 AZ, 3–5 узлов, десятки–сотни ГБ, low-to-medium QPS.

Это соответствует текущей архитектуре и не требует multi-tenancy/billing.
Публичный DBaaS — отдельный продукт с другим объёмом security/compliance.

### Идеальный первый ICP

- внутренний control plane / metadata store;
- сервис, которому нужны транзакции и range scans, но не PostgreSQL-совместимость;
- команда, готовая эксплуатировать Go-бинарники и Raft-кластер;
- SLA: 99.9%, RPO 0, RTO < 30 мин (после P0).

## 6. План до production

### Этап A — «private alpha» (P0, ~6–9 месяцев командой 3–5)

1. Membership: joint consensus **или** жёсткий операционный протокол
   one-change-at-a-time + decommission + membership snapshot.
2. Backup/PITR + restore drills + RPO/RTO.
3. TLS/mTLS, authn/authz, audit, encryption at rest.
4. Rolling upgrade/rollback + protocol versioning.
5. SLO-прогон: 3 узла, YCSB/TPC-C-подобный workload, p99, 1 ТБ soak.
6. Jepsen с реальными partitions/clock skew, длинный soak.
7. Helm/operator, health/readiness, alerts, runbooks.

**Выход:** можно доверять внутренние некритичные данные, есть on-call и restore.

### Этап B — «production single-tenant» (+3–6 месяцев)

1. Multi-AZ placement, capacity-aware rebalance, decommission.
2. PITR/incremental backup.
3. SQL: AND/OR, >2 JOIN, parallel scan, pgwire catalog/JDBC, TLS.
4. Performance: follower reads, batching, 10k+ ops/s.
5. Security review, license review, incident drills.

**Выход:** SLA 99.9%, RPO 0, RTO < 30 мин, восстановление проверено.

### Этап C — «product/DBaaS» (+12 месяцев и больше)

Multi-tenancy, billing, quotas, compliance, geo, support, SLA-контракты.
Это уже другой продукт; текущий код — только ядро.

## 7. Production gates (Definition of Done)

- [ ] Joint consensus или формально замороженный one-change протокол.
- [ ] Membership в snapshot + decommission.
- [ ] Backup + PITR + restore drill с RPO/RTO.
- [ ] TLS/mTLS + authn/authz + audit + encryption at rest.
- [ ] Rolling upgrade/rollback + protocol compatibility.
- [ ] SLO-прогон 3 узла + 1 ТБ soak, p99 в бюджете.
- [ ] Jepsen real-network partitions/clock skew, длинный soak.
- [ ] Helm/operator, alerts, dashboards, runbooks.
- [ ] Security/license review.
- [ ] Multi-AZ placement и capacity policy.

## 8. Честные риски

| Риск | Последствие |
|---|---|
| Нет joint consensus | смена состава при сбое может нарушить кворум |
| Membership не в snapshot | stale voter set после компакции |
| Cross-range 2PC без wait-for графа | долгие блокировки/таймауты вместо диагностируемого deadlock |
| Backup без PITR | потеря данных между бэкапами |
| Нет TLS/auth | любой доступ к сети = доступ к данным |
| Нет upgrade-пути | обновление = простой/ручной риск |
| SQL full scan/CBO без гистограмм | плохие планы на больших таблицах |
| Один регион | WAN/geo не поддержаны |

## 9. Ссылки

- [ASSESSMENT.md](ASSESSMENT.md)
- [EDGE-HASH-CACHE.md](EDGE-HASH-CACHE.md)
- [PERFORMANCE.md](PERFORMANCE.md)
- [ROADMAP.md](ROADMAP.md)
- [ARCHITECTURE-TARGET.md](ARCHITECTURE-TARGET.md)
- [CONTRACTS-V2.md](CONTRACTS-V2.md)
- [ADR 0002 — MVCC/Percolator](adr/0002-mvcc-percolator.md)
- [ADR 0003 — Multi-Raft](adr/0003-multiraft-ranges.md)
- [ADR 0004 — SQL](adr/0004-sql-layer.md)
