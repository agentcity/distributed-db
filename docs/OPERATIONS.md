# Operations manual: private single-tenant deployment

- Date: 2026-11-17
- Scope: the reference Helm chart in [`deploy/helm/distributed-db`](../deploy/helm/distributed-db)
  and the v2 binaries (`cmd/kv2`, `cmd/sqlsrv`, `cmd/pgsrv`, `cmd/kvctl`,
  `cmd/kvbackup`).
- Companion: [RUNBOOKS.md](RUNBOOKS.md) — step-by-step incident and maintenance
  procedures.
- Product assessment: [PRODUCT-READINESS.md](PRODUCT-READINESS.md),
  [PRODUCTION-PLAN.md](PRODUCTION-PLAN.md).

## 0. Status and non-claims

**This is an operations baseline, not a production certification.** The
repository's own assessment classifies v2 as experimental alpha and lists
blocking gates (G1 durability evidence, G2 membership safety, G3 backup/PITR,
G4 security, G5 upgrade, G6 SLO/chaos, G7 operations). The chart makes the
topology deployable and repeatable; it does not close those gates.

The chart and these documents were validated by `helm lint`, `helm template`,
YAML parsing, kubeconform schema checks and shell syntax checks only. **No
cluster deployment was performed and no failover, backup, restore or upgrade
was executed against real infrastructure.** Treat every procedure here as a
plan to rehearse in staging.

Two immediate consequences:

1. The image is built from the repository `Dockerfile` and contains `db`,
   `kv2`, `sqlsrv`, `pgsrv`, `kvctl`, `kvbackup` and `edgehash`; publish it to
   your registry and override `image.repository`/`image.tag` (§3.1).
2. SQL HTTPS uses the native sqlsrv TLS listener and a single static Bearer
   token (`-auth-token`). It is transport security plus one operator
   credential, not RBAC, per-user identity or audit. The optional ingress adds
   a second TLS hop but no user authentication.

## 1. Topology

### 1.1 Components

```
                       TLS ingress (optional)
                               |
  clients ──HTTPS/bearer──> sqlsrv Deployment (2) ──mTLS gRPC──┐
                                                               ├──> KV StatefulSet (3–5)
  psql ───TLS/password────> pgsrv Deployment (2) ──mTLS gRPC──┘         │  Multi-Raft ranges
                                                               │        │  Pebble data + Raft logs
                                                               └──> meta StatefulSet (3)
                                                                    replicated Raft: range map + TSO
```

| Component | Workload | Default replicas | Ports | Persistent state |
|---|---|---|---|---|
| meta / TSO | StatefulSet | 3 (3 or 5) | 9090 gRPC | `/data/meta` (Raft WAL/snapshots) |
| KV data plane | StatefulSet | 3 (3–5) | 7001 gRPC | `/data` (`db/shared`, `raft/range-<id>`, `ranges.json`) |
| sqlsrv | Deployment | 2 | 8080 HTTPS (TLS + bearer) | none |
| pgsrv | Deployment | 2 | 5432 pgwire, 9187 metrics/health | none |
| ConfigMap | — | — | — | startup scripts + topology |
| Secrets | operator-managed | — | — | TLS, admin token, pgwire password |

Replica-count rules are enforced by the chart: meta must be 3 or 5; KV must be
3–5. An even meta group is rejected at render time.

### 1.2 Identity and DNS

Every pod uses a fully qualified, stable identity. Node IDs are **DNS names,
never pod IPs**:

```
<base>-meta-<i>.<base>-meta-headless.<namespace>.svc.<clusterDomain>:9090
<base>-kv-<i>.<base>-kv-headless.<namespace>.svc.<clusterDomain>:7001
```

`<base>` is the Helm fullname (release name plus chart name unless
`fullnameOverride` is set). The startup scripts derive the local identity from
`POD_NAME` and the peer list from the headless service; the local node is
excluded from `-meta-peers` / `-peers`.

The SQL and pgwire gateways are addressed through ClusterIP services:

```
<base>-meta.<namespace>.svc.<clusterDomain>:9090
<base>-kv.<namespace>.svc.<clusterDomain>:7001
<base>-sqlsrv.<namespace>.svc.<clusterDomain>:8080
<base>-pgsrv.<namespace>.svc.<clusterDomain>:5432
```

`sqlsrv` and `pgsrv` receive the complete KV pod list in `-nodes`. The meta
service is only a bootstrap seed: v2client calls `Leader` and then dials the
reported meta leader's stable pod DNS name. Certificate SANs must therefore
cover both pod wildcards and the service names (see §3.2).

### 1.3 Availability design

- Three AZs: `topologySpreadConstraints` with
  `topology.kubernetes.io/zone`, `maxSkew: 1`, `DoNotSchedule` for meta and KV
  by default. This gives one meta replica per AZ and spreads 3–5 KV pods.
  On a cluster with fewer than three zones, set
  `meta.topologySpread.whenUnsatisfiable=ScheduleAnyway` (and the same for KV),
  otherwise pods stay Pending.
- A second spread constraint uses `kubernetes.io/hostname` with
  `ScheduleAnyway`, plus preferred pod anti-affinity, to avoid co-locating two
  replicas when the scheduler has a choice.
- PDBs: meta `minAvailable: 2`, KV `minAvailable: 2`, sqlsrv/pgsrv
  `minAvailable: 1`. With three replicas only one voluntary disruption is
  allowed. A PDB is not a quorum guarantee: it cannot make a node drain safe if
  another replica is already unhealthy.
- `podManagementPolicy: Parallel` for meta/KV. All pods start together so a
  static peer set can form. Readiness is TCP-only (see §7), so a pod can be
  "ready" before its Raft group has a leader or quorum.
