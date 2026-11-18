# Надёжность и корректность: аудит и классификация отказов

- Дата: 2026-11-17
- Область: только анализ. Код и существующие документы не менялись.
- Прочитано: `internal/raft` (membership, snapshot, leader transfer, node),
  `internal/mvcc` (2PC, recovery, GC), `internal/kvserver`, `internal/metaserver`,
  `internal/v2client`, `internal/backup`, `test/jepsen*`,
  `test/crash_consistency_test.go`, `test/cross_range_*`, `test/split_merge_test.go`,
  ADR 0005/0006, `docs/PRODUCTION-PLAN.md`.

## 1. Вердикт

Компоненты в happy path зрелые: 1PC/2PC с идемпотентным повтором, chunked
snapshot, детерминированные split/merge prep, Raft-лог с fsync на каждую запись.
Но production-профиль по-прежнему не безопасен. Четыре блокера:

1. **Decommission не подключён к RPC/CLI**, а joint consensus не проверен
   1000 random changes под failover. Config в snapshot и joint quorum есть;
   операционная ошибка смены состава остаётся главным риском.
2. **Split/merge finish реплицирован и переживает crash**, но переход
   meta↔ranges.json не атомарен: crash в окне оставляет старую карту до retry.
3. **Lifecycle есть, но не отмасштабирован**: periodic snapshot/GC/ENOSPC
   guard реализованы; GC сканирует range целиком, snapshot на follower не
   gated лидерством, Raft WAL I/O вне disk guard.
4. **Backup не доказан**: ScanForBackup RPC и lock-aware scanner есть, но
   1PC apply barrier не подключён к backup, PITR archive не интегрирован.

Блокеров четыре: membership, split/merge, storage lifecycle, backup.

## 2. Классификация отказов

Класс: **S** — safety/потеря подтверждённого, **D** — durability, **L** —
liveness/доступность, **C** — capacity. Вероятность — при эксплуатации 3–5 узлов
без дополнительных мер.

