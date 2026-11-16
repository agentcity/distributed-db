# ADR 0002: MVCC и Percolator-style 2PC

- Статус: accepted
- Дата: 2026-11-16
- Контекст: M1 KV v2, подготовка SQL

## Контекст

SQL-слой требует транзакций, snapshot isolation и чтения на timestamp. Если
сделать простой `Put(key,value)` без версий, SQL придётся переписывать KV.
Нужна модель, которая:

- даёт консистентный снимок на `start_ts`;
- поддерживает атомарную запись нескольких ключей;
- переживает падение клиента/координатора между prewrite и commit;
- масштабируется по range и не требует глобального лока.

## Решение

MVCC + optimistic **Percolator-style 2PC**:

- `StartTS` и `CommitTS` выдаёт TSO из meta service (батчами).
- Данные: `d | userKey | commitTS` → value.
- Write-record: `w | userKey | commitTS` → startTS/kind (нужен для MVCC lookup
  и GC).
- Lock: `l | userKey` → {primary, startTS, kind, value, ttl}.
- `Prewrite` пишет locks и data-версии, проверяет write-conflict после
  `startTS`; primary lock — точка разрешения.
- `Commit` проверяет locks, пишет write-records, удаляет locks; идемпотентен.
- `Rollback` удаляет locks и пишет rollback-record.
- Чтение: самая новая data-версия с `commitTS <= readTS`; lock с
  `startTS <= readTS` → `ErrLocked` (разрешение локов — следующий этап).
- GC удаляет версии старше safe point; locks не удаляет — их разрешает
  lock resolver через Raft.

Транзакция optimistic: конфликт обнаруживается на prewrite/commit, клиент
повторяет с новым `startTS`.

## Альтернативы

| Вариант | Почему нет |
|---|---|
| Single-version KV | SQL не получит snapshot isolation без переделки |
| Пессимистичные локи на все ключи | глобальные/range-локи, deadlocks, хуже latency |
| 2PC с отдельным координатором | координатор — SPOF и лишний hop; Percolator переносит состояние в KV |
| Calvin / deterministic ordering | другой профиль latency и сложность; не для M1 |
| Raft-replicated transaction log | дороже по latency, чем locks в KV |

## Последствия

- Нужны TSO, lock resolution, GC, TTL и тесты на падения в середине 2PC.
- Возможны вечные локи: обязательны TTL, cleanup и метрики `locks_oldest`.
- `ErrLocked` на чтении нужно уметь разрешать (rollback/commit по primary).
- SQL сможет использовать `start_ts`/`commit_ts` и range-скан версий.
- Цена: больше данных на диск (data+write records) и latency на commit.

## Проверка

- Snapshot isolation, write conflict, lock conflict, rollback, reopen.
- Падение после prewrite до commit → lock разрешается, данные не теряются.
- GC не удаляет версии, нужные активным транзакциям.
- SQL-совместимость: row encoding и вторичные индексы ложатся на MVCC.