- Headless services set `publishNotReadyAddresses: true` so starting peers can
  resolve each other.

### 1.4 What is deliberately not deployed

- No coordinator (`cmd/db coordinator`) is deployed. The v2 topology uses
  meta/TSO + KV + SQL/pgwire; the v1 coordinator has a separate protocol and
  port layout.
- No metrics aggregator (`cmd/metricsrv`), no backup CronJob, no object-store
  uploader, no NetworkPolicy, no ServiceMonitor and no alert rules. §7 and §9
  describe the gaps.

## 2. Capacity

### 2.1 Default requests and limits

| Component | CPU request / limit | Memory request / limit | PVC |
|---|---|---|---|
| meta | 100m / 1 | 256Mi / 512Mi | 20Gi |
| KV | 1 / 2 | 4Gi / 8Gi | 100Gi |
| sqlsrv | 250m / 1 | 512Mi / 1Gi | — |
| pgsrv | 250m / 1 | 256Mi / 512Mi | — |

These are reference values chosen to make the chart schedulable. They are not
the result of a 1 TB soak or a p99 run. Size from your own measurements.

### 2.2 Disk sizing

Each range is replicated (production placement is three replicas). A rough
usable-capacity formula:

```
raw_needed_per_cluster ≈ logical_data × replication_factor / (1 - headroom)
raw_per_kv_pod          ≈ raw_needed_per_cluster / kv.replicaCount
```

With RF=3 and 30% headroom for Pebble compaction, snapshots and temporary split
copies, 1 TiB of logical data needs ~4.3 TiB raw. That is ~1.43 TiB per pod on
three KV pods or ~860 GiB per pod on five. The chart default of 100 GiB per KV
pod is a smoke-test size, not a 1 TiB production size.

Also account for:

- Raft logs and snapshots per range, which can temporarily exceed the data size
  during catch-up or a split;
- `kvbackup`/`kvctl backup` output. A logical backup is roughly the logical data
  size plus chunk framing; it is not compressed by the chart and is not
  encrypted by the database;
- meta WAL/snapshots. 20 GiB is ample for a range map unless range counts grow
  into the very high thousands; watch it before a split-heavy workload.

Use a block storage class with **predictable fsync latency** (local NVMe or
equivalent). `-durability=sync` fsyncs every state-machine batch. NFS/SMB and
burst-only network volumes are unsuitable: Raft heartbeat and commit latency
will follow the storage tail latency.

### 2.3 Memory, IOPS and connections

- KV memory is dominated by Pebble block cache, memtables, per-range Raft state
  and snapshot buffers. Start at 4 GiB request / 8 GiB limit per pod; monitor
  Go heap and OOM kills. Do not raise replicas to solve a memory hotspot before
  checking range distribution.
- Disk IOPS and fsync latency dominate write p99. Capacity without IOPS is not
  capacity.
- SQL and pgwire are stateless. Scale them horizontally within the limits of
  the KV layer; they do not add KV capacity.
- There is no connection pooler or query admission control in the chart.
  PostgreSQL clients can open many connections; pgsrv has no `max_connections`.
  Cap clients and CPU before relying on pgsrv under load.
- Check the container runtime's open-file limit for Pebble. The chart does not
  set `ulimits`; verify on the node if the workload hosts many ranges.

### 2.4 Scaling is not a normal operation

The peer lists are rendered from the replica count. Changing
`meta.replicaCount` or `kv.replicaCount` changes the arguments of **all**
pods on the next rollout; it does not perform a Raft membership change.
Joint consensus and an operator-driven decommission flow are not implemented
(see [adr/0005-membership-safety.md](adr/0005-membership-safety.md)).

- Do not scale a running KV or meta StatefulSet to add capacity.
- Do not change a replica count to replace a permanently lost node.
- Treat a different replica count as a new cluster, installed from a backup, or
  wait for membership tooling.

The safe day-2 operation is replacing a pod **with the same ordinal and the
same PVC** (node recovery), not adding or removing voters.

## 3. Deployment

### 3.1 Build the image first

Build and publish the multi-binary image:

```bash
make image IMAGE_REPOSITORY=<registry>/distributed-db IMAGE_TAG=<tag>
make image-push IMAGE_REPOSITORY=... IMAGE_TAG=...
```

The image contains `/app/kv2`, `/app/sqlsrv`, `/app/pgsrv`, `/app/kvctl`,
`/app/kvbackup`, `/app/edgehash`, `/app/db`, CA certificates and `/bin/sh`.
It runs as UID/GID 10001 and writes only under `/data` and `/tmp`.
Override `image.repository`/`image.tag`; defaults are a local tag, not a
published image.

The chart targets a build whose `sqlsrv -h` includes `-tls-cert`, `-tls-key`,
`-tls-ca`, `-tls-server-name` and `-auth-token`, and whose `kv2 -h` includes
`-mode=meta`, `-admin-token` and the cluster TLS flags. Verify these before
installing:

```bash
docker run --rm <image> /app/sqlsrv -h 2>&1 | grep -E -- '-tls-cert|-auth-token'
docker run --rm <image> /app/kv2 -h 2>&1 | grep -E -- '-admin-token|-mode'
```

If a flag is missing, the image predates the chart; do not deploy it. The chart
does not fall back to plaintext SQL or to a sidecar proxy.

### 3.2 Secrets and certificates (never in values.yaml)

The chart does not create Secrets. Create these in the release namespace before
installing.

**TLS Secret** (default name `distributed-db-tls`), keys `ca.crt`, `tls.crt`,
`tls.key`:

