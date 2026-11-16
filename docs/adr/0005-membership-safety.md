# ADR 0005: Membership safety — joint consensus, snapshot-resident configuration, decommission

- Статус: proposed (blocking production)
- Дата: 2026-11-16
- Контекст: Sprint 1 / production gate G2 (`docs/PRODUCTION-PLAN.md`)
- Связанные документы: `docs/PRODUCTION-PLAN.md`, `docs/ASSESSMENT.md`,
  `docs/PRODUCT-READINESS.md`, ADR 0003 (Multi-Raft)
- Реализация: не начата. Этот документ фиксирует решение и план; кода он не
  меняет.

> Язык: русский, как в ADR 0001–0004. Английские термины (joint consensus,
> commit, apply, snapshot, learner) оставлены как есть, потому что они уже
> используются в коде и документации. При необходимости документ переводится
> без изменения решений.

> **English summary (for reviewers).** Adopt joint consensus (`C_old,new`) plus
> the configuration in the Raft snapshot envelope, a monotonic config epoch, and
> a real decommission flow. The current single-server protocol with an in-memory
> `configInFlight` gate and membership only in a separate WAL record is not
> production-safe: a node that misses two or more config changes and then
> installs a snapshot keeps a stale voter set, can win an election with a quorum
> disjoint from the current one, and can acknowledge conflicting writes (F4).
> The "one change at a time + operator lock" option is insufficient: it does not
> survive a leader failover between commit and apply and does not repair stale
> snapshot configuration; it is acceptable only as a temporary freeze. Effort:
> 14–19.5 person-weeks (~8–11 calendar weeks with two engineers); it is not a
> Sprint 1 deliverable. The full argument is below in Russian.

## Решение (TL;DR)

Принять **joint consensus (C_old,new)** из диссертации Raft, добавить
**конфигурацию в Raft-снапшот** (конверт снапшота, не KV state machine),
ввести **эпоху конфигурации** и реализовать **decommission** на уровне meta.
Протокол «одно изменение за раз + operator lock» не является достаточным
решением: он не переживает смену лидера между commit и apply и не спасает от
устаревшей конфигурации после snapshot/compaction. Его допустимо использовать
только как временный аварийный режим (заморозка изменений состава).

Ключевой инвариант, которого сейчас нет:

> Конфигурация состава является частью реплицированного состояния: она
> восстанавливается из snapshot + лога, и любые два кворума, используемые для
> решений, пересекаются.

Без конфигурации в снапшоте узел, пропустивший **два и более** изменения состава
и получивший снапшот, навсегда остаётся со старым набором голосующих. Такая
нода может выиграть выборы кворумом, не пересекающимся с текущим, и вместе с
текущим лидером подтвердить конфликтующие записи. Это не деградация
доступности, а потеря подтверждённых данных (сценарий F4 ниже).

## 1. Текущая реализация и её инварианты

### 1.1 Что есть в коде

- Модель состава: `internal/raft/membership.go`.
  - `Node.voters` (включая локальную ноду), `Node.learners`;
  - операции `AddLearner`, `PromoteLearner`, `RemoveVoter` — ровно одно
    изменение за раз, каждая операция — один `model.Entry` с типом
    `config_add_learner` / `config_promote_learner` / `config_remove_voter`
    (`internal/model/entry.go`);
  - `changeMembership` ставит in-memory флаг `Node.configInFlight`, ждёт
    `Propose` (успешный `Propose` возвращается после apply), затем снимает флаг
    в `applyConfigEntry`;
  - `applyConfigEntry` детерминированно меняет `voters`/`learners` и пишет
    отдельную WAL-запись `SaveMembership` (`internal/raft/filestore.go`).
- Кворум: `Node.majority()` = `len(voters)/2 + 1`
  (`internal/raft/node.go:1941`). Используется в подсчёте голосов, в
  `maybeCommitLocked` (коммит), в `hasQuorumLocked` (lease/read barrier).
- Выборы: `HandleRequestVote` (`internal/raft/node.go:1103`) отклоняет
  кандидата, которого нет в локальном `voters`, но **сначала принимает более
  высокий term** от любого отправителя.
- Снапшот: `Snapshot`/`SnapshotMeta` содержат только `Index`, `Term`, `Data`
  (и размер/CRC для стриминга), `internal/raft/types.go:161`.
  `RestoreSnapshot`/`RestoreSnapshotStream` заменяют данные и лог, но **не
  трогают `voters`/`learners`.
- Персистентность: `walRecord{Type: "membership"}` — отдельная запись WAL;
  `initMembership` читает её при старте, иначе берёт статический `Config`.
  Commit index не персистится; конфиг-записи из лога при старте не
  переигрываются.
- Control plane: `internal/metaserver/membership.go` оркестрирует
  `AddReplica`/`RemoveReplica` (learner → catch-up → promote; transfer →
  remove), а карта range → реплики живёт в meta state machine отдельно от
  Raft-конфигурации. `internal/kvserver/membership.go` проверяет
  `Epoch` (версию карты) на membership-RPC.

### 1.2 Инварианты, на которые код опирается

1. В каждый момент существует не более одного незавершённого изменения.
2. Изменения применяются в порядке лога, и каждый узел видит один и тот же
   порядок.
3. Кворум считается по текущему (применённому) набору голосующих.
4. Набор голосующих восстанавливается из WAL-записи, даже если лог
   скомпактирован.
5. Лидер, которого нет в новом составе, уходит в отставку после apply.

### 1.3 Где эти инварианты не выполняются

| # | Заявлено | Фактически |
|---|---|---|
| I1 | «одно изменение за раз» | Флаг `configInFlight` живёт в памяти одного узла. `becomeLeaderLocked` и `stepDownWithErrorLocked` его сбрасывают (`internal/raft/node.go:1595`, `:2022`, `:2045`). Новый лидер не знает, что предыдущий лидер оставил в логе незакоммиченную/неприменённую конфиг-запись. |
| I2 | «после commit изменение действует» | Решение о кворуме использует **применённую** конфигурацию. Лидер может закоммитить второе изменение старым кворумом, пока первое ещё не применено (сценарий F2). |
| I3 | «состав восстановим после компакции» | В снапшоте состава нет. Узел, пропустивший изменения и получивший снапшот, теряет конфиг-записи навсегда (F4). |
| I4 | «удалённый узел не опасен» | Отдельной WAL-записи достаточно только если она актуальна. Узел, пропустивший удаление/добавления, при рестарте верит старой записи. |
| I5 | «meta и Raft согласованы» | `RemoveReplica` меняет Raft-состав, затем карту. Если `ApplyRanges` упал, карта врёт; `AddReplica` при `containsAddr` возвращает `nil`, не проверяя Raft (`internal/metaserver/membership.go:60-65`), поэтому расхождение не чинится ретраем. |
| I6 | «эпоха защищает от stale» | `Epoch` — версия карты в meta, не версия Raft-конфигурации; она не входит в лог/снапшот Raft и не мешает старой ноде голосовать. |

## 2. Постановка задачи и сценарии отказа

### 2.1 Формальная постановка

Пусть C_i — конфигурация (множество голосующих и learners) после i-го
применённого изменения. Требуются:

- **S1 (linearizability состава).** Успешный ответ на изменение означает, что
  изменение применено на кворуме C_i и не будет потеряно ни при каком отказе,
  при котором выживает кворум C_i.
- **S2 (quorum intersection).** Любые два кворума, на которых принимались
  решения (выборы, commit, lease), пересекаются, даже если узлы находятся в
  разных состояниях применения.
- **S3 (recoverability).** `config = fold(snapshot.config, config entries >
snapshot.index)` на каждом узле; снапшот и лог не могут дать разные составы.
- **S4 (fencing).** Узел, исключённый из C_i, не может ни проводить кампанию, ни
  обслуживать клиентов; узел с устаревшей конфигурацией не может влиять на
  кворум.
- **L1 (liveness).** После смены лидера незавершённый переход либо
  завершается, либо откатывается; нет вечного «joint» и вечной блокировки
  изменений.
- **L2 (decommission).** Узел выводится без потери кворума, с сохранением
  данных на замене, и операция либо завершается, либо безопасно отменяется.

### 2.2 Сценарии отказа

Ниже `C_old = {A,B,C}`, если не указано иное. «Подтверждено» = клиент получил
успех.

#### F1. Лидер падает во время add/remove (до commit)

Лидер A добавляет learner D, реплицирует запись на D, но не получает кворум
C_old и падает. Запись остаётся в логе A/D. Новый лидер B (без этой записи)
либо перезапишет её, либо (если получил) закоммитит. Клиент не получил успех,
поэтому потеря незакоммиченного изменения корректна. Опасность в другом:
новый лидер сбрасывает `configInFlight` и может начать **второе** изменение,
пока первое ещё не применено (F2). Также meta-ретрай может прийти к другому
лидеру и создать второе намерение.

