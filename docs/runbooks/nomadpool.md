# NomadPool runbook

`NomadPool` is a **managed lifecycle** object — you author the CR, the operator
`Register`s the declared pool onto Nomad and `Delete`s it when the CR is
removed. It mirrors the `NomadCluster` create/update/delete model, not the
reflector model `NomadNode` uses.

## Create a pool
    kubectl apply -f - <<EOF
    apiVersion: nomad.operator.io/v1alpha1
    kind: NomadPool
    metadata:
      name: gpu-workers
      namespace: nomad-system
    spec:
      clusterRef:
        name: prod
      poolName: gpu-workers   # exact Nomad pool name; may differ from metadata.name
      description: "GPU worker nodes"
      meta:
        team: ml
        tier: gpu
    EOF

- **`spec.poolName`** is the exact Nomad node-pool name and is kept separate
  from `metadata.name` because Nomad pool names allow characters (underscores,
  uppercase) that a Kubernetes object name does not. It is immutable, as is
  `spec.clusterRef.name` — a pool belongs to one cluster for its lifetime.
- **`spec.description`** and **`spec.meta`** are optional and map straight to
  `NodePool.Description`/`NodePool.Meta` (Nomad Community Edition).
- **`spec.meta` is fully managed.** The map you declare owns the pool's `Meta`
  entirely — any key set out-of-band (CLI, another tool) is overwritten on the
  next reconcile. There is no merge.

## Why `default` and `all` are rejected
`default` and `all` are Nomad's **built-in** pools — every client that
doesn't declare a pool lands in `default`, and `all` is a virtual pool
matching every client. Neither can be created, modified, or deleted, so
neither is representable as a managed CR: `spec.poolName` is CEL-validated to
reject both at admission, before the operator ever calls Nomad.

## Enterprise `scheduler_config` is preserved, not overwritten
Nomad Enterprise supports a per-pool `scheduler_config` override
(`SchedulerConfiguration`) and a `node_identity_ttl`, neither of which this
CRD models (out of scope — no present consumer on Community Edition). Rather
than blindly overwriting the pool on every `Register` — which would silently
wipe an Enterprise scheduler override the first time the operator adopts an
existing pool — the reconciler does a **read-modify-write**: it fetches the
current pool, overlays only the managed fields (`Description`, `Meta`) from
`spec`, and preserves whatever `SchedulerConfiguration`/`NodeIdentityTTL` is
already set. `Register` is only called when a managed field actually differs
from what's live, so a steady-state resync does not churn Raft.

## Deletion semantics — the finalizer blocks until the pool is empty
`kubectl delete nomadpool <name>` does **not** delete instantly. The operator
holds a finalizer and calls Nomad's `Delete` on the pool; Nomad itself refuses
to delete a pool that still has **nodes** or **non-terminal jobs** in it. If
that happens:

- The CR stays `Terminating`.
- A `DeleteBlocked=True, reason=PoolNotEmpty` condition appears, with
  `status.nodeCount` / `status.jobCount` populated so you can see *why* it's
  stuck without going to the Nomad CLI.

To unblock it: drain/reassign the nodes in the pool (see the `NomadNode`
runbook for cordon/drain) and stop or move any jobs still targeting the pool,
then let the operator's requeue retry the delete — no need to re-run
`kubectl delete`.

If the pool's `NomadCluster` is unreachable or not `Ready` when you delete the
CR, the finalizer also stays and `DeleteBlocked=True, reason=ClusterNotReady`
is set until the cluster recovers.

## Duplicate `poolName` — `PoolNameConflict`
Two `NomadPool` CRs that declare the same `poolName` on the same
`clusterRef` would otherwise fight over one Nomad pool, each re-`Register`ing
it every resync. Instead, the reconciler detects the collision (a namespaced
`List` filtered for the same cluster+poolName), sets
`Ready=False, reason=PoolNameConflict` on **every** colliding CR, emits a
Warning event, and skips the `Register` call entirely — so the pool's
last-written state is left alone rather than flapping. There is no automatic
winner election: resolve it by renaming or deleting one of the conflicting
CRs.

## Behavior notes
- **Cascade.** Deleting the `NomadCluster` cascades and removes all its
  `NomadPool` CRs (an ownerReference is set on create). This is safe under
  both background and foreground `--cascade` modes: if the cluster CR is gone
  *or* itself being deleted, the pool's finalizer drops without calling
  `Delete` — the Nomad control plane (and the pool with it) is gone or going
  away regardless.
- **Orphan risk on `--cascade=orphan`.** If you orphan the `NomadCluster`
  while its Nomad control plane keeps running, the pool CRs are removed
  without a Nomad-side `Delete` — the live pool is left in place
  (non-destructive; re-`apply` the CR to resume managing it). Same premise
  `NomadNode` already relies on for its own cascade behavior — not a new risk.
- **`status.nodeCount`** refreshes every steady-state resync (~60s).
  **`status.jobCount`** is only populated on the delete-blocked path — it is
  not a routine per-resync value.
