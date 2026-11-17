# Оценка предложения и план спринтов

- Дата: 2026-11-18
- Контекст: после safety/recovery wave (joint consensus, split recovery,
  ScanForBackup, PITR archive MVP, admin auth).
- Планирование: **8 субагентов на спринт**, 4 недели на спринт.

## 1. Вердикт

Предложение **полезное, но написано по устаревшему состоянию**. Два пункта уже
закрыты или частично закрыты:

- joint consensus уже реализован в `internal/raft/joint.go`;
- mTLS уже реализован (опционально), не хватает fail-closed и RBAC/audit.

Остальные пункты годные. Главный приоритет — **не Edge Hash Cache, а
доказательства безопасности и восстановления**: 72 ч soak, 1 ТБ, PITR,
decommission, RBAC/audit. Edgehash — отдельный продуктовый трек, его нельзя
смешивать с production-готовностью БД.

## 2. Разбор по пунктам

| # | Предложение | Факт | Вердикт | Приоритет |
|---|---|---|---|---|
| 1 | Joint Consensus | Уже есть: C_old,new, joint quorum, auto-finalize, snapshot/epoch. Нет 1000 random changes, e2e через gRPC, decommission RPC/CLI | **Принять частично**: не переписывать, а доказать и закрыть decommission | P0 |
| 2 | Per-chunk CRC снапшотов | Есть общий CRC32 всего снапшота и incremental CRC потока; проверка — в конце. Per-chunk CRC нет | **Принять**: defense-in-depth, ранний отказ, меньше IO при битом чанке. Не safety-critical: общий CRC уже не даёт установить битый снапшот | P1 |
| 3 | Edgehash как продукт | Bloom+Merkle+Ed25519, HTTP/cache, TLS/Bearer; mobile — каркас; нет Workers/Nginx/Envoy рецептов | **Принять как отдельный трек**: быстрый adoption, но не заменяет безопасность БД | P1 |
| 4 | Soak 72 ч / 100 ГБ–1 ТБ / p99 | Есть loopback-бенчи и 1 ТБ harness; реального 1 ТБ и 72 ч прогона нет. 8.6k tx/s — loopback pipeline, не SLO | **Принять, P0**: это главный недостающий evidence | P0 |
| 5 | mTLS + RBAC + audit | mTLS/TLS есть opt-in; RBAC/audit/at-rest нет; KV admin token есть, но meta fan-out его не передаёт | **Принять с поправкой**: mTLS не «доделать», а сделать fail-closed; RBAC/audit — новое | P0 |
| 6 | Разделить roadmap на треки и туториалы | Сейчас один ROADMAP; документация уже без исторических цифр | **Принять, P2**: полезно для community, но после safety/evidence | P2 |

### 2.1 Что в предложении неверно

- «Joint consensus отсутствует» — неверно, реализован; отсутствует его
  **стресс-доказательство** и decommission-обвязка.
- «Нужен полноценный mTLS» — mTLS уже есть, но выключен по умолчанию; задача —
  fail-closed профиль, RBAC/audit, а не написание TLS заново.
- «8.6k tx/s» — это aggregate throughput на loopback с pipeline 256, не latency
  одного запроса и не production SLO.
- Edgehash «близок к продакшену» — альфа-компонент: API/формат ещё могут
  меняться, mobile — каркас, рецептов интеграции нет.

## 3. Приоритеты

### P0 — без этого production невозможен

1. 72 ч soak + 1 ТБ multi-host + p99/память/WAL/compaction.
2. Decommission RPC/CLI, meta fan-out admin-token, 1000 random changes.
3. PITR integration: Raft WAL/meta/TSO, 1PC barrier wiring, restore drill,
   RPO/RTO.
4. RBAC/audit/fail-closed TLS, encryption at rest.
5. Per-chunk CRC и corruption chaos — после P0 evidence, но в том же спринте.

### P1 — продуктовый трек Edgehash

1. Заморозить API/формат манифеста/сегментов.
2. Go SDK + CLI.
3. Cloudflare Worker, Nginx, Envoy recipes.
4. Mobile SDK: pinning, Bearer, offline cache.
5. Бенчмарки: latency, размер, false-positive Bloom.

### P2 — community/docs

