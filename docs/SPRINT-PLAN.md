# Оценка предложения и план спринтов

- Дата: 2026-11-18
- Контекст: после Sprint 1, Docker-спринта и фикса joint convergence.
- Планирование: **8 субагентов на спринт**, 4 недели на спринт.

## 1. Вердикт

Предложение **полезное, но написано по устаревшему состоянию**. Два пункта уже
закрыты или частично закрыты:

- joint consensus уже реализован в `internal/raft/joint.go`;
- mTLS уже реализован (опционально), не хватает fail-closed и RBAC/audit.

Остальные пункты годные. Главный приоритет — **не Edge Hash Cache, а
доказательства безопасности и восстановления**: 72 ч soak, 1 ТБ, PITR,
decommission, RBAC/audit. Edgehash остаётся **встроенным компонентом
репозитория**; отдельная сборка — только опциональный спайк, не новый продукт.

## 2. Разбор по пунктам

| # | Предложение | Факт | Вердикт | Приоритет |
|---|---|---|---|---|
| 1 | Joint Consensus | C_old,new, joint quorum, snapshot/epoch, decommission CLI, 1000-change stress (3× race) и CI job | **Закрыто по stress**; остались pre-vote и multi-host | P0 |
| 2 | Per-chunk CRC снапшотов | Есть общий CRC32 всего снапшота и incremental CRC потока; проверка — в конце. Per-chunk CRC нет | **Принять**: defense-in-depth, ранний отказ, меньше IO при битом чанке. Не safety-critical: общий CRC уже не даёт установить битый снапшот | P1 |
| 3 | Edgehash как отдельная сборка | Bloom+Merkle+Ed25519, HTTP/cache, TLS/Bearer; mobile — каркас; рецептов Workers/Nginx/Envoy нет | **Не выделять в продукт.** Оставить в репо; опциональный `cmd/edgehash` build — идея на подумать | P2 |
| 4 | Soak 72 ч / 100 ГБ–1 ТБ / p99 | Harness готов (`test/soak_test.go`), реального 1 ТБ и 72 ч прогона нет. 8.6k tx/s — loopback pipeline, не SLO | **Принять, P0**: главный недостающий evidence | P0 |
| 5 | mTLS + RBAC + audit | TLS/mTLS, RBAC/audit MVP, admin token и meta fan-out есть; нет fail-closed, CLI wiring, rotation, at-rest | **Принять с поправкой**: включить и ужесточить, не писать заново | P0 |
| 6 | Разделить roadmap на треки и туториалы | Сейчас один ROADMAP; документация уже без исторических цифр | **Принять, P2**: полезно для community, но после safety/evidence | P2 |

### 2.1 Что в предложении неверно

- «Joint consensus отсутствует» — неверно, реализован; отсутствует его
  **стресс-доказательство** и decommission-обвязка.
- «Нужен полноценный mTLS» — mTLS уже есть, но выключен по умолчанию; задача —
  fail-closed профиль, RBAC/audit, а не написание TLS заново.
- «8.6k tx/s» — это aggregate throughput на loopback с pipeline 256, не latency
  одного запроса и не production SLO.
- Edgehash «близок к продакшену» — альфа-компонент: API/формат ещё могут
  меняться, mobile — каркас. Но это **не повод** делать отдельный продукт:
  компонент ценен именно внутри репозитория.

## 3. Приоритеты

### P0 — без этого production невозможен

1. 72 ч soak + 1 ТБ multi-host + p99/память/WAL/compaction.
2. Decommission RPC/CLI, meta fan-out admin-token, 1000 random changes.
3. PITR integration: Raft WAL/meta/TSO, 1PC barrier wiring, restore drill,
   RPO/RTO.
4. RBAC/audit/fail-closed TLS, encryption at rest.
5. Per-chunk CRC и corruption chaos — после P0 evidence, но в том же спринте.

### P1 — production hardening

1. Per-chunk CRC и corruption chaos.
2. `kvbackup` TLS + admin-token.
3. Decommission/meta fan-out token.
4. PITR integration/restore drill.
5. RBAC/audit enforcement.

### P2 — Edgehash как встроенный компонент (не продукт)

1. Опциональная сборка `cmd/edgehash` — только convenience, не отдельный репозиторий.
2. Интеграционные рецепты Workers/Nginx/Envoy — если будет спрос.
3. Mobile SDK — только после реального запроса.
4. Бенчмарки компонента.
5. API/формат не замораживать до внешних пользователей.

### P2 — community/docs

1. Три раздела документации: KV Cluster, SQL/NewSQL, Edgehash (в одном репо).
2. Quickstart «SQL за 5 минут», «Edgehash за 10 минут».
3. Contribution guide, RFC template, roadmap по трекам.

## 4. Спринты по 8 субагентов

### Статус

