# Production plan: что реализовать и в каком порядке

- Дата: 2026-11-16
- HEAD: `13c03c2`
- Связанные документы: [ASSESSMENT.md](ASSESSMENT.md),
  [PRODUCT-READINESS.md](PRODUCT-READINESS.md), [PERFORMANCE.md](PERFORMANCE.md).

## 1. Что считаем «продакшеном»

Целевой продукт — **private single-tenant transactional KV + SQL**:

- один регион, 3 AZ, 3–5 узлов;
- десятки–сотни ГБ (далее 1 ТБ после soak);
- low-to-medium QPS;
- SLA 99.9%, RPO 0, RTO < 30 минут;
- команда сама эксплуатирует Go-бинарники и Raft-кластер.

Это **не** публичный DBaaS, не multi-tenant, не OLAP и не замена PostgreSQL.
Для DBaaS нужен отдельный продукт (billing, quotas, compliance, support).

## 2. Ворота production (Definition of Done)

| # | Ворота | Измеримый критерий |
|---|---|---|
| G1 | Durability | kill -9 на каждой стадии commit/snapshot/truncate; ни одна подтверждённая запись не потеряна; 72 ч soak |
| G2 | Membership safety | joint consensus или формально замороженный one-change протокол; membership в snapshot; decommission без потери кворума; 1000 random changes под failover |
| G3 | Backup/PITR | PITR на любую секунду в окне 7 дней; restore в изолированный кластер; RPO 0; RTO < 30 мин; ежемесячный drill |
| G4 | Security | TLS 1.3/mTLS на gRPC/HTTP/pgwire; authn/authz/RBAC; audit; encryption at rest; secrets; pen-test без critical/high |
| G5 | Upgrade | rolling upgrade N-1 → N без простоя; rollback; protocol version negotiation; schema/data migrations |
| G6 | SLO/chaos | 3/5 узлов, 1 ТБ, YCSB/TPC-C-подобный workload; p99 в бюджете; real-network partitions/clock skew/disk full/slow disk; linearizability checker |
| G7 | Ops | Helm/operator, multi-AZ placement, capacity/decommission, dashboards, alerts, runbooks, on-call |
| G8 | SQL (если в scope) | SQL P0+ семантика, pgwire TLS/auth/JDBC, planner stats, backup consistency |

Пока G1–G7 не закрыты, это **не production**, даже если бенчмарки хорошие.

## 3. P0: что реализовать

### A. Durability и crash consistency

Почему первым: сейчас `SyncWrites=false` у state machine, durability опирается
на replay Raft-лога. Это может быть корректно, но не доказано crash-тестами.

Реализовать:

1. **Production-профиль** с `Config.SyncStateMachine=true` по умолчанию.
   Fast-профиль (`false`) — только opt-in с документированным риском.
2. **Crash-injection harness**: kill -9 в точках prewrite/commit/apply/snapshot/
   truncate/compaction; проверка «подтверждённое не потеряно».
3. Аудит fsync: Raft log, snapshot, manifest, backup; disk full/ENOSPC.
4. Formal invariant: state machine можно восстановить из snapshot + log;
   доказать/протестировать replay после частичного apply.
5. Метрики: replay lag, snapshot age, fsync latency, WAL size.

**Выход:** G1. Без этого production-профиль не включать.

### B. Membership safety

Сейчас: learner→voter→remove, single-server change, без joint consensus;
membership не в state-machine snapshot.

Реализовать:

1. **Joint consensus** (C_old,new) или формально замороженный one-change
   протокол с operator lock и запретом второй change до завершения.
2. **Membership в snapshot**: voter/learner set входит в state-machine snapshot,
   а не только в отдельный WAL record.
3. **Decommission**: drain data, transfer leaders, remove voter, cleanup.
4. **Learner catch-up policy**: не промоутить, пока lag > threshold.
5. Тесты: 1000 random changes, fail leader во время change, partition,
   stale node rejoin, snapshot compaction.

**Выход:** G2.

### C. Backup и PITR

Сейчас: логический backup/restore, без PITR; restore неатомарен.

Реализовать:

1. **Continuous WAL/change archiving** в S3/MinIO (или локальный object store).
2. **Incremental backup** + full backup; retention/GC.
3. **PITR restore**: restore в изолированный кластер, replay до timestamp.
4. **Restore isolation**: писатели остановлены, epoch/fencing.
5. **Backup encryption** и checksum; verify job.
6. **Restore drills**: RPO/RTO замеры, отчёт.

**Выход:** G3.

### D. Security

Сейчас: v2 gRPC и pgwire без TLS/auth; v1 HTTP имеет Basic/TLS; edgehash —
TLS/Bearer.

Реализовать:

1. TLS 1.3 + mTLS для gRPC (KV/Raft/meta), HTTP, pgwire.
2. Authn: mTLS client cert или JWT/password; authz: RBAC (admin/read/write).
3. **Audit log**: кто/что/когда, immutable/export.
4. **Encryption at rest**: KMS/keys, rotation; snapshot/backup encryption.
5. Secrets management, TLS cert rotation, no plaintext tokens.
6. Rate limits/quotas на KV/SQL; tenant isolation (если multi-tenant).
7. Security review/pen-test.

**Выход:** G4.

### E. Upgrade и lifecycle

