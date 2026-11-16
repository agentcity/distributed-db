# ADR 0004: SQL-слой поверх транзакционного KV

- Статус: proposed
- Дата: 2026-11-16
- Контекст: M5, зависит от M3 (2PC, TSO, smart client)
- Связанные документы: `docs/ROADMAP.md`, `docs/CONTRACTS-V2.md`,
  `docs/adr/0002-mvcc-percolator.md`

## Контекст

`sqldb` строится только над транзакционным KV API: MVCC-версии,
Percolator-style 2PC, упорядоченные range-сканы, TSO и range-шардирование.
SQL не знает о Raft и лидерах; он видит `Txn` с `start_ts`, операции
`Get`/`Scan`/`Prewrite`/`Commit`/`Rollback` и ошибки конфликтов. Второй
журнал или координатор размножит источники истины.

ADR фиксирует дорогие поздние изменения: формат ключей, границу
gateway/storage, транзакции, диалект, порядок поставки.

### Допущения

| Допущение | Значение |
|---|---|
| Нагрузка | OLTP: point get/put, короткие range-сканы, до сотен строк |
| Диалект | PostgreSQL-подобное подмножество, не полная совместимость |
| Объем | 1–10 ТБ, таблицы до 10^9 строк |
| Изоляция | snapshot isolation; serializable не в v1 |
| Регион | один регион, 3 AZ; мультирегион не цель |
| Аналитика | OLAP, columnar, большие агрегации не цель |
| Транзакции | до 100 тыс. мутаций и 100 МБ, TTL 10 минут |

### Границы v1

Не входят: CTE, оконные функции, триггеры, процедуры, внешние ключи,
`SELECT FOR UPDATE`, `MERGE`, `DECIMAL`, массивы, JSON, пользовательские
типы, вьюхи, несколько баз. Одна база, схема `public`; их имена есть в
каталоге и wire.

## Parser, AST и planner

### Диалект v1

`CREATE TABLE` (PK обязателен, `NOT NULL`, `DEFAULT`-константа), `DROP TABLE`,
`CREATE INDEX`/`DROP INDEX`, `INSERT`, `SELECT` (projection, `WHERE`,
`INNER JOIN`, `GROUP BY`, `HAVING`, `ORDER BY`, `LIMIT`/`OFFSET`), `UPDATE`,
`DELETE`, `BEGIN`/`COMMIT`/`ROLLBACK`, `EXPLAIN`, ограниченные `SHOW`/`SET`.
Подзапросы — некоррелированные, в `FROM` и `WHERE`; `UNION` — после v1.

### Варианты парсера

| Вариант | Лицензия | Диалект | Go и сборка | Состояние | Минус |
|---|---|---|---|---|---|
| Свой lexer + recursive descent | своя | PG-подмножество | pure Go, stdlib | пишем | 4–6 недель, риск дыр |
| `vitessio/vitess` sqlparser | Apache-2.0 | MySQL | pure Go, крупные зависимости | активен | MySQL-синтаксис и AST |
| `pingcap/parser` | Apache-2.0 | MySQL | pure Go, тяжелые зависимости | заморожен с 2023 | тянет TiDB, MySQL |
| `cockroachdb-parser` | Apache-2.0 | PostgreSQL | pure Go, извлечен из CRDB | архив с 04.2026 | AST чужой семантики |
| `auxten/postgresql-parser` | Apache-2.0 | PostgreSQL (CRDB 20.1) | pure Go | мало мейнтейнеров | форк устарел, bus factor |
| `pg_query_go` | BSD-3-Clause | PostgreSQL | cgo + libpg_query | активен, v6 | cgo, protobuf-AST, версия PG |

CockroachDB-репа не годится как зависимость: BSL 1.1 и огромное дерево. `pg_query_go`
точен, но требует cgo, усложняет static/cross-compile, а его AST — protobuf.

### Решение по парсеру

Пишем свой lexer, AST и recursive-descent парсер: PostgreSQL-синтаксис
вместо MySQL, свой AST для binder/planner, чистый Go без cgo, малое
подмножество. Ориентир — 4–6 человеко-недель.

`pg_query_go` используем только как test-oracle: differential-тест «наш
парсер принял/отверг то же, что PostgreSQL» под build tag `cgo`.