Вывод: сам по себе этот отказ безопасен, но он открывает F2/F3.

#### F2. Лидер падает после commit, до apply/ответа (потеря подтверждённого изменения)

1. A добавляет D: `e1` (promote D) закоммичен кворумом C_old (A+B), A применил
   e1 и подтвердил клиенту. A падает **до** того, как B узнал commit index.
2. B содержит e1 в логе, но не применил его: `voters` B = C_old.
3. B избран лидером (B+C), сбрасывает `configInFlight`, принимает `e2` =
   remove C. Валидация e2 идёт против C_old, commit — тоже C_old (B+C = 2 из 3).
4. B применяет e1 (C1 = {A,B,C,D}), затем e2 (C2 = {A,B,D}). Чтобы
   реплицировать e2 на C, B сначала досылает C запись e1, так что e1 и e2
   лежат на B и C. e2 **не лежит на кворуме C1** (B и C — 2 из 4), и **не
   лежит на кворуме C2** (C уже вне состава), хотя клиент получил успех на e2.
5. Если B и C недоступны, A и D не могут собрать кворум C1; при этом
   подтверждённое удаление C не восстановимо на кворуме C2.

Нарушены S1/S2: изменение подтверждено кворумом конфигурации, к которой оно
не применялось. В joint-протоколе e2 (leave-joint) требовал бы кворума **обеих**
C1 и C2.

#### F3. Два конкурентных изменения

- Один лидер: `configInFlight` сериализует, но ошибка/таймаут оставляет запись
  в логе; гейт снимается при stepdown, и следующий лидер может начать второе
  изменение.
- Два meta-лидера (или meta-лидер и оператор): `schedulerMu` — мьютекс
  процесса, не распределённый; после failover meta он не защищает. Последняя
  линия обороны — Raft-гейт, но он сбрасывается при смене лидера.
- Итог: изменения валидируются против разных конфигураций, возможны
  противоречивые операции (promote уже удалённого, remove уже удалённого,
  добавление дубликата) и потеря подтверждённого изменения (F2).

#### F4. Устаревший voter set после snapshot/compaction — потеря данных

Это главный сценарий, ради которого нужен конфиг в снапшоте.

1. C0 = {A,B,C}, learners D и E догнали лидера.
2. Пока B изолирован, коммитятся e1 = promote D (C1 = {A,B,C,D}) и
   e2 = promote E (C2 = {A,B,C,D,E}); после e2 состав не меняется.
3. Лидер компактит лог за e2 и позже отправляет B `InstallSnapshot`
   (`sendAppendEntries`, `internal/raft/node.go:1670`). B устанавливает
   снапшот: данные и лог — на индексе j, **состав остаётся {A,B,C}** и
   никогда не догонится: записи e1/e2 уже скомпактированы.
4. B полностью догоняет лог (обычные AppendEntries после снапшота), поэтому
   его лог не короче лога C; B — свежий кандидат по данным, но с устаревшей
   конфигурацией.
5. Раздел: {B,C} | {A,D,E}. B кампанию выигрывает голосами B и C. Для B это
   кворум C0 (2 из 3). C голосует, потому что B есть в его voters и лог B
   up-to-date; C не проверяет конфигурацию кандидата.
6. B считает себя лидером и коммитит w1 с ack от B и C. Под C2 это 2 из 5 —
   не кворум.
7. A избран лидером голосами A,D,E (3 из 5) и коммитит w2.
8. После восстановления связи один из лидеров уходит в отставку, и
   подтверждённая w1 теряется при truncate. Нарушены S1/S2, потеряны
   подтверждённые данные.

Ключевая арифметика: кворумы конфигураций, различающихся на **одного** узла,
пересекаются; кворумы C0 (2 из 3) и C2 (3 из 5) — нет:
`{B,C} ∩ {A,D,E} = ∅`. Любая схема, допускающая отставание конфигурации на
два и более изменения, небезопасна без joint consensus.

#### F5. Удалённый узел возвращается (stale rejoin / клон диска)

- Узел B удалён, но не получил запись удаления (был недоступен). Его WAL-запись
  говорит «voter B». При рестарте B кампанию проигрывает (A/C его не знают),
  но **принимает более высокий term** и заставляет лидера уйти в отставку:
  серия рестартов даёт election storm и рост term.
- Два узла из старого состава (например, B и C после удаления обоих) могут
  выбрать лидера кворумом старого C0 и обслуживать клиентов со старыми
  данными/эпохой, пока текущий состав C2 живёт своей жизнью → split brain.
- Клон диска/восстановленный backup с тем же `ID` и старой конфигурацией — тот
  же класс. `ForceLearner` помогает только если оператор явно вызвал
  `AddReplica`; обычный рестарт его не использует.
- Нет `ClusterID`/incarnation: узел из другой эпохи/клона не отличим от
  легитимного.

#### F6. Отстающий learner

- `waitLearnerCaughtUp` (`internal/metaserver/membership.go:285`) сравнивает
  `lastApplied` learner с commit index лидера, снятым **в начале** ожидания;
  затем promotion делает отдельный RPC. Между проверкой и promotion learner
  может отстать, а commit-index лидера — уйти вперёд.
- Нет порога по лагу (записи/байты), нет требования свежего ack, нет проверки
  `matchIndex` на самом лидере. Промоушен отставшего learner уменьшает
  доступный кворум; при потере ещё одной ноды кворум теряется навсегда.
- Нет автоматического demote/алерта, если learner отстал уже после promotion.

#### F7. Сетевой раздел во время изменения

- Лидер в меньшинстве ставит гейт, не может закоммитить, и до stepdown
  отклоняет все изменения (`ErrConfigChangeInProgress`). Это корректно, но
  new leader в большинстве не знает о намерении старого (F2).
- Meta: `RemoveReplica` после успешного `RemoveVoter` может упасть на
  `propagateRangeEpoch`/`ApplyRanges`; карта остаётся со старой репликой, а
  Raft-состав — без неё. `AddReplica` для адреса, который уже есть в карте,
  возвращает `nil` и не восстанавливает learner — расхождение устойчиво.
- `transferAwayFrom` перебирает `r.Replicas`, включая уже удалённые из Raft;
  трансфер может уйти в никуда.

#### F8. Установка снапшота затирает лог, но не состав

Даже при **одном** пропущенном изменении узел после `InstallSnapshot`
продолжает использовать старый `voters`: `RestoreSnapshot`/
`RestoreSnapshotStream` не меняют состав. Для одной ноды кворумы C0/C1
пересекаются, поэтому split brain не гарантирован, но:
- лидер может быть избран меньшинством реального кворума (2 из 4 вместо 3);
- удалённая нода может считать себя voter и голосовать;
- при следующем изменении отставание становится двухшаговым и приводит к F4.

#### F9. Decommission / замена узла

- Нет политики: `RemoveReplica` оставляет data dir, вызывает
  `stopReplicaOnTarget` best-effort и не чистит. Нет drain, нет порога
  catch-up, нет tombstone, нет запрета на повторный join со старым диском.
- Оператор, следуя runbook «удалить и стереть диск», может стереть диск до
  того, как карта/состав обновились, или наоборот оставить клон.
- Если удаляемая нода — лидер, transfer делается перебором реплик и может
  выбрать цель, которая уже не voter.

### 2.3 Классификация

| Сценарий | Класс | Последствие |
|---|---|---|
| F1 | liveness/at-most-once | потеря незакоммиченного изменения, ретрай |
| F2 | safety (S1/S2) | подтверждённое изменение не на кворуме новой конфигурации; потеря/недоступность конфигурации |
| F3 | safety | конкурирующие изменения, противоречивый состав |
| F4 | safety (S1/S2) | split brain, потеря подтверждённых данных |
| F5 | safety (при ≥2 stale) + liveness | split brain / election storm |
| F6 | liveness | потеря кворума, недоступность range |
| F7 | safety (meta vs Raft) | карта врёт, range без реплики, невозможность починки |
| F8 | safety/liveness | лидер меньшинством, stale voter |
| F9 | safety/ops | потеря данных при cleanup, повторный join |

F4, F2 и F7 — блокеры production. F5 — блокер при наличии клонов/старых
дисков; без pre-vote даже один stale узел вызывает term inflation.

## 3. Требования

- R1. Конфигурация — часть реплицированного состояния; в снапшоте и в
  восстановлении (S3).
- R2. Joint consensus для любого изменения множества голосующих; learner-only
  изменения могут быть простыми, но проходят тот же гейт.
