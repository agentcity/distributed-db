# Трек: SQL / NewSQL

- **Статус:** experimental SQL P0 поверх v2 KV. Это **не отдельная СУБД** и
  **не production**.
- **Что это:** слой `parser → planner → executor → v2client → KV`; SQL не
  владеет Raft-логом и не хранит данные отдельно от KV.
- **Не в этом треке:** сам KV-кластер — [kv-cluster.md](kv-cluster.md);
  edgehash — [edgehash.md](edgehash.md).
- **Практика:** [SQL за 5 минут](../quickstart/sql-5-minutes.md),
  [USER-GUIDE.md §5–7](../USER-GUIDE.md).

## Текущий статус

| Часть | Статус | Что это значит |
|---|---|---|
| SQL HTTP `cmd/sqlsrv` | experimental P0 | `POST /v1/sql`, JSON-ответы, маленькие данные |
| pgwire `cmd/pgsrv` | experimental | simple + extended protocol, psql; один статический пользователь |
| Планировщик | базовый | index selection, CBO по row count, plan cache; без гистограмм и JOIN order |
| Production | нет | Нет полной PostgreSQL-совместимости, RBAC/audit, p99/1 ТБ |

SQL P0 рассчитан на небольшие OLTP-наборы и учебные/внутренние сценарии, а не
на аналитику, публичный DBaaS или замену PostgreSQL.

## Точки входа

| Что | Где |
|---|---|
| SQL HTTP | `cmd/sqlsrv`: `POST /v1/sql`, `/health`, `/healthz`, `/metrics`, `/debug/slo` |
| PostgreSQL wire | `cmd/pgsrv`: `psql` и клиенты с simple/extended protocol |
| Парсер и AST | `internal/sqlparser` |
| Планировщик и executor | `internal/sqlengine` |
| HTTP-адаптер | `internal/sqlhttp` |
| pgwire | `internal/pgwire` |
| KV-клиент | `internal/v2client` (SQL использует его транзакции и TSO) |
| Решения | [ADR 0004](../adr/0004-sql-layer.md) |
| Тесты | `test/` (SQL P0 на реальном KV, split, pgwire) |

## Что поддерживается

**DDL**

- `CREATE TABLE`, `DROP TABLE`; PK обязателен; типы — `INT` и `TEXT`.
- `CREATE INDEX`, `DROP INDEX` — не уникальный вторичный индекс по одной
  колонке.
- Одна база, схема `public`; каталог, строки и индексы лежат в KV.

**DML и запросы**

- `INSERT` — одна строка за statement.
- `SELECT`: projection, `WHERE` по PK или indexed-колонке, `ORDER BY` по одной
  колонке, `LIMIT`.
- `WHERE`: `=`, `!=`, `<`, `<=`, `>`, `>=`, `BETWEEN`; только один предикат,
  без `AND/OR`.
- Агрегаты `COUNT/SUM/AVG/MIN/MAX`, `GROUP BY`, один `INNER JOIN` с
  равенством в `ON`.
- `UPDATE` и `DELETE` требуют `WHERE`; multi-row update/delete по
  PK/indexed-колонке. PK нельзя обновлять.
- Каждая команда — KV-транзакция; cross-range SQL идёт через 2PC; SQL
  работает после split.

**Интерфейсы и безопасность**

- HTTP: JSON `{"sql":"..."}`; ответы `columns/rows` или
  `rows_affected/message`.
- pgwire: simple + extended (`Parse/Bind/Execute`), параметры `INT/TEXT`.
- `sqlsrv`: опциональные TLS на HTTP-listener и Bearer-token; auth требует TLS.
- `pgsrv`: опциональный direct TLS и пароль для одного статического
  пользователя; auth требует TLS.
- Cluster TLS к meta/KV включается `-tls-ca`/`-tls-server-name`; TLS только на
  listener не шифрует gRPC-трафик к кластеру.

## Что не поддерживается или ограничено

- Нет `AND/OR`, более двух таблиц, `LEFT/RIGHT/FULL/CROSS JOIN`, CTE,
  оконных функций, `HAVING`, `OFFSET`, `EXPLAIN`, SQL-level
  `BEGIN/COMMIT/ROLLBACK`.
- Нет `NULL`, `DECIMAL`, `JSON`, внешних ключей, online DDL, массивов,
  пользовательских типов, views и нескольких баз.
- Нет `pg_catalog`, JDBC/pgx-совместимости и полного PostgreSQL-диалекта;
  pgwire не делает `SSLRequest` upgrade (нужен direct TLS), нет RBAC/audit.
- Нет parallel/distributed join и аналитики/OLAP; JOIN ограничен двумя
  таблицами и лимитами строк.
- `UPDATE/DELETE` без `WHERE` отклоняются; planner без гистограмм и
  JOIN order.
- Это не production: нет подтверждённых p99, 1 ТБ soak и полной
  security-модели.

## Следующие шаги

1. `AND/OR`, более двух JOIN, `NULL/DECIMAL/JSON/FK`, online DDL.
2. Планировщик: гистограммы, JOIN order, parallel/distributed join.
3. Совместимость: `pg_catalog`, JDBC/pgx, `SSLRequest`, больше типов.
4. Безопасность/эксплуатация: RBAC, audit, лимиты, multi-tenant.
5. SLO и soak на реальных объёмах; differential-тесты парсера.

Полный план — в [ROADMAP.md](../ROADMAP.md) (M5) и
[ADR 0004](../adr/0004-sql-layer.md); оценка — в
[ASSESSMENT.md](../ASSESSMENT.md).

## Связанные документы

- [USER-GUIDE.md](../USER-GUIDE.md) — SQL HTTP, psql, TLS/auth.
- [kv-cluster.md](kv-cluster.md) — KV-фундамент, на котором работает SQL.
- [PRODUCT-READINESS.md](../PRODUCT-READINESS.md) — гейты до production.