- **Sprint 1 — закрыт:** joint stress/fix, decommission CLI, PITR adapter/barrier,
  per-chunk CRC, RBAC/audit MVP, kvbackup TLS, soak harness, docs tracks.
- **Docker-спринт — закрыт:** multi-arch image, demo/cluster compose,
  bootstrap, quickstart.
- **Joint convergence — исправлен:** 1000 changes 3× race, CI job.

### Sprint 1 — Evidence & closure (закрыт, 4 недели)

| # | Срез | Владелец-пакет | Выход |
|---|---|---|---|
| 1 | Joint consensus stress: 1000 random changes, failover/partition, e2e membership over gRPC | `internal/raft/**` + tests | отчёт + тесты |
| 2 | Decommission RPC/CLI + metaserver fan-out token | `internal/kvserver`, `internal/metaserver`, `internal/kvctl` | команда + тест |
| 3 | PITR integration: archive ↔ Raft WAL/meta/TSO, 1PC barrier wiring | `internal/pitr`, `internal/backup`, `internal/mvcc` | restore-to-TS MVP |
| 4 | Per-chunk CRC + corruption injection | `internal/raft`, `proto`, `internal/pbconv` | чанк-тесты |
| 5 | 72 ч soak harness + 1 ТБ multi-host p99 | `test/`, `scripts/` | отчёт (запуск — Sprint 2) |
| 6 | RBAC/audit MVP: роли, audit log, fail-closed TLS | `internal/metaserver`, `internal/kvserver`, `cmd` | policy + tests |
| 7 | `kvbackup` TLS/admin-token + optional edgehash build spike | `internal/backup`, `cmd/edgehash` | backup TLS + build note |
| 8 | Docs tracks + quickstart skeleton | `docs/` | 3 трека + 2 гайда |

Критерий выхода: все срезы собираются, race-тесты зелёные; 1 ТБ и 72 ч ещё
**не** заявлены, если не прогнаны.

### Sprint 2 — Production evidence & hardening (4 недели)

| # | Срез | Выход |
|---|---|---|
| 1 | 72 ч soak на 3–5 узлах: memory/GC/WAL/compaction/p99 | отчёт с графиками |
| 2 | 1 ТБ multi-host на Linux/NVMe, YCSB/TPC-C-like | SLO-отчёт |
| 3 | PITR: Raft WAL/meta/TSO archive, 1PC barrier wiring, isolated restore | restore-to-TS + drill |
| 4 | Pre-vote + multi-host joint stress + decommission под failover | G2 evidence |
| 5 | Fail-closed TLS, RBAC CLI wiring, audit rotation/export, at-rest | security tests |
| 6 | Meta↔range-map atomic split/merge (distributed 2PC) | crash-consistent split |
| 7 | Registry publish + Helm install/upgrade/rollback на реальном кластере | release report |
| 8 | `kvctl ranges`/status: заполнять leader и health, убрать пустой LEADER | truthful status |

Критерий выхода: G1/G3/G6 evidence; security P0 закрыты или перечислены;
реальный кластер развёрнут и обновлён.

### Sprint 3 — Product & community (4 недели)

| # | Срез | Выход |
|---|---|---|
| 1 | Protocol versioning / mixed-version rolling upgrade | compatibility report |
| 2 | Multi-AZ placement/operator, decommission policy | ops runbook |
| 3 | SQL: AND/OR, >2 JOIN, NULL/DECIMAL, pg_catalog/JDBC | SQL scope decision |
| 4 | Edgehash integrated benchmarks/limits (not a product) | отчёт |
| 5 | SQL 5-minute and Docker 2-minute tutorials refresh | tutorials |
| 6 | CONTRIBUTING/RFC templates | docs |
| 7 | Backup/restore drill automation | CI drill |
| 8 | Final production gate audit | scorecard |

## 5. Зависимости и риски

- **Raft-файлы**: срезы 1 и 4 Sprint 1 оба трогают `internal/raft`; один
  владелец файлов, второй — только новые тесты/CRC-поля, иначе конфликт.
- **PITR**: без 1PC barrier wiring restore может пропустить commit; срез 3
  зависит от `internal/mvcc/barrier.go`.
- **Security**: RBAC/audit нельзя делать после Edgehash; это P0.
- **Edgehash**: не выделять в продукт; отдельная сборка — только после P0 и по запросу.
- **Edgehash API**: не замораживать до внешних пользователей.
- **Soak**: 72 ч и 1 ТБ нельзя «ускорить»; нужен отдельный стенд, не CI.
- **8 агентов**: максимум 8 параллельных срезов; при конфликте файлов дробить
  на 2 волны, а не запускать 16.

## 6. Что не делать

- Не переписывать joint consensus с нуля.
- Не позиционировать 8.6k tx/s как production SLO.
- Не выделять Edgehash в отдельный продукт/репозиторий.
- Не выпускать edgehash SDK до реального запроса.
- Не добавлять SQL-фичи до G1/G3 evidence.