- R3. Не более одного конфиг-изменения в логе между commit и apply; гейт
  выводится из лога и переживает смену лидера.
- R4. Кворум считается по C_old и C_new одновременно в joint; лидер обязан
  финализировать joint после apply; клиент получает успех только после
  финальной конфигурации.
- R5. Эпоха конфигурации монотонна и входит в снапшот/запись/Status;
  используется для CAS и fencing, не заменяет term.
- R6. Удалённая/устаревшая нода не проводит кампанию и не обслуживает клиентов; для
  этого — pre-vote (или эквивалент), config-index в записи, tombstone.
- R7. Decommission без потери кворума: add-before-remove, transfer, порог
  catch-up, точка невозврата.
- R8. Наблюдаемость: epoch, joint, pending change, длительность, ошибки apply,
  лаг learner, расхождения meta/Raft.

Non-goals: multi-AZ placement, capacity scheduler, PITR/backup, замена
state machine, динамический состав meta-группы (см. open questions).

## 4. Варианты

### 4.1 Вариант A: joint consensus (C_old,new) + конфиг в снапшоте

**Протокол.** Изменение состава — две записи:

1. `config_joint`: C_old,new = (текущие voters, целевые voters). Запись
   попадает в лог и с этого момента определяет `effective` конфигурацию для
   решений (§5.4); для API и снапшота действует после apply.
2. `config_final`: C_new. Лидер добавляет её автоматически после apply
   joint-записи (или новый лидер при старте). Действует после commit.

Пока действует joint, для выборов, commit и lease нужны **кворум C_old и
кворум C_new** (не max и не объединение). Новый лидер должен быть voter в
обоих наборах. Действующий лидер, которого нет в C_new (его удаляют), доводит
переход до final и только затем уходит в отставку — иначе decommission лидера
невозможен.

**Переход:** C → C_old,new → C_new. Оба перехода — отдельные записи лога,
поэтому любая пара используемых конфигураций различается не более чем на один
шаг, и их кворумы пересекаются (4.1.1). C_new не используется для решений до
commit final-записи, поэтому подтверждённое изменение лежит на кворуме C_new.

#### 4.1.1 Почему кворумы соседних конфигураций пересекаются

Для добавления (N → N+1): `C_new = C_old ∪ {x}`, `q_old = ⌊N/2⌋+1`,
`q_new = ⌊(N+1)/2⌋+1`, `Q_old ⊆ C_new`. Пересечение двух подмножеств вселенной
размера N+1 не меньше `q_old + q_new − (N+1)`; проверка: N=3 → 2+3−4=1,
N=4 → 3+3−5=1, N=5 → 3+4−6=1. Для удаления (N → N−1): `Q_old ∩ C_new` имеет
размер не меньше `q_old−1`, поэтому пересечение не меньше
`(q_old−1) + q_new − (N−1)`; N=4 → 2+2−3=1, N=5 → 2+3−4=1. Значит, кворумы
**соседних** конфигураций всегда пересекаются. Отсюда и граница безопасности:
пока каждый узел использует конфигурацию не старше чем на один шаг, S2
выполняется; отставание на два и более шага (F4) — нет.

**Плюсы:** безопасность не зависит от дисциплины оператора; переживает
failover; допускает удаление лидера; даёт корректную основу для decommission;
хорошо изучен (etcd, CockroachDB, TiKV).

**Минусы:** +1 запись и +1 round-trip на изменение; временно расширенный
кворум (доступность); существенная переделка quorum/apply/snapshot путей;
сложность тестирования.

**Оценка:** 6–9 pw на ядро + снапшот (см. §10).

### 4.2 Вариант B: формализованный one-change-at-a-time + operator lock

Чтобы этот вариант был безопасен, одного мьютекса недостаточно. Нужны:

1. replicated/лог-выводимый маркер «изменение в процессе» (не in-memory
   флаг);
2. использование конфигурации из лога (а не только применённой) для
   голосования/commit;
3. подтверждение только после того, как запись лежит на кворуме **новой**
   конфигурации (иначе F2);
4. конфиг в снапшоте (иначе F4);
5. epoch/fencing и reconcile с meta.

С этими пунктами B сходится к частному случаю A (пункт 3 — это и есть
joint-переход, выраженный вручную), но:
- поддерживает только односерверные изменения; «заменить 4 реплики» —
  последовательность из 8 изменений с окнами недоступности;
- незавершённое изменение блокирует все изменения до завершения (автоматической
  финализации нет, только ручная);
- safety-аргумент опирается на «все узлы используют конфигурацию не старше чем
  на одно изменение» — то же допущение, которое ломает F4, и его надо
  проверять вручную;
- operator lock не переживает failover meta; нужен replicated intent, то есть
  новый протокол, а не «lock».

**Safety-аргумент (при выполнении пунктов 1–4).** Пусть все узлы используют
конфигурацию из своего лога, в логе не более одного незавершённого изменения, а
изменения односерверные. Тогда в любой момент используются только соседние
конфигурации C_i и C_{i+1}, и по 4.1.1 их кворумы пересекаются. Лидер,
избранный кворумом C_i, обязан иметь в логе все закоммиченные записи C_i
(стандартное свойство Leader Completeness); если изменение C_{i+1} было
закоммичено, любой кворум C_i пересекается с кворумом C_{i+1}, поэтому
кандидат без этого изменения не соберёт кворум. Следовательно, подтверждённое
изменение не теряется, и два лидера не могут коммитить расходящиеся записи.
Аргумент **не работает**, если узел может использовать конфигурацию,
отстающую на два и более шага (F4), или если кворум считается по применённой,
а не по лог-конфигурации, и в логе уже есть следующее изменение (F2). Именно
поэтому B требует пунктов 2–4, а не только мьютекса.

**Границы.** B не даёт атомарной замены нескольких реплик; при потере лидера
между commit и apply восстановление зависит от того, успел ли новый лидер
увидеть запись; незавершённое изменение блокирует все последующие изменения
до ручного вмешательства. Поэтому B не рекомендуется как production-протокол,
только как заморозка.

**Где допустим:** как временная заморозка (feature flag off + ручной runbook +
snapshot config + reconcile), пока A не готов. Это снижает вероятность F2/F3,
но не устраняет F4; выключать автоматические изменения состава при этом
обязательно.

**Оценка:** 3–5 pw на «формальный» B (без decommission), причём большая часть
работы не выбрасывается, а входит в A.

### 4.3 Вариант C: только конфиг в снапшоте (без joint)

Закрывает F4/F8 (главный data-loss путь) и необходим в любом случае.
Не закрывает F2/F3. **Оценка:** 2–3 pw. Рекомендуется как первый шаг.

### 4.4 Вариант D: внешний координатор (etcd/Consul) для состава

Отвергнут: второй источник истины; Raft всё равно должен знать состав в своём
логе, чтобы голосовать; нужен распределённый commit между etcd и Raft
(невозможен без epoch/2PC); внешняя зависимость в data path. Не решает
per-range membership.

### 4.5 Сравнение

| Критерий | A: joint | B: one-change+lock | C: snapshot only | D: etcd |
|---|---|---|---|---|
| F2 (потеря подтверждённого) | да | только при п.3 (≈joint) | нет | нет |
| F4 (stale после snapshot) | да | только с C | да | нет |
| F3 (конкурентные) | да | при replicated gate | нет | нет |
| Удаление лидера | да | через transfer | через transfer | — |
| Multi-server замена | да | нет | нет | — |
| Сложность | высокая | средняя/высокая | низкая | высокая + ops |
| Оценка | 14–19.5 pw | 3–5 pw | 2–3 pw | 6–10 pw + зависимость |

### 4.6 Рекомендация

**A + C + epoch + decommission.** C выполняется первым (закрывает data-loss
путь), A — основная работа, B — только как аварийная заморозка на время
разработки. Обоснование:

- единственный вариант, который сохраняет S1–S4 при отказах, при которых
  выживает кворум, включая смену лидера между commit и apply;
- не требует доверия к оператору и к meta-lock;
- F4 — реальная потеря данных, а не теоретическая; C без A оставляет F2/F3;
- стоимость A предсказуема и локализована в Raft core; альтернативы не дешевле
  при честном выполнении требований.

## 5. Предлагаемые изменения

### 5.1 Модель конфигурации (Go-эскиз)

