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
- Optional, off-by-default `ConfigMap/<base>-range-bootstrap` and a
  post-install `Job/<base>-range-bootstrap` for a fresh cluster's range map
- Secret **references** only. The chart does not create or render Secrets.

`<base>` is the Helm fullname (release name + chart name, or
`fullnameOverride`).

## Prerequisites

1. Kubernetes 1.25+, three AZs, a block StorageClass that supports
   `ReadWriteOnce` (and expansion for disk growth).
2. A multi-binary image built from the repository `Dockerfile`. It contains
   `/app/kv2`, `/app/sqlsrv`, `/app/pgsrv`, `/app/kvctl`, `/app/kvbackup`,
   `/app/edgehash`, the legacy `/app/db`, `/bin/sh` and CA certificates, and
   runs as UID/GID 10001. The chart starts the v2 binaries by absolute path.
3. TLS Secret (default `distributed-db-tls`) with `ca.crt`, `tls.crt`,
   `tls.key`; SANs cover the meta/KV headless wildcards and the meta, sqlsrv and
   pgsrv Services; leaf has `serverAuth` and `clientAuth` EKUs.
4. Admin token Secret (default `distributed-db-admin-token`, key `token`).
5. pgwire auth Secret (default `distributed-db-pgsrv-auth`, keys `username`,
   `password`).

The chart fails at render time if a required Secret reference is empty. That
is deliberate. Never put secret values in `values.yaml`, `--set` on a shared
shell, or Git.

## Build and publish the image

The chart's default image name is `distributed-db:0.1.0`, which is the local
tag produced by:

```bash
make image IMAGE_REPOSITORY=distributed-db IMAGE_TAG=0.1.0
```

That repository is not published by this project. For a real cluster, publish
the image to a registry you control and override both values:

```bash
make image IMAGE_REPOSITORY=registry.example.com/distributed-db IMAGE_TAG=v0.1.0
make image-push IMAGE_REPOSITORY=registry.example.com/distributed-db IMAGE_TAG=v0.1.0

helm upgrade --install ddb deploy/helm/distributed-db \
  --namespace distributed-db --create-namespace \
  --set image.repository=registry.example.com/distributed-db \
  --set image.tag=v0.1.0 \
  --values prod-values.yaml --atomic --timeout 20m
```

The tag and the `VERSION`/`REVISION` build args default to `git describe` and
`git rev-parse HEAD`, never the wall clock, so rebuilding one commit produces
the same tag and metadata. `docker login` is required for `image-push`; the
target never stores credentials. `/app/db` is the legacy v1 binary and remains
the image's default `ENTRYPOINT` for `docker-compose.yml` compatibility; chart
pods override the command.

A `v*` git tag triggers `.github/workflows/release.yml`. That workflow runs the
local release dry-run, builds `linux/amd64` and `linux/arm64` with Buildx, and
pushes `ghcr.io/<owner>/<repo>:<tag>` using the short-lived `GITHUB_TOKEN`.
Buildx attaches SBOM and provenance attestations to the image. The workflow
also lints and packages this chart and attaches the `.tgz` and the image digest
to the GitHub release. It does not deploy to a cluster. No release has been
executed as part of this chart's validation.

## Install

```bash
make helm-lint

helm upgrade --install ddb deploy/helm/distributed-db \
  --namespace distributed-db --create-namespace \
  --values prod-values.yaml --atomic --timeout 20m
```

See the rendered NOTES for verification and rollback commands.

## Fresh-cluster range bootstrap (optional)

A fresh meta starts with range 1 and **no replicas**. The KV data path can use
the `-nodes` seed list, but split/merge and the placement scheduler need an
explicit replica list. The chart can perform the one-time bootstrap for a new
cluster:

```bash
helm upgrade --install ddb deploy/helm/distributed-db \
  --namespace distributed-db --create-namespace \
  --values prod-values.yaml \
  --set bootstrap.enabled=true \
  --atomic --timeout 20m
```