### Planner

Parse → bind (каталог, типы, имена) → logical plan → physical plan →
executor. Logical: `Scan`, `Filter`, `Project`, `Join`, `Aggregate`, `Sort`,
`Limit`, `Insert`, `Update`, `Delete`. Physical: `PointGet`, `TableScan`,
`IndexScan`, `IndexJoin`, `HashJoin`, `BroadcastHashJoin`, `HashAgg`,
`TopN`, `Sort`.

В v1 — rule-based planner без CBO. Правила: равенство/префикс по PK →
`PointGet` или узкий range; равенство по префиксу индекса → `IndexScan`;
`ORDER BY`, совпадающий с порядком индекса, → потоковое чтение; иначе
`TableScan`. Индекс не берем, если таблица меньше ~1000 строк или предикат
не по префиксу. `EXPLAIN` печатает план и оценку строк.

CBO и гистограммы — после v1: без статистики cost-модель врет.

## Каталог, row и index encoding

### Пространства ключей

Первый байт `userKey` задает namespace; MVCC добавляет свои `d`/`l`/`w`
уровнем ниже, коллизий нет.

| Префикс | Назначение | Ключ |
|---|---|---|
| `0x01` | каталог | `0x01, kind, id или имя` |
| `0x02` | данные | `0x02, table_id(8), encoded_pk` |
| `0x03` | индекс | `0x03, index_id(8), index_key, encoded_pk` |
| `0x04` | unique-marker | `0x04, index_id(8), index_key` |
| `0x05` | последовательности | `0x05, seq_id(8)` |
| `0x06` | статистика | `0x06, table_id(8), kind` |
| `0x07` | DDL jobs и lease | `0x07, kind, id(8)` |

`table_id` и `index_id` — глобальные `uint64` из meta `AllocID`, big-endian,
не переиспользуются: `DROP TABLE`/`CREATE TABLE` не дают ложных совпадений.

### Каталог

Ключи: `0x01,0x01,db_id` → database; `0x01,0x02,table_id` → table;
`0x01,0x03,index_id` → index; `0x01,0x10,db_id,table_name` → table_id;
`0x01,0x11,table_id,index_name` → index_id; `0x01,0x20` → `schema_version`.
Значения — protobuf с `format_version`. Table descriptor: `table_id`,
`db_id`, имя, колонки (id, имя, тип, nullable, default, ordinal), PK-колонки,
индексы, `schema_version`.

Каталог читается на `start_ts`. Gateway кэширует descriptor с lease 2
секунды и инвалидирует по `schema_version`; DDL выдерживает паузу в два
lease, прежде чем версия станет видимой.

### Row encoding

Ключ: `0x02 | table_id | encoded_pk`. Значение:
`format_version(1) | schema_version(uvarint) | null_bitmap | non-PK values`
в порядке ordinal. Ширины фиксированы для `INT4/INT8/FLOAT8/BOOL/TIMESTAMP`;
`TEXT`/`BYTEA` — uvarint length + bytes. Тегов колонок нет: типы берет
descriptor. `schema_version` в значении позволяет читать старые строки после
`ADD COLUMN` без переписывания таблицы.

PK обязан быть `NOT NULL` и иммутабельным; `UPDATE` PK — delete+insert в той
же транзакции. PK в ключе кодируется order-preserving: `INT` — big-endian
с инверсией sign bit; `TEXT` — `0x00` → `0x00 0xFF`, терминатор
`0x00 0x01`; `BOOL` — 0/1; `FLOAT8` — инверсия битов по знаку; `TIMESTAMP` —
int64 микросекунды UTC с инверсией sign bit.

### Вторичные индексы

Индекс: `0x03 | index_id | encoded(index cols) | encoded_pk`, значение
пустое — PK уже в ключе. Для `(a,b)` префикс `(a)` — валидный range scan.
`NULL` кодируется маркером `0x00`/`0x01`, чтобы `IS NULL` был префиксным
условием. Индексы v1 — ASC; `DESC` в v1 — сортировка.