```go
// package raft

type VoterSet struct{ Voters []string } // sorted, unique, non-empty

type Config struct {
    Epoch    uint64     // +1 на каждую применённую config-запись
    Old      *VoterSet  // != nil только в joint
    New      VoterSet
    Learners []string   // sorted, disjoint с Old/New
    ChangeID string     // последняя применённая операция (для идемпотентности)
    Index    uint64     // индекс последней config-записи, 0 = bootstrap
}

func (c Config) IsJoint() bool { return c.Old != nil }

// IsVoter: в joint — только пересечение C_old ∩ C_new.
func (c Config) IsVoter(id string) bool {
    if !c.New.contains(id) { return false }
    return c.Old == nil || c.Old.contains(id)
}

// QuorumSatisfied: в joint нужны оба кворума, а не max/union.
func (c Config) QuorumSatisfied(acked func(string) bool) bool {
    if c.Old != nil && !majority(c.Old.Voters, acked) { return false }
    return majority(c.New.Voters, acked)
}
```

Инварианты `Config`: `New` непуст; `Old` либо nil, либо непуст и не равен
`New`; learners не пересекаются с `Old`/`New`; всё отсортировано и
дедуплицировано; `Epoch` строго растёт на 1 на каждую запись; `Index`
совпадает с индексом записи.

### 5.2 Схема config-записей

Одна структура payload, три типа записи (три типа нужны, чтобы
`isConfigCommand` и snapshot/recovery не угадывали фазу):

```go
type ConfChange struct {
    Version   uint8    `json:"v"`               // 1
    Op        string   `json:"op"`              // add_learner|remove_learner|promote|demote|remove_voter
    Peer      string   `json:"peer,omitempty"`
    ChangeID  string   `json:"change_id"`       // UUID операции, идемпотентность
    Epoch     uint64   `json:"epoch"`           // ожидаемая текущая эпоха (CAS)
    OldVoters []string `json:"old_voters,omitempty"` // только joint
    Voters    []string `json:"voters"`          // полный C_new после шага
    Learners  []string `json:"learners"`        // полный learner set после шага
}
```

Типы `model.CommandType`:

- `config_simple` — add/remove learner; меняет только learners, кворум не
  трогается; одна запись.
- `config_joint` — promote/demote/remove voter; payload содержит `OldVoters` и
  `Voters`; одна запись.
- `config_final` — завершает joint; payload содержит `Voters`, `ChangeID`
  должен совпадать с joint; одна запись.

Запись несёт **полный** результат шага, а не дельту: apply не зависит от
намерений лидера и проверяем. Apply детерминирован и тотален:

- `config_joint`: `!cur.IsJoint()`, `payload.Epoch == cur.Epoch+1`,
  `sameSet(payload.OldVoters, cur.New)`, `!sameSet(payload.Voters, cur.New)`,
  нет дублей/пересечений → `Config{Old: payload.OldVoters, New: payload.Voters, ...}`;
- `config_final`: `cur.IsJoint()`, `Epoch == cur.Epoch+1`,
  `sameSet(payload.Voters, cur.New)`, `payload.ChangeID == cur.ChangeID` →
  `Config{Old: nil, ...}`;
- `config_simple`: `!cur.IsJoint()`, `Epoch == cur.Epoch+1`,
  `sameSet(payload.Voters, cur.New)` (не меняются), обновляются learners.

Повтор с тем же `ChangeID` и уже достигнутым состоянием — no-op (нужно для
ретраев после потери ответа и для `ConfigSync`); повтор с тем же `ChangeID`,
но другим состоянием — ошибка целостности (лог и снапшот расходятся), а не
штатный путь.

Нарушение предусловий — не паника и не частичное изменение: запись
игнорируется, инкрементируется `config_apply_errors_total`, логируется
epoch/op/index. Все реплики видят один лог, поэтому поведение детерминировано.
Лидер, предложивший такую запись, обязан был отклонить её на proposal-валидации;
это баг, а не штатный путь.

Отдельно обрабатывается **устаревшая конфигурация**: если `payload.Epoch >
cur.Epoch+1` или `!sameSet(payload.OldVoters, cur.New)` (то есть запись
ожидает конфигурацию, которой у узла нет), узел не пытается «догнать» её
эвристиками, а выставляет `configStale=true`, прекращает кампанию и
обслуживание клиентов и ждёт снапшот с конфигурацией (5.5). Это единственный
безопасный ответ: применить запись поверх неверного состава — значит получить
расхождение, которое невозможно доказать. Метрика
`config_stale_detected_total` и алерт обязательны.

Важно и обратное: apply не должен возвращать ошибку **после** успешной
замены конфигурации из-за неудачной записи `configRecord`. Лог — источник
истины; ошибка персистентности записи логируется и считается метрикой, но
proposal не фейлится (иначе клиент считает изменение неуспешным, хотя состав
уже изменён).

### 5.3 Кворум, выборы, commit, lease

Заменить `majority()` на `Config.QuorumSatisfied` во всех точках:

| Место | Правило |
|---|---|
| `HandleRequestVote` (grant) | кандидат и self — `cfg.IsVoter`; в joint кандидат должен быть приемлем для обоих наборов (иначе он не соберёт кворум) |
| подсчёт голосов (`electionLoop`, `countVotes`) | self + голоса только от `cfg.IsVoter`; нужен `QuorumSatisfied` |
| `becomeLeaderLocked` | только если `cfg.IsVoter(self)` (в joint — пересечение) |
| `maybeCommitLocked` | `acked(peer) = matchIndex[peer] >= index`; `QuorumSatisfied` |
| `hasQuorumLocked` (lease/read barrier) | `acked(peer) = lastAck в пределах election timeout`; `QuorumSatisfied` |
| `TransferLeadership` | цель — voter и в C_old, и в C_new; иначе отказ |
| `syncReplicatorsLocked` | цели — union(applied, effective voters/learners) + все voters из config-записей в `(applied, lastIndex]`; иначе удаляемая нода не получит свою final-запись |
| `replicationTarget` (long-lived loop check) | true, пока peer входит в union(applied, effective); удаление — только когда final применён и других config-записей про peer нет |
| `Status`/`Membership` | отдают `appliedConfig`, `effectiveConfig`, epoch, joint |

Важно: кворум и **цели репликации** — разные множества. В joint лидер
реплицирует и на C_old, и на C_new; удаляемая нода должна получить
`config_final` до того, как лидер перестанет ей слать, иначе она не узнает об
удалении (см. F5). После apply final, где её нет, — прекращать.

`maybeCommitLocked` сохраняет правило «коммитить напрямую только записи
текущего term»; config-записи прошлых term коммитятся индуктивно через noop
нового лидера. Это уже есть и менять не нужно.

#### 5.3.1 Ремонт отставшей конфигурации

`AppendEntriesRequest`/`Response` получают `ConfigEpoch` (аддитивное proto-поле).
Follower возвращает свою **applied** эпоху; лидер, увидев у отставшего, но
иначе догнавшего follower'а `ConfigEpoch < appliedConfig.Epoch`, инициирует
`InstallSnapshot` (в нём есть конфигурация) или отдельный узкий `ConfigSync`
RPC. Без этого узел, у которого конфигурация разошлась (старый снапшот,
миграция, баг), не может починиться: он уже не отстаёт по логу, поэтому
обычный snapshot-путь не сработает. `ConfigSync` предпочтительнее для
экономии трафика, если снапшот большой; в v1 достаточно `InstallSnapshot`,
так как расхождение — редкий аварийный случай.

Follower, получивший `ConfigSync`/снапшот, валидирует `Epoch >` своей и
`Config.Index >=` своей `Config.Index`; иначе — отказ и алерт. Это же поле
используется в §8 как инвариант: `ConfigEpoch` всех реплик должен совпадать
после стабилизации.

### 5.4 Commit/apply ordering и гейт

Две конфигурации:

- **effective** — конфигурация по последней config-записи в логе (даже
  незакоммиченной). Используется для голосования, кампании и commit-кворума —
  это правило «latest configuration in log» из §4.1 диссертации. Оно
  устраняет класс F2: лидер не может коммитить новое изменение старым
  кворумом, если в логе уже есть joint.
- **applied** — конфигурация после apply до `lastApplied`. Используется для
  API, `Status`, снапшота (снапшот обязан содержать applied на своём индексе).

Гейт (проверяется под `mu` перед постановкой в очередь и повторно при
append):

```go
func (n *Node) checkConfigGateLocked() error {
    if n.configInFlight { return ErrConfigChangeInProgress }     // быстрый путь
    if n.appliedConfig.IsJoint() || n.effectiveConfig.IsJoint() {
        return ErrJointConfigPending
    }
    if n.pendingConfigIndex > n.lastApplied {                    // запись в логе, но не применена
        return ErrConfigChangeInProgress
    }
    if n.configStale { return ErrStaleConfig }
    return nil
}
```