1. Три трека: KV Cluster, SQL/NewSQL, Edgehash.
2. Quickstart «SQL за 5 минут», «Edgehash за 10 минут».
3. Contribution guide, RFC template, roadmap по трекам.

## 4. Спринты по 8 субагентов

### Sprint 1 — Evidence & closure (4 недели)

| # | Срез | Владелец-пакет | Выход |
|---|---|---|---|
| 1 | Joint consensus stress: 1000 random changes, failover/partition, e2e membership over gRPC | `internal/raft/**` + tests | отчёт + тесты |
| 2 | Decommission RPC/CLI + metaserver fan-out token | `internal/kvserver`, `internal/metaserver`, `internal/kvctl` | команда + тест |
| 3 | PITR integration: archive ↔ Raft WAL/meta/TSO, 1PC barrier wiring | `internal/pitr`, `internal/backup`, `internal/mvcc` | restore-to-TS MVP |
| 4 | Per-chunk CRC + corruption injection | `internal/raft`, `proto`, `internal/pbconv` | чанк-тесты |
| 5 | 72 ч soak harness + 1 ТБ multi-host p99 | `test/`, `scripts/` | отчёт (запуск — Sprint 2) |
| 6 | RBAC/audit MVP: роли, audit log, fail-closed TLS | `internal/metaserver`, `internal/kvserver`, `cmd` | policy + tests |
| 7 | Edgehash API freeze + Go SDK contract | `internal/edgeset`, `internal/edgehttp` | API v1 draft |
| 8 | Docs tracks + quickstart skeleton | `docs/` | 3 трека + 2 гайда |

Критерий выхода: все срезы собираются, race-тесты зелёные; 1 ТБ и 72 ч ещё
**не** заявлены, если не прогнаны.

### Sprint 2 — Production evidence & hardening (4 недели)

| # | Срез | Выход |
|---|---|---|
| 1 | 72 ч soak на 3–5 узлах, memory/WAL/compaction | p99/leaks отчёт |
| 2 | 1 ТБ multi-host, Linux/NVMe, YCSB/TPC-C-like | SLO-отчёт |
| 3 | Restore drill: RPO/RTO, isolated restore, PITR | drill report |
| 4 | RBAC enforcement + audit export + key rotation | security tests |
| 5 | Per-chunk CRC chaos: corrupt/loss/latency | fail-closed тесты |
| 6 | Edgehash Cloudflare Worker recipe | рабочий пример |
| 7 | Edgehash Nginx/Envoy recipe + cache headers | рабочий пример |
| 8 | Mobile SDK: pinning/Bearer/offline | alpha SDK |

Критерий выхода: G1/G3/G6 evidence, security gaps P0 закрыты или явно
перечислены; edgehash примеры воспроизводимы.

### Sprint 3 — Product & community (4 недели)

| # | Срез | Выход |
|---|---|---|
| 1 | Edgehash standalone release/versioning | v0.1 |
| 2 | Edgehash benchmarks/limits | отчёт |
| 3 | SQL 5-minute quickstart | tutorial |
| 4 | Edgehash 10-minute quickstart | tutorial |
| 5 | Multi-arch image + chart install/upgrade/rollback | release |
| 6 | Protocol versioning / mixed-version test | compatibility report |
| 7 | CONTRIBUTING/RFC templates | docs |
| 8 | Final production gate audit | scorecard |

## 5. Зависимости и риски

- **Raft-файлы**: срезы 1 и 4 Sprint 1 оба трогают `internal/raft`; один
  владелец файлов, второй — только новые тесты/CRC-поля, иначе конфликт.
- **PITR**: без 1PC barrier wiring restore может пропустить commit; срез 3
  зависит от `internal/mvcc/barrier.go`.
- **Security**: RBAC/audit нельзя делать после Edgehash; это P0.
- **Edgehash**: API freeze до SDK/recipes, иначе придётся переписывать примеры.
- **Soak**: 72 ч и 1 ТБ нельзя «ускорить»; нужен отдельный стенд, не CI.
- **8 агентов**: максимум 8 параллельных срезов; при конфликте файлов дробить
  на 2 волны, а не запускать 16.

## 6. Что не делать

- Не переписывать joint consensus с нуля.
- Не позиционировать 8.6k tx/s как production SLO.
- Не выпускать edgehash SDK до freeze API.
- Не добавлять SQL-фичи до G1/G3 evidence.
