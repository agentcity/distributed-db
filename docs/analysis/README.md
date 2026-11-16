# Sprint 2: аудит проекта по критериям

- Дата: 2026-11-17
- HEAD: `843e670` (после gRPC TLS)
- Метод: чтение кода + существующие тесты/бенчмарки; без 1 ТБ, без реальной
  сети, без пентеста.
- Детали: [speed.md](speed.md), [data-volume.md](data-volume.md),
  [security.md](security.md), [reliability.md](reliability.md).

## 1. Сводная оценка

| Критерий | Оценка (1–10) | Вердикт |
|---|---:|---|
| Скорость, lab/fast | 6 | 12 мс single, ~480–820 tx/s parallel, batch ~5–7k tx/s |
| Скорость, production sync | 3 | ~44–185 tx/s после batch-фикса; было ~40–70 |
| Потенциал объёма | 3 | комфортно ≤50 ГБ; 50–300 ГБ рискованно; 1 ТБ — нет |
| Безопасность | 3 | gRPC TLS/mTLS есть, но опционален; нет RBAC/audit/at-rest |
| Надёжность | 3 | happy path крепкий, 4 production-блокера |
| **Production readiness** | **3** | **pre-alpha / trusted-network prototype** |

Оценки — суждение на основе кода и измерений, не SLA.

## 2. Десять главных выводов

1. **Ошибка в старых доках по скорости исправлена.** `ns/op` в parallel
   benchmark — это время одной операции, а не десяти. Fast 10 клиентов:
   ~480–820 tx/s, не 5.5k. Batch 256 даёт ~5–7k tx/s только амортизированно,
   при latency batch 36–51 мс.
2. **Production `sync` был главным регрессом и исправлен.** Per-command
   fsync внутри Raft-батча давал ~70 tx/s; один Pebble batch/fsync на батч
   поднял до ~185 tx/s. Это всё ещё ~37× медленнее PostgreSQL, но не ~100×.
3. **Объём ограничен lifecycle, а не диском.** MVCC GC не вызывается в
   production, Raft WAL не снапшотится периодически, replay читает WAL в RAM,
   backup квадратичен. 1 ТБ диска влезет, процесс — нет.
4. **Split/merge не атомарны.** Каждая реплика копирует своё состояние;
   запись, попавшая после prepare, может потеряться, новые группы расходятся.
   Ошибка после prepare оставляет range frozen.
5. **Membership — главный safety-блокер.** Конфигурация состава не входит в
   snapshot, joint consensus нет. Узел, пропустивший ≥2 changes и получивший
   snapshot, может подтвердить запись не тем кворумом.
6. **Backup не консистентен и без PITR.** `Scan` игнорирует locks; commitTS
   мог быть выдан до скана. RPO 0 не обеспечен.
7. **gRPC TLS/mTLS теперь есть**, включая 3-node Raft, meta и client; но
   plaintext остаётся default, а admin RPC всё ещё на data-порту, без RBAC и
   audit. SQL HTTP (`:8080`) и pgwire — отдельные слабые места.
8. **Админ-команды появились:** `kvctl ranges/status/split/merge/backup`,
   meta-only где возможно. Split/merge делегируют safety metaserver, CLI не
   заменяет проверки.
9. **Read path хорош:** Get ~109 µs, Scan 512 строк ~819 µs, TSO не главное
   горло. Основные потери — fsync, apply и аллокации.
10. **Честный статус:** private single-tenant production требует 12–15
    месяцев; без закрытия membership/split/lifecycle/backup — только dev/test.

## 3. Скорость: что реально и что делать

| Режим | Latency | Throughput |
|---|---:|---:|
| fast, 1 клиент | ~12 мс | ~83 tx/s |
| fast, 10 клиентов | ~1.2–2.1 мс/op | ~480–820 tx/s |
| batch 256, fast | ~36–51 мс/batch | ~5–7k tx/s |
| sync, 1 клиент | ~22.6 мс | ~44 tx/s |
| sync, 10 клиентов | ~5.4 мс/op | ~185 tx/s |

Приоритеты:

1. **Один Pebble batch/fsync на Raft-батч** — сделано, ~2.6× по throughput;
   следующий шаг — pipelining/parallel apply.
2. NVMe/Linux, где fsync дешевле; pipelining apply.
3. Увеличить batch/parallel apply; не путать амортизированный throughput с
   latency.
4. 1 ТБ harness прогнать на реальной сети/NVMe.

## 4. Объём: конверт

| Измерение | Комфортно | Рискованно | Нет |
|---|---|---|---|
| Range | ≤128 МБ | 128 МБ–1 ГБ | ≥2 ГБ |
| Кластер | ≤50 ГБ | 50–300 ГБ | ≥1 ТБ |
| Range/узел | ≤200 | 200–500 | >500 |
| Значение | ≤1 МБ | 1–8 МБ | >12 МБ |
| Backup | ≤10 ГБ | 10–30 ГБ | ≥50 ГБ |

Нужны: MVCC GC + safe point, периодический snapshot/compaction, лимит
range'ей, линейный backup, split без полного freeze.

## 5. Безопасность: что закрыто и что нет

Закрыто в Sprint 2:

- gRPC TLS 1.3/mTLS для KV, meta, Raft;
- v2client TLS для meta и KV;
- CLI-флаги kv2/sqlsrv/pgsrv/kvctl;
- тесты: mTLS 3-node Raft, отказ plaintext/certless.

Не закрыто:

- TLS **опционален**; по умолчанию plaintext;
- admin RPC на том же порту; нет отдельного admin-listener;
- SQL HTTP без TLS/auth; pgwire — один статический пользователь;
- нет RBAC, audit, encryption at rest, KMS;
- backup не шифруется; kvbackup пока plaintext;
- edgehash: legacy HMAC, статический Bearer, TLS опционален.

## 6. Надёжность: четыре блокера

1. Membership: config in snapshot + joint consensus + decommission.
2. Split/merge: реплицированный child snapshot, идемпотентный finish,
   разморозка при ошибке.
3. Storage lifecycle: periodic snapshot, MVCC GC, ENOSPC read-only, disk metrics.
4. Backup: lock-aware Scan/base barrier, PITR, restore drill, RPO/RTO.

Тестовые пробелы: 72 ч soak, power-loss, ENOSPC, split/merge failure injection,
linearizability checker, restore drill.

## 7. Что сделано в Sprint 2

- `kvctl` admin: ranges/status/split/merge/backup.
- gRPC TLS/mTLS для KV/meta/Raft и клиентов.
- Аудит скорости/объёма/безопасности/надёжности.
- Исправлены единицы benchmark и production-sync цифры в документации.

## 8. Итог

**Не production.** Сильные стороны: Raft/Multi-Raft, MVCC/2PC, range split,
тесты failover, gRPC TLS, удобный CLI. Слабые: production sync batching,
membership safety, split atomicity, storage lifecycle, backup/PITR, security
perimeter. До private single-tenant production — 12–15 месяцев, начиная с
membership/split/lifecycle/backup, а не с новых фич.