`pendingConfigIndex` обновляется при append/truncate (последняя config-запись
в логе), сбрасывается в 0 при apply; при `becomeLeaderLocked` гейт
пересчитывается из лога, а не из памяти. Если `appliedConfig.IsJoint()` —
лидер обязан немедленно поставить `config_final` (внутренняя операция, без
клиента), иначе joint зависнет при смене лидера.

Ответ на изменение:

- `AddLearner`/`RemoveLearner` — после apply `config_simple`;
- `Promote`/`Demote`/`RemoveVoter` — после apply **`config_final`**, не joint;
- при таймауте/stepdown клиент получает `ErrConfigChangeInProgress` с
  `ChangeID`/epoch и повторяет с тем же `ChangeID`; успех определяется по
  `appliedConfig.ChangeID`/целевому состоянию, а не по ошибке RPC.

`ChangeID` — единственный способ отличить «не применилось» от «применилось, но
ответ потерян». Он входит в `Config.ChangeID` и снапшот; для простых изменений
тоже.

### 5.5 Снапшот

Конфигурация кладётся в **конверт Raft-снапшота**, а не в KV state machine:
state machine не должен знать о Raft, а `Snapshotter.Snapshot()` возвращает
только `(data, index)`. Конверт — это `Snapshot`/`SnapshotMeta` + метаданные
чанков; с точки зрения восстановления это и есть «membership в снапшоте».

```go
type Snapshot struct {
    Index  uint64
    Term   uint64
    Data   []byte
    Config Config // applied config на Index
}

type SnapshotMeta struct {
    Index uint64; Term uint64; Size int64; CRC32 uint32; File string
    Config Config // Config.Index <= Index, последняя config-запись не позже снапшота
}
```

Изменения:

- `Snapshot()` берёт `snapshotter.Snapshot()` вне `applyMu`; чтобы приложить
  конфигурацию именно на индексе снапшота, вести небольшую историю
  `(index, Config)` для config-записей и брать последнюю с `index <= snap.Index`.
  Если config-записей нет, это bootstrap-конфигурация с `Index=0`.
- `SnapshotToStore`/`SaveSnapshotFromReader` принимают `Config`; `snapshotMeta`
  JSON получает поле `config` (versioned) — старые `meta.json` без него
  читаются как «конфигурация неизвестна».
- `proto/v2.proto`: `Snapshot { bytes config = 4; }` (аддитивное поле,
  совместимо), `InstallSnapshotChunkRequest { bytes config = 11;
  uint32 config_crc32 = 12; }`. Конфиг маленький; включать его в каждый чанк
  и проверять равенство между чанками (идемпотентно), либо в первый/последний
  с проверкой. Аддитивные поля не ломают старые бинарники; версия протокола —
  через `Status`/capability.
- `RestoreSnapshot`/`RestoreSnapshotStream`: после установки данных
  `appliedConfig = snapshot.Config`, `effectiveConfig` = snapshot.Config +
  config-записи из суффикса лога (`index > snapshot.Index`), которые ещё не
  применены. Если `snapshot.Config` отсутствует (старый снапшот, полученный
  от лидера) — **отклонить установку**: новый лидер всегда шлёт конфиг, пустой
  конфиг означает несовместимость, а не «оставить локальный».
- Локальный старый снапшот при старте: допустим, если снапшот содержит config
  либо `config`-запись имеет `AppliedIndex >= snapshot.Index` (то есть
  конфигурация отражает всё, что покрыто снапшотом). Legacy `membership`-запись
  без `AppliedIndex` не даёт такой гарантии: узел стартует с
  `configStale=true`, не проводит кампанию и не обслуживает клиентов, делает
  `ConfigSync` у лидера и снимает флаг только после успешной проверки. Это
  закрывает окно миграции, не блокируя кластер навсегда.
- Порядок сохранения: data file → `meta.json` (точка коммита, содержит config)
  → truncate prefix. Ошибка meta не должна приводить к truncate; существующий
  порядок сохраняется, добавляется только config.
- `SnapshotIndex`/`ConfigIndex` проверяются при загрузке: `Config.Index <=
  snapshot.Index` и `Config` непуст.

### 5.6 Персистентность и миграция WAL

Текущая запись `membership` остаётся для чтения старыми бинарниками, но
становится вторичной. Новая запись `config` (versioned JSON):

```go
type configRecord struct {
    Version     uint8    `json:"v"`
    Epoch       uint64   `json:"epoch"`
    OldVoters   []string `json:"old_voters,omitempty"`
    Voters      []string `json:"voters"`
    Learners    []string `json:"learners"`
    ChangeID    string   `json:"change_id,omitempty"`
    ConfigIndex uint64   `json:"config_index"`  // индекс последней config-записи
    AppliedIndex uint64  `json:"applied_index"` // на момент записи
}
```

Порядок восстановления при старте:

1. загрузить снапшот (`snapshot.Config` + index);
2. если конфига в снапшоте нет — прочитать `config`-запись (у неё есть
   `AppliedIndex`), иначе legacy `membership`; если ничего нет — статический
   `Config` (bootstrap, `Epoch=1`, `Index=0`);
3. наложить config-записи лога с `index > snapshot.Index` в порядке лога →
   `effectiveConfig`;
4. `appliedConfig` = fold от конфигурации на снапшоте через config-записи с
   `index <= lastApplied`; при старте `lastApplied = snapshot.Index`, поэтому
   `appliedConfig = snapshot.Config` (либо конфигурация из шага 2, если
   снапшота нет);
5. если `snapshot.Index > 0`, снапшот без config и запись не даёт
   `AppliedIndex >= snapshot.Index` — `configStale = true` и `ConfigSync` у
   лидера до кампании/обслуживания (см. 5.5).

Писать `config`-запись после каждого apply config-записи (и legacy
`membership` — для N-1). Ошибка записи не фатальна: лог и снапшот — источник
истины; запись лишь ускоряет старт. Но ошибку нужно логировать и считать
метрикой: она означает, что старт будет медленнее и что legacy-бинарник может
увидеть устаревший состав.

Миграция существующих WAL: при первом старте новой версии materialize
`config`-запись из legacy `membership`/static config с `Epoch=1` и
`ConfigIndex=0`; при первом снапшоте — записать конфиг в `meta.json`. Старые
снапшоты без конфига остаются валидными локально только по правилу 5.5.

### 5.7 Эпоха и fencing

- `ConfigEpoch` — версия **Raft-конфигурации**, +1 на каждую применённую
  config-запись (переход = +2). Входит в `Snapshot`, `configRecord`, `Status`,
  membership-RPC, метрики. Монотонна для committed-записей; при truncate
  незакоммиченной записи откатывается вместе с `effectiveConfig` (поэтому
  эпоха — часть `Config`, а не отдельный счётчик).
- `PlacementVersion` (существующий `meta.Range.Version`) — версия карты
  размещения, не эпоха Raft. Не смешивать: meta бампает её на add/remove,
  Raft — на каждый config-entry. В ответах отдавать обе.
- `Term` остаётся главным fencing-механизмом для лидеров. `ConfigEpoch` не
  заменяет term: он нужен для CAS при proposal, идемпотентности и для
  admission-проверок (клиент/нода с эпохой ниже — `ErrStaleConfig`).
- Fencing-правила:
  - `self ∉ appliedConfig` → узел не проводит кампанию, не обслуживает клиентов
    (`ErrNotMember`), не считается в кворуме; после apply final, где его нет,
    stepdown и persist tombstone.
  - `configStale` (5.5) → не проводит кампанию и не обслуживает клиентов, ждёт снапшот/`ConfigSync`.
  - membership-RPC с `PlacementVersion != текущей` → `ErrStaleEpoch`
    (существующий `acceptEpoch`).
  - удалённая нода, пытающаяся голосовать, отклоняется, потому что её нет в
    конфигурации актуальных нод; но её term всё равно принимается — поэтому
    нужен pre-vote (5.7.1).
  - клон/восстановленный диск: `ClusterID` в WAL/снапшоте и проверка в RPC —
    open question; до реализации — операционный запрет.

#### 5.7.1 Pre-vote

`HandleRequestVote` сначала принимает term, потом проверяет, что кандидат —
voter. Удалённая/устаревшая нода с более высоким term заставляет лидера уйти в
отставку, даже не выиграв выборы. Это F5. Решение — pre-vote (Raft §9.6):
кандидат сначала спрашивает, «выиграл бы я», не увеличивая term; только
получив pre-vote кворум, начинает реальные выборы. Pre-vote — отдельное
изменение, но без него fencing неполный. Рекомендуется включить в тот же
milestone (см. §11).

### 5.8 API и control plane

Raft (`internal/raft`):

