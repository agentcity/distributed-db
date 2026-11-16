# Architecture Decision Records

| ADR | Решение | Статус |
|---|---|---|
| [0001](0001-storage-lsm.md) | LSM на Pebble как storage engine | accepted |
| [0002](0002-mvcc-percolator.md) | MVCC и Percolator-style 2PC | accepted |
| [0003](0003-multiraft-ranges.md) | range-based Multi-Raft и metadata service | accepted |
| [0004](0004-sql-layer.md) | SQL-слой поверх транзакционного KV | proposed |

Правило: решение, которое меняет архитектуру, контракт пакета или формат
данных, сначала фиксируется здесь, затем реализуется. ADR не переписывается
задним числом — устаревшее решение помечается `superseded` и заменяется новым.
