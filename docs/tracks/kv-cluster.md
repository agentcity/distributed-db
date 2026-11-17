# Трек: KV cluster

- **Статус:** v1 — demo/educational; v2 KV — experimental alpha / внутренний
  прототип. Это **не production-СУБД**.
- **Что это:** два поколения одного KV-трека. v1 (`cmd/db`) — учебный
  CP-KV с hash-шардингом; v2 (`cmd/kv2`) — транзакционный Multi-Raft KV с
  range-шардированием, MVCC/2PC и replicated meta/TSO.
- **Не в этом треке:** SQL-слой — [sql-newsql.md](sql-newsql.md); edgehash —
  [edgehash.md](edgehash.md).
- **Практика:** [USER-GUIDE.md](../USER-GUIDE.md) (запуск, curl, psql,
  backup), [ROADMAP.md](../ROADMAP.md) (этапы и открытые пункты).

## Текущий статус

| Часть | Статус | Что это значит |
|---|---|---|
| v1 `cmd/db` | demo / educational | Рабочий CP-KV для проверки Raft, failover и hash-шардирования |
| v2 KV `cmd/kv2` | experimental alpha | Raft, Multi-Raft, MVCC/2PC, split/merge, meta/TSO есть; production-обвязки нет |
| Production | нет | Нет подтверждённых SLO, multi-AZ, rolling upgrade и полной security-модели |

v1 и v2 — разные кластеры и разные бинари. Не запускайте их одновременно на
одних портах (`8080`, `9090`, `7001–7003`).

## Точки входа

| Что | Где |
|---|---|
| v1: узел и координатор | `cmd/db` (`node`, `coordinator`), `scripts/dev-cluster.sh`, `scripts/smoke.sh` |
| v1: HTTP API | `PUT/GET/DELETE /v1/kv/{key}`, `/v1/status`, `/health`; схема в [openapi.yaml](../openapi.yaml) |
| v2: meta и KV | `cmd/kv2` (`-mode=meta`, `-mode=node`) |
| v2: клиенты | `internal/v2client`, `cmd/kvctl`, `cmd/kvbackup` |
| v2: тесты | `test/` (включая failover, split/merge, Jepsen-сценарии) |
| Решения | [ADR 0001–0006](../adr/README.md), [CONTRACTS-V2.md](../CONTRACTS-V2.md) |
| Эксплуатация | [OPERATIONS.md](../OPERATIONS.md), [RUNBOOKS.md](../RUNBOOKS.md) |

## Что поддерживается

**Консенсус и данные**

- Raft: выборы, репликация лога, group commit, snapshot и
  `InstallSnapshot`; chunked/gzip/resumable snapshot.
- Multi-Raft: несколько range-групп в одном процессе, у каждой свой Raft-лог,
  MVCC, snapshot и transport; shared Pebble на узел.
- MVCC/Percolator: версии, locks, prewrite/commit/rollback, lock resolution,
  GC safe point.
- Транзакции: single-range 1PC (`CommitSingle`) и cross-range 2PC
  (primary-first); `CommitBatch` — pipelining, не atomic batch.
- Range split/merge, auto-split/auto-merge, streaming copy, leader
  transfer/rebalance.

**Control plane**

- Replicated meta/TSO на 3/5 узлах; range map, heartbeat, TSO batch,
  клиентский redirect и failover.
- Range routing по ключу/range ID; epoch/fencing через `x-range-epoch`;
  `ranges.json` переживает рестарт.
- Membership: learner → voter → remove, single-server change; изменения
  сериализуются. `kvctl decommission` снимает реплику через
  transfer/promote/remove и обновляет meta range map; capacity-политика и
  1000 random changes не закрыты.
- Backup/restore — логический (`cmd/kvbackup`, TLS/admin-token); PITR archive
  MVP и adapter-seam есть, но runtime integration и restore drills нет.

**Эксплуатация и безопасность**

- `-durability=sync` — default с fsync state machine (production-профиль по
  durability, но не production-готовность); `fast` — opt-in и не
  production-safe до crash/soak-тестов.
- Опциональные TLS/mTLS для gRPC (KV/meta/Raft), meta admin-token; plaintext
  остаётся default.
- Prometheus-метрики, `/metrics`, `/debug/slo` (`cmd/metricsrv`, пакет
  `internal/metrics`).
- v1: hash-шардинг, stateless coordinator, lease-barrier чтения с лидера,
  Basic Auth и TLS на HTTP.

## Что не поддерживается или ограничено

- **Production:** нет multi-AZ/geo placement policy, rolling upgrade/operator,
  PITR runtime integration и restore-учений, operator-facing RBAC/audit,
  encryption at rest, follower reads, подтверждённых p99 и 1 ТБ soak.
  Это blockers, а не «мелочи».
- **Membership:** нет capacity-aware decommission-политики и доказательств
  1000 random changes; CLI есть. Путь Add/Remove replica не выполняет
  joint-переход `C_old,new` (joint-примитивы есть в Raft-слое, но
  orchestration этим путём не закрыт). Одно изменение за раз.
- **Безопасность:** TLS/mTLS опциональны и не fail-closed по умолчанию;
  admin-token покрывает не все RPC. RBAC/audit MVP есть в конфиге/пакетах, но
  нет операторских флагов в `kv2`, полного покрытия, rotation и шифрования
  at rest.
- **v2:** `fast` не даёт production-durability; plaintext default; нет
  follower reads и распределённых транзакций с wait-for графом.
- **v1:** нет snapshot/truncation Raft-лога, динамической карты шардов,
  транзакций, TTL и вторичных индексов; mTLS/проверка клиентских
  сертификатов неполные.
- **Производительность:** замеры в репозитории — не SLO; они не заменяют
  p99/1 ТБ прогон на реальной сети и диске.

## Следующие шаги

1. Закрыть decommission-политику и safety-доказательства (1000 random
   changes, failover/partition) и определить, где нужен joint-переход при
   изменении состава.
2. Довести PITR runtime integration и restore drills; проверить RPO/RTO на
   реальном объёме.
3. Сделать security fail-closed: TLS/auth для admin-путей, операторский
   RBAC/audit, encryption at rest.
4. Rolling upgrade/operator, multi-AZ placement, capacity-aware rebalance.
5. Подтвердить SLO: p99, 1 ТБ soak, chaos/failover на реальном железе.

Полный список и порядок — в [ROADMAP.md](../ROADMAP.md) и
[PRODUCT-READINESS.md](../PRODUCT-READINESS.md); что делать до production —
в [PRODUCTION-PLAN.md](../PRODUCTION-PLAN.md).

## Связанные документы

- [USER-GUIDE.md](../USER-GUIDE.md) — команды запуска v1/v2, SQL, psql, backup.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — контракты и инварианты.
- [CONTRACTS-V2.md](../CONTRACTS-V2.md) — границы пакетов v2.
- [ASSESSMENT.md](../ASSESSMENT.md) — честная оценка применимости.
