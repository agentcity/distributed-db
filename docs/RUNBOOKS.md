# Runbooks: distributed-db on Kubernetes

- Date: 2026-11-17
- Scope: the reference chart in [`deploy/helm/distributed-db`](../deploy/helm/distributed-db)
  and the v2 binaries.
- Companion: [OPERATIONS.md](OPERATIONS.md) — topology, capacity, deployment,
  backup/restore, TLS rotation, monitoring, known gaps.

> **These are rehearsable plans, not proven procedures.** No cluster deployment,
> failover, restore or upgrade was run while producing them. The database is
> experimental alpha; see [PRODUCT-READINESS.md](PRODUCT-READINESS.md). Rehearse
> every runbook in staging before relying on it.

## 0. Conventions and safety rails

Use a dedicated kubeconfig context and record it. All commands below assume:

```bash
NS=${NS:-distributed-db}
BASE=${BASE:-ddb-distributed-db}
DOMAIN=${DOMAIN:-cluster.local}
META="$BASE-meta.$NS.svc.$DOMAIN:9090"
KV_REPLICAS=${KV_REPLICAS:-3}
KV_PORT=${KV_PORT:-7001}
TLS_DIR=/etc/distributed-db/tls

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

# Runs kvctl from the sqlsrv pod, which has the TLS Secret mounted and the
# binary in the image. KVCTL_ADMIN_TOKEN is optional; admin calls need it.
kvctl_exec() {
  kubectl -n "$NS" exec deploy/"$BASE"-sqlsrv -c sqlsrv -- \
    env KVCTL_ADMIN_TOKEN="${KVCTL_ADMIN_TOKEN:-}" \
    /app/kvctl -meta "$META" -nodes "$NODES" \
      -tls-ca "$TLS_DIR/ca.crt" -tls-cert "$TLS_DIR/tls.crt" -tls-key "$TLS_DIR/tls.key" \
      "$@"
}

# For the rare admin call. Do not put the token in shell history or CI logs.
admin_token() {
  kubectl -n "$NS" get secret distributed-db-admin-token \
    -o jsonpath='{.data.token}' | base64 -d
}
```

If the image lacks `/bin/sh`, the chart's startup scripts already do not work;
fix the image before using these commands.

**Capture evidence before changing anything:**

```bash
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "incident-$STAMP"
kubectl -n "$NS" get pods -o wide > "incident-$STAMP/pods.txt"
kubectl -n "$NS" get events --sort-by=.metadata.creationTimestamp > "incident-$STAMP/events.txt"
kubectl -n "$NS" get pvc > "incident-$STAMP/pvc.txt"
kvctl_exec -format json status > "incident-$STAMP/status.json" 2>&1 || true
kvctl_exec -format json ranges > "incident-$STAMP/ranges.json" 2>&1 || true
```

**Never do these without an explicit, reviewed decision:**

- Delete a PVC, a StatefulSet, or a data directory to "reset" a node.
- Start a replacement pod with an empty volume under an existing node ID.
- Remove a Raft voter or edit membership by hand.
- Run `SetRanges` against an existing cluster; it replaces the whole map.
- Run a restore into a serving cluster.
- Disable TLS or the admin token on a serving production cluster.
- Delete `/data/db`, `/data/raft`, `ranges.json`, Pebble `MANIFEST`/`CURRENT`
  files, or WAL files to free space.

Escalate to the database owner when quorum is lost, ranges have no leader with a
healthy quorum, split/merge state is ambiguous, membership is uncertain, or the
restore target layout does not match the backup.

---

## 1. KV leader / range failover

**Trigger:** SQL or pgwire errors/timeouts; `kvctl ranges` shows a range with no
leader; one KV pod is NotReady, Pending, CrashLooping, or its node is gone.

**Impact:** With one of three replicas down, the Raft group keeps quorum and
should elect a new leader within an election timeout. In-flight requests fail;
clients retry. With two of three down, the range is unavailable — use §2.

### Steps

1. Confirm the blast radius and that only one replica is affected:

   ```bash
   kubectl -n "$NS" get pods -l app.kubernetes.io/component=kv -o wide
   kubectl -n "$NS" describe pod "$BASE-kv-<ordinal>"
   kubectl -n "$NS" logs "$BASE-kv-<ordinal>" -c kv --tail=200
   kvctl_exec status
   kvctl_exec ranges
   ```

   Do not restart the other two KV pods while one is already down.