Уникальность нельзя проверить чтением: при SI две транзакции увидят «записи
нет» и обе вставят. Для unique-индекса пишется marker
`0x04 | index_id | index_key` → PK в той же транзакции; конфликт prewrite на
marker-ключе отменяет одну из транзакций. Для `NULL` в unique-колонках
marker не пишется: PostgreSQL допускает несколько `NULL`.

Индексы обновляются в той же транзакции. Covering-индексы не в v1:
удваивают запись.

### Версионирование схемы и DDL

`schema_version` монотонно растет на каждый DDL. Строка помнит версию, по
которой записана; decoder берет описание этой версии из каталога.
`DROP COLUMN` только помечает колонку в новой версии; старые значения
игнорируются и вычищаются фоновой перезаписью.

`CREATE INDEX` в v1 блокирующий: запись останавливается, backfill идет
сканом, затем версия переключается. Online backfill (snapshot + dual-write +
delta) — отдельная фаза. DDL не смешивается с DML: берет lock-ключ
`0x07,0x01,table_id` с TTL, ждет активные DML и меняет каталог. Транзакция,
начавшаяся до DDL, проверяет `schema_version` перед commit и при
несовпадении получает retry, а не пишет строку в старом формате.

## Распределенное исполнение

### Где исполняется

v1: весь parse/bind/plan/execute — в stateless gateway; storage-узлы
остаются KV и не знают SQL. Gateway берет snapshot каталога на `start_ts`,
получает диапазоны ключей через client и шлет `Get`/`Scan` лидерам range.
Параллелизм — 8–16 воркеров, батчи по 128 строк.

Coprocessor (pushdown filter/projection/aggregate в storage-узел) — не v1:
нужен gRPC-сервис, выражения в proto, evaluator. Сначала измеряем
amplification gateway-only; при bottleneck добавляем узкий coprocessor.

### Сканы, фильтры, проекции

Point get — один `Get` по PK. Range scan — вычисление `[start,end)` из
предикатов, разбиение по границам range из meta и слияние упорядоченных
потоков. `Filter` и `Project` в v1 выполняются в gateway после декодирования
строки; проекция сокращает только трафик.

`LIMIT` проталкивается в KV только для скана без фильтра и в порядке
индекса: на каждый range уходит `limit+offset`, глобальный лимит считается
после слияния. При `Filter` ранний limit даст неверный результат.

### JOIN

| Тип | v1 | Позже |
|---|---|---|
| Index nested loop | да, по PK или unique-индексу | — |
| Broadcast hash join | да, build side до 64 МБ | адаптивный выбор |
| Shuffle hash join | нет | репартиционирование, spill |
| Sort-merge join | нет | при сортированных входах |
| Full outer join | нет | после v1 |

Порядок join — left-deep эвристика; перестановка для 3–4 таблиц по размеру.
Broadcast допустим, если правая сторона после фильтров меньше 64 МБ или
имеет `LIMIT`. Shuffle join не делаем: нужен обмен между воркерами, а OLTP
обходится index nested loop.

### Агрегаты, ORDER BY, LIMIT

Агрегация — hash aggregate в gateway, ключ группы кодируется тем же
order-preserving кодеком. При превышении 256 МБ — spill на диск. `COUNT(*)`
идет сканом; `SUM`/`MIN`/`MAX`/`AVG` считаются потоково по range с
частичными агрегатами в gateway. `DISTINCT` — hash set с тем же лимитом.

`ORDER BY` без индекса — top-N heap при наличии `LIMIT`, иначе external
merge sort с run-файлами.

## Транзакции и изоляция

### start_ts, commit_ts, 2PC

Gateway получает `start_ts` из TSO один раз на транзакцию; все чтения идут
на этом снимке. Мутации буферизуются в gateway и в конце: `Prewrite` с
primary (первый ключ), `commit_ts` из TSO, `Commit` primary и остальных.
Ошибка на любом шаге — `Rollback`; commit идемпотентен и повторяется после
сбоя. Read-only транзакция не делает prewrite и не получает `commit_ts`.

Read-your-writes дает локальный буфер; locks других транзакций с
`start_ts <= read_ts` дают `ErrLocked`. Транзакция больше 100 тыс. мутаций
или 100 МБ аварийно завершается: лимит лучше OOM.

### Изоляция