```bash
NS=distributed-db
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "$NS" create secret generic distributed-db-tls \
  --from-file=ca.crt="$TLS_DIR/ca.crt" \
  --from-file=tls.crt="$TLS_DIR/tls.crt" \
  --from-file=tls.key="$TLS_DIR/tls.key"
```

Certificate requirements:

- SANs must cover:
  `*.<base>-meta-headless.<ns>.svc.<clusterDomain>`,
  `*.<base>-kv-headless.<ns>.svc.<clusterDomain>`,
  `<base>-meta.<ns>.svc.<clusterDomain>`,
  `<base>-sqlsrv.<ns>.svc.<clusterDomain>`,
  `<base>-pgsrv.<ns>.svc.<clusterDomain>`.
- The leaf must have **both** `serverAuth` and `clientAuth` EKUs because the
  same identity is presented on outbound mTLS dials.
- The CA bundle in `ca.crt` must verify every pod certificate. During CA
  rotation it may contain two CA certificates (see §6).
- The Secret is mounted read-only; the binaries read certificates at process
  start. Updating the Secret does not reload a running pod.

`cert-manager` is optional. Set
`tls.certManager.enabled=true` and `tls.certManager.issuerRef.name=<issuer>` and
the chart renders a `Certificate` with the SANs above. Wildcard SANs require a
DNS-01 issuer. If your issuer does not place `ca.crt` in the Secret (for
example a public ACME issuer, which cannot mint a private CA), supply the CA
bundle yourself or use a CA/self-signed issuer; the cluster's mTLS will not work
without a shared trust root.

The chart uses **one shared certificate for every pod**. That is simpler to
rotate but gives every pod the same identity and private key. Per-pod
certificates are better and are a chart limitation, not a database limitation.

**Admin token Secret** (default `distributed-db-admin-token`, key `token`):

```bash
umask 077
openssl rand -base64 48 | tr -d '/+=' | cut -c1-48 > /tmp/ddb-admin.token
kubectl -n "$NS" create secret generic distributed-db-admin-token \
  --from-file=token=/tmp/ddb-admin.token
rm -f /tmp/ddb-admin.token
```

The value must be header-safe (`[A-Za-z0-9._~-]{32,128}`). The same token is
used by:

- `kv2 -mode=meta -admin-token`: a native Bearer check on `SetRanges`,
  `SplitRange`, `MergeRanges` (`metaserver.Config.AdminToken`);
- `sqlsrv -auth-token`: a native Bearer check on `/v1/sql`, `/metrics` and
  `/debug/slo`; `/health` and `/healthz` stay public.

It is not sent on read-only meta RPCs or data-path KV RPCs, and it is not a
per-user identity or RBAC system. Because the **same** token grants both meta
admin RPCs and full SQL access, do not hand it to SQL-only clients: anyone with
it can rewrite the range map if they can reach meta. There is no scoped token or
separate roles in this build.

Set `meta.adminToken.enabled=false` and `sqlsrv.auth.enabled=false` only for a
documented, isolated break-glass window. `sqlsrv` refuses a token without TLS,
so disabling TLS also requires disabling its auth.

**pgwire password Secret** (default `distributed-db-pgsrv-auth`, keys
`username`, `password`):

```bash
umask 077
read -r -s -p "pgsrv password: " PG_PASSWORD; echo
printf '%s' "$PG_PASSWORD" > /tmp/ddb-pg.pass
unset PG_PASSWORD
kubectl -n "$NS" create secret generic distributed-db-pgsrv-auth \
  --from-literal=username=dbadmin \
  --from-file=password=/tmp/ddb-pg.pass
rm -f /tmp/ddb-pg.pass
```

`pgsrv` refuses password auth without TLS. There is no default username or
password in the chart. The password is passed to the binary as a flag by the
startup script; it is visible in the container's process arguments to anyone
who can inspect that container. This is a pgsrv limitation.

### 3.3 Install

Create a production values override (never put Secret values in it):

```yaml
# prod-values.yaml — example only
clusterDomain: cluster.local
image:
  repository: registry.example.internal/distributed-db
  tag: "2026.11.0"
  pullPolicy: IfNotPresent

kv:
  replicaCount: 5
  persistence:
    size: 1Ti
    storageClass: fast-nvme
  resources:
    requests: {cpu: "2", memory: 8Gi}
    limits:   {cpu: "4", memory: 16Gi}

meta:
  persistence:
    size: 50Gi
    storageClass: fast-nvme

sqlsrv:
  replicaCount: 2
pgsrv:
  replicaCount: 2
```

Render and inspect before applying:

```bash
helm template ddb deploy/helm/distributed-db \
  --namespace "$NS" --values prod-values.yaml > /tmp/ddb.yaml

helm lint --strict deploy/helm/distributed-db --values prod-values.yaml

helm upgrade --install ddb deploy/helm/distributed-db \
  --namespace "$NS" --values prod-values.yaml \
  --atomic --timeout 20m
```

The chart's default `tls.enabled=true`, `tls.mtls=true`,
`meta.adminToken.enabled=true`, `sqlsrv.auth.enabled=true` and
`pgsrv.auth.enabled=true`. Install fails at render time if the corresponding
Secret reference is empty, or if sqlsrv auth is enabled without TLS. That is
intentional: fail closed rather than deploy a plaintext cluster by accident.

### 3.3.1 Range-map bootstrap (required before split/merge)

A fresh meta server starts with the default range 1, `(-inf, +inf)`, and **no
replicas** (`meta.New`). Each KV process starts with its legacy default range 1
and the static peer list from `-peers`. The data path can work because v2client
falls back to the `-nodes` seed list when the range descriptor has no replicas,
but this is not a production placement:

- `kvctl ranges` may show range 1 with an empty replica list and an empty leader;
- `kvctl split`/`merge` and the placement/scheduler paths require explicit
  replicas, and can fail with "range has no replicas" until the map is set;
- every KV pod is a member of range 1's Raft group because they were started
  with the full `-peers` list, regardless of what meta says.

`kvctl init-ranges -replicas <kv-pod-addresses>` sets range 1's replicas
idempotently. The chart can run it as an optional post-install Job:

```bash
helm upgrade --install ... --set bootstrap.enabled=true
```

The Job waits for a meta leader, calls `init-ranges` with the KV pod DNS
addresses, and is safe to retry (identical set no-op; a different existing set
is refused). `bootstrap.enabled` is **false** by default; enable it only for a
fresh, pre-data install. Do not run it against an existing cluster: `SetRanges`
replaces the whole range map and can orphan split ranges.

### 3.4 Verify

```bash
kubectl -n "$NS" get pods -o wide
kubectl -n "$NS" get pdb
kubectl -n "$NS" get endpoints "$BASE-meta" "$BASE-kv" "$BASE-sqlsrv" "$BASE-pgsrv"

kubectl -n "$NS" rollout status statefulset/"$BASE"-meta --timeout=10m
kubectl -n "$NS" rollout status statefulset/"$BASE"-kv   --timeout=10m
kubectl -n "$NS" rollout status deployment/"$BASE"-sqlsrv --timeout=5m
kubectl -n "$NS" rollout status deployment/"$BASE"-pgsrv  --timeout=5m
```

Define helpers used by this manual and the runbooks:

```bash
NS=${NS:-distributed-db}
BASE=${BASE:-ddb-distributed-db}
DOMAIN=${DOMAIN:-cluster.local}
META="$BASE-meta.$NS.svc.$DOMAIN:9090"
KV_REPLICAS=${KV_REPLICAS:-3}
KV_PORT=${KV_PORT:-7001}

kv_nodes() {
  out=""
  i=0
  while [ "$i" -lt "$KV_REPLICAS" ]; do
    out="${out}${out:+,}$BASE-kv-$i.$BASE-kv-headless.$NS.svc.$DOMAIN:$KV_PORT"
    i=$((i+1))
  done
  printf '%s' "$out"
}
NODES=$(kv_nodes)

kvctl_exec() {
  kubectl -n "$NS" exec deploy/"$BASE"-sqlsrv -c sqlsrv -- \
    env KVCTL_ADMIN_TOKEN="${KVCTL_ADMIN_TOKEN:-}" \
    /app/kvctl -meta "$META" -nodes "$NODES" \
      -tls-ca /etc/distributed-db/tls/ca.crt \
      -tls-cert /etc/distributed-db/tls/tls.crt \
      -tls-key /etc/distributed-db/tls/tls.key "$@"
}
```

If the image is distroless or lacks `/bin/sh`, these helpers do not work; the
chart's scripts already require a shell, so the image contract includes it.

Read-only checks:

```bash
kvctl_exec status
kvctl_exec ranges
```

Expected: a non-empty meta leader and term, and a range map that covers
`(-inf, +inf)` without overlaps or gaps. On a bootstrapped cluster every range
has a non-empty replica list and a leader; on a fresh install range 1 may still
show empty replicas/leader (see §3.3.1). `ranges` output is table format by
default; use `-format json` for machine checks.

SQL HTTPS (from a workstation, forward the Service locally; `--connect-to`
keeps the service DNS name for certificate verification while the TCP target is
localhost):

```bash
SVC="$BASE-sqlsrv.$NS.svc.$DOMAIN"
kubectl -n "$NS" port-forward svc/"$BASE"-sqlsrv 8080:8080 &
curl --cacert ca.crt --connect-to "$SVC:8080:127.0.0.1:8080" \
  "https://$SVC:8080/healthz"

TOKEN=$(kubectl -n "$NS" get secret distributed-db-admin-token \
  -o jsonpath='{.data.token}' | base64 -d)
curl --cacert ca.crt --connect-to "$SVC:8080:127.0.0.1:8080" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sql":"SELECT 1"}' "https://$SVC:8080/v1/sql"
```

From inside the cluster, use `https://$SVC:8080` without `--connect-to`.

`/health` and `/healthz` are public so kubelet can probe them; `/v1/sql`,
`/metrics` and `/debug/slo` require the bearer token. Without the token,
`POST /v1/sql` and `/metrics` must return `401`. Kubernetes probes are TCP
because kubelet cannot be configured with the private CA for an HTTPS probe.

pgwire (port-forward the Service; `hostaddr` forces the TCP target while `host`
is used for SNI and certificate verification):

```bash
SVC="$BASE-pgsrv.$NS.svc.$DOMAIN"
kubectl -n "$NS" port-forward svc/"$BASE"-pgsrv 5432:5432 &
psql "host=$SVC hostaddr=127.0.0.1 port=5432 user=dbadmin dbname=postgres \
      sslmode=verify-full sslrootcert=ca.crt"
```

`sslmode=verify-full` requires the server certificate SAN to include the
service DNS name and the client's CA bundle to match. Do not use
`sslmode=disable` for production verification.

### 3.5 Uninstall

```bash
helm uninstall ddb -n "$NS"
```

StatefulSet PVCs are not deleted by Helm or by the StatefulSet default policy.
List and remove them only after a verified backup and an explicit decision:

```bash
kubectl -n "$NS" get pvc -l app.kubernetes.io/name=distributed-db
# kubectl -n "$NS" delete pvc "data-$BASE-meta-0" "data-$BASE-kv-0" ...
```