- `Membership() (Config, error)`; `Status` получает `ConfigEpoch`, `Joint`,
  `OldVoters`, `AppliedConfigIndex`, `PendingChangeID`, `Removed`.
- `ChangeMembership(ctx, ConfChange) (Config, error)` — единая точка;
  `AddLearner`/`PromoteLearner`/`RemoveVoter` остаются thin wrappers.
- Типизированные ошибки: `ErrConfigChangeInProgress{ChangeID, Epoch}`,
  `ErrJointConfigPending`, `ErrStaleConfig`, `ErrNotMember`,
  `ErrUnsafeConfigChange` (последний voter, удаление лидера без transfer,
  learner не догнал), `ErrChangeAborted`.
- `ConfigAt(index)`/`AwaitConfig(ctx, epoch)` для тестов и meta.

KV (`internal/kvserver`):

- `Membership`/`Status` возвращают epoch/joint/old voters;
- `PromoteReplica` проверяет на лидере (под `mu`) `matchIndex[peer]` и свежий
  `lastAck`, а не только `lastApplied` по RPC (F6);
- `RemoveVoter` для learner — simple, для voter — joint; self-removal
  разрешён в core (decommission), но meta сначала делает transfer;
- `UpdateRangeEpoch`: не позволять обгонять `ConfigEpoch`; в идеале —
  deprecated, эпоха выводится из конфигурации; версия карты остаётся отдельной.

Meta (`internal/metaserver`):

- Персистентное намерение операции (replicated `meta_set_node`/`meta_change`
  command) с `change_id`, `op`, `target`, `epoch`, `state`, `deadline`;
  новый meta-лидер продолжает/отменяет по нему, а не начинает заново.
- Reconcile-цикл (на старте лидера и периодически): сравнить
  `meta.Range.Replicas` с фактическим `Membership()` лидера range.
  Расхождение чинится в сторону Raft-конфигурации с bump версии карты и
  алертом; `AddReplica` больше не делает early-return по `containsAddr`, а
  вызывает `EnsureLearner` (идемпотентно).
- `AddReplica`/`RemoveReplica` остаются оркестраторами, но каждый шаг
  идемпотентен и проверяет `ConfigEpoch`; `RemoveReplica` не считает успех,
  пока final не применён и карта не обновлена.

Proto (аддитивно): поля epoch/joint/old_voters/config в `Snapshot`,
`InstallSnapshotChunkRequest`, `MembershipResponse`, `StatusResponse`;
next free field numbers, без переиспользования.

### 5.9 Производительность

Config-записи редки; на горячем пути добавляется чтение `effectiveConfig`
(указатель/atomic snapshot) в `maybeCommitLocked`, `hasQuorumLocked`,
подсчёте голосов и `replicationTargetsLocked`. Чтобы не брать лишние блокировки и
не сканировать срезы на каждом heartbeat:

- хранить `Config` как immutable-значение, заменяемое целиком под `mu`;
- `QuorumSatisfied` вызывать с заранее собранным `acked func` по map;
- цели репликации кэшировать и инвалидировать на append/truncate/apply
  config-записи;
- измерить p99 commit/append до/после (ожидаемо в пределах шума).

## 6. Decommission

### 6.1 Состояния

```
planned → draining → drained → removed
             └──────→ aborted (только до point of no return)
```

`point of no return` — первый commit `config_final`, удаляющий ноду из состава
хотя бы одного range. После него — только roll-forward.

### 6.2 Поток

1. **Admission.** Meta-лидер фиксирует `DecommissionNode{node, reason,
   operator}` в replicated meta state; нода помечается `unschedulable`;
   новые range на неё не назначаются. Если нода — единственный voter
   какого-либо range, отказ до начала.
2. **Drain.** Для каждого range, где нода — voter:
   1. если нода — лидер: `TransferLeadership` на voter из C_new (в joint — из
      C_old ∩ C_new), дождаться `Leader() != node`, таймаут 5s, retry;
   2. выбрать замену (capacity/AZ-aware scheduler; в v1 — явный target от
      оператора); если замены нет — `aborted` до point of no return;
   3. `AddLearner` замены (идемпотентно), дождаться catch-up (6.3);
   4. `PromoteLearner` (joint), дождаться final;
   5. `RemoveVoter` старой ноды (joint), дождаться final; лидер продолжает
      реплицировать на старую ноду до commit final, чтобы она узнала об
      удалении;
   6. meta обновляет карту range (version bump) и `propagateRangeEpoch`;
   7. записать per-range progress в meta intent.
3. **Drained.** Все range перемещены; нода не входит ни в один состав. Она
   останавливает range-группы, отклоняет клиентов (`ErrNotMember`) и пишет
   локальный tombstone `decommissioned`.
4. **Removed.** Meta убирает ноду из registry/placement; процесс можно
   останавливать. Data dir не стирается автоматически: retention (например,
   24ч) + проверенный backup + подтверждение оператора. Wipe — отдельный
   шаг, после обновления карты и истечения retention.
5. **Cleanup.** Удалить временные learner-записи, метрики, placement; при
   повторном добавлении — только как learner (AddReplica с ForceLearner/
   новым incarnation), никогда как voter напрямую.

### 6.3 Порог catch-up

Промоушен разрешён, если на **лидере** (не по отдельному RPC) одновременно:

- `matchIndex[target] >= startIndex`, где `startIndex` — commit index на
  момент начала drain (или на момент добавления learner, что позже);
- `lagEntries = lastIndex - matchIndex[target] <= 64` и
  `lagBytes <= 8 MiB` (значения конфигурируемы, дефолты — предложение);
  `lagBytes` считается по подтверждённому offset репликации/снапшота, а если
  байтовый учёт не реализован — порог только по entries;
- условие держится 3 последовательных сэмпла (≥ 2s), `lastAck` свежий
  (в пределах election timeout);
- нет незавершённой установки снапшота на target;
- `len(C_new) >= 3` для production-range (иначе — явное подтверждение
  shrinking).

`lastApplied` target проверяется отдельно только для read-availability; для
голосования достаточно лога. Текущая проверка `lastApplied >= commit` в
`waitLearnerCaughtUp` сохраняется как дополнительная, но не заменяет
`matchIndex`.

### 6.4 Отмена и ошибки

- До point of no return: удалить добавленных learner'ов (simple remove),
  вернуть ноду в scheduling, `aborted`, intent сохранить для аудита.
- После point of no return: roll-forward. Если replacement потерян — добавить
  нового learner и продолжить; если range потерял кворум — остановить
  decommission и поднимать кворум (не удалять дальше).
- Потеря meta-лидерства: новый лидер читает intent и продолжает с шага, где
  остановились; незавершённый joint финализируется автоматически.
- Таймауты: per-range deadline (например, 10m), per-step (5s–2m); по
  истечении — `aborted`/`stalled` с алертом, без «дожать любой ценой».
- Удалённая нода недоступна: её ack не требуется для commit **final**
  (кворум C_new), но joint-запись требует кворума **C_old**; если без неё
  C_old кворум не собирается, decommission ждёт её возвращения или отменяется
  до point of no return. Data dir стирать только после истечения retention.

## 7. Семантика отказов и rollback

### 7.1 Точки падения

| Точка | Durable состояние | После рестарта | Ответ клиенту | Действие |
|---|---|---|---|---|
| до append config | ничего | старый состав | ошибка | retry с тем же ChangeID |
| после append, до commit | запись в логе (возможно, только у части) | effective включает её; новый лидер либо коммитит (noop), либо truncate | таймаут | retry; если запись потеряна — новое изменение; если коммитнулась — идемпотентный успех |
| после joint commit, до final | joint на кворумах C_old и C_new | лидер видит joint и добавляет final | «in progress» | retry/ожидание; алерт, если joint > 60s |
| после final commit, до apply | final в логе | apply при noop нового лидера | таймаут/успех | retry видит final |
| после apply, до persist record | память новая, WAL-запись старая | восстановление из snapshot+лог | успех | запись не критична; метрика ошибки |
| snapshot data saved, meta не saved | старый снапшот, лог цел | старый снапшот + лог | — | безопасно, повторить снятие |
| meta saved, prefix не truncated | новый снапшот + лог | снапшот + суффикс | — | безопасно |
| install snapshot: до meta | старые данные/состав | старые | ошибка | повторить install |
| install snapshot: после meta | новые данные + config | новый config | успех | — |
| decommission: до point of no return | intent + прогресс | продолжить/abort | ошибка/статус | abort или resume |
| decommission: после | removal commit | roll-forward | статус | resume |

### 7.2 Частичное применение

- Одна config-запись = одна атомарная замена immutable `Config` под `mu`;
  никаких частичных мутаций `voters`/`learners`. Собрать новый `Config`,
  провалидировать, затем swap.
