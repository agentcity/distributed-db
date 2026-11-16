# Скорость: честный разбор (Sprint 2)

- Дата: 2026-11-17
- HEAD: `b588e24`
- Железо: Apple M1 Pro (10 CPU), macOS, loopback, Go 1.23.5
- Fast-профиль: [speed_bench_test.go](../../test/speed_bench_test.go#L119), 1 meta + 3 KV в процессе
- Production-профиль: `sync` по умолчанию ([cmd/kv2/main.go:34](../../cmd/kv2/main.go#L34))

## 1. Что измерено

Запускал `BenchmarkV2Txn3Node`, `...Parallel`, `V2TxnBatch3Node`, `TestLargeBenchHarness` (0,05 GiB, sync), `CommitSingleConcurrent`, `ProposeParallel`, `AllocTS` — без фоновой нагрузки. Не запускал 1 ТБ ([large_bench_test.go](../../test/large_bench_test.go) — harness без прогона), PostgreSQL, реальную сеть. SQL/edgehash-бенчмарков в репозитории нет.

[PERFORMANCE.md](../PERFORMANCE.md) указывает HEAD `8c173bf`, хотя файл появился позже (`b733726`), а batch-код — ещё позже (`bee83c0`, `da8147f`); HEAD не соответствует ни документу, ни описанному коду.

## 2. Скоркарта

| Направление | Оценка (1–10) | Измерено сейчас | В docs | Confidence |
|---|---:|---|---|---|
| 1 клиент, fast | 6 | ≈12,0 мс, ≈83 tx/s | 11,6 мс / 86 tx/s | высокая |
| 10 клиентов, fast | 4 | ≈1,6 мс/op (wall), ≈630 tx/s, ≈16 мс/клиент | 1,8 мс/op, «5,5k tx/s» | средняя |
| Batch 256, fast | 5 | 200 µs/txn, ≈5,0k tx/s, latency батча 51 мс | 0,15 мс/txn, 6,6–6,9k | средняя |
| Get / Scan | 7 | 109 µs; 512 строк за 819 µs | нет | высокая |
| Sync, 1 клиент | 2 | p50 25,2 мс, p99 37,8 мс, 40 tx/s | «медленнее» | высокая |
| Sync, 10 клиентов | 1 | p50 151 мс, p99 176 мс, 70 tx/s | «медленнее» | высокая |
| SQL | 3 | нет данных; оценка 0,3–0,5 мс read / 12–26 мс write | нет | низкая |
| Edgehash | — | нет данных | нет | нет |

«10 клиентов ≈630 tx/s» — это `ns/op` из `b.RunParallel`, то есть wall-clock/N, а не задержка клиента. Похоже, «5 500» получено умножением `1,8 мс/op` на 10; корректно это ≈555 tx/s. Потолок — 10/12 мс ≈ 850 tx/s; 630 — 73%.

## 3. Бюджет записи: fsync — главная статья

| Компонент | Измерено | Комментарий |
|---|---:|---|
| `os.File.Sync` (Go, F_FULLFSYNC), 50 итераций | p50 4,0 мс; p95 7,9 мс | Python `os.fsync` даёт 0,05 мс — другой syscall |
| Raft Propose, 1 узел, 10 конкурентов, group commit | 505 µs/op (≈2,0k/s) | `AppendBatch`: один fsync на батч |
| Raft Propose, per-entry | 3,99 мс/op (≈250/s) | один fsync на запись |
| Fast, 3 узла, 1 клиент | ≈12,0 мс | лидер + кворум: 2 fsync + окна + apply |
| Sync, 3 узла, 1 клиент | p50 25,2 мс | + fsync state machine |

Модель (оценка): 12 мс ≈ 2 fsync (4+4 мс) + окна 1,5 мс ([batch.go](../../internal/kvserver/batch.go#L23) 500 µs + [node.go](../../internal/raft/node.go#L39) 1 мс) + gRPC/apply. Sync добавляет ≈4 мс на state machine — сходится с 25 мс.

«Fast» не отменяет fsync Raft-лога, а убирает лишь второй fsync state machine; 12 мс — пол F_FULLFSYNC диска, не сети.

## 4. Production sync против fast

| Профиль | 1 клиент | 10 клиентов | Чтение |
|---|---:|---:|---:|
| fast | ≈12 мс / 83 tx/s | ≈1,6 мс/op, ≈630 tx/s | 109 µs |
| sync | p50 25,2 мс / 40 tx/s | p50 151 мс / 70 tx/s | seq 4,6k/s, p99 0,66 мс |

Sync в 2,1× медленнее на одном клиенте; на 10 — в 9× по throughput и 9,5× по p50. Причина — fsync на каждую транзакцию в Raft-батче: [service.go:91-92](../../internal/kvserver/service.go#L91-L92) применяет команды циклом, каждая вызывает `CommitSingle` с отдельным Pebble batch ([commit_single.go:177](../../internal/mvcc/commit_single.go#L177)) и `SyncWrites`. 10 коммитов коалесцируются в один Raft-entry, а 10 fsync'ов идут последовательно в apply loop: конкурентность превращается в задержку.

Серверный бенчмарк: batched 4,30 vs direct 5,28 мс/commit — 1,23×, не 10×. Оценка (confidence средне-высокая): в sync потолок batch-режима ≈230–250 tx/s на лидера. Обещание «batch 256 ≈6,6–6,9k» — только для fast. Что двинет: один Pebble batch на Raft-батч (один fsync, оценка 5–10×), pipeline apply или fast + crash-replay.

## 5. Batch и окна

| Размер batch | Latency батча | На txn | Throughput |
|---:|---:|---:|---:|
| 1 | 13,8 мс | 13,8 мс | ≈73/s |
| 16 | 13,9 мс | 871 µs | ≈1,15k/s |
| 64 | 16,6 мс | 259 µs | ≈3,85k/s |
| 256 | 51,1 мс | 200 µs | ≈5,0k/s |

200 µs/txn — амортизация: батч на 256 ждёт ≈51 мс. Рост 14→51 мс — цена throughput.

Потолок: `maxBatch=64` команд за Raft-round ≈12,8 мс → ≈5,0k tx/s, мы ровно на нём. Дальше нужен `maxBatch` > 64, параллельный apply или несколько range-лидеров. Окна дают 1,5 мс фиксированной латентности (12% от 12 мс).

## 6. Чтение, TSO/meta

Get: 109 µs (≈9,2k/s), p95 317 µs (24 замера). Scan 512 строк: 819 µs, 683 КБ аллокаций, 1,6 µs/строка; storage-скан 10k ключей — 458 µs (46 нс/ключ), overhead 35× на RPC/MVCC/аллокации, не Pebble. Sync-чтение: seq 4,6k/s, p99 0,66 мс; 10 клиентов 17,3k/s, p99 2,88 мс. Чтение fsync не платит, но платит ReadBarrier.

TSO: 1,49 µs амортизированно (0,008 meta-RPC/op), 128 µs без батчинга — не горло. В стенде meta standalone; replicated meta гонит каждый 128-й timestamp через Raft (оценка: ≈39 коммитов/s при 5k tx/s). Idle-probe 50 мс при sync-паузах 150 мс — probe на каждый Begin (оценка).

## 7. SQL и edgehash: не измерено

SQL-бенчмарка нет. По коду `Execute` = Begin (локальный TSO) + Get каталога + план + Get/Scan данных + Commit. Read-only SELECT по PK: 2 Get ≈ 0,3–0,5 мс; INSERT/UPDATE/DELETE: + CommitSingle, то есть ≈12 мс (fast) или ≈25 мс (sync). Index scan — +1 Get на строку; join — два полных скана в память без pushdown. Оценка, confidence низкая.

Edgehash: бенчмарков нет. Локальный Bloom — память и k хешей; промах — HTTP + Merkle-proof; загрузка — чтение + SHA-256 + zstd. Чисел нет: нечем подтвердить.

Диск: 51,2 MiB логических → 579 MiB на 3 узлах (11,3×, 3,8× на реплику), preload 4,9 MiB/s. Для 1 ТБ — ≈59 ч preload и ≈11 ТБ суммарно, 3,8 ТБ на узел (оценка); compaction под нагрузкой не проверялся.

## 8. p99, loopback, 1 ТБ

- Fast p99 нет: speed-bench даёт только mean, smoke p95 на 24 операциях бесполезен.
- Sync p99 — на 121–220 write-замерах, гистограмма ±6%.
- Batch p99, SQL p99, edgehash p99 отсутствуют.
- Всё на loopback: нет RTT, потерь. Оценка: +0,5 мс RTT → +≈1 мс на запись; WAN 20 мс → +20–40 мс.
- 1 ТБ: harness есть, прогона нет; проверено 51 MiB.

## 9. Узкие места по рангу

| # | Горло | Доказательство | Что двинет | Эффект |
|---:|---|---|---|---|
| 1 | F_FULLFSYNC на Raft-append | 4–8 мс; 12 мс end-to-end | NVMe/Linux (fsync 0,1–0,5 мс), реже fsync, pipeline | 5–10× (оценка) |
| 2 | fsync на каждую команду в sync-батче | 151 мс, 70 tx/s; batched 1,23× | один Pebble batch на Raft-batch | 5–10× (оценка) |
| 3 | Сериализация append/apply | batch-потолок 5k/s | maxBatch > 64, parallel apply, multi-range | до 2–4× (оценка) |
| 4 | Окна 500 µs + 1 мс | 1,5 мс на коммит | адаптивное окно | −10% latency |
| 5 | Apply/encode в batch | 200–260 µs/txn | бинарный codec, batch-apply | до 2× (оценка) |
| 6 | Аллокации Scan | 683 КБ / 512 строк | reuse буферов, потоковая отдача | меньше GC |
| 7 | TSO/meta в replicated | не измерено | lease, больший batch | неизвестно |

## 10. Что не доказано

- p99 fast/batch и tail на реальной сети; 1 ТБ — preload, compaction, read amp, деградация за часы.
- SQL и edgehash: ни одной цифры, только структурные оценки.
- Масштабирование 3/5/7 узлов и несколько range-лидеров.
- PostgreSQL: цифры из docs не перепроверялись; sync 70 tx/s против заявленных 6,8k — это ≈97× (производная оценка).
- Расхождение 5,5k vs 630 tx/s: похоже на ошибку единиц, исходный прогон не воспроизведён.

## 11. Вывод

Fast воспроизводится на одном клиенте (≈12 мс) и в batch (≈5k tx/s), но не на 10 клиентах: реально ≈630 tx/s, а не 5,5k. Production sync — не «немного медленнее»: 151 мс p50 и 70 tx/s на 10 клиентах, потому что state machine fsync'ится на каждую транзакцию внутри батча. Пока это не исправлено, batch-обещания относятся к fast, не production-safe.

Оценка для production: 3/10; для lab/fast: 6/10. Уверенность в цифрах — средняя, в выводе про sync — высокая.