Secrets are not deleted by the chart because it did not create them. Rotate or
delete them separately.

## 4. Upgrade and rollback

There is **no tested N-1 compatibility guarantee and no protocol version
negotiation** in this codebase. Upgrading a production cluster is a
maintenance-window operation until that exists.

Preconditions:

1. Read the release notes and any on-disk/protocol changes.
2. Take a logical backup and verify it (`kvbackup verify`). Copy it off the
   cluster.
3. Confirm `kvctl status` is healthy, every range has a leader, no split/merge
   is in progress, and no pod is crash-looping.
4. Confirm PVC free space (at least 25% free) and that the new image is
   pullable from every node.
5. Rehearse the exact upgrade in staging with a copy of production data.

Recommended sequence (only after staging proves compatibility):

```bash
helm upgrade --install ddb deploy/helm/distributed-db \
  --namespace "$NS" --values prod-values.yaml \
  --atomic --wait --timeout 20m
```

StatefulSets update one pod at a time in reverse ordinal order; sqlsrv/pgsrv
use `maxUnavailable: 0`, `maxSurge: 1`. Watch, in parallel:

```bash
kubectl -n "$NS" get pods -w
kubectl -n "$NS" logs -l app.kubernetes.io/component=meta --tail=50 --prefix
kubectl -n "$NS" logs -l app.kubernetes.io/component=kv   --tail=50 --prefix
```

Do not upgrade meta and KV at the same time by hand. Do not delete a
StatefulSet to "force" an update; delete one Pod and wait for readiness.

Rollback:

```bash
helm history ddb -n "$NS"
helm rollback ddb "<REVISION>" -n "$NS" --wait --timeout 20m
```

A Helm rollback restores manifests, not data. If the new binary performed an
incompatible on-disk or protocol change, the old binary may not read the data.
In that case, stop the cluster and restore the verified backup into a new
cluster. Never downgrade by copying old binaries into a cluster that already
wrote with the new format.

