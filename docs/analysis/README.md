# Аудит проекта по критериям

- Дата: 2026-11-17
- Метод: чтение кода + существующие тесты/бенчмарки; без 1 ТБ, без реальной
  сети, без пентеста.
- Детали: [speed.md](speed.md), [data-volume.md](data-volume.md),
  [security.md](security.md), [reliability.md](reliability.md).

## 1. Сводная оценка

| Критерий | Оценка (1–10) | Текущее состояние |
|---|---:|---|
| Скорость, fast | 7 | 12 мс single; pipeline 64–256 на 10 клиентах — 12–19.5k tx/s |
| Скорость, sync | 4 | pipeline 64–256 — 4–8.6k tx/s; один in-flight — ~200 tx/s |
| Потенциал объёма | 3 | комфортно ≤50 ГБ; 50–300 ГБ рискованно; 1 ТБ — нет |
| Безопасность | 3 | gRPC TLS/mTLS opt-in; нет RBAC/audit/at-rest |
| Надёжность | 3 | happy path крепкий; 4 production-блокера |
| **Production readiness** | **3** | **pre-alpha / trusted-network prototype** |

Оценки — суждение на основе кода и измерений, не SLA.

## 2. Ключевые выводы

1. **Fast-профиль:** 1 клиент ~12 мс; 10 клиентов с одним in-flight
   ~480–820 tx/s; batch 256 ~7k tx/s амортизированно.
2. **Sync-профиль:** один Pebble batch/fsync на Raft-батч; 10 клиентов с
   pipeline 256 — ~8.6k tx/s. Один in-flight latency-bound (~200 tx/s).
3. **Объём:** 1 ТБ диск вместит, но процесс — нет: MVCC GC не вызывается,
   Raft WAL не снапшотится периодически, split копирует всё под freeze,
   backup квадратичен.
4. **Membership:** конфигурация не в snapshot, joint consensus нет; узел
   после snapshot может подтвердить запись не тем кворумом.
5. **Split/merge:** не атомарны и не реплицированы; запись после prepare
   может потеряться, ошибка оставляет range frozen.
6. **Backup:** `Scan` игнорирует locks; PITR нет; RPO 0 не обеспечен.
7. **Безопасность:** gRPC TLS/mTLS реализован, но opt-in/plaintext default;
   SQL HTTP без TLS/auth; admin RPC на data-порту; нет RBAC/audit/at-rest.
8. **Админ-плоскость:** `kvctl ranges/status/split/merge/backup` есть;
   split/merge safety делегирована metaserver.

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

1. Membership: config in snapshot + joint consensus + decommission.
2. Split/merge: реплицированный child snapshot, идемпотентный finish,
   разморозка при ошибке.
3. Storage lifecycle: periodic snapshot, MVCC GC, ENOSPC read-only, disk metrics.
4. Backup: lock-aware Scan/base barrier, PITR, restore drill, RPO/RTO.

## 7. Итог

**Не production.** Сильные стороны: Raft/Multi-Raft, MVCC/2PC, range split,
failover tests, gRPC TLS, CLI. Слабые: single-range throughput, membership
safety, split atomicity, storage lifecycle, backup/PITR, security perimeter.
