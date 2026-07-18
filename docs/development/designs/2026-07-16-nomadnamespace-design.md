# NomadNamespace — Design

**Type:** design · **Date:** 2026-07-16 · **Status:** proposed
**Feature:** a `NomadNamespace` CRD that declaratively manages a Nomad **namespace** on a `NomadCluster` (the operator `Register`s/`Delete`s it), **plus** namespace-threading in the existing `NomadJob` reconciler so a job can be placed into, observed in, and cleanly removed from a non-`default` Nomad namespace.

This is the fifth managed-lifecycle CRD, completing the core object chain **Cluster → Node → Pool → Job → Namespace**. It originated from a live-kind end-to-end finding (2026-07-16): the `NomadJob` operator only fully supports the `default` Nomad namespace — a job whose `spec.job` declared `namespace: <non-default>` registered correctly (Nomad's `Register` reads `job.Namespace`) but then the operator's `GetJob`/`PlanJob`/`JobGroupSummary`/`DeregisterJob` calls used the client's **default** namespace, so status 404-looped and, on delete, `Deregister` 404'd in the wrong namespace → `nomad.IsNotFound` dropped the finalizer → **the real job was orphaned**.

The minimal fix considered first was to *reject* non-default namespaces. The chosen direction is to *support* them: if the operator is the interface from Kubernetes CRs to the Nomad API, then "the namespace must exist before a job can target it" is the operator's responsibility to satisfy — via a first-class `NomadNamespace` CRD — not a reason to refuse.

Every Nomad-domain claim below is grounded in `go doc` against the pinned `github.com/hashicorp/nomad/api` (`v0.0.0-20260707172059-5b83b133998a`, == v2.0.4). The namespace endpoint surface was verified during brainstorming (`Namespaces().Register/Delete/Info/List/PrefixList`, `api.Namespace{Name,Description,Quota,Capabilities,NodePoolConfiguration,Vault/Consul config,Meta,RequiredExtraClaims,OptionalExtraClaims,...}`). Exact server-side *wordings/patterns* (delete-refusal body, reserved set, name regex) are flagged as plan-time spikes (§6), the same discipline used for the NomadPool I-2 spike.

> **Amended 2026-07-16** after an independent sr-go-engineer *design* review (Fable model), verdict *amend-before-planning* — no blocking issues; the root-cause orphan fix, the Nomad namespace API surface, the per-call namespace seam, the `NomadNamespaceOps` shape, and the `contract.go` pins were all verified SOUND against the pinned `api`. Folded corrections: **I-1** — `Jobs().List` returns **all** jobs including terminal ones (`JobListStub.Status` exists precisely because the list is unfiltered), so `status.jobCount` is defined as **total jobs in the namespace** (informational; the delete gate is `IsNamespaceNotEmpty` on the `Delete` call, never this count), and the §6.4 spike now separates namespace-scoping from terminal-`Status` filtering (the namespace wildcard `*` spans namespaces and does **not** filter job state). **I-2** — the `NomadJobOps` change is **3 methods gain a `namespace` parameter** (`GetJob`/`DeregisterJob`/`JobGroupSummary`) **+ 2 derive it from `job.Namespace`** with no signature change (`PlanJob`/`RegisterJob`); an earlier "four methods" phrasing in §4 was wrong and is corrected. **M-1** — the `*job.Namespace` read in `PlanJob`/`RegisterJob` is nil-guarded in the client method. **M-2** — the orphan fix assumes `spec.nomadNamespace` matches where the job actually lives; a dev-era non-default job predating the field must be recreated, not migrated in place (bounded — `v1alpha1` unreleased, no stored CRs). **M-3** — the §6.2 reserved-name spike must confirm `default` is the only server-reserved namespace name (`*`/`all` carry no namespace meaning). **M-4** — `spec.nomadNamespace` immutability follows from Nomad job identity `(namespace, jobID)`, stated at the field definition.

---

## 1. Background & framing

### 1.1 What a Nomad namespace is

A Nomad **namespace** is a multi-tenancy partition *inside a single Nomad cluster* — entirely distinct from a Kubernetes namespace:

- **Identity.** A job's full identity is `(namespace, jobID)`; `team-a/web` and `team-b/web` are two different jobs in one cluster.
- **Access & quota.** ACL policies scope per-namespace; resource quotas attach per-namespace.
- **`default` always exists** and everything lands there unless told otherwise; `default` cannot be deleted.
- **Namespaces must pre-exist.** Nomad does **not** auto-create a namespace on job submit — a `Register` into a missing namespace fails. A namespace is created via `Namespaces().Register` (`nomad namespace apply`).

That last fact is why supporting non-default namespaces requires the operator to *own namespace creation*, which is what the `NomadNamespace` CRD provides.

### 1.2 Two namespaces, never conflated

| | Kubernetes namespace | Nomad namespace |
|---|---|---|
| What | Where the CRs (`NomadCluster`/`NomadJob`/`NomadNamespace`) and their Secrets live | A tenancy partition inside one Nomad cluster |
| Set via | `metadata.namespace` | `NomadNamespace.spec.namespaceName` / `NomadJob.spec.nomadNamespace` |
| Status before this slice | Already correct (colocation model, §1.3) | The finding — broken for non-`default` |

### 1.3 Colocation model (unchanged, and why passthrough was rejected)

Like `NomadPool`/`NomadJob`, a `NomadNamespace` resolves its `clusterRef` in **its own Kubernetes namespace** (`nomadjob_controller.go:111` / `nomadpool_controller.go:83`). So a `NomadCluster` and all the `NomadPool`/`NomadJob`/`NomadNamespace` CRs targeting it are colocated in one Kubernetes namespace.

A tempting alternative — **derive the Nomad namespace from the CR's Kubernetes namespace** ("passthrough") — was rejected: under colocation, *every* job of a cluster shares that cluster's single Kubernetes namespace, so passthrough would map them all into **one** Nomad namespace (a rename of `default`), not per-tenant partitioning. Real passthrough tenancy would require cross-namespace `clusterRef` (jobs in `team-a`/`team-b` referencing a cluster in `nomad-system`) — a larger architectural change with RBAC implications, deferred (§5). The Nomad namespace name is therefore chosen **explicitly** via spec fields, not derived.

### 1.4 Managed lifecycle, and the NomadPool symmetry this rests on

A `NomadNamespace` is a **managed-lifecycle object** exactly like `NomadPool` (slice 4): the CR is the single source of truth; the operator brings the namespace into being and tears it down.

The Nomad namespace API is **structurally identical to the node-pool API**, which is why this design mirrors the merged, reviewed `NomadPool` almost line-for-line:

| Concern | Node pools (`NomadPool`, merged) | Namespaces (this slice) |
|---|---|---|
| Create/update | `NodePools().Register` (upsert) | `Namespaces().Register` (upsert) |
| Delete | `NodePools().Delete` | `Namespaces().Delete` |
| Delete refused when… | pool has nodes **or** non-terminal jobs | namespace has **non-terminal jobs** |
| Struct | `NodePool{Name,Description,Meta,SchedulerConfiguration}` | `Namespace{Name,Description,Meta,Quota,Capabilities,...}` |
| Reserved names | `default`, `all` | `default` |

The divergences are contained and enumerated in §3.

---

## 2. Scope of this slice

**In scope:**
- The `NomadNamespace` CRD (`nomad.operator.io/v1alpha1`, namespaced), managed-lifecycle, mirroring `NomadPool`.
- A `NomadNamespace`-keyed reconciler: upsert (read-modify-write) via `Namespaces().Register`, delete via a block-until-empty finalizer, duplicate-name conflict detection, bounded status (`jobCount`).
- Additive `internal/nomad.Client` namespace methods + `contract.go` pins, each backed by a real call; a new `IsNamespaceNotEmpty` error helper.
- A new `NomadNamespaceOps` consumer interface + fake; reuse of the shared `clusterNomadConfig` helper.
- **NomadJob namespace-threading:** a new immutable `spec.nomadNamespace` (default `default`), authoritative injection into `job.Namespace`, and threading the namespace through `GetJob`/`PlanJob`/`DeregisterJob`/`JobGroupSummary` and the finalizer.
- envtest coverage (both reconcilers, injected fakes) + a hermetic integration spike + runbook.

**Out of scope (YAGNI; additive later — §5):**
- Cross-namespace `clusterRef` / true Kubernetes→Nomad tenancy passthrough.
- Managing namespace `Quota`, `Capabilities`, `NodePoolConfiguration`, Vault/Consul config, extra-claims (preserved via read-modify-write, not owned in v1).
- A hard `NomadJob.spec.namespaceRef` dependency (by-name coupling only, §3.7 — mirrors the existing job→pool by-name relationship).
- ACL policy/role/token management per namespace.

---

## 3. Design

### 3.1 CRD — `nomad.operator.io/v1alpha1`, kind `NomadNamespace` (namespaced)

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadNamespace
metadata:
  name: team-a                   # user-authored, RFC 1123
  namespace: nomad-system        # same K8s namespace as its NomadCluster
  ownerReferences:               # set by the operator to its NomadCluster (§3.5)
    - {apiVersion: nomad.operator.io/v1alpha1, kind: NomadCluster, name: prod}
spec:
  clusterRef:
    name: prod                   # NomadCluster in this K8s namespace (immutable)
  namespaceName: team-a          # EXACT Nomad namespace name (immutable); reject "default"
  description: "Team A workloads" # managed
  meta:                          # fully-managed key/value map
    owner: team-a
status:
  jobCount: 4                    # non-terminal jobs in the namespace (delete-blocked path)
  observedGeneration: 7
  conditions:
    - {type: Ready, status: "True"}           # namespace registered onto Nomad
    - {type: DeleteBlocked, status: "False"}  # present during a finalizer-blocked delete
```

**Field-level decisions (mirroring `NomadPool`):**

- **`spec.namespaceName` is a separate immutable field** from `metadata.name` — Nomad namespace names may contain characters illegal in a Kubernetes object name. It is the exact Nomad namespace name.
- **Reserved-name guard:** `namespaceName` may not be `default` (always exists, cannot be deleted). CEL rejects it at admission; a Go defense-in-depth guard rejects it in the reconciler with `reason=ReservedNamespace` (unreachable behind CEL, but structural — mirrors NomadPool's `BuiltinPool`). `all` is a node-pool wildcard, **not** a namespace concept, so it is *not* rejected here. The exact reserved set is a plan-time spike (§6).
- **`spec.description` + `spec.meta` are the managed body.** `Register` is a read-modify-write: read the live `Namespace`, overwrite `Description`+`Meta`, **preserve** every other field (`Quota`, `Capabilities`, `NodePoolConfiguration`, Vault/Consul config, `RequiredExtraClaims`/`OptionalExtraClaims`), write back — exactly how `NomadPool` preserved `SchedulerConfiguration`. Compare-before-write (`Description` equal **and** `maps.Equal(Meta)`) avoids a redundant `Register` each resync.
- **`spec.clusterRef`** names a `NomadCluster` in the same K8s namespace (immutable); +ownerReference for GC cascade (§3.5).
- **No `phase` enum** — a `Ready` condition + `observedGeneration` (follows NomadPool/NomadJob).
- **Printer columns:** `NAME`, `CLUSTER` (`spec.clusterRef.name`), `NAMESPACE` (`spec.namespaceName`), `READY` (`Ready` condition), `AGE`.

**CEL validation:**
- `spec.namespaceName` immutable (`self == oldSelf`); `spec.clusterRef.name` immutable.
- `spec.namespaceName != 'default'`.
- `spec.namespaceName` matches Nomad's namespace-name pattern (**assumed** `^[a-zA-Z0-9-_]{1,128}$`; exact regex verified at plan time, §6).

### 3.2 Reconciler — the `NomadNamespace`-keyed managed loop

`SetupWithManager`: `For(&NomadNamespace{})`, `Watches(&NomadCluster{}, → namespaces with that clusterRef)`, `Named("nomadnamespace")`, `RequeueAfter: 60s`. An independent `Named` controller coexisting with the cluster/node/pool/job controllers (workqueue-isolated). Mirrors `nomadpool_controller.go`.

**Normal reconcile** (CR not being deleted; `clusterRef` resolves and is `Ready`):
1. **Ensure finalizer**; set the ownerReference to the `NomadCluster` (write only when it changes — the NomadPool M-1 pattern).
2. **Duplicate-name check** (§3.3). On conflict → `Ready=False, reason=NamespaceNameConflict`; skip `Register`; requeue.
3. **Reserved-name guard** (`default`) → `Ready=False, reason=ReservedNamespace` (defense-in-depth behind CEL).
4. **Build the client** via `clusterNomadConfig` (region-scoped at construction — `api.Config.Region`, not per-call).
5. **Upsert** (§3.4) — read-modify-write `Register` only when `Description`/`Meta` drift. `Ready=True` on success.
6. **Status** — `jobCount` refreshed (the namespace-scoped non-terminal job count, §3.6); `observedGeneration`.

**Cluster not resolvable / not Ready** (identical to NomadPool/NomadJob):
- `clusterRef` **NotFound** → `Ready=False, reason=ClusterNotFound`; requeue; status left stale.
- `clusterRef` exists but **not `Ready`** → `Ready=False, reason=ClusterNotReady`; requeue.

### 3.3 Duplicate-name conflict detection

Two `NomadNamespace` CRs in the same K8s namespace naming the same `namespaceName` on the same cluster would fight over one Nomad namespace. Detected via a plain namespaced `List` (no field indexer — the NomadPool decision): if another non-`Terminating` CR with the same `clusterRef`+`namespaceName` exists, the reconcile emits `Ready=False, reason=NamespaceNameConflict` + a Warning event and **skips `Register`**. `Terminating` siblings (`DeletionTimestamp != 0`) are ignored so a same-name replacement isn't blocked by a GC-pending predecessor (the NomadPool M-3 fix, adopted from the start).

### 3.4 Apply rule — read-modify-write, compare-before-write

`Register` is an upsert over the whole `*Namespace`. To avoid clobbering fields the operator does not manage and to avoid churn:
- **Read** the live namespace (`Namespaces().Info`; `(nil,nil)` on 404 → treat as create).
- **Compare** managed fields: if `live.Description == spec.Description` **and** `maps.Equal(live.Meta, spec.Meta)`, skip the write (in sync).
- **Modify + write** otherwise: start from the live struct (preserving `Quota`/`Capabilities`/config/claims), set `Description`+`Meta`, `Register`.

This is the No-Wall/DRY choice: the operator owns only what it declares, and the giant server-managed remainder is preserved, not re-derived.

### 3.5 Lifecycle, finalizer & deletion

Deleting a `NomadNamespace` has a real external side-effect, so — like `NomadPool` — it uses a finalizer. The **entire finalizer path reuses the NomadPool §3.4 shape verbatim** (battle-tested, merged):

- `clusterRef` **NotFound, or the `NomadCluster` is being deleted** (`DeletionTimestamp != nil`) → **drop the finalizer without calling `Delete`.** Control plane gone/going ⇒ the namespace is gone/going too. Closes both background and foreground cascade.
- `clusterRef` present, not deleting, but **not `Ready`** → keep the finalizer; `DeleteBlocked=True, reason=ClusterNotReady`; requeue (don't orphan on a blip).
- `clusterRef` **`Ready`** → `Namespaces().Delete(namespaceName)`:
  - success, or namespace **already absent** (404, via `nomad.IsNotFound`) → **drop the finalizer.**
  - **non-empty refusal** (namespace still holds non-terminal jobs, via the new `IsNamespaceNotEmpty`, §4) → keep the finalizer; `DeleteBlocked=True, reason=NamespaceNotEmpty`; populate `status.jobCount`; requeue. This is the meaningful divergence from NomadJob (whose Deregister is never refused) and the direct analogue of NomadPool's `PoolNotEmpty`.
  - other transient error → keep the finalizer; `DeleteBlocked=True, reason=DeleteFailed`; requeue.

Control flow keeps the finalizer on **any** non-`IsNotFound` `Delete` error; `IsNamespaceNotEmpty` only selects the friendlier `NamespaceNotEmpty` reason (the NomadPool I-1 lesson — never treat an ordinary Delete error as "done").

**Orphan premise (acknowledged, consistent with the project).** The cluster-gone-or-going short-circuit rests on "cluster CR gone/going ⇒ control plane gone," false under `--cascade=orphan` or a manual CR removal while the StatefulSet runs — the same premise `NomadNode`/`NomadPool`/`NomadJob` already rely on.

### 3.6 Status — bounded

- `status.jobCount` — total jobs in the namespace (`Jobs().List` scoped to the namespace, §4). **Informational**, not a gate: the delete-blocked decision is `IsNamespaceNotEmpty` on the `Delete` call, never this count. `Jobs().List` returns all jobs including terminal ones (SGE I-1), so this is a raw total unless the plan opts to filter non-terminal `JobListStub.Status`. Refreshed on the delete-blocked path and steady-state resync, mirroring NomadPool's `jobCount`.
- `status.observedGeneration`, `status.conditions[Ready]`.

No per-job or per-alloc mirroring — that belongs to `NomadJob`.

### 3.7 NomadJob namespace-threading (Part 2)

The `NomadJob` reconciler is extended so a job can target a non-`default` namespace end-to-end.

**Linkage — by name, not by ref.** A new immutable field:

```yaml
# NomadJob.spec
nomadNamespace: team-a           # immutable; default "default"; the Nomad namespace to place the job in
```

By-name coupling (no `namespaceRef`) is deliberate and consistent with the established stance (NomadJob design §9: *"a job targets a pool by name; coupling is by-name only, no CR-level dependency enforced in v1"*). `default` needs no `NomadNamespace` CR. If the named namespace does not exist, Nomad's `Register` errors → surfaced `Ready=False` and self-heals once the `NomadNamespace` CR lands.

`spec.nomadNamespace` is **immutable** (CEL) because Nomad job identity is `(namespace, jobID)` — changing the namespace names a *different* job (§1.1). This mirrors `spec.jobID`'s immutability rationale (`nomadjob_types.go`), not an arbitrary restriction (SGE M-4).

**Identity injection (mirrors `spec.jobID`).** In `decodeJob`, after decoding and the jobID-mismatch check:
- If the blob's `job.Namespace` is non-empty and `!= spec.nomadNamespace` → reject with `reason=NamespaceMismatch` (`spec.nomadNamespace` is authoritative — invalid states unrepresentable).
- Otherwise inject `job.Namespace = &spec.nomadNamespace`.

The prior "reject non-default namespace" guard is **superseded** and not built.

**Threading through the client calls.** The namespace is **job-scoped**, so it is passed as a **per-call parameter**, never through the cluster-scoped shared `clusterNomadConfig` (which the node/pool reconcilers also use). The `NomadJobOps` methods that take only a `jobID` gain a `namespace` parameter; the methods that take the full `*api.Job` derive it from `job.Namespace` and set `WriteOptions.Namespace` for symmetry:

- `GetJob(ctx, namespace, jobID)` — `QueryOptions.Namespace`
- `DeregisterJob(ctx, namespace, jobID, purge)` — `WriteOptions.Namespace`
- `JobGroupSummary(ctx, namespace, jobID)` — `QueryOptions.Namespace`
- `PlanJob(ctx, job)` / `RegisterJob(ctx, job)` — set `WriteOptions.Namespace` from `job.Namespace` (already carries it post-injection; the deref is **nil-guarded** in the client method — SGE M-1)

**The finalizer reads `spec.nomadNamespace` directly** (`finalizeJob` never decodes the blob) → `DeregisterJob(ctx, nj.Spec.NomadNamespace, jobID, true)` hits the right namespace → **the orphan is impossible.** This is the root-cause fix; a currently-invalid blob no longer breaks deletion.

**Caveat (SGE M-2):** the fix assumes `spec.nomadNamespace` matches where the job actually lives. A dev-era job registered into a non-default namespace *before* this field existed defaults to `default` (immutable) on upgrade → `NamespaceMismatch` on reconcile and a `default`-namespace Deregister on delete → the orphan would recur. Such a CR must be **recreated, not migrated in place**. This is bounded and acceptable: `v1alpha1` is unreleased with no production-stored CRs (§9).

**Region parity (unchanged).** `decodeJob` continues to inject `job.Region = nc.Spec.Region`, and the client stays region-scoped via `clusterNomadConfig` (`api.Config.Region`). Namespace is orthogonal to region: it is the one dimension threaded per-call (§3.7 above), because a single region-scoped client serves every namespace.

---

## 4. Per-cluster client, `internal/nomad` additions & `contract.go` pins

**Client seam.** A new consumer-side interface — `NomadNamespaceOps` — defined in the controller package; no existing interface (`NomadOps`/`NomadNodeOps`/`NomadPoolOps`/`NomadJobOps`) is widened:

```go
// NomadNamespaceOps is the namespace reconciler's consumer interface.
type NomadNamespaceOps interface {
    GetNamespace(ctx context.Context, name string) (*api.Namespace, error)  // Info; nil,nil on 404
    UpsertNamespace(ctx context.Context, ns *api.Namespace) error           // Register
    DeleteNamespace(ctx context.Context, name string) error                 // Delete
    CountNamespaceJobs(ctx context.Context, name string) (int, error)       // Jobs().List scoped to ns
}
```

Built by a `NewNomadNamespaceClient` factory (faked in envtest); config via the existing `clusterNomadConfig` helper (DRY).

**`NomadJobOps` (existing) namespace change (§3.7, SGE I-2):** three methods gain an explicit `namespace` parameter (`GetJob`/`DeregisterJob`/`JobGroupSummary`); the two that take a full `*api.Job` (`PlanJob`/`RegisterJob`) derive it from `job.Namespace` with **no signature change**. Its fake is updated accordingly.

**Additive `internal/nomad.Client` methods**, each backed by a real `api` call:
- `GetNamespace` (`Namespaces().Info`; `(nil,nil)` on 404 via the existing `IsNotFound`/`errors.AsType` pattern).
- `UpsertNamespace` (`Namespaces().Register`).
- `DeleteNamespace` (`Namespaces().Delete`).
- `CountNamespaceJobs` (`Jobs().List` with `QueryOptions.Namespace = name`, returns `len`).
- `GetJob`/`DeregisterJob`/`JobGroupSummary` gain a `namespace` argument setting `Query/WriteOptions.Namespace`; `PlanJob`/`RegisterJob` set `WriteOptions.Namespace` from `job.Namespace` (nil-guarded — SGE M-1).

**New error helper** in `internal/nomad/errors.go`: `IsNamespaceNotEmpty(err) bool` — `errors.AsType[api.UnexpectedResponseError]` + body-substring match on Nomad's namespace-delete refusal wording (exact string a plan-time spike, §6), mirroring `IsNodePoolNotEmpty`.

### 4.1 `contract.go` additions (backed by real calls)

Pin rule (from Foundation): only pin symbols a real call exercises.
- **Accessor pin:** `(*api.Client).Namespaces`.
- **Method pins:** `(*api.Namespaces).Info`, `.Register`, `.Delete`; `(*api.Jobs).List` (namespace-scoped job count).
- **Type pins:** `api.Namespace` (named in `GetNamespace`/`UpsertNamespace`), `api.JobListStub` (named reading `Jobs().List` for the count).
- **Not pinned:** `Namespaces().List`/`.PrefixList` (unused), `api.NamespaceCapabilities`/`NamespaceVaultConfiguration`/etc. (preserved as opaque struct fields, never named).

**`config/crd/kustomization.yaml`.** Manually add `- bases/nomad.operator.io_nomadnamespaces.yaml` to `resources:` (the slice-3 `6c3e0c1` lesson — `controller-gen` regenerates the base but not the list).

**`cmd/main.go`.** Register the `NomadNamespaceReconciler`.

---

## 5. Explicitly not built (YAGNI)

- **Cross-namespace `clusterRef` / Kubernetes→Nomad passthrough tenancy** (§1.3) — a larger architectural change with RBAC implications; additive later. Nothing here forecloses it.
- **Managing `Quota`, `Capabilities`, `NodePoolConfiguration`, Vault/Consul config, extra-claims** — preserved via read-modify-write, not owned in v1. Additive as new `spec` fields feeding the same `Register`.
- **A hard `NomadJob.spec.namespaceRef` dependency + `NamespaceNotReady` wait state** — by-name coupling only (§3.7).
- **ACL policies/roles/tokens per namespace** — a separate concern/CRD.
- **A field indexer for conflict detection** — plain `List` suffices (NomadPool precedent).

---

## 6. Open items / spikes (resolve at plan/implementation time)

These follow the project's "pin against real calls" discipline (the NomadPool I-2 spike pattern):

1. **Namespace-delete refusal wording** — the exact v2.0.4 error body when `Namespaces().Delete` refuses a namespace holding non-terminal jobs, so `IsNamespaceNotEmpty` matches by substring. Resolved in the §8 integration spike.
2. **Reserved-name set** — confirm `default` is the only server-reserved / unmanageable / undeletable namespace name (and that `*`/`all` carry no namespace meaning — SGE M-3), for the CEL + Go guard.
3. **Namespace-name regex** — confirm Nomad v2.0.4's exact validation pattern for the `namespaceName` CEL rule (server-side in `structs`, so a plan-time item like the jobID regex).
4. **Namespace-scoped job count** — confirm `Jobs().List` with `QueryOptions.Namespace` returns that namespace's jobs (for `jobCount`). Note (SGE I-1): the namespace wildcard `*` spans **namespaces** and does **not** filter job state — `Jobs().List` returns terminal jobs too, so decide separately whether `jobCount` should filter non-terminal `JobListStub.Status` (a `Status` check, orthogonal to namespace scoping) or stay a raw total.
5. **`Register` create-vs-update dedup** — whether a read-modify-write skip is sufficient or a Register of an unchanged namespace churns `ModifyIndex` (compare-before-write already guards this; confirm in the spike).

---

## 7. Definition of Done

- `NomadNamespace` CRD + reconciler and the `NomadJob` threading implemented; `make manifests generate fmt vet` and `make test` green (zero regen drift).
- Creating a `NomadNamespace` `Register`s the namespace onto Nomad; `kubectl get nomadnamespaces` shows `CLUSTER`/`NAMESPACE`/`READY`; editing `description`/`meta` re-`Register`s only on drift; out-of-band `Quota`/config is preserved.
- Two CRs with the same `namespaceName` on one cluster → `NamespaceNameConflict` + Warning, no `Register`; a `Terminating` same-name sibling does not block a replacement.
- `namespaceName: default` is rejected (CEL + Go guard).
- Deleting a `NomadNamespace` while it still holds non-terminal jobs holds in `Terminating` with `DeleteBlocked=NamespaceNotEmpty` + `status.jobCount`; once empty it `Delete`s and completes; already-absent → completes; cluster unreachable → holds with `DeleteBlocked=ClusterNotReady`.
- Under **both** background and foreground cascade, deleting the `NomadCluster` cascade-deletes its `NomadNamespace` CRs without a stuck `Terminating`.
- A `NomadJob` with `spec.nomadNamespace: <non-default>` (with the namespace present) registers into, reports status from, and on delete deregisters from **that** namespace — **no orphan**; a blob `namespace` disagreeing with `spec.nomadNamespace` → `NamespaceMismatch`; `spec.nomadNamespace` is immutable (CEL) and defaults to `default`.
- `contract.go` compiles against the pinned `api` with every new pin backed by a real call.
- envtest coverage (both reconcilers, fakes) per §8; runbook section added; `config/crd/kustomization.yaml` lists the base; `cmd/main.go` wires the reconciler.

---

## 8. Testing

- **Unit** (`internal/nomad`): each new namespace method's argument mapping; `GetNamespace` 404 → `(nil,nil)`; `IsNamespaceNotEmpty` against the pinned error shape; the four `*Job` methods set `Query/WriteOptions.Namespace`. Plus a `decodeJob` test: `spec.nomadNamespace` injected into `job.Namespace`; blob-namespace mismatch → `NamespaceMismatch`; unset defaults to `default`.
- **envtest** (`internal/controller`):
  - `NomadNamespace`: inject a fake `NomadNamespaceOps`; assert upsert-on-drift/skip-when-synced, read-modify-write preservation, `NamespaceNameConflict`, reserved-name rejection, finalizer success / already-gone / **not-empty-blocked** / cluster-gone-or-going short-circuit (both cascade modes) / cluster-unreachable-blocked, status `jobCount`.
  - `NomadJob`: inject a fake asserting the **namespace argument** reaches `GetJob`/`PlanJob`/`DeregisterJob`/`JobGroupSummary`; a non-default job reconciles Ready and its finalizer Deregisters in the right namespace (the orphan-regression test).
- **Integration** (`-tags integration`, hermetic, real Nomad v2.0.4): create a namespace via `UpsertNamespace`; register a job into it; attempt `DeleteNamespace` while the job is non-terminal → confirm the refusal is matched by `IsNamespaceNotEmpty` (closes spike §6.1); deregister the job; `DeleteNamespace` succeeds. Reuses the containerized harness from slices 3–5. Live run deferred if no `nomad` binary is present.

---

## 9. Reconcile with the roadmap

- **Fifth managed-lifecycle CRD**, completing Cluster → Node → Pool → Job → **Namespace**. Reuses `clusterNomadConfig` + the NomadPool finalizer/conflict/read-modify-write shapes; widens no existing interface.
- **Supersedes** the interim "reject non-default namespace" fix that slice-6 hardening had scoped; the two remaining live-e2e / hardening workstreams stay their own later slices: NomadCluster restart resilience (per-pod stable ClusterIP advertise) and the envtest/backlog cleanup.
- **`v1alpha1` still unreleased:** the new CRD and the additive `NomadJob.spec.nomadNamespace` field ship without a conversion webhook.
- **Relates to slice 4:** namespaces and node pools are Nomad-API siblings; this CRD is the namespace analogue of `NomadPool`, and a job's `nomadNamespace` (this slice) and `nodePool` (slice 4) are independent by-name partitions.
