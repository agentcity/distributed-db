# Sprint 2 report — аудит и админ-плоскость

- Дата: 2026-11-17
- HEAD: `843e670` (TLS) + docs
- Аудит: [docs/analysis/README.md](analysis/README.md)

## 1. Что сделано

| Направление | Результат |
|---|---|
| Скорость | аудит + исправление ошибки единиц в документации |
| Объём данных | конверт ёмкости, блокеры GC/WAL/split/backup |
| Безопасность | 14 findings; gRPC TLS/mTLS интегрирован |
| Надёжность | 4 production-блокера, top-5 инвестиций |
| kvctl admin | ranges/status/split/merge/backup |
| gRPC TLS | KV/meta/Raft + v2client + CLI |

## 2. kvctl admin

```bash
kvctl -meta 127.0.0.1:9090 ranges
kvctl -meta 127.0.0.1:9090 status
kvctl -meta 127.0.0.1:9090 split 1 m
kvctl -meta 127.0.0.1:9090 merge 1 2
kvctl -nodes ... backup -out ./backup
```

- `ranges/status/split/merge` — meta-only, без `-nodes`.
- `backup` использует `internal/backup`; логический, без PITR.
- Split/merge safety делегирована metaserver; CLI не заменяет проверки.
- E2E: split → get → ranges → merge → backup → verify — PASS.

## 3. gRPC TLS

- `grpctls.Options`; TLS 1.3/min, mTLS через client CA.
- `kvserver`: listener + peer transport.
- `raftgrpc`: meta Raft peer transport.
- `metaserver`: listener + все исходящие dials.
- `v2client`: meta failover + KV dials.
- CLI: `kv2/sqlsrv/pgsrv/kvctl` флаги `-tls-*`.
- Тесты: mTLS 3-node Raft, plaintext/certless rejection, TLS client
  round-trip, meta-only TLS.
- **Gap:** TLS opt-in; plaintext default; `kvbackup` plaintext; admin RPC
  на data-порту; нет RBAC/audit/at-rest.

## 4. Главные находки

1. **Скорость:** старые 5.5k tx/s — ошибка единиц; fast 10 клиентов
   ~480–820 tx/s; production sync ~40–70 tx/s, p50 151 мс.
2. **Sync batching:** fsync на каждую команду внутри Raft-батча; один
   Pebble batch на батч — ожидаемо 5–10×.
3. **Объём:** 1 ТБ диска влезет, процесс — нет: GC/WAL/split/backup.
4. **Reliability:** membership не в snapshot, split не атомарен, lifecycle
   отсутствует, backup не консистентен.
5. **Security:** transport TLS закрыт, но perimeter (SQL HTTP, admin RPC,
   RBAC/audit/at-rest) — нет.

## 5. Следующий спринт

1. Один Pebble batch/fsync на Raft-батч (sync throughput).
2. Membership config in snapshot + joint consensus.
3. Реплицированный split/merge child snapshot.
4. Периодический snapshot + MVCC GC + ENOSPC guard.
5. Lock-aware backup/base barrier.
6. SQL HTTP TLS/auth; admin-listener + RBAC.