- Apply идёт в порядке лога; падение между записями — нормальный replay.
- «Половина» joint (C_old применён, C_new нет) невозможна: joint — одна
  запись; после apply обе части действуют одновременно.
- Снапшот фиксирует `appliedConfig` на своём индексе; отсюда
  `config = fold(snapshot, log)`.

### 7.3 Rollback состава

Raft не умеет «откатить» закоммиченную конфигурацию; откат — это новое
изменение:

- promote → demote (joint);
- remove → add learner + catch-up + promote (не переиспользовать старый диск);
- decommission — только abort до point of no return; после — roll-forward.

Откат бинарника (new → old) разрешён только если: нет joint, нет
незакоммиченных config-записей, old-бинарник умеет читать `config`-запись
(минимум v1 из rollout), и в снапшоте нет обязательного для new-кода поля,
которое old не понимает. Иначе old-бинарник должен отказаться стартовать
(текущий `Filestore` возвращает ошибку на неизвестный тип WAL-записи — это
лучше, чем тихо испортить состав). Жёсткое правило: **rollback после
включённого joint запрещён** до финализации и полного snapshot-цикла.

### 7.4 Stale node

| Ситуация | Детект | Поведение | Восстановление |
|---|---|---|---|
| Удалённый узел не получил removal | при кампании: его нет в конфигурации актуальных voters; `Status.Removed=false`, `ConfigEpoch` ниже | кампанию проигрывает, но распространяет высокий term; без pre-vote — election storm | quarantine по адресу; не возвращать в voter set; wipe или re-add как learner с новым incarnation |
| Узел с устаревшей конфигурацией после snapshot | `ConfigEpoch` в `AppendEntriesResponse` ниже лидерской; `Config.Index`/`Epoch` не совпадают | `configStale=true`: не кампании, не обслуживает клиентов, не считается в кворуме | `ConfigSync`/`InstallSnapshot` от лидера; если конфигурация уже скомпактирована — re-add как learner |
| Клон диска / backup | `ClusterID`/incarnation (после реализации); иначе — расхождение epoch и «лишний» voter в старом составе | может собрать stale-кворум с другим клоном | никогда не возвращать старый диск в живой кластер; wipe; admission только через meta |
| Отставший learner | `matchIndex`/`lagEntries` на лидере | не голосует, не промоутится выше порога | догнать; при невозможности — удалить learner |
| Stale leader (partition) | более высокий term / нет lease-кворума | не может коммитить без кворума своей конфигурации; уходит в отставку при контакте | после heal — stepdown, truncate незакоммиченного, apply final |

Ключевое правило: **один stale-узел не должен быть способен повлиять на
кворум**. Он может только шуметь term'ом; это лечится pre-vote. Два stale-узла
из одного старого состава — это уже потенциальный split brain (F5), поэтому
decommission обязан оставлять tombstone, а старые диски — не возвращаться в
кластер.

## 8. План тестирования

### 8.1 Harness

- Детерминированная симуляция: существующий in-memory `testCluster`
  (`internal/raft/membership_test.go`) + сетевой планировщик с seed
  (deliver/drop/reorder/partition/heal), виртуальные часы, без реальных
  sleep. Seed печатается при падении.
- Failpoints на точных переходах: append/commit/apply joint, append/commit/
  apply final, snapshot save data/meta, truncate prefix, install snapshot
  (начало/середина/финал), decommission шаги. Каждая точка: kill -9 →
  restart → проверка инвариантов.
- E2E: 3/5/7 in-process узлов с реальным gRPC; опционально Docker.
- Partition-тесты: существующий proxy-harness `test/jepsen_partition_test.go`
  (асимметричные линки, `JEPSEN=1`), плюс сценарии «stale node + один
  актуальный voter».

### 8.2 Инварианты-оракулы (после каждого события)

1. `config = fold(snapshot.config, config entries ≤ applied)` на каждой ноде.
2. `Epoch` растёт ровно на 1 на запись; нет дыр; `Index` согласован.
3. Не более одной неприменённой config-записи в логе.
4. Нет двух лидеров в одном term; committed-префиксы не расходятся.
5. Любые два commit/elect кворума пересекаются (инструментировать каждое
   решение: epoch + набор ack).
6. Подтверждённое изменение лежит на кворуме **результирующей** конфигурации
   (для joint — обеих).
7. После install snapshot `appliedConfig == snapshot.Config`; stale-узел не
   может проводить кампанию или коммитить.
8. Нет вечного joint: после стабилизации сети final применяется за bounded
   время; pending change не старше T.
9. Meta map == Raft config (или зафиксированное расхождение с алертом и
   reconcile).

### 8.3 Матрица

| Тест | Объём | Что проверяет |
|---|---|---|
| random changes | 1000 операций × 3/5/7 узлов, случайные add/promote/demote/remove/learner, kill лидера, restart, snapshot | S1–S4, L1 |
| concurrent changes | 200 пар одновременных операций с одним/разными ChangeID | идемпотентность, F3 |
| failover в каждой точке | по 20 прогонов на точку из 7.1 | F2, L1 |
| stale snapshot | 100 прогонов: 2+ пропущенных изменения + install + partition | F4 (не должно быть split brain) |
| removed rejoin | 50 прогонов: удаление, рестарт со старым диском, одиночный и парный stale | F5, term inflation |
| learner lag | 50 прогонов: медленный learner, promotion на границе порога | F6 |
| partitions | 100 прогонов, включая асимметричные и stale+actual | F5/F7, quorum |
| snapshot compaction | каждое изменение → snapshot → install (unary и chunked), обрыв чанка, CRC | S3, F8 |
| mixed-version | rolling N-1→N→N+1, capability off/on, попытка rollback | §9 |
| decommission | 50 прогонов, разное число range, лидер на ноде, abort/resume | L2, кворум |
| linearizability | Porcupine (KV register) + отдельный checker истории конфигураций | S1 |
| crash/ENOSPC | kill -9, disk full на WAL/snapshot, slow disk | durability |
| soak | 72h, churn каждые 30s, YCSB, snapshots | стабильность, утечки |

### 8.4 Линеаризуемость состава

История: `Invoke(op, args, change_id)` / `Return(op, result, config_epoch,
config)` + чтения `Membership`. Проверки:

- каждый успешный op соответствует переходу в тотальном порядке;
- неуспешный op либо не применён, либо применён (неопределённость допустима),
  но не «применён дважды»;
- чтение возвращает конфигурацию из истории;
- epoch монотонен вдоль истории;
- после успеха remove целевая нода отсутствует во всех последующих чтениях.

### 8.5 Производительность и наблюдаемость

- Бенчмарки: commit latency p50/p99 при простом изменении и joint, стоимость
  snapshot с config, overhead `QuorumSatisfied` на heartbeat.
- Метрики: `raft_config_epoch`, `raft_config_joint`,
  `raft_config_change_duration_seconds{op}`, `raft_config_change_failures_total`,
  `raft_config_apply_errors_total`, `raft_pending_config_rejections_total`,
  `raft_snapshot_config_missing_total`, `raft_stale_config_rejects_total`,
  `raft_learner_lag_entries{peer}`, `raft_learner_lag_bytes{peer}`,
  `raft_decommission_ranges_total{state}`, `raft_election_term_inflation_total`.
- Алерты: joint > 60s; pending > 5m; epoch divergence между репликами;
  snapshot без config; decommission stalled; рост term без выборов.

## 9. Миграция, совместимость, rollout

### 9.1 Версии

- **v1 (N):** понимает config-записи и config в снапшоте, пишет `config`- и
  legacy `membership`-записи, но **не предлагает** joint; все изменения —
  старые simple-записи. Capability `supports_joint=false`.
- **v2 (N):** после rolling upgrade всех voters и meta — replicated
  `cluster_version` flip; с этого момента новые изменения идут joint-записями.
- **v3 (N+1):** удаление legacy-пути и legacy-записи после полного цикла
  снапшотов (все существующие снапшоты содержат config).

### 9.2 Правила совместимости

- Старый узел **не должен** получать `config_joint`/`config_final`: его
  `isConfigCommand` их не знает, и запись уйдёт в KV state machine как
  неизвестная команда. Поэтому flip capability — только когда все voters
  (и meta-группа) на v1+; старые ноды в группе недопустимы.
- Старый узел читает новый снапшот: лишние proto-поля игнорируются, config
  теряется, но старый узел использует свою WAL-запись — это допустимо только
  вне joint. При включённом joint старые бинарники в группе запрещены.
- Новый узел читает старый снапшот: config отсутствует → правило 5.5
  (`AppliedIndex`/`ConfigSync`) или отказ install от лидера.
