# Аудит проекта по критериям

- Дата: 2026-11-17
- Метод: чтение кода + существующие тесты/бенчмарки; без 1 ТБ, без реальной
  сети, без пентеста.
- Детали: [speed.md](speed.md), [data-volume.md](data-volume.md),
  [security.md](security.md), [reliability.md](reliability.md).

## 1. Сводная оценка

| Критерий | Оценка (1–10) | Текущее состояние |
|---|---:|---|
| Скорость, fast | 6 | macOS 12 мс; Linux-контейнер ~6.8 мс; pipeline 256 — ~19.5k tx/s (1 range) |
| Скорость, sync | 4 | pipeline 256 — ~8.6k tx/s; один in-flight — ~200 tx/s |
| Потенциал объёма | 3 | комфортно ≤50 ГБ; 50–300 ГБ рискованно; 1 ТБ — нет |
| Безопасность | 3 | gRPC TLS/mTLS opt-in; нет RBAC/audit/at-rest |
| Надёжность | 4 | joint consensus/split recovery/backup barrier есть; остались wiring и PITR |
| **Production readiness** | **3** | **pre-alpha / trusted-network prototype** |

Оценки — суждение на основе кода и измерений, не SLA.

## 2. Ключевые выводы

1. **Fast-профиль:** 1 клиент ~12 мс; 10 клиентов с одним in-flight
   ~480–820 tx/s; batch 256 ~7k tx/s амортизированно.
2. **Sync-профиль:** один Pebble batch/fsync на Raft-батч; 10 клиентов с
   pipeline 256 — ~8.6k tx/s. Один in-flight latency-bound (~200 tx/s).
3. **Объём:** periodic snapshot/GC/ENOSPC guard есть, но GC сканирует весь
   range и может быть memory-heavy; 1 ТБ не прогнан.
4. **Membership:** joint consensus, config snapshot, decommission CLI и
   1000-change CI stress есть; pre-vote/multi-host остаются.
5. **Split/merge:** replicated finish/clear и crash recovery есть; meta↔
   ranges.json переход не атомарен.
6. **Backup:** ScanForBackup RPC и barrier API есть; 1PC barrier не подключён
   к backup, PITR archive не интегрирован; RPO 0 не доказан.
7. **Безопасность:** gRPC TLS/mTLS реализован, но opt-in/plaintext default;
   SQL HTTP TLS/Bearer и meta admin-token есть opt-in; KV admin RPC,
   RBAC/audit/at-rest — нет.
8. **Админ-плоскость:** `kvctl ranges/status/split/merge/backup` есть;
   split/merge safety делегирована metaserver.
9. **Multi-range:** 4/8 range на одном диске не быстрее одного range;
   fan-out требует разных узлов/дисков и достаточной нагрузки на range.

## 3. Скорость

| Режим | Latency | Throughput |
|---|---:|---:|
| fast, 1 клиент | ~12 мс | ~83 tx/s |
| fast, 10 клиентов, 1 in-flight | ~1.2–2.1 мс/op | ~480–820 tx/s |
| fast, pipeline 256 | ~138 µs/txn (batch ~35 мс) | ~7.2k tx/s |
| sync, 1 клиент | ~22.6 мс | ~44 tx/s |
| sync, 10 клиентов, 1 in-flight | ~5.4 мс/op | ~185 tx/s |
| sync, pipeline 256 | ~117 µs/txn (batch ~30 мс) | ~8.6k tx/s |

10 workstations, pipeline на клиента:

| Pipeline | fast | sync | sync batch latency |
|---:|---:|---:|---:|
| 1 | ~712 tx/s | ~200 tx/s | ~5 мс |
| 16 | ~4.4k tx/s | ~1.47k tx/s | ~11 мс |
| 64 | ~11.8k tx/s | ~3.98k tx/s | ~16 мс |
| 256 | ~19.5k tx/s | ~8.57k tx/s | ~30 мс |

## 4. Объём

| Измерение | Комфортно | Рискованно | Нет |
|---|---|---|---|
| Range | ≤128 МБ | 128 МБ–1 ГБ | ≥2 ГБ |
| Кластер | ≤50 ГБ | 50–300 ГБ | ≥1 ТБ |
| Range/узел | ≤200 | 200–500 | >500 |
| Значение | ≤1 МБ | 1–8 МБ | >12 МБ |
| Backup | ≤10 ГБ | 10–30 ГБ | ≥50 ГБ |

Блокеры: MVCC GC/safe point, periodic snapshot, лимит range’ей, линейный
backup, split без полного freeze.

## 5. Безопасность

Реализовано:

- gRPC TLS 1.3/mTLS для KV/meta/Raft, v2client, CLI;
- pgwire TLS + password (cleartext inside TLS);
- edgehash TLS/Bearer/rate limit, Ed25519-манифесты.

Не реализовано:

- TLS opt-in/plaintext default;
- SQL HTTP TLS/auth;
- отдельный admin-listener, RBAC, audit;
- encryption at rest, encrypted backups;
- `kvbackup` TLS;
- edgehash key rotation, mobile pinning.

## 6. Надёжность: четыре блокера

1. Membership: joint config/snapshot/decommission CLI есть; stress и
   multi-host доказательство остаются.
2. Split/merge: replicated finish/clear + crash recovery есть; meta↔range-map
   atomicity остаётся.
3. Storage lifecycle: periodic snapshot, MVCC GC, ENOSPC read-only есть.
4. Backup: ScanForBackup RPC + barrier API есть; PITR integration/restore drill
   остаются.

## 7. Итог

**Не production.** Сильные стороны: Raft/Multi-Raft, MVCC/2PC, range split,
failover tests, gRPC TLS, CLI. Слабые: single-range throughput, membership
safety, split atomicity, storage lifecycle, backup/PITR, security perimeter.
