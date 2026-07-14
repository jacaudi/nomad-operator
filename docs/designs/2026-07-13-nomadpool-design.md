# NomadPool — Design

**Type:** design · **Date:** 2026-07-13 (amended 2026-07-13 after SGE design + plan reviews) · **Status:** proposed
**Feature:** slice 4 — a `NomadPool` CRD that declaratively manages a Nomad **node pool** on a `NomadCluster`: the user owns the pool's definition, the operator applies it to Nomad's control plane.

Follows slice 2 (NomadCluster HA control plane, merged `a1e4d6a`), the external-access-modes follow-up (merged `b91df1a`), and slice 3 (NomadNode, merged `1f02e49`; main HEAD `6c3e0c1`). Slice 3's `NomadNode` mirrors each node's `status.nodePool`; this slice manages the *pools* those nodes reference.

> **Amended 2026-07-13** after an independent sr-go-engineer *design* review (Fable model), verdict *amend-before-planning*. The review verified the managed-lifecycle model, the unmanaged-field preservation, the contract.go pin discipline, and the client seam as sound (all Nomad-api claims re-checked against the pinned `api` source). Folded corrections: **I-1** — the finalizer short-circuit now fires on `cluster NotFound` **or** `cluster.DeletionTimestamp != nil`, closing a `--cascade=foreground` deadlock the pure-NotFound test left open (§3.4); **I-2** — the `Delete` "pool non-empty" signal is elevated to a pre-plan spike, load-bearing for the whole `DeleteBlocked` branch (§6.3); **I-3** — a same-`poolName`+cluster collision is now *detected and surfaced* (`PoolNameConflict` condition + Warning event + skipped `Register`), not merely a prose caveat (§3.5); plus minor clarifications (orphan-premise acknowledgment §3.4, explicit compare-before-write comparator + "Meta is fully managed" §3.3, lazy `jobCount` §3.2). Every folded api claim was re-grounded against the pinned `api` v2.0.4.
>
> **Also amended 2026-07-13** after an independent sr-go-engineer *plan* review (Fable): the duplicate-`poolName` collision check (§3.5) drops the field indexer in favour of a plain namespaced `List` + in-Go filter — the indexer was speculative (the list cost is not what §3.5 avoids), and a custom field selector is unsupported on a bare (envtest) client. Behaviour is unchanged; the implementation is simpler and testable on both cached and bare clients.

Every Nomad-domain claim below is grounded in `go doc` against the pinned `github.com/hashicorp/nomad/api` (`v0.0.0-20260707172059-5b83b133998a`, == v2.0.4) and the HashiCorp docs, verified during brainstorming — not assumed from training.

---

## 1. Background & framing

A Nomad **node pool** groups clients for scheduling. Every client belongs to exactly one pool (`default` if unset); a job targets a pool via `node_pool = "…"`. Two pools are **built-in and immutable**: `default` (the implicit pool) and `all` (a virtual pool matching every client). A user-defined pool carries a name, a description, arbitrary metadata, and — on Nomad **Enterprise** only — a scheduler-configuration override.

### 1.1 Managed lifecycle, not representation (the model this rests on)

Unlike `NomadNode` (slice 3), a node pool does **not** self-register. Nodes are edge machines that join Nomad on their own, so the operator can only *reflect* them (operator owns Create+Delete via a reflector; user owns a bounded R+U surface). A pool, by contrast, is brought into being by an admin calling `NodePools().Register(...)`. It is **declared control-plane configuration**, so `NomadPool` is a **managed lifecycle object** shaped like `NomadCluster`:

| Verb | Owner | Mechanism |
|------|-------|-----------|
| **C**reate | user | write a `NomadPool` CR → operator `Register`s the pool onto Nomad |
| **R**ead | user | `kubectl get nomadpools` — declared pools + live node/job counts |
| **U**pdate | user | edit `spec.description` / `spec.meta` → operator re-`Register`s |
| **D**elete | user | `kubectl delete` → operator `Delete`s the Nomad pool (finalizer-gated) |

