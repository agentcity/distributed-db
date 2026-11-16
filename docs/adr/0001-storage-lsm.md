# ADR 0001: LSM на Pebble как storage engine

- Статус: accepted
- Дата: 2026-11-16
- Контекст: M1 KV v2

## Контекст

v1 хранит `map[string][]byte` в RAM и WAL, а Raft-лог читает целиком при
рестарте. Для базы на 1–10 ТБ нужен движок, который:

- держит на диске больше, чем RAM;
- даёт упорядоченные range-скан;
- умеет атомарные batch, snapshot и быстрый reopen;
- не требует cgo и внешнего сервиса;
- хорошо ложится на Go и умеет compression/block cache.

## Решение

Использовать **Pebble v1.1.5** (LSM от CockroachDB) как `internal/storage`:

- memtable + SSTable + leveled compaction;
- block cache, bloom-фильтры, snappy/zstd;
- `Batch`, `Snapshot`, `Iterator` как базовые примитивы;
- WAL Pebble — durable log для state machine;
- параметры (cache, memtable, max open files, sync) выносятся в `Options`.

Ключи — произвольные байты, порядок лексикографический. MVCC и range-кодирование
живут выше, storage не знает о таблицах и транзакциях.

## Альтернативы

| Вариант | Почему нет |
|---|---|
| Badger | тоже LSM, но своя модель транзакций и меньше контроля над compaction; Pebble ближе к TiKV/CockroachDB |
| bbolt | B+tree, отличный для read-heavy, но write amplification и отсутствие LSM-компакции на больших объёмах |
| RocksDB (cgo) | быстрее на некоторых нагрузках, но cgo усложняет сборку/деплой, а выигрыш не гарантирован |
| Свой LSM | годы работы и багов; нет смысла для M1 |
| Оставить map+WAL | не масштабируется по объёму и рестарту |

## Последствия

- Появляется tuning: cache/memtable/compaction/sync — без него легко получить
  плохой p99.
- Нужны метрики Pebble (disk size, memtable, stalls) и алерты.
- `SyncWrites` — компромисс: durability против latency; для Raft-лога
  синхронность обязательна, для state machine можно group commit.
- Snapshot Pebble используется для консистентного чтения и будущих backup.
- Один процесс — один каталог; multi-tenant изоляция появится выше.

## Проверка

- CRUD, batch, iterator, snapshot, reopen, concurrency, `-race`.
- Benchmark Set/Get/Scan на NVMe; сравнение с v1 map+WAL.
- Рестарт на 100 ГБ данных — цель < 30 с.
