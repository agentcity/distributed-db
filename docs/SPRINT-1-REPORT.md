# Sprint 1 report — production gates G1/G2/G3/G4/G6

- Дата: 2026-11-16
- HEAD: `cc15879` (до финального отчёта)
- План: [PRODUCTION-PLAN.md](PRODUCTION-PLAN.md)
- Оценка: [ASSESSMENT.md](ASSESSMENT.md)

## 1. Итог

Sprint 1 закрыл **компоненты и ADR**, но не production-ворота целиком.

| # | Задача | Статус | Артефакт |
|---|---|---|---|
| 1 | Durability production profile | ✅ | `-durability=sync|fast`, sync default |
| 2 | Crash-injection harness | ✅ | `test/crash_consistency_test.go` |
| 3 | Membership safety RFC | ✅ ADR | `docs/adr/0005-membership-safety.md` |
| 4 | PITR RFC | ✅ ADR | `docs/adr/0006-pitr.md` |
| 5 | pgwire TLS/auth | ⚠️ partial | direct TLS + cleartext-over-TLS |
| 6 | 1 ТБ benchmark harness | ✅ harness, no run | `test/large_bench_test.go` |
| 7 | Nightly chaos CI | ✅ | workflow + script + Makefile |
| 8 | gRPC TLS/mTLS helper | ⚠️ component | `internal/grpctls`, not integrated |

## 2. Что проверено

- `go build ./...` — OK.
- `go vet ./...` — OK.
- `gofmt` tracked Go files — OK.
- `go test -race ./...` — все пакеты зелёные.
- Crash test:
  - fast: SIGKILL → restart → все 32 acknowledged keys восстановлены replay Raft-лога;
  - sync: SIGKILL → restart → все keys восстановлены, Pebble WAL replay.
- Jepsen partitions (после фикса harness):
  - `TestJepsenPartitionMinorityFollower`,
  - `TestJepsenPartitionLeaderIsolation`,
  - `TestJepsenPartitionAsymmetric`,
  - `TestJepsenCrossRangePartition` — PASS.
- Nightly script smoke: 15 tests PASS, 0 FAIL, ~66 s (1 round).
- Large bench smoke (0.05 GiB):
  - 209,715 keys, logical 51.2 MiB, 0 errors;
  - seq-write p99 ~35.9 ms, par-write p99 ~139 ms (100 samples);
  - seq-read p99 ~2.1 ms, par-read p99 ~1.7 ms;
  - disk ~560 MiB.
- pgwire: TLS 1.3 handshake, wrong password 28P01, auth-without-TLS rejected.
- gRPC TLS helper: TLS 1.3/mTLS handshakes, wrong CA rejected, TLS 1.2 rejected.

## 3. Критические находки ADR

### Membership (ADR 0005)

- **Реальный data-loss path**: узел, пропустивший ≥2 membership changes и
  установивший snapshot, навсегда остаётся со старым voter set (snapshot не
  содержит membership). После раздела два кворума разных конфигураций могут
  подтвердить конфликтующие записи.
- `configInFlight` — in-memory; смена лидера между commit и apply позволяет
  второе изменение.
- Meta/Raft divergence: `AddReplica` early-return по `containsAddr` без проверки
  Raft; `RemoveReplica` может удалить voter и не обновить range map.
- Нужны joint consensus + config в snapshot + epoch + decommission.
  Оценка: **14–19.5 person-weeks**; это не Sprint 1.

### PITR (ADR 0006)

- Текущий logical backup не гарантирует exact-at-TSO: `mvcc.Scan` игнорирует
  locks, коммит с `commitTS <= T_base`, разрешённый после скана, может быть
  пропущен. Нужен base barrier.
- RPO 0 только в strict ack mode (quorum + archive checkpoint).
- RTO < 30 min на 1 ТБ требует base ≤24 ч и ≥2.5 GB/s restore.
- Оценка production: **20.5–27.5 person-weeks**; MVP 12–15 ew.

## 4. Где Sprint 1 не закрыл ворота

- **G1**: crash harness — process SIGKILL, не power-loss; production default
  sync, fast — opt-in. Нужен longer soak и disk/power fault injection.
- **G2**: только ADR; кода joint consensus нет. Это главный блокер.
- **G3**: только ADR; PITR нет.
- **G4**: pgwire TLS direct (не libpq SSLRequest upgrade); gRPC TLS helper не
  интегрирован в kvserver/meta/v2client; нет mTLS на кластере, RBAC, audit,
  encryption at rest.
- **G6**: harness есть, 1 ТБ не прогнан; p99 на loopback, не real network.
- **G7**: nightly CI есть, но GitHub-hosted workflow не запускался; алерты,
  operator, runbooks отсутствуют.

## 5. Исправленный Jepsen harness

До спринта `JEPSEN=1` partition-тесты падали: node `ID` = client proxy,
`Peers` = directed link proxy; voter IDs не совпадали. Добавлен
`kvserver.Config.PeerAddrs` (logical peer ID → dial address), тест использует
стабильные client-facing ID и направленные proxy-адреса. Все 4 partition
сценария проходят.

Это **тестовая инфраструктура**, не production-фича; в production `PeerAddrs`
обычно nil.

## 6. Следующий спринт

1. **G2 joint consensus** — начать с config entry schema, joint quorum,
   snapshot envelope; затем decommission. Это 8–11 недель с 2 инженерами.
2. **G4 gRPC/meta TLS integration** — применить `grpctls` к kvserver,
   meta, v2client, Raft; mTLS; тесты mixed plaintext/TLS rejection.
3. **G1 soak** — 72 ч crash/disk fault, power-loss model.
4. **G3 PITR MVP** — base barrier + archive + restore to isolated cluster.
5. **G6 1 ТБ** — реальный прогон на 3 узлах/NVMe/сети, не loopback.

## 7. Вывод

Sprint 1 дал **проверяемые компоненты** и обнаружил два серьёзных
корректностных разрыва (membership snapshot, PITR base barrier). Это полезнее,
чем зелёный CI, который их скрывал.

До production по-прежнему **12–15 месяцев** для private single-tenant
(3–5 инженеров), и главный путь — G2, а не новые фичи.