Detailed steps: [RUNBOOKS.md](RUNBOOKS.md#8-rolling-upgrade-and-rollback).

## 5. Backup and restore

### 5.1 What the backup is

`kvbackup` and `kvctl backup` produce a **logical, single-TSO** backup: all
ranges are scanned in one transaction at one timestamp. It is not continuous,
not incremental and not PITR. There is no WAL archiving, no object-store
uploader, no retention service and no encryption in the backup format.

Consequences:

- RPO equals the backup interval. A daily backup has up to 24 h of loss.
- Restore **overwrites keys present in the backup and does not delete keys
  that are absent**; restore into a new, empty cluster.
- Restore is not atomic and not isolated. Stop writers and fence clients.
- `kvbackup` is plaintext-only and cannot connect to this TLS-enabled chart.
  Use `kvctl backup` for the backup; the restore path is a known blocker (§9).

### 5.2 Backup procedure (TLS-capable)

`kvctl` supports the cluster TLS client flags. Run it inside a pod whose image
contains `/app/kvctl` and whose TLS Secret is mounted (sqlsrv/pgsrv pods
qualify). For small data sets:

```bash
BACKUP="/tmp/backup-$(date -u +%Y%m%dT%H%M%SZ)"

kubectl -n "$NS" exec deploy/"$BASE"-pgsrv -c pgsrv -- \
  /app/kvctl -meta "$META" -nodes "$NODES" \
    -tls-ca /etc/distributed-db/tls/ca.crt \
    -tls-cert /etc/distributed-db/tls/tls.crt \
    -tls-key /etc/distributed-db/tls/tls.key \
    -timeout 30m \
    backup -out "$BACKUP"

kubectl -n "$NS" cp "$BASE-pgsrv-<pod>:$BACKUP" "./$BACKUP"
```

`-out` must not already exist. `kubectl cp` requires a tar binary in the image;
the image contract includes Alpine/busybox, but verify it.

For larger data sets, do not use the pod's small `/tmp` emptyDir. Create a
dedicated Job/Pod with a dedicated PVC (and ideally direct object-store upload)
and mount the TLS Secret read-only. The chart does not ship that Job.

Offline verification (no cluster TLS needed):

```bash
kubectl -n "$NS" exec deploy/"$BASE"-pgsrv -c pgsrv -- \
  /app/kvbackup verify -in "/backups/<snapshot-dir>"
```

Verification checks the manifest, chunk checksums, framing and totals. It does
not prove that the backup can be restored into the current range map; use
`-strict-epoch` / `-check` at restore time.

Store backups encrypted at rest (bucket encryption or an encrypted volume),
outside the cluster's failure domain, with a retention policy. The database does
not encrypt them.

### 5.3 Restore outline

The full runbook is
[RUNBOOKS.md](RUNBOOKS.md#7-restore-from-backup). The constraints:

- Restore into a **new isolated cluster**, not into a serving one.
- The restore tool is `kvbackup restore`; it has no TLS or admin-token flags.
  A TLS-enabled target cannot use it, and a target with
  `meta.adminToken.enabled=true` rejects `-ensure-ranges` (`SetRanges` is an
  admin RPC). Current workaround: build/restore into a plaintext, token-free
  target on an isolated network, verify, then turn on TLS/admin auth before
  cutover (which requires a restart). Do not weaken a serving production
  cluster for a restore.
- `-ensure-ranges` rewrites the meta range map and requires the target KV nodes
  to already host the same boundaries. Without it, the target range layout must
  match the backup exactly.
- `-strict-epoch` requires target range versions to match the backup; use it
  only when you know the target is the same layout.
- `-check` reads every restored key back and compares values. It costs time;
  use it for drills and high-value restores.
- Extra keys in the target are not deleted. Use an empty target.

RPO/RTO are not proven. Measure your own backup interval and restore time; the
product target in [PRODUCTION-PLAN.md](PRODUCTION-PLAN.md) is RPO 0 and
RTO < 30 min, which the current logical backup cannot meet.

## 6. TLS and credential rotation

### 6.1 Leaf certificate renewal (same CA)

If the CA is unchanged (cert-manager renews a leaf, or you reissue from the same
CA):

1. Put the new `tls.crt`/`tls.key` and the existing `ca.crt` into the Secret.
2. Roll the pods so each process reloads the mounted files. The binaries read
   certificates at startup, not on every connection.
3. Restart sqlsrv/pgsrv first (they are clients), then KV, then meta. Or use
   `kubectl rollout restart` per workload and wait for readiness after each.
4. Verify mTLS (`kvctl_exec status`, SQL `/v1/sql`, `psql`) and confirm no
   certificate errors in logs.

Because all pods share one certificate and one key, there is no per-pod
identity to rotate independently.

### 6.2 CA rotation (mTLS trust root)

A single atomic switch of leaf + CA breaks mTLS: pods that have not restarted
still trust only the old CA, and new pods reject old pods' certificates. Use a
two-phase overlap:

1. Create the new CA and a new leaf signed by it.
2. **Phase 1:** update the Secret with the **old leaf/key** and a `ca.crt`
   bundle containing **old + new CA**. Roll every pod. Every process now trusts
   both CAs while still presenting the old certificate.
3. **Phase 2:** update the Secret with the new leaf/key and the same combined
   `ca.crt`. Roll every pod. Old and new pods trust both CAs during the roll.
4. **Phase 3:** remove the old CA from `ca.crt`, roll again, and verify.
5. Keep the old CA/leaf in a secure archive until the rollback window closes.

External SQL/pgwire clients verify the listener certificate too. Give them the
combined CA bundle before phase 1 and the final CA bundle before phase 3, or
their TLS verification will fail during the rotation. The ingress certificate
is separate (§6.5).

Roll one workload at a time and wait for `kubectl rollout status` before the
next. If a phase fails, stop and restore the previous Secret; do not continue
with a partially rotated trust root.

### 6.3 Admin token rotation

The token is read at process start by meta (`-admin-token`) and by sqlsrv
(`-auth-token`). Both support exactly one token, so there is no overlap window.

1. Announce a maintenance window for admin operations (`split`, `merge`,
   `SetRanges`); data-path reads/writes are unaffected by the token itself.
2. Update the Secret with the new token.
3. Roll meta:

   ```bash
   kubectl -n "$NS" rollout restart statefulset/"$BASE"-meta
   kubectl -n "$NS" rollout status statefulset/"$BASE"-meta --timeout=10m
   ```

4. Roll sqlsrv:

   ```bash
   kubectl -n "$NS" rollout restart deployment/"$BASE"-sqlsrv
   kubectl -n "$NS" rollout status deployment/"$BASE"-sqlsrv --timeout=5m
   ```

5. Update operator scripts/clients (`KVCTL_ADMIN_TOKEN`) and SQL HTTPS clients.
   Old tokens receive `401` from restarted meta/sqlsrv pods; the window is
   mixed while the workloads roll.
6. Verify an admin operation with the new token and a read-only operation
   without it.

### 6.4 pgwire password rotation

1. Update the pgwire Secret (`username`, `password`).
2. Roll pgsrv; it reads credentials at startup. Existing sessions are
   terminated by the restart; new connections must use the new password.
3. Verify with `psql` and `sslmode=verify-full`.
4. Keep the old password only as long as the rollback window requires; there is
   no dual-password mode.

### 6.5 Ingress TLS (optional)

If `ingress.enabled=true`, the ingress certificate is a separate Secret
(`ingress.tls.secretName`). Rotate it according to the ingress controller's
documentation; it is independent of the cluster mTLS Secret. The ingress TLS
protects client-to-ingress traffic. The ingress backend must speak HTTPS
(`nginx.ingress.kubernetes.io/backend-protocol: HTTPS` for nginx); ingress-to-pod
traffic is TLS with the pod certificate and is also protected by the bearer
token.

## 7. Monitoring

### 7.1 What emits metrics

| Source | Endpoint | Notes |
|---|---|---|
| sqlsrv | `:8080/metrics`, `/health`, `/healthz`, `/debug/slo` | native TLS; `/metrics` and `/debug/slo` require the bearer token, health endpoints are public |
| pgsrv | `:9187/metrics`, `/healthz`, `/debug/slo` | `metrics-addr`; no auth on this listener |
| kubelet / node-exporter | node, container, PVC stats | CPU, memory, disk, restarts, volume usage |
| KV / meta | **none** | no HTTP metrics server in `cmd/kv2`; TCP probes only |
| Logs | stdout | text `slog`; no structured JSON for kv2 (the `db` binary has JSON, kv2 does not) |

SQL metric families (prefix from the code):

```
sql_requests_total
sql_errors_total
sql_inflight
sql_request_duration_seconds_bucket
sql_statement_requests_total{kind="SELECT|INSERT|UPDATE|DELETE|..."}
sql_statement_errors_total
sql_statement_inflight
sql_statement_request_duration_seconds_bucket
```

pgsrv adds `pgwire_requests_total`, `pgwire_errors_total`, `pgwire_inflight`
and `pgwire_request_duration_seconds`. It also exposes the SQL engine metrics
under `sql_*`.

There are **no Raft, TSO, range, split/merge, WAL, snapshot-age or lock
metrics**. You cannot alert on meta leader changes or KV quorum from
Prometheus with this build. Use `kvctl status` as a synthetic check and logs
for diagnosis. The sqlsrv `/metrics` endpoint now requires the admin token; a
Prometheus scrape without it gets `401` and shows the target as down.

### 7.2 Minimum Prometheus scrape config

```yaml
scrape_configs:
  - job_name: distributed-db-sqlsrv
    scheme: https
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/distributed-db-admin.token
    tls_config:
      ca_file: /etc/prometheus/distributed-db-ca.crt
      server_name: <base>-sqlsrv.<ns>.svc.<clusterDomain>
    static_configs:
      - targets: ["<base>-sqlsrv.<ns>.svc.<clusterDomain>:8080"]

  - job_name: distributed-db-pgsrv
    scheme: http
    metrics_path: /metrics
    static_configs:
      - targets: ["<base>-pgsrv.<ns>.svc.<clusterDomain>:9187"]
```

Mount the admin token and CA into Prometheus from the same Secrets (or copy
them into a dedicated monitoring Secret). The pgsrv metrics listener is plain
HTTP and unauthenticated; restrict it with NetworkPolicy if metrics are
sensitive.

### 7.3 Alerts worth having first

| Alert | Expression sketch | Meaning |
|---|---|---|
| SQL gateway down | `up{job="distributed-db-sqlsrv"} == 0` | both replicas unreachable, or scrape broken |
| pgwire gateway down | `up{job="distributed-db-pgsrv"} == 0` | clients cannot connect |
| Pod restarts | `increase(kube_pod_container_status_restarts_total{namespace="<ns>"}[15m]) > 3` | crash loop or OOM |
| PVC filling | `kubelet_volume_stats_used_bytes / kubelet_volume_stats_capacity_bytes > 0.85` | ENOSPC risk |
| Node disk filling | `node_filesystem_avail_bytes / node_filesystem_size_bytes < 0.15` | affects PVCs and image pulls |
| SQL error ratio | `rate(sql_errors_total[5m]) / rate(sql_requests_total[5m]) > 0.01` | engine-level failures |
| SQL p99 | `histogram_quantile(0.99, sum(rate(sql_request_duration_seconds_bucket[5m])) by (le))` | latency regression |
| pgwire connections | `pgwire_inflight` and connection counters | saturation or leak |

Add a synthetic `kvctl status` check (for example a CronJob running the
command from §3.4 and exporting success as a metric) because neither meta nor
KV expose readiness beyond "TCP port open". A TCP probe passing does not mean
quorum exists.

### 7.4 Dashboards

At minimum: SQL/pgwire request rate, errors, p50/p95/p99, inflight; pod
restarts and phase; CPU/memory per component; PVC used/capacity and inode
usage; node disk latency/IOPS; network errors. Mark meta/KV leadership panels
as unavailable until the binaries expose metrics; do not invent Raft panels
from node-exporter data.

## 8. Failure modes

| Failure | Detection | Impact | First action |
|---|---|---|---|
| One meta pod down | pod not Ready, meta logs, `kvctl status` still has leader | none if 2/3 quorum; admin ops/TSO continue | replace pod; do not restart the other two |
| Meta quorum lost (2/3) | `kvctl status` fails/timeouts, SQL errors, meta logs | no TSO, no range-map updates; existing transactions may fail | freeze writers, recover meta pods; runbook |
| One KV pod down | pod not Ready, `kvctl ranges` leader missing on some ranges | affected ranges fail over if quorum; brief errors | wait for election, then replace pod |
| KV quorum lost in a range | `kvctl ranges` no leader, SQL timeouts | that range (and SQL touching it) unavailable | do not delete PVCs; recover nodes; restore if unrecoverable |
| Disk full / ENOSPC | PVC usage, `df -h /data`, write errors | writes fail, WAL/snapshot may stall | expand PVC or free safe space; do not delete Pebble/Raft files |
| Slow disk / fsync stall | p99, logs, node disk latency | Raft heartbeats/elections unstable | move to faster storage; do not increase timeouts blindly |
| Stuck split/merge | `kvctl ranges`, meta logs, version/leader anomalies | range map inconsistent; writes to affected keys fail | stop writers, preserve logs, escalate; no abort command |
| Stale node rejoin | repeated elections, old term, log mismatch | can disrupt quorum if it was removed or formatted incorrectly | keep it down, keep the PVC, verify membership; do not reuse the ID |
| Certificate expiry / wrong SAN | mTLS handshake errors in all components | cluster unavailable to clients/peers | restore a valid Secret and roll pods; runbook |
| Admin token mismatch | `401`/permission denied on split/merge, SQL HTTPS 401 | admin operations and SQL API only; data path unaffected | update Secret and roll meta/sqlsrv |
| pgsrv password mismatch | `password authentication failed` | new pgwire connections fail | update Secret and roll pgsrv |
| PVC Pending | pod Pending, events | replica never starts | fix storage class/capacity/AZ; do not relax spread blindly |
| Image pull failure | `ErrImagePull` | rollout stuck | fix image registry/pull secret; rollback |
| DNS/CNI failure | pods cannot resolve peers | Raft cannot form | fix CoreDNS/CNI; do not reformat data |
| Clock skew | lease/TSO anomalies, election churn | correctness/availability risk | fix NTP/chrony; restart affected pods after time is stable |
| OOM kill | restart count, `OOMKilled` | replica loss, possible quorum loss | raise memory or reduce cache; investigate range/snapshot size |

The step-by-step procedures are in [RUNBOOKS.md](RUNBOOKS.md).

## 9. Known gaps and assumptions

### 9.1 Blocking gaps (do not paper over)

1. **No production evidence**: no cluster install, 72 h soak, p99 run, Jepsen
   run or 1 TB test backs this chart.
2. **Image**: the multi-binary image builds, but the default tag is local;
   publish and override `image.repository`/`image.tag`.
3. **SQL auth is one static token**: sqlsrv has native TLS and a native Bearer
   check, but only one operator token for `/v1/sql`, `/metrics` and
   `/debug/slo`; `/health` and `/healthz` are public. The same token is the meta
   admin credential, so SQL access and range-map administration cannot be
   separated. There is no per-user identity, RBAC, authorization policy or
   audit. Anyone with the token has full SQL access. Kubernetes probes are TCP
   because kubelet cannot verify the private CA.
4. **Backup/restore is incomplete**: `kvbackup` is plaintext-only, has no admin
   token, restore is not atomic and does not delete extra keys, and there is no
   PITR/continuous archive. A TLS-enabled cluster cannot run the existing
   restore path without a new build; see §5.3.
5. **Membership**: joint consensus, config snapshot and decommission
   orchestration exist in code, but decommission is not wired to an RPC/CLI
   and the chart should still treat voter replacement as manual.
6. **No rolling-upgrade guarantee**: no protocol versioning or mixed-version
   test. Treat upgrades as maintenance windows with backups.
7. **No RBAC/audit/at-rest encryption**: mTLS and a shared admin token are not
   a substitute. Disk and backup encryption are the operator's responsibility.
8. **No KV/meta observability**: no metrics/health endpoints, TCP-only probes.
9. **Shared TLS identity**: one certificate/key for all pods; no per-pod
   identity or automatic rotation.
10. **Secret exposure in process arguments**: the meta admin token, sqlsrv
    bearer token and pgwire password are passed as flags by the startup scripts
    (binary limitation). Anyone who can inspect the container process can read
    them.
11. **No NetworkPolicy**: the chart does not ship one. Apply a default-deny
    policy and allow only the required ports (meta 9090, KV 7001, sqlsrv 8080,
    pgsrv 5432/9187, DNS egress) in the cluster.
12. **Reliance on stable DNS and time**: node IDs are DNS names; TSO/leases
    assume reasonably synchronized clocks. IP-only networking or clock drift
    is unsupported.
13. **Range-map bootstrap is opt-in**: enable `bootstrap.enabled` and provide
    KV pod addresses for a fresh install (§3.3.1). Restoring a multi-range
    backup still requires the target to host the same range boundaries;
    `-ensure-ranges` updates metadata but cannot start groups.

### 9.2 Assumptions this baseline makes

- Kubernetes 1.25+ with three failure domains (zones), a default StorageClass
  that supports `ReadWriteOnce` and online expansion, and working CoreDNS.
- The cluster domain matches `clusterDomain`; if not, override it and the
  certificate SANs together.
- All pods can reach each other on the component ports; no service mesh
  terminates or rewrites mTLS.
- The image contains `/bin/sh`, `kv2`, `sqlsrv`, `pgsrv`, `kvctl`, `kvbackup`,
  CA certificates, and runs as UID 10001; `/data` and `/tmp` are writable.
- The TLS Secret has a CA bundle and a leaf valid for all SANs with both EKUs.
- Operators keep Secrets out of Git, values files and shell history, and have a
  secure backup of the CA/keys and admin token.
- Clients use `sslmode=verify-full` (or equivalent) for pgwire and HTTPS for
  SQL; no plaintext fallback is acceptable outside an isolated network.

### 9.3 Validation performed for this baseline

- Helm 4.3.2 and Helm 3.16.4 `helm lint --strict`: 0 failures (one
  informational "icon is recommended").
- `helm template` with defaults, TLS disabled, 5 KV replicas, cert-manager +
  ingress, no PVCs, pgsrv auth disabled: all rendered and parsed as YAML.
- kubeconform 0.8.0 with Kubernetes 1.29 schemas (`-strict
  -ignore-missing-schemas`): 16/16 default resources valid, and 16/16 for the
  no-TLS, 5-KV, no-PVC and pgsrv-no-auth variants. The cert-manager/ingress
  variant has 18 resources: 17 valid and 1 skipped (the cert-manager
  `Certificate` CRD).
- Negative renders: even meta replica count, KV replica count outside 3–5,
  missing TLS Secret, missing admin token Secret, pgsrv auth without TLS all
  fail at render time with explicit errors.
- Startup scripts: `sh -n` clean, and executed with fake `/app/kv2`,
  `/app/pgsrv` and `/app/sqlsrv` binaries to check peer lists, TLS flags,
  admin-token flags, sqlsrv auth and pgsrv auth. The sqlsrv script emits
  `-tls-cert/-tls-key/-tls-ca` and `-auth-token` only when enabled.
- Negative renders now also include sqlsrv auth without TLS (with pgsrv auth
  disabled so the sqlsrv check is the one that fires).
- No `helm install`, no `kubectl apply`, no cluster deployment, no failover or
  restore test was run.

## 10. Related documents

- [RUNBOOKS.md](RUNBOOKS.md) — incident and maintenance procedures.
- [PRODUCT-READINESS.md](PRODUCT-READINESS.md) — current product verdict.
- [PRODUCTION-PLAN.md](PRODUCTION-PLAN.md) — gates G1–G7.
- [adr/0005-membership-safety.md](adr/0005-membership-safety.md) — why scaling
  and decommission are blocked.
- [adr/0006-pitr.md](adr/0006-pitr.md) — target PITR design (not implemented).
- [USER-GUIDE.md](USER-GUIDE.md) — CLI, SQL and pgwire examples.