The CR is the **single source of truth**. The operator owns nothing it did not declare: pools created out-of-band (CLI/API) are invisible to the operator, and the built-in `default`/`all` are not manageable through a CR at all (§3.1 CEL).

### 1.2 Nomad node-pool facts this design rests on (verified against the pinned `api`)

- **The endpoint is `Client.NodePools() *NodePools`**, with: `List(q) ([]*NodePool, …)`, `Info(name, q) (*NodePool, …)`, **`Register(pool *NodePool, w) (*WriteMeta, error)`** (an **upsert** — the same call creates and updates; there is no separate Create/Update), `Delete(name, w) (*WriteMeta, error)`, and read-only `ListNodes(pool, q) ([]*NodeListStub, …)` / `ListJobs(pool, q) ([]*JobListStub, …)` / `PrefixList`.
- **`api.NodePool`** = `{ Name string; Description string; Meta map[string]string; NodeIdentityTTL time.Duration; SchedulerConfiguration *NodePoolSchedulerConfiguration; CreateIndex, ModifyIndex uint64 }`. `CreateIndex`/`ModifyIndex` are server-assigned.
- **`api.NodePoolSchedulerConfiguration`** = `{ SchedulerAlgorithm SchedulerAlgorithm ("binpack"|"spread"); MemoryOversubscriptionEnabled *bool }`. **This block is Nomad Enterprise-only** — node pools themselves are Community Edition, but per-pool scheduler config is inert/rejected on CE ([HashiCorp docs](https://developer.hashicorp.com/nomad/docs/other-specifications/node-pool)).
- **Built-in pools cannot be created, modified, *or* deleted.** `api.NodePoolDefault == "default"` and `api.NodePoolAll == "all"` are exported constants ([Node pools](https://developer.hashicorp.com/nomad/docs/architecture/cluster/node-pools)).
- **A pool with nodes or non-terminal jobs cannot be deleted** — `Delete` errors until the pool is empty ([node pool delete](https://developer.hashicorp.com/nomad/commands/node-pool/delete)).

---

## 2. Scope of this slice

**In scope:**
- The `NomadPool` CRD (`nomad.operator.io/v1alpha1`, namespaced), managed-lifecycle model.
- A `NomadPool`-keyed reconciler that `Register`s/`Delete`s pools onto a `NomadCluster` and derives status.
- A finalizer that blocks CR deletion until the Nomad pool is actually gone (§3.4).
- Five additive `internal/nomad.Client` methods + `contract.go` pins, backed by real calls (§4).
- Reuse of slice 3's `clusterNomadConfig` helper and the per-cluster client seam; a **new** `NomadPoolOps` consumer interface + fake (§4).
- envtest coverage with an injected fake pool client + a runbook.

**Out of scope (YAGNI; additive later — §5):**
- `spec.schedulerConfig` (Enterprise-only; no present consumer, inert on CE).
- `spec.nodeIdentityTTL` (niche Nomad-2.0 node-identity knob).
- Reclaim policy (`Retain`), configurable resync cadence, multi-cluster binding.
- Duplicate-`poolName` **arbitration** (winner election) + a validating admission webhook — the reconciler *detects and surfaces* the collision (§3.5); arbitration/webhook are the deferred parts (§5).

---

## 3. Design

### 3.1 CRD — `nomad.operator.io/v1alpha1`, kind `NomadPool` (namespaced)

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadPool
metadata:
  name: gpu-workers               # user-authored, RFC 1123 (user picks a valid K8s name)
  namespace: nomad-system         # same namespace as its NomadCluster
  ownerReferences:                # set by the operator to its NomadCluster (§3.4)
    - apiVersion: nomad.operator.io/v1alpha1
      kind: NomadCluster
      name: prod
spec:
  clusterRef:
    name: prod                    # NomadCluster in this namespace (immutable)
  poolName: gpu-workers           # EXACT Nomad pool name (immutable); see below
  description: "GPU worker nodes" # optional → NodePool.Description   (Community Edition)
  meta:                           # optional → NodePool.Meta          (Community Edition)
    team: ml
    tier: gpu
status:
  observedGeneration: 3
  nodeCount: 4                    # len(NodePools().ListNodes(poolName))
  jobCount: 2                     # len(NodePools().ListJobs(poolName)); populated on the delete-blocked path (§3.4)
  conditions:
    - type: Ready                 # operator successfully Registered the pool onto Nomad
      status: "True"
    - type: DeleteBlocked         # present only during a finalizer-blocked delete (§3.4)
      status: "False"
```

**Field-level decisions:**

- **`spec.poolName` is separate from `metadata.name`.** Nomad node-pool names permit characters that are illegal in a Kubernetes object name (underscores, uppercase, up to 128 chars), so `poolName` carries the exact Nomad name while `metadata.name` is any valid RFC 1123 name the user chooses (mirrors `NomadNode`'s `nodeName`/`metadata.name` split). Because the user authors the CR (managed model), **no operator-side sanitization is needed** — the user simply picks a valid `metadata.name`. `poolName` is *not* defaulted from `metadata.name` (explicit over magical).
- **`spec.clusterRef`** names a `NomadCluster` in the same namespace. One `NomadPool` CR = one pool on one cluster; "the same pool on N clusters" is N CRs (each cluster's pool is its own object — no shared knowledge duplicated). The CR carries an **ownerReference to that `NomadCluster`** for GC cascade (§3.4).
- **`spec.description`** (optional) → `NodePool.Description`. Community Edition.
- **`spec.meta`** (optional `map[string]string`) → `NodePool.Meta`. Community Edition; arbitrary pool labels.
- **No `phase` enum.** The pool lifecycle is thin (registered or not); a `Ready` condition plus `observedGeneration` is sufficient. This follows `NomadNode` (which dropped phase), not `NomadCluster`'s phase machine.
- **Status style:** `status` fields follow existing `nomadcluster_types.go`/`nomadnode_types.go` conventions (`+optional`, `omitzero`).
- **Printer columns:** `NAME`, `CLUSTER` (`spec.clusterRef.name`), `POOL` (`spec.poolName`), `READY` (`Ready` condition), `NODES` (`status.nodeCount`), `AGE`.

**CEL validation:**
- `spec.poolName` immutable (`self == oldSelf`) — the pool's identity.
- `spec.clusterRef.name` immutable — a pool belongs to one cluster.
- `spec.poolName not in ['default', 'all']` — the built-in pools cannot be created, modified, or deleted, so they are not representable as a managed CR (invalid states unrepresentable).
- `spec.poolName` matches Nomad's node-pool-name pattern (assumed `^[a-zA-Z0-9-_]{1,128}$`; **exact regex verified at plan time**, §6).

### 3.2 Reconciler — the `NomadPool`-keyed managed loop

`SetupWithManager`:
- `For(&NomadPool{})` — the primary object is the pool CR.
- `Watches(&NomadCluster{}, → pools with that clusterRef)` — a cluster becoming `Ready` re-reconciles its pending pools promptly (a `NomadPool` created before its cluster is Ready would otherwise wait for the resync).
- `RequeueAfter: 60s` steady-state resync — refreshes `nodeCount` and re-asserts the declared spec (self-heals out-of-band drift). Gentler than `NomadNode`'s 30s because pool config is near-static; **not user-configurable in v1**.

**Normal reconcile** (CR not being deleted; `clusterRef` resolves and is `Ready`):
1. **Ensure finalizer**; set the ownerReference to the `NomadCluster`.
2. **Build the client** via the shared slice-3 `clusterNomadConfig` helper (in-cluster API Service, CA/client PEM from the cert Secret, token from the cluster's status bootstrap-token Secret, `TLSServerName=server.<region>.nomad`). No global singleton, no `api.DefaultConfig()`.
3. **Collision check** (§3.5) — `List` the namespace's `NomadPool`s and filter for others targeting the same `poolName` on the same cluster. If any exist, set `Ready=False, reason=PoolNameConflict`, emit a Warning event, and **skip the `Register`** (do not churn Raft) — then still refresh status counts and requeue. Otherwise proceed.
4. **Read-modify-write + compare-before-write** (§3.3) — `Info(poolName)`; build the desired pool preserving unmanaged fields; `Register` only if the managed fields differ. Set `Ready=True` on success.
5. **Derive status** — `nodeCount = len(ListNodes(poolName))` (feeds the `NODES` printer column) and `observedGeneration`. **`jobCount` is computed only on the finalizer delete-blocked path** (§3.4) where it is surfaced — not on every steady-state resync — avoiding a second per-pool list call each minute for a value with no printer column.

**Cluster not resolvable / not Ready:**
- `clusterRef` **NotFound** → `Ready=False, reason=ClusterNotFound`; requeue; existing `status` left stale (last-known), never wiped.
- `clusterRef` exists but **not `Ready`/unreachable** → `Ready=False, reason=ClusterNotReady`; requeue. Pools are not Registered against a non-Ready cluster (its API can't be reached). Consistent with `NomadNode`.

### 3.3 Apply rule — read-modify-write, preserving unmanaged fields (labeled decision)

`NodePools().Register` replaces the whole `*NodePool`. The CRD models only `Name`/`Description`/`Meta`. Building the desired pool from `spec` alone (`SchedulerConfiguration=nil`, `NodeIdentityTTL=0`) and Registering would **silently wipe** any out-of-band Enterprise `scheduler_config` or `node_identity_ttl`. That is an irreversible, surprising, hard-to-debug side effect the first time the operator reconciles an adopted pool.

**Decision:** read-modify-write, preserving unmanaged fields.

- `existing, err := GetNodePool(poolName)`.
- If **not found** (404) → `desired := fromSpec()` (fresh `NodePool{Name, Description, Meta}`).
- Else → `desired := existing` with the **managed** fields overlaid (`Description`, `Meta` from spec); **preserve** `existing.SchedulerConfiguration`, `existing.NodeIdentityTTL`.
- **Compare-before-write:** `Register(desired)` only when a **managed** field differs — i.e. `existing.Description != spec.description || !maps.Equal(existing.Meta, spec.meta)`. (`nil` and empty `Meta` are both len-0 → equal, so no spurious `Register`. Do **not** `reflect.DeepEqual` the whole struct — it would diff the server-set `ModifyIndex` and churn forever.) Avoids Raft writes on every resync.

**`Meta` is a fully-managed field.** `spec.meta` owns the pool's `Meta` map entirely — out-of-band `Meta` keys are overwritten on the next `Register`. The preservation guarantee above applies **only** to `SchedulerConfiguration` and `NodeIdentityTTL` (the fields the CRD does not model); it is not a merge over `Meta`.

This costs **no extra call** (the `Info` is already needed for the compare) and is the **No-Wall seam**: when `spec.schedulerConfig` is added to the CRD later, a preserved field simply becomes a managed overlay field — no rewrite.

**Alternative (not chosen):** strict declarative ownership — the CR fully owns the pool and Register wipes unmanaged fields. Simpler and more purely "source of truth," but destroys Enterprise config on adoption. Rejected on the data-loss asymmetry; recorded here so the trade-off is explicit and reversible if strict ownership is later preferred.

### 3.4 Lifecycle, finalizer & deletion

Deleting a `NomadPool` has a real external side-effect (removing a Nomad pool) that must be **confirmed** and can be **refused** (non-empty). So — unlike `NomadNode`, whose CR-delete never touches Nomad — `NomadPool` uses a **finalizer**.

**Finalizer delete path** (CR has a `DeletionTimestamp`), evaluated **in this order**:
- `clusterRef` **NotFound, *or* the `NomadCluster` is itself being deleted** (`cluster.DeletionTimestamp != nil`) → **drop the finalizer without calling `Delete`.** If the cluster CR is gone *or going*, the Nomad control plane (and its pools) are gone/going too — there is nothing to clean up. This single predicate is what makes the ownerReference cascade safe under **both** cascade modes (see below); it must be checked **before** the unreachable case, or foreground cascade deadlocks.
- `clusterRef` exists, is **not** being deleted, but is **unreachable/not `Ready`** → keep the finalizer; `DeleteBlocked=True, reason=ClusterNotReady`; requeue. Do **not** orphan the pool on a transient control-plane blip.
- `clusterRef` **`Ready`** → `NodePools().Delete(poolName)`:
  - success, or pool **already absent** (404) → **drop the finalizer.**
  - refused because **non-empty** → keep the finalizer; `DeleteBlocked=True` surfacing `nodeCount`/`jobCount` (this is the concrete reason node/job counts are in status — a stuck-`Terminating` CR tells the user *why*); requeue.
  - other transient error → keep the finalizer; requeue.

**ownerReference + finalizer cascade interaction (resolved for both cascade modes).** `NomadPool` carries an ownerReference to its `NomadCluster` so Kubernetes GC removes the pool CRs when the cluster is deleted. GC deletes owned objects by setting `DeletionTimestamp`, which **still runs finalizers**. Naively this deadlocks: cluster deleted → pool CR finalizer fires → `Delete` against a gone cluster fails forever → CR stuck `Terminating`. The **cluster-gone-*or-going* short-circuit above breaks the deadlock in both modes:**

- **Background cascade** (the default): the `NomadCluster` CR is removed promptly (it has **no finalizer** of its own — verified, only the auto-generated RBAC marker), so the pool's `Get(cluster)` returns `NotFound` and the finalizer is dropped without a `Delete`.
- **Foreground cascade** (`--cascade=foreground`): Kubernetes puts a `foregroundDeletion` finalizer on the cluster and holds it in `Terminating` until every child with `blockOwnerDeletion=true` is gone. `controllerutil.SetControllerReference` (which this reconciler uses, mirroring slice 3) sets `Controller=true` **and `BlockOwnerDeletion=true`**, so the pool CR blocks the cluster's removal *and* the cluster CR stays `NotFound`-negative (present, `Terminating`) — the pure "NotFound" test would never fire and the pool's `Delete` against the torn-down API would fail forever. The **`cluster.DeletionTimestamp != nil`** half of the predicate handles exactly this: the pool sees the cluster `Terminating`, drops its finalizer without a `Delete`, which in turn unblocks the cluster's foreground deletion. No deadlock in either mode.

The near-simultaneous edge (pool reconciles while the cluster CR is briefly still present-and-*not*-terminating but Nomad already torn down) resolves to at worst a transient "Delete failed → requeue" that self-heals once the cluster CR is `NotFound`/`Terminating` — never a permanent stuck state.

**Orphan premise (acknowledged).** The short-circuit rests on "cluster CR gone/going ⇒ Nomad control plane gone." That is **false** under `kubectl delete nomadcluster --cascade=orphan`, or a manual CR removal while the StatefulSet keeps running: the pool finalizer then drops without `Delete`, orphaning a live Nomad pool. This is **non-destructive** (no data loss; the pool is re-appliable) and is the **same premise slice-3 `NomadNode` already relies on** for its prune/cascade behavior — a consistent project stance, not a new defect. Recovery is to re-apply the `NomadPool` (or delete the pool via the Nomad CLI).

**Conditions:**
- `Ready` — the operator successfully Registered the declared pool onto Nomad.
- `DeleteBlocked` — present during a finalizer-blocked deletion (reasons: `PoolNotEmpty`, `ClusterNotReady`).
- `ClusterNotFound` / `ClusterNotReady` — surfaced on `Ready=False` during normal reconcile.
- `PoolNameConflict` — another `NomadPool` targets the same `poolName` on the same cluster (§3.5); `Ready=False`, `Register` skipped.

### 3.5 Duplicate-`poolName` collision — detect and surface (no arbitration)

Two `NomadPool` CRs declaring the *same* `poolName` on the *same* cluster would otherwise fight over one Nomad pool, each re-`Register`ing every resync — flapping `ModifyIndex` and writing Raft continuously (worse than `NomadNode`'s read-only `DuplicateNodeName` stall, because this is *write* churn). The reconciler **detects and surfaces** the collision rather than silently churning or arbitrating a winner:

- The reconciler `List`s the namespace's `NomadPool`s (a bounded, namespaced list) and filters in Go for others whose `spec.clusterRef.name`+`spec.poolName` matches. No field indexer is used: at namespaced-pool scale a plain `List` is trivially cheap, it works identically on a cached (production) and a bare (envtest) client, and an indexer would be speculative machinery for a scale that does not exist (KISS/YAGNI). The list cost is not what §3.5 avoids — the *skipped `Register`* is.
- When one or more *other* `NomadPool`s share the key, the reconciler sets `Ready=False, reason=PoolNameConflict`, **emits a Warning event**, and **skips the `Register`** — stopping the Raft write-amplification. Status counts are still mirrored so the CR stays informative.
- **No winner arbitration and no admission webhook** (both deferred — §5). The operator does not pick which CR "owns" the pool; it refuses to churn and tells the user to resolve the duplicate. Because *every* colliding CR skips its `Register`, the pool's last-written state is left as-is until the conflict is removed — no flapping.

This is the proportionate middle: cheap (one namespaced `List` on the reconcile that already reads the pool), additive, and it converts a silent write-amplification failure mode into a visible condition + event.

---

## 4. Per-cluster client, `internal/nomad` additions & `contract.go` pins

**Client seam.** The reconciler defines its **own** consumer-side ops interface (interface-at-consumer convention) — slice-2's `NomadOps` and slice-3's `NomadNodeOps` are **not** widened (that would couple the controllers' test seams):

```go
// NomadPoolOps is the pool reconciler's consumer interface (defined in the controller pkg).
type NomadPoolOps interface {
    GetNodePool(ctx context.Context, name string) (*api.NodePool, error) // nil,nil on 404
    UpsertNodePool(ctx context.Context, pool *api.NodePool) error
    DeleteNodePool(ctx context.Context, name string) error
    CountNodePoolNodes(ctx context.Context, name string) (int, error)
    CountNodePoolJobs(ctx context.Context, name string) (int, error)
}
```

Built by a `NewNomadPoolClient` factory (faked in envtest, per the slice-2/3 pattern). Config is constructed via the **existing slice-3 `clusterNomadConfig` helper** (DRY — reuse, don't fork).

**Five additive `internal/nomad.Client` methods**, each backed by a real `api` call:

```go
func (c *Client) GetNodePool(ctx context.Context, name string) (*api.NodePool, error)      // NodePools().Info; (nil,nil) on 404
func (c *Client) UpsertNodePool(ctx context.Context, pool *api.NodePool) error              // NodePools().Register
func (c *Client) DeleteNodePool(ctx context.Context, name string) error                     // NodePools().Delete
func (c *Client) CountNodePoolNodes(ctx context.Context, name string) (int, error)          // NodePools().ListNodes → len
func (c *Client) CountNodePoolJobs(ctx context.Context, name string) (int, error)           // NodePools().ListJobs → len
```

### 4.1 `contract.go` additions (backed by real calls)

The pin rule (from Foundation): only pin symbols a real call exercises (avoid the existence-only-pin gotcha).

- **Accessor pin:** `(*api.Client).NodePools`.
- **Method pins** (each exercised by a `Client` method above): `(*api.NodePools).Info`, `.Register`, `.Delete`, `.ListNodes`, `.ListJobs`.
- **Type pin:** `api.NodePool` (named in `GetNodePool`/`UpsertNodePool`).
- **Constant pins** `api.NodePoolDefault` / `api.NodePoolAll` — pinned **only** alongside a Go-level defense-in-depth guard in the reconciler that rejects those `poolName`s (CEL is the primary admission gate; the Go guard covers any non-admission path and *is what makes the constant pins honest*). If the guard is dropped, the constants stay unpinned and CEL alone gates — the two decisions are coupled; never pin the constants without the guard. (The CEL rule hardcodes the literals `'default'`/`'all'` while the Go guard uses the `api.*` constants — CEL cannot reference Go constants, so this is an acceptable **Tier-A external-contract duplication** across the admission boundary, not a DRY violation.)
- **Not pinned:** `api.NodePoolSchedulerConfiguration` (never named — carried through §3.3 as an opaque `*NodePool.SchedulerConfiguration` pointer); the `ListNodes`/`ListJobs` element types (`NodeListStub` already pinned; `JobListStub` not named — results are `len()`'d). Pinning either would reintroduce the existence-only-pin risk.

**`config/crd/kustomization.yaml`.** Because the CRD Go types are hand-authored, manually add `- bases/nomad.operator.io_nomadpools.yaml` to the `resources:` list — `controller-gen` regenerates the base but not the kustomization list, and `make deploy` silently omits the CRD otherwise (the slice-3 `6c3e0c1` lesson; unit/envtest won't catch it, only a real deploy does).

**`cmd/main.go`.** Register the `NomadPoolReconciler` with the manager.

---

## 5. Explicitly not built (YAGNI)

- **`spec.schedulerConfig`** (`{algorithm: binpack|spread, memoryOversubscriptionEnabled: bool}`) — Nomad **Enterprise-only**; inert/rejected on Community Edition, no present consumer. Additive later as an optional block that the §3.3 preservation seam already anticipates.
- **`spec.nodeIdentityTTL`** — per-pool Nomad-2.0 node-identity JWT TTL; niche. Additive later.
- **Reclaim policy (`Retain`)** — v1 always deletes the Nomad pool on CR delete (finalizer-gated). A `spec.reclaimPolicy` (Delete|Retain) is a second code path with no present requirement.
- **Configurable resync cadence** — fixed 60s in v1.
- **Multi-cluster binding** — one CR = one cluster; a selector/list is speculative generality.
- **Duplicate-`poolName` *arbitration* + admission webhook (not built; detection *is* — §3.5).** The reconciler **detects** a same-`poolName`+cluster collision and surfaces it (`PoolNameConflict` condition + Warning event + skipped `Register`, §3.5). What is deferred is (a) **winner arbitration** — deterministically electing one CR to keep managing the pool while the others stand down — and (b) a **validating admission webhook** that rejects the duplicate at create time. Both are additive; detection-and-surface is the proportionate v1 (stops the write churn, makes the misconfiguration visible) without arbitration logic/tie-break rules for a rare case.

---

## 6. Open items / assumptions to verify at plan/implementation time

1. **Node-pool-name regex** — confirm Nomad v2.0.4's exact validation pattern for the `spec.poolName` CEL rule (assumed `^[a-zA-Z0-9-_]{1,128}$`).
2. **`NodePools().Info` 404 shape** — confirm a missing pool surfaces as `api.UnexpectedResponseError{StatusCode: 404}` (already pinned) vs a typed error vs `(nil, nil)`, so `GetNodePool` can return `(nil, nil)` for not-found reliably.
3. **`Delete` non-empty signal (pre-plan spike — load-bearing).** The entire `DeleteBlocked`/`PoolNotEmpty` finalizer branch (§3.4) depends on distinguishing "pool has nodes/jobs" from other `Delete` failures. This is **server-side and not derivable from the pinned `api`** — resolve it with a real-Nomad spike *before* planning, not as an implementation detail. Likely fallback if there is no clean status code: **body-substring matching**, the same pattern `internal/nomad/errors.go` already uses for ACL-bootstrap-already-done (`aclBootstrapAlreadyDoneText`) — the plan should budget for a small fragile-match helper.
4. **Two controllers/watches on `NomadCluster`** — confirm the pool controller's `Watches(&NomadCluster{})` coexists cleanly in `SetupWithManager` with slice-2's `For(&NomadCluster{})` reconciler and slice-3's `For(&NomadCluster{})` reflector (separate `Named(...)` controllers, independent workqueues).
5. **`Register` upsert semantics on adoption** — confirm `Register` on a pre-existing out-of-band pool updates in place (adoption) rather than erroring, and that the §3.3 preservation overlay round-trips through `Info`→`Register` without dropping server-set fields.

---

## 7. Definition of Done

- `NomadPool` CRD + the managed reconciler implemented; `make manifests generate fmt vet` and `make test` green (zero regen drift).
- Creating a `NomadPool` Registers the pool onto Nomad; `kubectl get nomadpools` shows `READY=True` with correct `NODES`/`jobCount`.
- Editing `spec.description`/`spec.meta` re-Registers (compare-before-write: no redundant `Register` when unchanged); an out-of-band Enterprise `scheduler_config` on the pool is **preserved** across reconciles (§3.3).
- `poolName ∈ {default, all}` is rejected by CEL (and the Go guard); `poolName`/`clusterRef` are immutable.
- Deleting a `NomadPool` whose pool is **empty** deletes the Nomad pool and completes; deleting one whose pool is **non-empty** holds in `Terminating` with `DeleteBlocked` + counts until emptied, then completes.
- Under **both background and foreground cascade**, deleting the `NomadCluster` cascade-deletes its `NomadPool` CRs without a stuck-`Terminating` (the cluster-gone-*or-going* short-circuit, §3.4); a transient cluster blip during a **standalone** `NomadPool` delete does **not** orphan or wrongly drop the finalizer.
- `contract.go` compiles against the pinned `api` with every new pin backed by a real call.
- envtest coverage (fake pool client) for: register/upsert with compare-before-write, unmanaged-field preservation, status counts, `Ready`, `ClusterNotFound`/`ClusterNotReady`, `PoolNameConflict` detection (two CRs same `poolName`+cluster → both skip `Register`, condition + Warning event), finalizer delete success, delete-blocked (non-empty), the cluster-gone-*or-going* finalizer short-circuit (**both** background `NotFound` **and** foreground `cluster.DeletionTimestamp != nil`), CEL reject of built-ins, immutability. Runbook section added.
- `config/crd/kustomization.yaml` lists the `nomadpools` base; `cmd/main.go` wires the reconciler.

---

## 8. Testing

- **Unit** (`internal/nomad`): the five `Client` methods' argument mapping and `GetNodePool` 404 → `(nil, nil)` handling.
- **envtest** (`internal/controller`): inject a fake `NomadPoolOps` returning scripted pool/node/job data; assert the DoD behaviors above — register/upsert, compare-before-write no-op, unmanaged-field preservation, status counts, `PoolNameConflict` detection (skip `Register` + condition + event), the finalizer paths (success / delete-blocked / cluster-gone-or-going short-circuit, incl. the foreground `DeletionTimestamp` case), CEL rejections, and cluster-not-ready degradation. No real Nomad/pods needed.
- **Integration** (`-tags integration`, hermetic, real Nomad v2.0.4): register a pool, add a node to it, attempt delete (blocked), empty it, delete (succeeds) — the same containerized method used to close Foundation open-item #1. Live run deferred if no `nomad` binary is present in-env (as in slice 3).

---

## 9. Reconcile with the roadmap

- **Depends on slice 2/3:** a `Ready` `NomadCluster` and the per-cluster client wiring (the `clusterNomadConfig` helper extracted in slice 3). The only cross-slice reuse is that helper; no slice-2/3 interface is widened.
- **Relates to slice 3:** `NomadNode.status.nodePool` names the pool a node is in; `NomadPool` manages those pools. The pool→node relationship is read-only here (via `ListNodes` for the count); assigning a node to a pool is the node's own client config, not pushable through the pool API.
- **Enables slice 5 (`NomadJob`):** a job targets a pool by name (`node_pool`); a managed `NomadPool` gives that reference a K8s-native object to depend on.
- **`v1alpha1` still unreleased:** the new CRD ships without a conversion webhook.