1. **Protocol versioning**: gRPC/HTTP/pgwire compatibility, feature negotiation.
2. **Rolling upgrade**: N-1 → N, mixed-version cluster, drain, restart.
3. **Rollback** и downgrade policy.
4. Schema/data migrations; online DDL (если SQL).
5. Binary packaging, config compatibility, deprecation policy.

**Выход:** G5.

### F. SLO, chaos и большие данные

1. **Benchmark harness**: 3/5 узлов, real network (не loopback), NVMe,
   YCSB + TPC-C-подобный, 1 ТБ.
2. **p50/p95/p99**, errors, saturation, per-range hotspots.
3. **Chaos**: partitions, clock skew, disk full/slow, OOM, node kill,
   rolling restart; nightly/weekly.
4. **Long soak**: 72 ч / 7 дней, memory/compaction/WAL growth.
5. **Linearizability checker** на всех сценариях.
6. Сравнение с PostgreSQL+Patroni/Citus на том же workload.

**Выход:** G6 + подтверждённые SLO, а не маркетинговые цифры.

### G. Operations

1. Helm chart/operator: topology, 3 AZ, anti-affinity, PDB.
2. Multi-AZ placement, capacity-aware rebalance, decommission.
3. Dashboards/alerts: availability, latency, errors, disk, WAL, snapshot,
   membership, 2PC locks.
4. Runbooks: failover, restore, disk full, split brain, upgrade, key rotation.
5. On-call/escalation, backup drills, incident process.

**Выход:** G7.

### H. SQL production (если SQL входит в ICP)

1. `AND/OR`, >2 JOIN, LEFT/RIGHT JOIN, subqueries.
2. NULL/DECIMAL/JSON/FK, constraints, online DDL.
3. pg_catalog, JDBC/pgx compatibility, pgwire TLS/auth.
4. Planner statistics/histograms, join order, parallel/distributed join.
5. SQL backup consistency, schema migrations, upgrade.
6. SQL correctness/linearizability tests.

**Выход:** G8. Если первый ICP — metadata KV, SQL можно не блокировать.

## 4. P1: после production

- follower reads / ReadIndex; read scaling.
- parallel apply, pipelined AppendEntries, async I/O (`io_uring`).
- parallel SQL/distributed join.
- batch tuning, 10k–30k ops/s target.
- cost-aware placement, tiered storage.
- edgehash: mobile device tests, CDN multi-region, key rotation.

## 5. P2: не сейчас

- public DBaaS, multi-tenancy, billing, quotas.
- multi-region active-active.
- OLAP/columnar.
- замена PostgreSQL.
- хранение весов DeepSeek.
- телефоны как узлы консенсуса.

## 6. Порядок и оценка

Команда 3–5 инженеров с опытом distributed systems. Оценки — порядок
величины, не гарантия.

| Фаза | Что | Длительность |
|---|---|---|
| 0 | Заморозить ICP, production-профиль, crash/SLO harness | 2–4 недели |
| 1 | Durability + membership safety | 6–8 недель |
| 2 | PITR/backup + restore drills | 4–6 недель |
| 3 | Security (TLS/mTLS/RBAC/audit/encryption) | 4–6 недель |
| 4 | Upgrade/operator/observability | 4–6 недель |
| 5 | 1 ТБ soak, chaos, SLO, security review | 4–8 недель |
| **Итого** | private single-tenant production | **6–9 месяцев** |
| SQL | P0+ до production-уровня | +6–12 месяцев |
| DBaaS | multi-tenant/compliance/billing | +12 месяцев и больше |

Фазы можно частично параллелить, но **G1/G2 нельзя обходить**: это safety,
а не фичи.

## 7. Первый спринт (2–4 недели)

1. Включить `SyncStateMachine=true` в production-профиле; fast — opt-in.
2. Написать crash-injection harness (kill -9 на стадиях commit/snapshot).
3. RFC: joint consensus vs one-change; membership в snapshot.
4. RFC: PITR (WAL archiving, restore, retention).
5. TLS/mTLS для gRPC и pgwire (minimum viable).
6. 1 ТБ benchmark harness + p99, real network.
7. Nightly chaos CI: kill leader, partition, disk full.

## 8. Критерии, что «продакшен готов»

- [ ] G1 durability: crash-тесты + 72 ч soak.
- [ ] G2 membership: joint consensus/one-change, snapshot, decommission.
- [ ] G3 PITR: RPO 0, RTO < 30 мин, restore drill.
- [ ] G4 security: TLS/mTLS, RBAC, audit, encryption, pen-test.
- [ ] G5 upgrade: rolling + rollback.
- [ ] G6 SLO: 1 ТБ, p99, chaos, linearizability.
- [ ] G7 ops: operator, multi-AZ, alerts, runbooks, on-call.
- [ ] G8 SQL (если в scope): P0+ и pgwire TLS/auth/JDBC.
- [ ] Нет open critical/high security findings.
- [ ] Есть владелец продукта, on-call и бюджет.

## 9. Главный совет

Не добавлять новые фичи, пока не закрыты **G1 (durability)** и
**G2 (membership)**. Это два места, где ошибка означает потерю данных или
потерю кворума, а не «медленно работает». Performance уже достаточен для
маленьких данных; большой масштаб доказывается стендом, а не оптимизациями.