2. If the pod is Pending, read the scheduler event. Common causes: zone spread
   `DoNotSchedule` on a two-zone cluster, insufficient CPU/memory, or a PVC
   bound in a different AZ. Fix capacity/placement; do not delete the PVC to
   force scheduling.

3. If the pod is CrashLooping, fix the cause before deleting it repeatedly:
   expired/missing certificate, full disk, corrupted data directory, OOM,
   wrong Secret reference. `kubectl logs --previous` is usually the fastest
   signal.

4. If the pod is Running but NotReady (TCP probe fails), check whether the
   process is listening and whether the data directory is readable:

   ```bash
   kubectl -n "$NS" exec "$BASE-kv-<ordinal>" -c kv -- \
     sh -c 'ls -la /data; df -h /data; df -i /data'
   ```

5. Wait at least 2–3 election timeouts (the code defaults to 300–600 ms for KV
   Raft; give it 30–60 s including gRPC reconnect and lease expiry). Re-check
   `ranges`. A new leader should appear without operator action.

6. If the pod is stuck (Running but the process is unresponsive; kubelet cannot
   restart it), delete **one** pod gracefully and wait:

   ```bash
   kubectl -n "$NS" delete pod "$BASE-kv-<ordinal>" --grace-period=30 --wait=true
   kubectl -n "$NS" rollout status statefulset/"$BASE"-kv --timeout=10m
   ```

   The StatefulSet recreates the same ordinal with the same PVC; Raft catches up
   from the log/snapshot. Do not delete the PVC.

