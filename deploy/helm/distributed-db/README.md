# distributed-db Helm chart

Reference deployment for a private, single-tenant v2 deployment: replicated
meta/TSO, a 3–5 node Multi-Raft KV plane, and stateless `sqlsrv`/`pgsrv`
gateways.

> **This chart is an operations baseline, not a production certification.**
> The database is experimental alpha; the repository's production gates G1–G7
> are still open. No cluster deployment is claimed. Read
> [docs/OPERATIONS.md](../../../docs/OPERATIONS.md) and
> [docs/RUNBOOKS.md](../../../docs/RUNBOOKS.md) before installing.

## What it creates

- `StatefulSet/<base>-meta` (3 or 5 pods, PVCs, headless service) — meta/TSO
- `StatefulSet/<base>-kv` (3–5 pods, PVCs, headless service) — KV data plane
- `Deployment/<base>-sqlsrv` (2 pods, native HTTPS listener + Bearer auth)
- `Deployment/<base>-pgsrv` (2 pods, TLS listener + password auth)
- ClusterIP and headless Services, ConfigMap with startup scripts, PDBs,
  ServiceAccount, optional cert-manager `Certificate` and Ingress
- Secret **references** only. The chart does not create or render Secrets.

`<base>` is the Helm fullname (release name + chart name, or
`fullnameOverride`).

## Prerequisites

1. Kubernetes 1.25+, three AZs, a block StorageClass that supports
   `ReadWriteOnce` (and expansion for disk growth).
2. An image containing `/app/kv2`, `/app/sqlsrv`, `/app/pgsrv`, `/app/kvctl`,
   `/app/kvbackup`, `/bin/sh` and CA certificates, running as UID 10001.
   The repository Dockerfile builds only `cmd/db`; the default image is an
   explicit placeholder. See OPERATIONS §3.1.
3. TLS Secret (default `distributed-db-tls`) with `ca.crt`, `tls.crt`,
   `tls.key`; SANs cover the meta/KV headless wildcards and the meta, sqlsrv and
   pgsrv Services; leaf has `serverAuth` and `clientAuth` EKUs.
4. Admin token Secret (default `distributed-db-admin-token`, key `token`).
5. pgwire auth Secret (default `distributed-db-pgsrv-auth`, keys `username`,
   `password`).

The chart fails at render time if a required Secret reference is empty. That
is deliberate. Never put secret values in `values.yaml`, `--set` on a shared
shell, or Git.

## Install

```bash
helm lint --strict deploy/helm/distributed-db

helm upgrade --install ddb deploy/helm/distributed-db \
  --namespace distributed-db --create-namespace \
  --values prod-values.yaml --atomic --timeout 20m
```

See the rendered NOTES for verification and rollback commands.

## Important defaults

| Value | Default | Notes |
|---|---|---|
| `meta.replicaCount` | `3` | 3 or 5 only |
| `kv.replicaCount` | `3` | 3–5 only |
| `meta.port` / `kv.port` | `9090` / `7001` | gRPC |
| `tls.enabled` / `tls.mtls` | `true` / `true` | cluster gRPC only |
| `meta.adminToken.enabled` | `true` | native `-admin-token` |
| `sqlsrv.auth.enabled` | `true` | native Bearer on `/v1/sql`, `/metrics`, `/debug/slo`; requires TLS |
| `pgsrv.auth.enabled` | `true` | requires TLS |
| `meta.persistence.size` | `20Gi` | reference size |
| `kv.persistence.size` | `100Gi` | **not** a 1 TiB production size |
| `*.topologySpread` | zone `DoNotSchedule` for meta/KV | use `ScheduleAnyway` on fewer than 3 zones |

## Deliberate limitations

- kv2/meta expose no HTTP metrics/health; probes are TCP-only.
- sqlsrv uses native TLS and one static Bearer token, not RBAC/audit. `/metrics`
  requires the token (configure Prometheus), while `/health` and `/healthz` are
  public. Probes are TCP-only because kubelet cannot verify the private CA.
- `kvbackup` is plaintext-only; restore on a TLS/token cluster is blocked.
- No joint consensus/decommission, no tested rolling upgrade, no PITR, no
  RBAC/audit/at-rest encryption, no per-pod certificates, no NetworkPolicy.
- Fresh installs need a one-time `SetRanges` bootstrap for split/merge
  (OPERATIONS §3.3.1). The chart does not automate it.

## Validation

The chart is checked in CI-style with:

```bash
helm lint --strict deploy/helm/distributed-db
helm template ddb deploy/helm/distributed-db --namespace distributed-db | \
  ruby -ryaml -e 'YAML.load_stream(STDIN.read); puts "yaml ok"'
```

Schema validation (Kubernetes 1.29, CRDs skipped):

```bash
helm template ddb deploy/helm/distributed-db --namespace distributed-db | \
  kubeconform -strict -summary -ignore-missing-schemas -kubernetes-version 1.29.0
```

Also render the no-TLS (with sqlsrv/pgsrv auth disabled), 5-KV,
cert-manager/ingress, no-PVC and pgsrv-no-auth variants. See OPERATIONS §9.3 for the exact matrix.
