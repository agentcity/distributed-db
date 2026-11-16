---
task_id: "0005"
type: research
title: SQL-слой поверх транзакционного KV — ADR
created_at: 2026-11-16T01:30:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# SQL-слой поверх KV — ADR

## Проблема

Второй продукт — SQL — нельзя проектировать до стабильного KV API. Нужно
зафиксировать, как SQL будет использовать MVCC/2PC/range scans, какие
компоненты писать, а какие брать библиотекой, и в каком порядке.

## Требования ADR

- выбор parser/AST/planner (своё vs библиотека, лицензия, Go-совместимость);
- схема каталога, row encoding, secondary indexes;
- distributed execution: point/range scans, joins, aggregates, order/limit;
- транзакции и изоляция поверх KV 2PC;
- PostgreSQL wire: объём совместимости;
- поэтапный план и риски;
- рекомендация: минимальный SQL subset первым.

## Критерии приёмки

- [ ] `docs/adr/0004-sql-layer.md` с сравнением вариантов и рекомендацией.
- [ ] План фаз SQL согласован с `docs/ROADMAP.md` (M5).
- [ ] Явно перечислены не-цели и цена каждого варианта.

## Ссылки

- `docs/ROADMAP.md`
- `docs/CONTRACTS-V2.md`