Snapshot isolation. Write skew возможна: два чтения видят непересекающиеся
множества, обе записи коммитятся. Serializable не в v1, потому что требует
predicate locks или SSI, которых нет в KV API. Уникальность защищена
marker-локами, внешние ключи и `SELECT FOR UPDATE` не поддерживаются.

### Retry и разрешение локов

| Ошибка | Действие |
|---|---|
| `ErrWriteConflict` | новый `start_ts`, повтор всей транзакции |
| `ErrLocked` | `CheckTxnStatus` по primary, commit/rollback, retry |
| `ErrNotLeader`/`ErrUnavailable` | обновить range cache, retry с тем же ts |
| timeout/ctx canceled | abort, без retry |
| schema version mismatch | перечитать каталог, перепланировать |
| transaction too large | abort, ошибка клиенту |

Повторяется вся транзакция, а не statement: иначе нарушится снимок SI. Для
autocommit допустим statement-level retry. Бюджет — 10 попыток или 10
секунд, exponential backoff с jitter. Lock resolution:
primary закоммичен → докатить вторичные; primary отменен → откатить; primary
пропал и нет commit-record → откатить по TTL. Без этого `ErrLocked`
превращается в вечный цикл.

### DDL и транзакции

DDL — отдельная транзакция с блокировкой таблицы, не смешивается с DML.
DML фиксирует `schema_version` на старте и проверяет перед commit; при
несовпадении транзакция отменяется и повторяется. Долгие сканы удерживают
safe point GC, поэтому TTL транзакции 10 минут, а GC не идет дальше самой
старой активной транзакции.

### Чего не хватает в KV-контракте

Блокеры M5; закрыть в M3/M4:

1. `internal/client` не имеет транзакционного API и TSO: только
   `Get/Put/Delete/Scan` без `start_ts`. Нужен `Txn` с `Get`, `Scan`,
   `Prewrite`, `Commit`, `Rollback` и доступом к TSO.
2. `Scan` принимает один адрес и не покрывает range-границы. Нужен
   range-aware iterator, переразрешающий лидера при split.
3. В `internal/mvcc` нет `CheckTxnStatus`/`ResolveLock`; есть только
   `Get/Scan/Prewrite/Commit/Rollback/GC`. Без них SQL не разрешает локи.
4. TSO живет в in-memory `meta.Cluster`, а не в сетевом сервисе: либо
   gateway ко-локализуется с meta, либо meta получает RPC до M5.

## PostgreSQL wire

Wire — продуктовый интерфейс: без него SQL бесполезен для драйверов. v1:
startup, `SSLRequest`, SCRAM-SHA-256, simple query, extended protocol
(`Parse`/`Bind`/`Describe`/`Execute`/`Sync`), text format для всех типов v1
и binary для `INT2/INT4/INT8/FLOAT8/BOOL/TEXT/TIMESTAMP`, `RowDescription`,
`DataRow`, `CommandComplete`, `ReadyForQuery`, SQLSTATE, `CancelRequest`.

Сообщения кодируем через `jackc/pgx/v5/pgproto3` (MIT): framing и разбор
пакетов, серверный state machine наш. SCRAM собираем на
`golang.org/x/crypto` (pbkdf2/hmac, уже в `go.mod`).

`pg_catalog` минимальный: `pg_type`, `pg_class`, `pg_namespace`,
`pg_attribute`, `pg_database`, `pg_settings`, функции `version()`,
`current_database()`, `current_schema()`, `pg_backend_pid()`. Этого мало для
ORM: GORM/SQLAlchemy/Rails лезут глубже. Совместимость с 2–3 ORM — отдельный
gate. `server_version` сообщаем как 15.x только после compat-тестов: клиенты
включают по нему синтаксис и форматы.

Не входят: `COPY`, протокол репликации, `LISTEN/NOTIFY`, курсоры с частичной
выборкой, `MD5`/`GSSAPI`, клиентские сертификаты, несколько результатов в
extended protocol, binary `NUMERIC`, полный `information_schema`,
расширения. `COPY` и курсоры — первые кандидаты в v2.

## План поставки и оценки

Оценки — человеко-недели.

