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

1. **Конфигурация состава не входит в снапшот**, joint consensus нет. Узел,
   пропустивший ≥2 изменения и установивший снапшот, навсегда остаётся со старым
   voter set и может подтвердить запись не тем кворумом (F4 из ADR 0005).
2. **Split/merge не атомарны и не реплицированы**: каждая реплика копирует своё
   локальное состояние и создаёт собственную группу. Записи, попавшие в окно
   между prepare и копией, теряются, а состояния новых групп расходятся.
3. **Нет lifecycle хранения**: периодического snapshot/compaction Raft-лога и
   MVCC GC в продакшене нет. WAL и версии растут неограниченно — это отложенный
   disk full и деградация RTO.
4. **Backup не является согласованным срезом**, PITR нет: `Scan` игнорирует
   локи, а commitTS мог быть выдан до скана.

Блокеров четыре: membership, split/merge, storage lifecycle, backup.

## 2. Классификация отказов

Класс: **S** — safety/потеря подтверждённого, **D** — durability, **L** —
liveness/доступность, **C** — capacity. Вероятность — при эксплуатации 3–5 узлов
без дополнительных мер.

| # | Отказ | Класс | Blast radius | Вероятность | Доказательство |
|---|---|---|---|---|---|
| R1 | Stale voter set после InstallSnapshot (конфига в снапшоте нет) | S | кластер: split brain, потеря ack | низкая–средняя (≥2 change + snapshot) | [raft/node.go:827](../../internal/raft/node.go#L827), [raft/types.go:161](../../internal/raft/types.go#L161) |
| R2 | Изменение состава подтверждается кворумом применяемой, а не лог-конфигурации; `configInFlight` только в памяти | S | range: потеря конфигурации | средняя при failover в change | [raft/membership.go:82](../../internal/raft/membership.go#L82), [:177](../../internal/raft/membership.go#L177) |
| R3 | meta/Raft divergence: `AddReplica` делает early-return по `containsAddr`, не сверяя Raft; `RemoveReplica` меняет Raft раньше карты | S (control plane) | range: карта врёт, починки нет | средняя | [metaserver/membership.go:58](../../internal/metaserver/membership.go#L58), [:162](../../internal/metaserver/membership.go#L162) |
| R4 | Split копирует состояние каждой реплики отдельно; поздние записи теряются в `DeleteRange`, новые группы расходятся | S/D | range (оба ребёнка) | средняя при записи во время split | [kvserver/split.go:47](../../internal/kvserver/split.go#L47), [service.go:485](../../internal/kvserver/service.go#L485) |
| R5 | Ошибка split/merge после prepare не снимает freeze (`clearSplit` не вызывается) | L | range: все записи отклоняются до рестарта | средняя (ошибка копирования/диска) | [service.go:515](../../internal/kvserver/service.go#L515), [group.go:161](../../internal/kvserver/group.go#L161) |
| R6 | Восстановление снапшота пишет в живой engine до durable-фиксации; крах = смесь старого и нового | D | реплика, затем range | низкая–средняя (большой снапшот) | [raft/node.go:922](../../internal/raft/node.go#L922), [snapshot_stream.go:505](../../internal/kvserver/snapshot_stream.go#L505) |
| R7 | Нет периодического snapshot/compaction Raft-лога | C/D | узел: рост WAL, replay и RTO | высокая на дистанции | [kvserver/server.go:484](../../internal/kvserver/server.go#L484) (только ручной вызов) |
| R8 | MVCC GC не вызывается в продакшене | C | узел: рост версий и локов → disk full | высокая | [mvcc/gc.go:33](../../internal/mvcc/gc.go#L33); вызовов вне тестов нет |
| R9 | Нет обработки ENOSPC: fsync-ошибка не переводит узел в read-only, метрик диска нет | L/D | узел и его range | средняя | в `internal/*` нет `Statfs`/ENOSPC-политики |
| R10 | TTL лока считается по локальным часам против TSO; lease чтений без запаса на drift | S | транзакция/ключ | низкая–средняя (skew > 3 s) | [mvcc/lock.go:52](../../internal/mvcc/lock.go#L52), [mvcc/mvcc.go:71](../../internal/mvcc/mvcc.go#L71), [raft/node.go:2098](../../internal/raft/node.go#L2098) |
| R11 | Осиротевшие cross-range вторичные локи: серверный resolver их пропускает, записи транзакции нет | L | ключ: чтения блокируются до появления трафика | средняя | [kvserver/server.go:412](../../internal/kvserver/server.go#L412) |
| R12 | Backup не консистентный срез: `Scan` не видит локи, commitTS мог быть выдан до скана | S (backup) | бэкап: частичная транзакция | средняя (2PC во время бэкапа) | [mvcc/read.go:112](../../internal/mvcc/read.go#L112), [backup/backup.go:98](../../internal/backup/backup.go#L98), ADR 0006 G-4 |
| R13 | Нет pre-vote: stale узел принимается с более высоким term | L | range: election storm, term inflation | средняя при возврате диска | [raft/node.go:1117](../../internal/raft/node.go#L1117) |

Замечания к таблице:

- **R1** — единственный сценарий потери подтверждённых данных без отказа диска.
  Арифметика ADR 0005 верна: кворумы конфигураций, различающихся на два шага, не
  пересекаются (`{B,C} ∩ {A,D,E} = ∅`).
- **R2** усиливает R1: `commitIndex` не персистится, config-записи не
  переигрываются ([node.go:192](../../internal/raft/node.go#L192)), поэтому даже
  одно изменение может оказаться неприменённым после рестарта.
- **R4/R5** — самая недооценённая пара. `split_prepare` фиксируется в Raft и
  замораживает реплики, но `isFrozen` проверяется только **до** `Propose`
  ([service.go:325](../../internal/kvserver/service.go#L325)): команда из batcher
  применится после prepare. Копия лидера может её включить, копия follower — нет,
  затем `DeleteRange` удалит правую половину. Итог: потеря ack и расхождение
  реплик новой группы, у которой пустой лог и нет механизма сверки.
- **R6** — документированный, но не обработанный случай: при крахе посреди
  restore на диске остаётся частично перезаписанный engine, а durable-снапшот
  указывает на старую точку; Raft доиграет лог поверх смеси.
- **R10** вероятен в облаке: TSO физически = часы meta-лидера, expiry считает
  любой kvserver по своим часам. При уходе вперёд > 3 с живая транзакция будет
  откачена, а после commit на primary — даст неустранимый `ErrCommitIncomplete`.
- **R9** усугубляется R7/R8: диск заполняется предсказуемо, система узнаёт об
  этом только через ошибку записи.

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
Но консистентность среза не гарантирована (R12), archive, global checkpoint и
restore-to-TSO отсутствуют — ADR 0006 остаётся проектом. RPO 0 недостижим:
`durability=sync` защищает только от потери одного узла.

## 5. Пробелы тестов

| Область | Что есть | Чего нет |
|---|---|---|
| Membership | 6 unit-тестов, learner-from-snapshot, ForceLearner | нет теста stale voter после snapshot; нет 1000 random changes под failover; нет config-в-снапшоте |
| Crash | 1 узел, 32 ключа, SIGKILL, sync/fast | нет power-loss, краха в 2PC/split/snapshot, ENOSPC, slow disk |
| 2PC | recovery после commit primary, commit-incomplete, resolver, response loss | нет смерти клиента с осиротевшим secondary-локом, нет краха во время commit |
| Split/merge | happy path, distributed split/merge, persist | нет инъекции отказа meta/диска, записи во время split, проверки расхождения реплик |
| Backup | roundtrip, verify, manifest | нет restore-drill с транзакцией в полёте, PITR, замеров RPO/RTO |
| Soak/clock | nightly 3×20 мин; TSO-монотонность на 1 узле | нет 72 ч, multi-node skew, lease-skew, linearizability checker |

## 6. Топ-5 инвестиций (по порядку)

| # | Инвестиция | Зачем | Эффект | Оценка |
|---|---|---|---|---|
| 1 | Конфиг в снапшоте + joint consensus + epoch/fencing | R1/R2 — единственный путь потери подтверждённых данных | снимает главный блокер production | 6–9 pw ядро |
| 2 | Периодический snapshot + MVCC GC + disk guard (ENOSPC → read-only, метрики) | R7–R9 | останавливает рост WAL/версий, держит RTO | 3–4 pw |
| 3 | Атомарный split/merge: split как реплицированная запись, снапшот ребёнка от лидера, идемпотентное завершение и разморозка | R4/R5 | убирает потерю ack и расхождение реплик | 4–6 pw |
| 4 | Crash/chaos-контур: инъекции в 2PC/commit/snapshot/split, ENOSPC, power-loss, 72 ч soak, linearizability checker | нет доказательств для G1/G6 | превращает «зелёный CI» в проверяемые инварианты | 4–6 pw |
| 5 | Backup как консистентный срез: lock-aware Scan (`ErrLocked`) или base barrier + fence карты; restore drill, RPO/RTO | R12, G3 | бэкап становится пригодным для восстановления | 3–4 pw MVP |

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