7. Verify the range map and a data path:

   ```bash
   kvctl_exec -format json ranges
   kvctl_exec put smoke:failover "$(date -u +%FT%TZ)"
   kvctl_exec get smoke:failover
   ```

   Every range should have a leader and a non-empty replica list. On a fresh
   unbootstrapped install range 1 may have an empty replica list; see
   [OPERATIONS.md §3.3.1](OPERATIONS.md#331-range-map-bootstrap-required-before-splitmerge).

### Abort / escalate

- A second KV replica becomes unhealthy before the first recovers.
- No leader after several minutes with a healthy quorum.
- The data directory reports I/O errors, checksum failures, or ENOSPC.
- The affected pod's PVC is missing or bound to an unavailable zone.

Do not "fix" this by deleting the remaining replicas' PVCs or by starting empty
pods.

---

## 2. Quorum loss

**Trigger:** Two of three (or three of five) replicas of a KV range or the meta
Raft group are down; SQL requests fail; `kvctl status` has no meta leader;
`kvctl ranges` has ranges with no leader.

**Impact:** The affected Raft group cannot commit. Reads may fail; writes fail.
Meta quorum loss also stops TSO allocation and range-map changes, which affects
SQL even if KV is healthy. There is no automatic quorum recovery.

### 2.1 Freeze writers first

Stop client traffic so no partial work continues and so you can reason about
the last committed state. Prefer fencing at the ingress/Service layer. Keeping
the sqlsrv pod alive preserves `kvctl_exec`; scaling it to zero removes your
tooling.

```bash
# Save the selectors so they can be restored exactly.
kubectl -n "$NS" get svc "$BASE"-sqlsrv -o jsonpath='{.spec.selector}' > /tmp/sql-selector.json
kubectl -n "$NS" get svc "$BASE"-pgsrv  -o jsonpath='{.spec.selector}' > /tmp/pg-selector.json

# Fence SQL HTTPS and pgwire without killing the pods.
kubectl -n "$NS" patch svc "$BASE"-sqlsrv --type=json \
  -p='[{"op":"replace","path":"/spec/selector","value":{"app.kubernetes.io/component":"fenced"}}]'
kubectl -n "$NS" patch svc "$BASE"-pgsrv --type=json \
  -p='[{"op":"replace","path":"/spec/selector","value":{"app.kubernetes.io/component":"fenced"}}]'
```

This leaves the pods running for `kubectl exec`, but no client can reach them
through the Services. If you do not have a tools pod, do not scale sqlsrv to
zero. Do not scale meta/KV down.

### 2.2 Classify and recover

1. Determine whether the lost group is meta or KV:

   ```bash
   kubectl -n "$NS" get pods -l app.kubernetes.io/component=meta -o wide
   kubectl -n "$NS" get pods -l app.kubernetes.io/component=kv -o wide
   kvctl_exec status           # may fail if meta has no leader
   kvctl_exec -format json ranges || true
   ```

2. For each failed pod, distinguish infrastructure failure from data loss:
   node down, PVC detached/unavailable, image pull failure, crash loop. Fix the
   infrastructure and bring the **original** pod/PVC back. A returning voter
   with its original data is the only supported recovery path.

3. If a pod's PVC is intact and the pod is crash-looping, capture logs and fix
   the cause (cert, token, disk). Do not wipe the volume.

4. If a node/AZ is permanently lost, check whether the PV can be reattached in
   another zone (most RWO block volumes are zone-bound). If the volume cannot be
   recovered, that voter's committed data is gone; the group may still have
   quorum through the other voters. Do not create an empty replacement at the
   same node ID.

5. If quorum is permanently unrecoverable (two voters' data gone), the cluster
   cannot be repaired by restarting pods. Treat the surviving node as
   read-only evidence, keep all volumes, and restore the last verified backup
   into a new cluster (§7). RPO is the backup interval; there is no PITR.

### 2.3 Meta quorum loss

If meta loses quorum:

- Stop SQL/pgwire writers as above; no new TSO can be allocated and the range
  map cannot change.
- Recover the original meta pods/PVCs. Do not bootstrap a new meta group over
  the old volumes and do not delete `/data/meta/raft`.
- If the old meta data is unrecoverable, restore into a new cluster. A new
  empty meta group with old KV data is not a valid recovery: the range map/TSO
  high-water mark are part of the control plane state.

### 2.4 Verify

```bash
kubectl -n "$NS" get pods -o wide
kvctl_exec status
kvctl_exec -format json ranges
# Restore the Service selectors only after the data path is healthy.
kubectl -n "$NS" patch svc "$BASE"-sqlsrv --type=json \
  -p="[{\"op\":\"replace\",\"path\":\"/spec/selector\",\"value\":$(cat /tmp/sql-selector.json)}]"
kubectl -n "$NS" patch svc "$BASE"-pgsrv --type=json \
  -p="[{\"op\":\"replace\",\"path\":\"/spec/selector\",\"value\":$(cat /tmp/pg-selector.json)}]"
```

Run a read and write smoke test. Check that the meta term advanced and there is
exactly one leader.

### Abort / escalate

Escalate immediately if you are unsure whether a node was removed from
membership, if two voters have unrecoverable data, or if the surviving node's
log/term is unclear. Do not attempt manual membership changes; there is no
supported decommission/joint-consensus tool in this build.

---

## 3. Meta / TSO degraded

**Trigger:** SQL errors such as "meta", "TSO", "not leader", deadline exceeded;
`kvctl status` fails or shows no leader; one or two meta pods are down. There are
no meta metrics, so logs, `kvctl`, and pod state are the signals.

**Impact:**

- 1 of 3 meta pods down: quorum remains; TSO and range-map updates continue.
  Clients may see a brief error while the leader is rediscovered.
- 2 of 3 down: no leader/TSO; SQL and any admin operation fail. Treat as §2.3.

### Steps

1. Capture state and check pods/logs:

   ```bash
   kubectl -n "$NS" get pods -l app.kubernetes.io/component=meta -o wide
   kubectl -n "$NS" logs -l app.kubernetes.io/component=meta -c meta --tail=200 --prefix
   kvctl_exec status
   ```

2. If only one pod is down, fix its cause and wait for it to rejoin. Do not
   restart the healthy leader.

3. If the leader is gone but two pods remain, wait for an election (meta
   election timeout defaults to 150–300 ms; allow tens of seconds for
   reconnect). Check `kvctl status` for a new leader.

4. If SQL reports TSO errors while two meta pods are healthy, verify the leader
   is reachable from the SQL pods (DNS, NetworkPolicy, certificate SANs). The
   meta service is a bootstrap seed; clients dial the leader's stable pod DNS
   name, so a bad wildcard SAN or a DNS policy can break TSO even though the
   meta pods are Ready.

5. If the meta process is up but the listener is unresponsive, delete one pod
   and wait; do not delete all three.

6. Verify TSO by running a trivial write through SQL and checking `kvctl
   status` term/leader; then a `kvctl_exec ranges`.

### Abort / escalate

- Meta leader flaps repeatedly: suspect clock skew, disk fsync stalls, or
  network loss. Stop and investigate; do not raise election timeouts as a
  "fix" without understanding the cause.
- Two meta voters lost: go to §2.3/§7.
- Certificate errors on every meta dial: go to §9 (TLS rotation).

---

## 4. Disk full / ENOSPC

**Trigger:** Writes fail; logs contain `ENOSPC`, `no space left on device`, or
Pebble/Raft errors; PVC usage alert; pod CrashLoop after a full disk.

**Impact:** A full KV data volume can stop Raft appends and state-machine
writes; a full meta volume can stop the control plane. Recovery is possible if
you free/expand space before data is corrupted. Never delete database files.

### Steps

1. Identify the volume and confirm both bytes and inodes:

   ```bash
   kubectl -n "$NS" get pvc
   kubectl -n "$NS" exec "$BASE-kv-<ordinal>" -c kv -- \
     sh -c 'df -h /data; df -i /data; du -x -d1 /data 2>/dev/null | sort -h | tail -20'
   kubectl -n "$NS" describe pvc "data-$BASE-kv-<ordinal>"
   ```

2. Stop writers to the affected range/cluster if the database is still trying
   to append. Fence the SQL/pgwire Services (see §2.1) rather than deleting the
   pods, so the pod remains available for `df`/`kvctl`. Do not let it run at
   100% disk.

3. Expand the PVC if the StorageClass allows it:

   ```bash
   kubectl -n "$NS" patch pvc "data-$BASE-kv-<ordinal>" \
     -p '{"spec":{"resources":{"requests":{"storage":"150Gi"}}}}'
   kubectl -n "$NS" get pvc "data-$BASE-kv-<ordinal>" -w
   ```

   Check `allowVolumeExpansion` and the filesystem resize status. A pod restart
   may be required to pick up the larger filesystem; restart one pod, not all.

4. If expansion is not possible, free only **safe** space:

   - backup output stored on the same volume (`/tmp`, `/backup`) after copying
     it off-cluster;
   - core dumps or stray operator files, after confirming they are not database
     files;
   - never `/data/db`, `/data/raft`, `ranges.json`, Pebble `MANIFEST`/`CURRENT`,
     WAL, or snapshot files.

5. If the process will not start because the disk is full, expand first, then
   restart. Do not format the volume or delete the data directory.

6. After space is available, restart the affected pod and verify:

   ```bash
   kubectl -n "$NS" rollout status statefulset/"$BASE"-kv --timeout=10m
   kvctl_exec status
   kvctl_exec ranges
   kvctl_exec put smoke:disk "$(date -u +%FT%TZ)"
   kvctl_exec get smoke:disk
   ```

7. If checksums, Pebble open errors, or Raft log corruption appear after
   ENOSPC, stop and restore from backup; do not attempt file-level surgery.

### Abort / escalate

- The volume cannot be expanded and no safe space remains.
- More than one replica of the same range is full.
- The meta volume is full (no TSO/control plane).
- Filesystem or checksum errors after freeing space.

---

## 5. Stuck split / merge

**Trigger:** `kvctl split` or `kvctl merge` times out or returns an error;
`kvctl ranges` shows overlapping boundaries, a gap, a missing leader for the new
range, or repeated version changes; meta logs show split/merge retries.

**Impact:** The range map may not match the data placement. Writes to affected
keys can fail or route to the wrong range. This is a correctness-sensitive
state; do not guess.

### Steps

1. Freeze writers (gateways to zero or ingress block) and take a fresh backup
   if the cluster is readable. Preserve logs and the range map.

   ```bash
   STAMP=$(date -u +%Y%m%dT%H%M%SZ)
   kvctl_exec -format json ranges > "split-$STAMP.json" 2>&1 || true
   kubectl -n "$NS" logs -l app.kubernetes.io/component=meta -c meta --tail=500 > "meta-$STAMP.log"
   kubectl -n "$NS" logs -l app.kubernetes.io/component=kv -c kv --tail=500 > "kv-$STAMP.log"
   ```

2. Check the prerequisites before retrying anything:

   ```bash
   kvctl_exec status            # meta leader/term
   kubectl -n "$NS" get pods -l app.kubernetes.io/component=kv -o wide
   kubectl -n "$NS" get pods -l app.kubernetes.io/component=meta -o wide
   ```

   Split/merge requires a meta leader and reachable replicas. If quorum is
   missing, fix that first (§2); do not retry the split.

3. Check whether the operation actually committed even though the client saw a
   timeout. Compare the range map to the pre-change capture. A range with a new
   ID and a consistent boundary may mean the operation succeeded and only the
   response was lost. Do not re-run blindly.

4. If the map is unchanged and all replicas are healthy, the operation can be
   retried **once** with the admin token:

   ```bash
   export KVCTL_ADMIN_TOKEN="$(admin_token)"
   kvctl_exec split "<range-id>" "<split-key>"
   # or: kvctl_exec merge "<left-id>" "<right-id>"
   unset KVCTL_ADMIN_TOKEN
   ```

   Before retrying, confirm the split key is inside the range and not equal to
   an existing boundary. For merge, confirm the ranges are adjacent and have
   the same replica set.

5. If the map changes but the new range never gets a leader, check the KV pods
   hosting it, their logs, and disk. A split creates a new Raft group; the
   target replicas must be up and able to persist. Do not delete the old range.

6. If the map has an overlap/gap or the data/range map cannot be reconciled,
   stop. There is no supported abort/repair CLI. Restore the last verified
   backup into a new cluster (§7) and escalate with the captured artifacts.

### Abort / escalate

- Any overlap/gap in `ranges`.
- A split/merge committed but the new group has no leader after recovery.
- Different replicas disagree about range boundaries/versions.
- The operation involved a range with an empty replica list (fresh install
  without the bootstrap in OPERATIONS §3.3.1).

---

## 6. Stale node rejoin / decommission

**Trigger:** A KV or meta pod was down for a long time, scaled down, replaced, or
its node was rebuilt; it returns with old data. Logs show elections, term
mismatches, "not hosted", or snapshot catch-up loops.

**Impact:** A stale voter that is still in the Raft configuration can disrupt
elections if its log is behind or if it was supposed to be removed. This build
has joint consensus, snapshot-resident membership and decommission
orchestration in code, but decommission is not wired to an RPC/CLI. There is no
`kvctl remove` command.

### 6.1 Safe rejoin (same member, same volume)

Use this only if **all** are true:

- the pod ordinal was never removed from membership;
- it uses the **same PVC** and data directory;
- no replacement node was introduced at that identity;
- the rest of the group is healthy and has quorum.

Steps:

1. Keep the gateways scaled down or traffic fenced if the group is degraded.
2. Start/recreate only that pod:

   ```bash
   kubectl -n "$NS" delete pod "$BASE-kv-<ordinal>" --grace-period=30 --wait=true
   kubectl -n "$NS" rollout status statefulset/"$BASE"-kv --timeout=15m
   kubectl -n "$NS" logs "$BASE-kv-<ordinal>" -c kv -f
   ```

3. Watch for snapshot catch-up and a non-empty leader/term. Re-check
   `kvctl ranges`; the returning node should not claim a higher term or a
   conflicting leadership.

4. Verify a read/write probe and that every range has a leader.

### 6.2 Stale or removed node

If the node was decommissioned, removed, or its identity was reused, **do not
start it** against the cluster. Keep the PVC and pod scaled down as evidence.
Starting a removed voter can win an election with a stale configuration and
acknowledge conflicting writes (the F4 risk in ADR 0005).

There is no supported decommission procedure in this build. Options:

- Keep the node down and operate with the remaining quorum (if the range's
  replica set is still ≥3 and healthy).
- For a permanent replacement, build a new cluster from the last verified
  backup (§7), not by joining an empty node.
- Escalate to engineering for an explicit membership change; do not hand-edit
  Raft membership files.

### 6.3 Decommission (not supported)

Do not:

- delete the PVC of a live or removed voter to "clean up";
- reuse its node ID for a new empty pod;
- manually remove entries from `/data/raft`;
- assume `kubectl scale statefulset --replicas=N` removes a voter. It only
  changes the number of pods; the peer list is rendered from the chart value
  and a rollout can change every pod's arguments.

Record the PVC, node identity, and evidence. Plan a backup/restore migration if
capacity must be reduced.

---

## 7. Restore from backup

**Trigger:** Data loss/corruption, unrecoverable quorum, a failed upgrade, or a
scheduled restore drill.

**Target:** restore into a **new, isolated** cluster. Never restore into a
serving cluster.

### 7.1 Preconditions and hard limits

- Backup is a logical, single-TSO backup. Restore is not atomic and does not
  delete keys absent from the backup.
- The current restore CLI (`kvbackup restore`) has **no TLS and no admin-token
  flags**. It cannot talk to a TLS-enabled chart or a target with
  `meta.adminToken.enabled=true`. It also has no `RequireEmpty` flag.
- `-ensure-ranges` only replaces the meta range map; it cannot create range
  groups. The target KV nodes must already host ranges with the same boundaries.
  A fresh single-range target cannot host a multi-range backup. If the backup
  has multiple ranges and you cannot prepare the target layout, stop; restoring
  will fail or write to the wrong ranges.
- RPO is the backup interval. There is no PITR. RTO is unproven; measure it in
  the drill.

If these limits block the restore, the correct action is to escalate and build a
TLS/token-capable restore path; do not disable TLS on the serving production
cluster.

### 7.2 Prepare the backup

```bash
# Offline verification. This does not need cluster TLS.
kubectl -n "$NS" exec deploy/"$BASE"-pgsrv -c pgsrv -- \
  /app/kvbackup verify -in "/backup/<snapshot-dir>"
```

Record the manifest's snapshot TSO, range count, key count, byte count and
checksums. Copy the backup to an isolated location before touching the target.

### 7.3 Build the isolated target

Use a separate namespace and a plaintext, token-free target **only on an
isolated network** for the restore path that exists today. Example overrides:

```yaml
# restore-values.yaml — isolated target only, never exposed to clients
tls:
  enabled: false
  mtls: false
meta:
  adminToken:
    enabled: false
sqlsrv:
  auth:
    enabled: false
pgsrv:
  auth:
    enabled: false
```

```bash
RESTORE_NS=ddb-restore
helm upgrade --install ddb-restore deploy/helm/distributed-db \
  --namespace "$RESTORE_NS" --create-namespace \
  --values restore-values.yaml --atomic --timeout 20m
```

Do not attach this target to a public network, an ingress, or a shared
namespace. Disable all clients.

### 7.4 Restore

1. Confirm the target is empty and inspect its range layout:

   ```bash
   RESTORE_BASE=ddb-restore-distributed-db
   RESTORE_META="$RESTORE_BASE-meta.$RESTORE_NS.svc.$DOMAIN:9090"
   RESTORE_NODES=""
   i=0
   while [ "$i" -lt "$KV_REPLICAS" ]; do
     RESTORE_NODES="${RESTORE_NODES}${RESTORE_NODES:+,}$RESTORE_BASE-kv-$i.$RESTORE_BASE-kv-headless.$RESTORE_NS.svc.$DOMAIN:$KV_PORT"
     i=$((i+1))
   done
   kubectl -n "$RESTORE_NS" exec deploy/"$RESTORE_BASE"-sqlsrv -c sqlsrv -- \
     /app/kvctl -meta "$RESTORE_META" -nodes "$RESTORE_NODES" ranges
   ```

2. Compare the backup manifest's ranges with the target. If they differ:
   - stop unless the target's KV nodes already host the backup's boundaries;
   - `-ensure-ranges -replicas "$RESTORE_NODES"` updates the map but does not
     start groups. A fresh target with one range cannot take a multi-range
     backup correctly.

3. Run the restore. Start without `-strict-epoch` on a new target (versions will
   differ); use `-check` for a drill or high-value restore:

   ```bash
   kubectl -n "$RESTORE_NS" exec ddb-restore-tools -- \
     /app/kvbackup restore \
       -meta "$RESTORE_META" \
       -nodes "$RESTORE_NODES" \
       -in "/backup/<snapshot-dir>" \
       -ensure-ranges -replicas "$RESTORE_NODES" \
       -check -timeout 2h
   ```

   The tools pod must have the backup volume and `/app/kvbackup`; the chart
   image includes the binary, but the chart does not ship a tools pod or PVC.

4. Verify counts and a sample of keys:

   ```bash
   kubectl -n "$RESTORE_NS" exec deploy/"$RESTORE_BASE"-sqlsrv -c sqlsrv -- \
     /app/kvctl -meta "$RESTORE_META" -nodes "$RESTORE_NODES" ranges
   kubectl -n "$RESTORE_NS" exec deploy/"$RESTORE_BASE"-sqlsrv -c sqlsrv -- \
     /app/kvctl -meta "$RESTORE_META" -nodes "$RESTORE_NODES" get "<known-key>"
   ```

5. If the restore reports a range mismatch after `-ensure-ranges`, stop. Do not
   write more and do not delete the target's data. Capture manifest, range map
   and logs.

### 7.5 Cutover

1. Keep the old cluster fenced (no writers, ingress disabled) and retain its
   volumes and backups until the rollback window closes.
2. Re-enable TLS/admin auth on the restored cluster **before** exposing it if it
   was disabled for the restore. This means installing/updating Secrets and
   rolling pods; verify mTLS and the admin token.
3. Point clients at the new service/ingress. Update DNS/TLS names if they
   differ; certificates must match the new service names.
4. Rotate credentials that may have been exposed during the restore.
5. Monitor errors/latency and run the smoke checks. Record RPO (backup TSO gap)
   and RTO (start to serving) in the incident/change record.

### Abort / escalate

- Backup verify fails.
- Target is not empty (restore does not delete extra keys).
- Range layouts differ and no prepared target exists.
- Restore reports a mismatch or a partial failure.
- You cannot enable TLS/admin auth before cutover.

---

## 8. Rolling upgrade and rollback

**Trigger:** Applying a new image or chart release. There is no tested N-1
compatibility or protocol negotiation; use a maintenance window.

### 8.1 Preflight

1. Read the release notes and any on-disk/protocol change. If the change is
   major or unknown, do a full-cluster stop/restore plan instead of rolling.
2. Take and verify a backup; copy it off-cluster.
3. Confirm `kvctl status` has a leader, all ranges have leaders, no split/merge
   is stuck, and every pod is Ready.
4. Confirm ≥25% free space on every PVC and that the new image pulls on every
   node.
5. Rehearse in staging with a copy of production data.

### 8.2 Apply

```bash
helm lint --strict deploy/helm/distributed-db --values prod-values.yaml

helm upgrade --install ddb deploy/helm/distributed-db \
  --namespace "$NS" --values prod-values.yaml \
  --atomic --wait --timeout 20m
```

StatefulSets roll one pod at a time in reverse ordinal order; sqlsrv/pgsrv use
`maxUnavailable: 0`, `maxSurge: 1`. Watch:

```bash
kubectl -n "$NS" get pods -w
kubectl -n "$NS" logs -l app.kubernetes.io/component=meta -c meta --tail=100 --prefix
kubectl -n "$NS" logs -l app.kubernetes.io/component=kv -c kv --tail=100 --prefix
kvctl_exec status
```

After each meta/KV pod becomes Ready, verify quorum and range leaders before
continuing. If a pod fails to become Ready, stop the rollout and diagnose; do
not delete several pods at once.

### 8.3 Rollback

```bash
helm history ddb -n "$NS"
helm rollback ddb "<REVISION>" -n "$NS" --wait --timeout 20m
kubectl -n "$NS" rollout status statefulset/"$BASE"-meta --timeout=10m
kubectl -n "$NS" rollout status statefulset/"$BASE"-kv   --timeout=10m
kvctl_exec status
```

Rollback caveats:

- Helm restores manifests, not data. PVC contents, Raft logs, Pebble files,
  certificates and Secrets are not rolled back.
- If the new binary wrote an incompatible format, the old binary may not read
  the data. Stop and restore from backup; do not downgrade by copying binaries
  into a cluster that already wrote with the new format.
- `--atomic` rolls back the release if the upgrade fails, but it may leave a
  partially rolled StatefulSet. Wait for all pods and verify quorum before
  declaring success.

### 8.4 If a rollout is stuck

```bash
kubectl -n "$NS" describe pod "$BASE-kv-<ordinal>"
kubectl -n "$NS" logs "$BASE-kv-<ordinal>" -c kv --tail=200
kubectl -n "$NS" get events --sort-by=.metadata.creationTimestamp | tail -30
```

Delete one stuck pod to let the StatefulSet retry; do not delete the PVC or the
whole StatefulSet. If the image cannot pull, roll back the release. If the new
binary crashes on the data, restore.

---

## 9. Key rotation

This runbook covers four independent credentials:

1. cluster TLS leaf certificate (same CA);
2. cluster TLS CA (mTLS trust root);
3. meta/SQL admin token;
4. pgwire password;
5. optional ingress TLS.

The chart uses **one shared cluster certificate/key for every pod**. There is no
per-pod identity to rotate separately.

### 9.1 Leaf renewal (same CA)

1. Issue a new leaf with the existing CA and the exact SANs/EKUs
   (`serverAuth,clientAuth`) described in OPERATIONS §3.2.
2. Update the Secret atomically (all three keys together; do not leave a
   mismatched cert/key):

   ```bash
   kubectl -n "$NS" create secret generic distributed-db-tls \
     --from-file=ca.crt="$TLS_DIR/ca.crt" \
     --from-file=tls.crt="$TLS_DIR/tls.crt" \
     --from-file=tls.key="$TLS_DIR/tls.key" \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

3. Roll sqlsrv/pgsrv (clients), then KV, then meta, one workload at a time:

   ```bash
   kubectl -n "$NS" rollout restart deployment/"$BASE"-sqlsrv
   kubectl -n "$NS" rollout status deployment/"$BASE"-sqlsrv --timeout=5m
   kubectl -n "$NS" rollout restart deployment/"$BASE"-pgsrv
   kubectl -n "$NS" rollout status deployment/"$BASE"-pgsrv --timeout=5m
   kubectl -n "$NS" rollout restart statefulset/"$BASE"-kv
   kubectl -n "$NS" rollout status statefulset/"$BASE"-kv --timeout=15m
   kubectl -n "$NS" rollout restart statefulset/"$BASE"-meta
   kubectl -n "$NS" rollout status statefulset/"$BASE"-meta --timeout=15m
   ```

4. Verify mTLS and clients:

   ```bash
   kvctl_exec status
   kvctl_exec get "<key>"
   psql "host=$BASE-pgsrv.$NS.svc.$DOMAIN port=5432 user=dbadmin dbname=postgres sslmode=verify-full sslrootcert=ca.crt"
   ```

### 9.2 CA rotation (two-phase overlap)

An atomic CA+leaf switch breaks mTLS: unrolled pods trust only the old CA and
will reject new pods. Use overlap.

1. Create the new CA and a new leaf signed by it.
2. **Phase 1:** Secret = old leaf/key + `ca.crt` bundle containing old and new
   CA. Roll every workload (as in 9.1). Verify.
3. **Phase 2:** Secret = new leaf/key + the same combined `ca.crt`. Roll every
   workload. Old and new pods both trust both CAs during the roll. Verify.
4. **Phase 3:** Secret = new leaf/key + `ca.crt` containing only the new CA.
   Roll every workload. Verify.
5. Keep the old CA/leaf in a secure archive until the rollback window closes.
6. If a phase fails, restore the previous Secret and roll back; do not continue
   with a half-rotated trust root.

### 9.3 Admin token rotation

Meta reads the token at startup (`-admin-token`); sqlsrv reads it at startup
(`-auth-token`). There is no dual-token overlap.

1. Announce a short window for admin operations. Data-path reads/writes are not
   gated by this token, but the SQL HTTPS API is.
2. Update the Secret:

   ```bash
   umask 077
   openssl rand -base64 48 | tr -d '/+=' | cut -c1-48 > /tmp/ddb-admin.token
   kubectl -n "$NS" create secret generic distributed-db-admin-token \
     --from-file=token=/tmp/ddb-admin.token \
     --dry-run=client -o yaml | kubectl apply -f -
   rm -f /tmp/ddb-admin.token
   ```

3. Roll meta, then sqlsrv:

   ```bash
   kubectl -n "$NS" rollout restart statefulset/"$BASE"-meta
   kubectl -n "$NS" rollout status statefulset/"$BASE"-meta --timeout=15m
   kubectl -n "$NS" rollout restart deployment/"$BASE"-sqlsrv
   kubectl -n "$NS" rollout status deployment/"$BASE"-sqlsrv --timeout=5m
   ```

4. Update operator scripts/clients (`KVCTL_ADMIN_TOKEN`). During the meta roll,
   old and new pods will accept different tokens; expect transient 401s on
   admin calls. Do not run split/merge during the roll.
5. Verify: SQL without token returns 401, with new token succeeds; an admin
   operation with the new token succeeds.

### 9.4 pgwire password rotation

1. Generate a new password and update `distributed-db-pgsrv-auth` with the
   existing username and the new password (`username`, `password` keys).
2. Roll pgsrv. The restart terminates existing sessions; new connections must
   use the new password.
3. Verify with `sslmode=verify-full`.
4. Keep the old password only for the rollback window; there is no dual-password
   mode.

### 9.5 Ingress TLS

Rotate `ingress.tls.secretName` according to the ingress controller. This is
independent of cluster mTLS. Verify HTTPS with the ingress host; the backend is
HTTPS and still requires the bearer token on `/v1/sql`. Configure the ingress
controller for an HTTPS backend (nginx: `backend-protocol: HTTPS`).

### Abort / escalate

- mTLS handshake failures across components after a phase.
- Certificate SAN does not match the service/pod DNS being dialed.
- The CA bundle update did not propagate to running pods (they read at startup;
  roll them).
- The old admin token was leaked: rotate immediately and review access logs;
  there is no audit log in the database itself.

---

## 10. Incident closeout

For every runbook execution, record:

- start/end time, operator, namespace, release revision, image tag;
- observed symptom and affected component/range IDs;
- commands run and their output captures;
- whether quorum, range map and data probes were verified;
- data-loss window (backup interval), RTO, and any restored snapshot TSO;
- follow-up: missing metrics, alert, test or tooling that would have shortened
  detection or recovery.

File follow-ups against the production gates in
[PRODUCTION-PLAN.md](PRODUCTION-PLAN.md), not just against this chart.