What the Job does:

- `post-install` hook only. `helm upgrade` does not run it, so it cannot rewrite
  an existing range map.
- Waits for a meta leader (bounded by `bootstrap.waitTimeoutSeconds`), then runs
  `/app/kvctl ... init-ranges -replicas <KV_NODES>` using the fully qualified
  KV pod addresses rendered by the chart.
- Is idempotent and fail-closed: `init-ranges` only bootstraps a meta with
  exactly one range and no replicas; re-running it with the same replica set is
  a no-op; it refuses a split map or a different existing replica set because
  `SetRanges` replaces the whole range map.
- Reads the admin token from `auth.adminToken.existingSecret` via
  `secretKeyRef`; there is no literal/default token. If
  `meta.adminToken.enabled=false`, it uses the same unauthenticated admin RPCs
  the cluster already exposes.
- Has `bootstrap.backoffLimit`, `activeDeadlineSeconds` and
  `ttlSecondsAfterFinished`; a persistent failure fails the install instead of
  leaving a half-bootstrapped cluster unnoticed.

Enable it **only** for the initial install of a new cluster. Do not enable it
for a cluster that already has data, split ranges or a restored range map.
When disabled, the range map stays as-is; the rendered NOTES prints the exact
`kvctl init-ranges` command to run manually.

## Important defaults

| Value | Default | Notes |
|---|---|---|
| `image.repository` / `image.tag` | `distributed-db` / `0.1.0` | local build tag; publish and override |
| `meta.replicaCount` | `3` | 3 or 5 only |
| `kv.replicaCount` | `3` | 3–5 only |
| `meta.port` / `kv.port` | `9090` / `7001` | gRPC |
| `tls.enabled` / `tls.mtls` | `true` / `true` | cluster gRPC only |
| `meta.adminToken.enabled` | `true` | native `-admin-token` |
| `sqlsrv.auth.enabled` | `true` | native Bearer on `/v1/sql`, `/metrics`, `/debug/slo`; requires TLS |
| `pgsrv.auth.enabled` | `true` | requires TLS |
| `bootstrap.enabled` | `false` | one-time post-install range-map Job |
| `bootstrap.ttlSecondsAfterFinished` | `600` | Job/pod cleanup after completion |
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
- The optional bootstrap is a metadata-only, single-range operation. It does
  not create KV range groups, repair a split map, or prove Raft quorum and data
  availability. It is not a general repair tool.

## Validation

The chart and image are checked in CI-style with:

```bash
./scripts/validate-release.sh          # shell, Helm, YAML; set WITH_IMAGE=1 to build
./scripts/verify-release.sh            # release dry-run; prints the Docker command, never pushes
make helm-lint
make image IMAGE_REPOSITORY=distributed-db IMAGE_TAG=0.1.0
```

`values.schema.json` validates the critical values before templates render:
image repository/tag (an unpinned `latest` tag is rejected), meta/KV replica
counts, TLS/mTLS and Secret names/keys, the admin-token and pgSRV auth Secret
references, and bootstrap integer/timeout ranges. It contains references only;
it defines no default passwords, tokens, or certificates.

The validation script renders the default and bootstrap-enabled variants and
parses every document as YAML. It skips Helm only if Helm is not installed
(pass `REQUIRE_HELM=1` to make that fatal). `verify-release.sh` additionally
checks the release workflow, the multi-arch build command in dry-run mode, the
Dockerfile and chart files, and that the values schema rejects invalid input.
It also skips Helm when it is unavailable.

Kubernetes API schema validation remains the operator's gate; for example:

```bash
helm template ddb deploy/helm/distributed-db --namespace distributed-db | \
  kubeconform -strict -summary -ignore-missing-schemas -kubernetes-version 1.29.0
```

Also render the no-TLS (with sqlsrv/pgsrv auth disabled), 5-KV,
cert-manager/ingress, no-PVC and pgsrv-no-auth variants. See OPERATIONS §9.3 for
the exact matrix.