- WAL: `config`-запись неизвестна старому коду → он откажется стартовать
  (`unknown WAL record type`). Это осознанный fail-stop; rollback на бинарник
  без поддержки `config`-записи запрещён после первого старта v1.
- Capability discovery: `Status`/heartbeat отдаёт `supports_joint` и
  `config_epoch`; meta агрегирует и не включает joint, пока не все живы и
  обновлены. При добавлении новой ноды — сначала upgrade, потом add.

### 9.3 Порядок rollout

1. v1 на все ноды (rolling), meta последним; проверить, что снапшоты
   содержат config и `config`-записи читаются.
2. Включить `supports_joint` на всех; flip `cluster_version` (replicated
   meta command); дождаться, пока все range лидеры увидят версию.
3. Включить автоматические изменения состава (scheduler) только после
   успешного 1000-change теста на staging и 72h soak.
4. v3 — после того как все снапшоты перезаписаны (snapshot age > retention).
5. Откат: только в пределах v1+ и только при `!joint`; процедура — в runbook,
   с проверкой `Status` всех range.

### 9.4 Bootstrap и meta

- Свежий кластер: materialize `Config` с `Epoch=1`, `Index=0` и записать в
  WAL/снапшот до старта выборов; иначе первый снапшот будет без config.
- Meta-группа: статический состав; динамические изменения meta-группы — вне
  scope этого ADR, но core-изменения применимы. Decommission meta-ноды —
  отдельный runbook/ADR.
- Meta state machine: новый replicated command для node lifecycle; снапшот
  meta уже сериализует `meta.State` — добавить поле и обеспечить обратную
  совместимость (JSON omitempty).

## 10. Оценка трудозатрат

Человеко-недели (pw), один senior Go/Raft инженер, без календарного soak.
Оценка — с учётом сложности `internal/raft/node.go` (~2.2k строк) и отсутствия
готового harness для model-based тестов.

| Работа | pw |
|---|---|
| Config model, quorum, election/commit/lease интеграция | 2–3 |
| Joint-протокол: записи, переходы, финализация, гейт, replication union, stepdown | 2–3 |
| Snapshot config: unary + chunked + recovery + миграция | 1.5–2 |
| Персистентность `config`-записи, восстановление, epoch/fencing | 1–1.5 |
| API, kvserver, proto, meta reconcile/intent | 2–2.5 |
| Decommission + learner policy | 2–2.5 |
| Тесты: 1000 changes, failpoints, partitions, linearizability, mixed-version | 2.5–3.5 |
| Rollout, runbooks, документация, soak-подготовка | 1–1.5 |
| **Итого** | **14–19.5** |

С двумя инженерами и ревьюером — 8–11 календарных недель при хорошем
параллелизме (snapshot/meta/decommission можно вести параллельно core).
Если сузить первый релиз (без mixed-version, без автоматического placement,
decommission вручную) — 9–12 pw.

Честно: это **не Sprint 1**. Sprint 1 даёт ADR; реализация — отдельный
milestone. Оценка в `docs/PRODUCTION-PLAN.md` (6–8 недель на G1+G2 вместе)
оптимистична: G2 в одиночку при выполнении заявленного gate (1000 changes,
snapshot, decommission, mixed-version) — 14–19.5 pw. Риск недооценки высокий;
лучше планировать верхнюю границу.

## 11. Открытые вопросы и риски

### 11.1 Открытые вопросы

1. **Pre-vote.** Включать в этот же milestone или отдельным ADR? Без него
   F5 (term inflation) остаётся. Рекомендация: отдельная реализация, но
   блокирующая для G2.
2. **Snapshot config: конверт vs payload state machine.** Выбран конверт
   (Raft header). Нужно подтвердить, что внешние потребители снапшотов
   (backup/PITR) умеют читать новый `meta.json` и proto-поле.
3. **Chunk protocol.** Аддитивные поля vs новая RPC-версия; как старый лидер
   отправляет снапшот новому follower'у (без config) — отклонять или
   принимать с локальной конфигурацией? Предложение: отклонять install без
   config, но разрешить локальный старый снапшот по правилу 5.5.
4. **Self-removal лидера.** Разрешать в core (нужно decommission) или
   требовать transfer? Предложение: core разрешает, meta всегда transfer.
5. **Reconcile meta↔Raft.** Кто авторитетен при расхождении без intent:
   Raft-конфигурация (предложение) или карта? Нужна политика для
   автоматического исправления и алерта.
6. **ClusterID / incarnation / backup restore.** Клон диска и восстановление
   backup в живой кластер — отдельный класс F5. Нужен `ClusterID` в
   WAL/снапшоте и проверка в RPC; возможно, ADR 0006.
7. **Multi-range decommission.** Нет глобальной атомарности: нода может быть
   выведена из части range. Допустимо ли это для оператора? Предложение: да,
   с per-range прогрессом и статусом.
8. **Meta-группа.** Динамический состав meta и decommission meta-ноды — вне
   scope; зафиксировать как ограничение.
9. **Совместимость снапшотов с backup/PITR.** Если backup читает `Snapshot`
   напрямую, добавление config меняет формат; нужен versioned envelope.
10. **Производительность.** Проверить, что `QuorumSatisfied` на каждом
    heartbeat не даёт регрессию p99; при необходимости — кэш per-config.
11. **Кто ревьюит safety-аргумент.** Желателен внешний Raft-эксперт; без
    этого риск тонкого бага в quorum/apply высок.
12. **Политика shrinking.** Разрешать ли уменьшение кластера ниже 3 voters
    для non-production range; кто подтверждает.

### 11.2 Риски

| Риск | Серьёзность | Митигация |
|---|---|---|
| Ошибка в подсчёте joint-кворума (max вместо двух множеств) | высокая | property-тесты, инвариант-оракул, ревью |
| Stale config всё же возможен через старый снапшот | высокая | правило `AppliedIndex`/`ConfigSync`, отказ install без config |
| Mixed-version: старый узел получает joint-запись | высокая | capability gate, hard fail, тест rolling |
| Потеря подтверждённого изменения при failover | высокая | joint + final перед ack + failpoint-тесты |
| Joint зависает (нет автофинализации) | средняя | finalize on apply/election, алерт, тест |
| Снапшот-совместимость ломает backup | средняя | versioned envelope, тест restore |
| Meta↔Raft расхождение | средняя | intent + reconcile + алерт |
| Decommission стирает диск слишком рано | высокая | point of no return, retention, backup check |
| Тесты медленные/флаки | средняя | deterministic simulation, seeds, nightly matrix |
| Недооценка трудозатрат | высокая | верхняя граница, отдельный milestone, staged rollout |
| Нет pre-vote → election storm | средняя | pre-vote в том же milestone |
| Клон диска/backup | высокая | ClusterID/incarnation, ops-запрет до реализации |

## 12. Последствия

Плюсы:

- закрывает data-loss путь F4 и подтверждённую потерю F2;
- состав становится частью реплицированного состояния и восстанавливается
  детерминированно;
- decommission становится операцией с точками отказа и отката, а не набором
  RPC;
- появляется основа для observability (epoch, joint, pending change);
- meta и Raft перестают быть двумя независимыми источниками истины (intent +
  reconcile).

Минусы:

- существенная переделка Raft core (quorum, append, apply, snapshot,
  recovery) — самое рискованное изменение со времён Multi-Raft;
- +1 запись/+1 round-trip на изменение состава, временно расширенный кворум
  (в joint доступность ниже: 3→4 add требует 3 ack из 4);
- новые форматы WAL/снапшота и жёсткие правила отката;
- тесты и soak — недели календарного времени;
- операционные изменения: decommission, retention, capability flip, runbooks.

Пересмотреть ADR, если: появится требование multi-AZ/placement-aware
decommission (расширить §6); будет выбран внешний координатор (отклонить A);
pre-vote окажется несовместим с текущим транспортом; backup/PITR потребует
другого формата снапшота.

## 13. Ссылки

- D. Ongaro, J. Ousterhout. *In Search of an Understandable Consensus
  Algorithm (Extended Version)*, §4.1 (membership changes), §4.2.1 (joint
  consensus), §9.6 (pre-vote).
- etcd raft: `ConfChangeV2`, joint quorum, `pendingConfIndex` — практическая
  реализация тех же правил.
- CockroachDB/TiKV: range membership, learner catch-up, decommission.
- `internal/raft/membership.go`, `internal/raft/node.go`,
  `internal/raft/filestore.go`, `internal/raft/types.go`,
  `internal/metaserver/membership.go`, `internal/kvserver/membership.go`,
  `proto/v2.proto`, `test/jepsen_partition_test.go`.