| # | Отказ | Класс | Blast radius | Вероятность | Доказательство |
|---|---|---|---|---|---|
| R1 | Pre-vote и 1000-change stress в CI; нет multi-host stress и decommission под failover | S | кластер: ошибка оператора | средняя | [kvserver/decommission.go](../../internal/kvserver/decommission.go), [raft/joint.go](../../internal/raft/joint.go) |
| R2 | Joint quorum реализован, но configInFlight/lease и snapshot install требуют multi-host проверки | S | range: потеря конфигурации | низкая–средняя | [raft/joint.go](../../internal/raft/joint.go) |
| R3 | meta/Raft divergence: `AddReplica` ранний возврат по `containsAddr`; `RemoveReplica` меняет Raft раньше карты | S (control plane) | range: карта врёт, починки нет | средняя | [metaserver/membership.go:58](../../internal/metaserver/membership.go#L58), [:162](../../internal/metaserver/membership.go#L162) |
| R4 | Split/merge finish реплицирован, но meta↔ranges.json переход не атомарен; crash в окне оставляет старую карту | S/D | range (оба ребёнка) | средняя при crash | [kvserver/split.go](../../internal/kvserver/split.go), [service.go](../../internal/kvserver/service.go) |
| R5 | Admin record переживает restart, но snapshot install обновляет in-memory markers только на следующем apply | L | range: временный freeze | низкая | [kvserver/split.go](../../internal/kvserver/split.go) |
| R6 | Восстановление снапшота пишет в живой engine до durable-фиксации; крах = смесь старого и нового | D | реплика, затем range | низкая–средняя (большой снапшот) | [raft/node.go:922](../../internal/raft/node.go#L922), [kvserver/snapshot_stream.go:505](../../internal/kvserver/snapshot_stream.go#L505) |
| R7 | Periodic snapshot/GC есть, но GC сканирует весь range и может быть memory-heavy; snapshot на follower не gated лидерством | C/L | узел: pause/память на большом range | средняя на 100+ ГБ | [kvserver/lifecycle.go](../../internal/kvserver/lifecycle.go), [mvcc/gc.go](../../internal/mvcc/gc.go) |
| R8 | ENOSPC guard покрывает MVCC/RPC; прямой Raft WAL/snapshot I/O не обёрнут, окно до statfs | L/D | узел и его range | низкая–средняя | [kvserver/lifecycle.go](../../internal/kvserver/lifecycle.go) |
| R9 | Крах между DeleteRange/cfg update и ranges.json не атомарен; данные могут остаться под новым префиксом | D | range | низкая–средняя | [kvserver/split.go](../../internal/kvserver/split.go) |
| R10 | TTL лока по локальным часам против TSO; lease чтений без запаса на drift | S | транзакция/ключ | низкая–средняя (skew > 3 s) | [mvcc/lock.go:52](../../internal/mvcc/lock.go#L52), [mvcc/mvcc.go:71](../../internal/mvcc/mvcc.go#L71) |
| R11 | ScanForBackup RPC, strict RemoteScanner и backup barrier есть; PITR archive не интегрирован, remote strict требует barrier | D | бэкап: пропущенный commit | средняя при активной записи | [backup/scanner.go](../../internal/backup/scanner.go), [mvcc/barrier.go](../../internal/mvcc/barrier.go), [pitr](../../internal/pitr) |



## 3. Failover, RTO и стоимость

Отказ лидера range укладывается в единицы секунд: продовые heartbeat 100 мс и
election timeout 300–600 мс, `ReadBarrier` требует кворума и шага apply
([node.go:459](../../internal/raft/node.go#L459)), `TransferLeadership` ждёт
catch-up и проверяет term ([leader_transfer.go:33](../../internal/raft/leader_transfer.go#L33)).
Но RTO < 30 мин не обеспечен: он определяется replay лога (R7) и полным
снапшотом при добавлении реплики. Хуже: lease чтений равен ElectionTimeoutMin,
а follower стартует выборы уже на этом же интервале — окно stale-чтения при
«медленных» часах лидера не закрыто.

## 4. Backup и PITR

Логический backup сделан аккуратно: один Txn на все range, staging + rename,
recheck карты range ([backup.go:119](../../internal/backup/backup.go#L119)).
Lock-aware scanner закрывает in-process store; remote path не видит локи на
никогда не коммитившихся ключах (R11), а 1PC apply barrier не закрыт. Archive,
global checkpoint и restore-to-TSO отсутствуют — ADR 0006 остаётся проектом.
RPO 0 недостижим: `durability=sync` защищает только от потери одного узла.

## 5. Пробелы тестов

| Область | Что есть | Чего нет |
|---|---|---|
| Membership | joint consensus, pre-vote, config snapshot/epoch, decommission CLI, 1000-change CI stress | нет multi-host |
| Crash | 1 узел, 32 ключа, SIGKILL, sync/fast | нет power-loss, краха в 2PC/split/snapshot, ENOSPC, slow disk |
| 2PC | recovery после commit primary, commit-incomplete, resolver, response loss | нет смерти клиента с осиротевшим secondary-локом, нет краха во время commit |
| Split/merge | freeze/unfreeze, generation-check, duplicate finish, copy-failure | нет краха/рестарта с prepare в логе, replicated clear, расхождения реплик |
| Backup | ScanForBackup RPC, strict RemoteScanner, barrier, checksums | нет PITR/restore-drill, RPO/RTO |
| Soak/clock | nightly 3×20 мин; TSO-монотонность на 1 узле | нет 72 ч, multi-node skew, lease-skew, linearizability checker |

## 6. Топ-5 инвестиций (по порядку)

| # | Инвестиция | Зачем | Эффект | Оценка |
|---|---|---|---|---|
| 1 | Multi-host joint stress + decommission failover | R1/R2 | закрывает остаточный G2-риск | 4–6 pw |
| 2 | Lifecycle hardening: GC memory/range, snapshot leadership gate, Raft WAL ENOSPC | R7/R8 | держит RTO и диск на 100+ ГБ | 3–4 pw |
| 3 | Атомарный split/merge: split как реплицированная запись, снапшот ребёнка от лидера, идемпотентное завершение и разморозка | R4/R5 | убирает потерю ack и расхождение реплик | 4–6 pw |
| 4 | Crash/chaos-контур: инъекции в 2PC/commit/snapshot/split, ENOSPC, power-loss, 72 ч soak, linearizability checker | нет доказательств для G1/G6 | превращает «зелёный CI» в проверяемые инварианты | 4–6 pw |
| 5 | PITR integration + 1PC barrier wiring + restore drill, RPO/RTO | R11, G3 | бэкап становится пригодным для восстановления | 8–12 pw MVP |

Порядок не случаен: (1) и (3) — safety, (2) — предотвращает отказ, который
случится сам; (4) даёт доказательства, без которых (1)–(3) нельзя принимать;
(5) можно делать параллельно, но не раньше (2), иначе бэкап нечего
восстанавливать на 1 ТБ.

## 7. Runbooks (минимум)

Runbooks нет. Нужны шесть, каждый с проверками и точкой отказа:

1. **Потеря кворума range** — живые реплики, `Status`, когда допустим
   `RemoveReplica`, когда нужен restore.
2. **Degraded meta / TSO** — статический состав meta, порядок рестарта,
   проверка монотонности TSO после failover.
3. **Зависший split/merge** — признак `range frozen`, безопасный retry,
   когда размораживать вручную, как сверять карту range.
4. **Disk full / ENOSPC** — что отключать, как освобождать (snapshot, GC),
   почему нельзя удалять raft-каталоги.
5. **Stale node rejoin / decommission** — почему нельзя поднимать старый диск,
   как использовать `AddReplica`/`ForceLearner`, когда стирать каталог.
6. **Restore из backup** — verify манифеста, изолированный кластер, проверка
   транзакций и переключение эпохи.

## 8. Вывод

Корректность ядра 1PC/2PC и Raft-логирования в happy path подтверждается кодом
и тестами. Надёжность системы ограничена не качеством этих путей, а четырьмя
разрывами: состав не в снапшоте, split/merge не атомарны, lifecycle хранения
отсутствует, backup не консистентен. Пока (1)–(3) не закрыты, эксплуатация
допустима только как dev/test-стенд; RPO 0 и RTO < 30 мин не обеспечены ни
механизмом, ни измерением.