| Фаза | Содержание | Выход | pw |
|---|---|---|---|
| P0 | `Txn`, range-aware scan, lock resolution, TSO RPC | SQL читает/пишет через 2PC | 3–4 |
| P1 | Lexer, AST, parser, ошибки, fuzz | парсит диалект v1 | 4–6 |
| P2 | Каталог, кодеки, DDL, `CREATE TABLE` | таблица создается и читается | 4–5 |
| P3 | Binder, planner, executor: scan, filter, project, DML | один стол без индексов | 6–8 |
| P4 | Индексы, unique marker, index scan/join | индекс ускоряет запросы | 4–6 |
| P5 | Broadcast hash join, aggregate, sort, подзапросы | JOIN/GROUP BY/ORDER BY | 6–8 |
| P6 | PG wire, auth, pg_catalog, prepared statements | psql и pgx работают | 5–7 |
| P7 | Retry, lock resolution, метрики, compat- и crash-тесты | можно включать | 4–6 |
| P8 | Coprocessor, online index, CBO | производительность | 8–12 |

До P7: 36–50 pw; для двух инженеров 5–7 месяцев. Demo P0–P3 (17–23 pw) —
2–3 месяца. «3–6 месяцев» из roadmap реальны лишь для среза без wire и JOIN.

## Риски

| Риск | Серьезность | Митигация |
|---|---|---|
| Planner расползается | высокая | заморозить грамматику v1, CBO после P7 |
| Нет статистики — плохие планы | высокая | эвристики, `EXPLAIN`, счетчики позже |
| Сетевой amplification | высокая | батчи, index-only, coprocessor в P8 |
| Конфликты на горячих ключах | средняя | backoff, бюджет retry, метрики |
| Долгие сканы блокируют GC | средняя | TTL транзакции, safe point, ctx |
| Ошибки online DDL | высокая | blocking DDL, version check на commit |
| Долгий хвост `pg_catalog` | высокая | 2–3 ORM как gate |
| Формат ключей зафиксирован | высокая | version byte, golden-тесты, ID не переиспользуются |
| Память в join/aggregate | средняя | лимиты, spill, отказ вместо OOM |
| Баги конкурентности | средняя | `-race`, crash-тесты, Jepsen-style |

## Отклоненные альтернативы

| Альтернатива | Почему нет |
|---|---|
| SQLite/DuckDB через cgo | не распределенная, нет MVCC/2PC |
| CockroachDB parser в runtime | BSL-репа, извлеченный парсер архивлен, AST чужой |
| `pg_query_go` в runtime | cgo, статические сборки, protobuf-AST |
| MySQL-диалект (vitess/pingcap) | продукт заявлен как PostgreSQL-like |
| CBO сразу | нет статистики, вырастет срок |
| Coprocessor-first | сначала нужны стабильные семантика и proto |
| Одна KV-запись на колонку | write amplification, много ключей на строку |
| Hash-шардинг вместо range | убивает сканы и префиксы индексов |
| Serializable/2PL | нет predicate locks в KV, latency и deadlocks |
| Отдельное хранилище для SQL | два источника истины, обход 2PC |
| Свой wire-протокол | теряем драйверы, ORM и psql |
| Полная совместимость с PostgreSQL | недостижима в v1; фиксируем non-goals |

## Последствия

Плюсы: SQL использует существующие 2PC/MVCC/range-сканы, не добавляет
второй координатор; формат ключей order-preserving и версионируемый;
gateway stateless; поставка разбита на работающие срезы.

Минусы: SI допускает write skew; blocking DDL и отсутствие FK — реальные
ограничения; gateway-only исполнение дает amplification; без CBO возможны
плохие планы; wire-совместимость — долгий хвост каталога.

Пересмотреть ADR, если KV не получит `ResolveLock`/`Txn` до M5; появится
требование serializable; понадобится OLAP.

## Проверка

- Golden-тесты кодеков: порядок ключей, NULL, unicode, границы int.
- Property-тесты encode/decode row для всех версий схемы.
- Интеграция: `CREATE TABLE` → `INSERT` → `SELECT` через 2PC; отказ узла
  между prewrite и commit; retry на конфликте.
- Уникальность: две конкурентные вставки одного значения — ровно одна
  коммитится.
- Wire: psql, pgx, GORM; `SELECT 1`, prepared statement, SQLSTATE.
- Chaos: kill gateway после prewrite, split range во время скана.
