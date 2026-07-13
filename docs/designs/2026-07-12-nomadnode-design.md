# NomadNode — Design

**Type:** design · **Date:** 2026-07-12 (amended 2026-07-12 after SGE review) · **Status:** proposed
**Feature:** slice 3 — a `NomadNode` CRD that represents a registered edge client of a `NomadCluster` and gives the user a control surface over its scheduling eligibility and drain. Includes a companion optional `NomadClusterSpec.nodeGCThreshold` field.

Follows slice 2 (NomadCluster HA control plane, merged `a1e4d6a`), FR-1 (single-node `servers: 1`, `349f5cc`), and the external-access-modes follow-up (merged `b91df1a`). This is the first CRD that *manages* an existing cluster rather than *provisioning* one.

> **Amended 2026-07-12 (twice).** First after an independent sr-go-engineer *design* review (Fable), then again after an independent sr-go-engineer *plan* review (Fable) surfaced three design-level refinements folded back here: (§3.3) drain issuance is gated to **at most once per `spec.drain` generation** — re-issuing a still-running drain slides its relative deadline forever, so "in progress at this generation" is now a no-op, not a re-issue; (§3.1) `spec.drain.deadline` is a **`*metav1.Duration`** so an explicit `0` (no-deadline) is distinguishable from unset (→1h), and `spec.eligible` is **non-`omitempty`** so a seeded `false` is not clobbered by its `default=true`; (§3.4) hand-authored CRs are **out of scope for v1** and `NodeNotFound` is deferred, while `ClusterNotReady` is implemented on the cluster's nodes.
>
> **Original amendment,** verdict *amend-before-planning*. The review verified the core representation model, the eligibility/drain composition at terminal states, the prune rules, the W5 retirement, and the YAGNI cuts as sound; the amendments fix targeted correctness gaps it surfaced — drain-completion convergence (§3.3, was a re-drain-forever bug), re-registration duplicate-Name handling (§3.2), the cluster-keyed reflector contract (§3.2, the previous `For(&NomadNode{})` shape could not mint first CRs), the `NodeListStub`-has-no-`DrainStrategy` seed fix (§3.2), the `clientFor` shared-config extraction (§4, "verbatim reuse" was impossible), CR-name sanitization (§3.1/§3.2), an ownerReference for cluster-delete GC (§3.1/§3.4), and dropping the meta-mirror contradiction (§7). `nodeGCThreshold` is now **optional with no default** (§5). Every domain claim below is grounded in `go doc` against the pinned api or Nomad docs.

---

## 1. Background & framing

Nomad clients are **edge machines** (e.g. TrueNAS) that run only a container runtime. They *self-register* with a `NomadCluster`'s control plane over mTLS RPC (the external join surface built in slice 2). Once registered, a client is a row in the servers' node table — reachable and mutable only through the servers' HTTP API, never dialed directly by the operator.

The operator therefore models a node as a Kubernetes-native **representation**, not a lifecycle object. Creating or deleting the CR does not create or destroy the machine; the machine's existence is owned by the edge device joining Nomad.

### 1.1 The Kubernetes `Node` precedent (the model this rests on)

`NomadNode` is deliberately shaped like the built-in Kubernetes `Node` object. You never `kubectl create node` — the kubelet self-registers it — and you don't destroy a machine by deleting the object; but you *do* update it (`kubectl cordon` sets `spec.unschedulable`, `kubectl drain`, add taints). CRUD is split by owner:

| Verb | Owner | Trigger |
|------|-------|---------|
| **C**reate | operator (reflector) | node appears in Nomad's node list → mirrored into a CR |
| **R**ead | user | `kubectl get nomadnodes` — live fleet inventory + status |
| **U**pdate | user | set `spec.eligible` / `spec.drain` → operator drives it onto Nomad |
| **D**elete | operator | node garbage-collected from Nomad → CR pruned |

Field ownership is partitioned — the operator owns *existence + status*, the user owns the *control knobs*, the operator reconciles those knobs onto Nomad — so the two writers never conflict. When the reflector first mints a CR it **seeds `spec.eligible`/`drain` from the observed Nomad state**, so a freshly-mirrored node is born matching reality rather than asserting a stale desired state.

### 1.2 Nomad node facts this design rests on (verified against the pinned `api`)

All confirmed against `github.com/hashicorp/nomad/api` at the pinned commit (`5b83b133998a` == v2.0.4) via `go doc`, and where noted against real Nomad v2.0.4:

- **Node identity is ephemeral, Name is stable.** `Node.ID` is a UUID regenerated when a client re-registers (reinstall, data-dir wipe); `Node.Name` is client-configured and stable across re-registration. Nomad does **not** guarantee Name uniqueness. Critically, Nomad keys nodes by **ID**: a re-registered box's *old* record persists in the node list as `down` until garbage-collected, so a single list can transiently contain two same-Name entries (one `down`, one live) — see §3.2.
- **Status axis** = `Node.Status` ∈ `initializing | ready | down | disconnected` (the raw constant `NodeStatusInit == "initializing"`; observed value on a healthy real v2.0.4 node = `ready`), plus `Node.SchedulingEligibility` ∈ `eligible | ineligible` and `Node.Drain` (bool) / `Node.DrainStrategy` / `Node.LastDrain` (`DrainMetadata{StartedAt, UpdatedAt, Status, AccessorID, Meta}`, where `DrainStatus` ∈ `draining | complete | canceled`).
- **`Nodes().List()` stubs carry the status-mirror fields but not the drain *spec*.** `NodeListStub` has `ID, Name, Status, StatusDescription, SchedulingEligibility, Drain (bool), NodeClass, NodePool, Datacenter, LastDrain, CreateIndex` — but **no `DrainStrategy`** and **no `Meta`** map (only `Attributes`). So the status mirror is one `List` per cluster; reading the *active drain spec* or node `meta` requires `Nodes().Info` (see §3.2 seed rule, §7).
- **Server-mutable control knobs** (the only ones the server API can set): `Nodes().ToggleEligibility(nodeID string, eligible bool, q *WriteOptions)`; `Nodes().UpdateDrain(nodeID string, spec *DrainSpec, markEligible bool, q *WriteOptions)` where `DrainSpec = { Deadline, IgnoreSystemJobs }` and a **nil** spec *cancels* the drain (`markEligible` applies on cancel); `Nodes().Meta().Apply(...)` (dynamic metadata). `NodeClass`/`NodePool`/`Datacenter` are set in the *client's* config on the edge box and are **not** pushable through the server API — mirror-only.
- **Imperative node actions exist but are not desired-state**: `ForceEvaluate`, `GC`, `GcAlloc`, `Purge`, `Identity().Renew`. They do not fit a declarative `spec` and are excluded from this CRD (see §7).

### 1.3 Retiring ripsheet W5 (the "node introduction status" item is VOID)

`docs/design/idea.md` W5 asks to surface a Nomad 2.0 "node introduction/identity" *status* into `NomadNodeStatus.Status`. Verified against the pinned `api`: node "introduction/identity" in 2.0 is `Nodes().Identity().Get/Renew` — a **JWT node-identity token** issued and renewed by the ACL/auth subsystem — **not** a `Node.Status` lifecycle value. There is no pending-introduction status to map. W5's premise is retired: `NomadNode` mirrors the *real* status axis (§1.2) and nothing bogus. The `nomadnode_reflector.go` filename W5 anticipated survives in spirit as this slice's reflector loop.

---

## 2. Scope of this slice

**In scope:**
- The `NomadNode` CRD (`nomad.operator.io/v1alpha1`, namespaced).
- A cluster-keyed reflector reconciler that mirrors registered nodes into CRs and reconciles eligibility + drain onto Nomad.
- A small **shared per-cluster `nomad.Config` construction helper**, extracted from slice 2's `NomadClusterReconciler.clientFor` (the genuinely shared knowledge — endpoint/PEM/token/`TLSServerName`), called by both reconcilers (§4).
- Companion **optional** `NomadClusterSpec.nodeGCThreshold` field (slice-2 amendment, §5).
- Two additive `internal/nomad.Client` methods + `contract.go` pins, backed by real calls.
- envtest coverage with an injected fake Nomad client (as in slice 2) + a runbook.

**Out of scope (YAGNI; additive later — see §7):**
- `spec.meta` (dynamic node metadata) — not surfaced at all in v1 (not even mirrored; the stub carries no `Meta`).
- Imperative node actions (ForceEvaluate / GC / Purge / Identity renew).
- Surfacing `nodeClass` / `nodePool` as writable spec (not server-mutable).
- Node introduction/identity JWT management (a future ACL/auth concern, not a node-status one).

---

## 3. Design

### 3.1 CRD — `nomad.operator.io/v1alpha1`, kind `NomadNode` (namespaced)

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadNode
metadata:
  name: truenas-01              # SANITIZED node Name (RFC 1123); see below
  namespace: nomad-system       # same namespace as its NomadCluster
  ownerReferences:              # set by the operator to its NomadCluster (§3.4)
    - apiVersion: nomad.operator.io/v1alpha1
      kind: NomadCluster
      name: prod
spec:
  clusterRef:
    name: prod                  # NomadCluster in this namespace
  nodeName: truenas-01          # EXACT Nomad node Name this CR represents (match key)
  eligible: true                # +kubebuilder:default=true — eligibility when NOT draining
  drain:                        # OPTIONAL — presence = drain, absence = don't/cancel
    deadline: 1h                # *metav1.Duration; nil→1h default, explicit 0→no deadline
    ignoreSystemJobs: true      # maps to DrainSpec.IgnoreSystemJobs
status:
  nodeID: a1b2c3d4-…            # resolved ephemeral Nomad node ID
  status: ready                 # raw Node.Status: initializing|ready|down|disconnected
  schedulingEligibility: eligible
  draining: false               # Node.Drain
  drainObservedGeneration: 3    # generation at which the current drain was issued (§3.3)
  lastDrain:                    # summarized Node.LastDrain, when present
    status: complete            # DrainStatus: draining|complete|canceled
    startedAt: 2026-07-12T10:00:00Z
    updatedAt: 2026-07-12T10:04:00Z
  nodeClass: truenas            # mirror, read-only
  nodePool: default             # mirror, read-only
  datacenter: dc1               # mirror, read-only
  observedGeneration: 3
  conditions:
    - type: Reconciled          # operator drove spec.eligible/drain onto Nomad
      status: "True"
```

**Field-level decisions:**

- **CR key = the node `Name`, sanitized.** The exact Nomad `Name` lives in `spec.nodeName` and is the reflector's match key; `metadata.name` is a **sanitized** form (lowercased, illegal runes replaced, length-capped to RFC 1123 subdomain) because Nomad Names default to the client hostname and may contain characters illegal in a Kubernetes object name. Referencing by ID was rejected: a re-registered box would silently become a *different* `NomadNode`, orphaning the old one. The ephemeral `nodeID` lives in `status`, re-resolved from the `List` every reconcile. Post-sanitization name collisions are handled by the same disambiguation guard as duplicate Names (§3.2).
- **`clusterRef`** names a `NomadCluster` in the same namespace. `NomadNode` CRs are always co-located with their cluster (the reflector creates them there) and carry an **ownerReference to that `NomadCluster`** so Kubernetes GC cascades on cluster delete (§3.4).
- **`spec.eligible`** (`+kubebuilder:default=true`, **not** `omitempty`) is the eligibility target *when the node is not actively draining* (see §3.3). The default covers a hand-omitted field on create; because the field is non-`omitempty`, the operator's seeded value — including a deliberate `false` for an observed-ineligible node — is always sent on the wire and never clobbered by the default. The reflector seeds it from observed state at mint.
- **`spec.drain`** is an optional block mirroring `api.DrainSpec`. Presence ⇒ drain; absence ⇒ no drain (cancel any active drain). `deadline` is a **`*metav1.Duration`** (a pointer, so "unset" and an explicit `0` are distinguishable): `nil` ⇒ the operator substitutes the `1h` default (the `nomad node drain` CLI default); an explicit value is used verbatim, where `> 0` force-stops stragglers after that long and `0` drains gracefully with **no** deadline. The `-force`/immediate variant is deferred (would be an additive `force: bool` or negative-deadline encoding later).
- **No `phase` enum.** Nomad's own `status` field *is* the node-health axis; a parallel operator phase would duplicate it. The "desired applied" axis is the `Reconciled` condition instead (the ripsheet's two-axis Reconciled-vs-health split, §5 of `idea.md`).
- **Status style:** new `status` fields follow the existing `nomadcluster_types.go` conventions (`+optional`, `omitzero`).
- **Printer columns:** `NAME`, `CLUSTER` (`spec.clusterRef.name`), `STATUS` (`status.status`), `ELIGIBLE` (`status.schedulingEligibility`), `DRAINING` (`status.draining`), `AGE`.

**CEL validation:**
- `spec.nodeName` immutable (`self == oldSelf`) — it is the CR's node identity.
- `spec.clusterRef.name` immutable — a node belongs to one cluster.
- (No cross-field eligible/drain rule is needed — the composition in §3.3 makes them non-conflicting by construction.)

### 3.2 Reconciler — the cluster-keyed reflector loop

Because Nomad is **not** a Kubernetes watch source, reflection is a **poll loop**. The reconciler's *primary object is the `NomadCluster`*, not `NomadNode` — a `NomadNode`-keyed reconciler cannot discover a node that has no CR yet, so it could never mint the first CR. Concretely (`SetupWithManager`):

- `For(&NomadCluster{})` — a **second** controller on `NomadCluster` (controller-runtime permits multiple controllers on one type; the slice-2 reconciler keeps its own). Each reconcile pass handles *one cluster's whole node set*.
- `Watches(&NomadNode{}, → spec.clusterRef)` — maps a user's `spec` edit on any `NomadNode` back to its cluster key, so eligibility/drain changes reconcile promptly instead of waiting for the resync.
- `RequeueAfter: 30s` steady-state resync — the poll that catches nodes appearing/disappearing.

Each pass, for a cluster in `Ready` phase (an un-`Ready`/unreachable cluster is skipped — its nodes can't be listed):

1. **Build the client** via the shared per-cluster `nomad.Config` helper (§4) — in-cluster API Service, CA/client PEM from the cert Secret, token from the cluster's status bootstrap-token Secret, `TLSServerName=server.<region>.nomad`. No global singleton, no `api.DefaultConfig()`.
2. **List once.** `Nodes().List()` — one call, all stubs.
3. **Resolve identity per Name.** Group stubs by `Name`. Bind the CR to the unique **non-`down`** stub; if several non-`down` stubs share a Name, tie-break on the highest `CreateIndex`. Raise a `DuplicateNodeName` condition **only when two or more non-`down` stubs share a Name** (genuine ambiguity) — a `down` straggler from a re-registered box is *not* a duplicate. This is the fix for the re-registration case: a re-imaged node keeps being managed instead of freezing for up to the GC window.
4. **Upsert per bound node.** Upsert a `NomadNode` (named after the *sanitized* `stub.Name`, in the cluster's namespace, `clusterRef` + ownerReference set). On *first creation*, seed `spec.eligible` from `stub.SchedulingEligibility` and seed `spec.drain` *presence* from `stub.Drain`; because the stub carries no `DrainStrategy`, fetch `Nodes().Info` to populate `deadline`/`ignoreSystemJobs` **only** for a node that is actively draining at first mint (rare). On subsequent passes, **never overwrite `spec`** — only `status`.
5. **Mirror status** from the stub (`nodeID`, `status`, `schedulingEligibility`, `draining`, `lastDrain`, `nodeClass`, `nodePool`, `datacenter`).
6. **Reconcile desired → Nomad** for each node with a bound CR (§3.3), setting the `Reconciled` condition on success.
7. **Prune** CRs for this cluster whose Name is absent from *this successful* list (§3.4).

**Resync cadence:** default **30s** `RequeueAfter`. Not user-configurable in v1 (add later if a need appears).

### 3.3 Eligibility + drain composition (with convergence)

`spec.eligible` and `spec.drain` are independent user knobs whose interaction mirrors how `UpdateDrain`'s `markEligible` actually behaves — and, critically, drain must **converge** (a drain is a one-shot process; naive "drain desired ⇒ call UpdateDrain every pass" would re-issue forever, churning Raft and flapping status, because after completion the observed `DrainStrategy` is `nil` and never equals the desired spec).

The key insight is that `UpdateDrain` must be issued **at most once per `spec.drain` generation**. `DrainSpec.Deadline` is *relative to the drain's start time*, so re-issuing a still-running drain every resync restarts its clock — the deadline slides forever and never fires, on top of a Raft write per pass. So issuance is gated on the generation, not on whether the drain has finished. Define, for a `spec.drain` that is present:

> **already-handled-this-generation** ⇔ `status.drainObservedGeneration == metadata.generation` **and** the node is either *in progress* (`Node.Drain == true`) **or** *complete* (`Node.Drain == false && SchedulingEligibility == ineligible && LastDrain.Status == complete`).

Reconcile rule:

- **`spec.drain` present and already-handled-this-generation** → **no call** (in progress → converging; complete → converged).
- **`spec.drain` present and *not* already-handled** → `UpdateDrain(id, drainSpec, markEligible=false)`; record `status.drainObservedGeneration = metadata.generation`. This fires exactly once for a new/edited drain (a `deadline` edit bumps `metadata.generation`), and again if the drain was cancelled out-of-band (`Node.Drain == false` with `LastDrain.Status == canceled` matches neither in-progress nor complete → re-issued, spec wins). The node is forced **ineligible** while draining; `spec.eligible` is dormant during an active drain (the API enforces ineligibility).
- **`spec.drain` removed** (node was draining) → cancel with `UpdateDrain(id, nil, markEligible=spec.eligible)` → the node returns to the eligibility the user asked for.
- **No drain** → reconcile eligibility straight to `spec.eligible` via `ToggleEligibility(id, spec.eligible)` (compare-before-write; only call on mismatch).

So `spec.eligible` reads as *"eligibility when the node isn't actively draining,"* and drain transiently dominates it — matching `kubectl cordon` (eligible) vs `kubectl drain` (drain) as independent verbs.

**Documented behavior (spec-wins surprise):** because "`spec.drain` absent ⇒ cancel active drain," an operator who runs `nomad node drain -enable` out-of-band on a node whose CR has no `spec.drain` will see it un-drained within one resync. This is intentional (the CR is the source of truth) but non-obvious; it is called out here and in the runbook.

### 3.4 Lifecycle & prune

CR existence tracks Nomad's node registry:

- **Node down but still present** (client offline, not yet GC'd) → **keep the CR**, mirror `status.status: down`. This is the fleet-visibility value: the dead box stays visible in `kubectl get nomadnodes`.
- **Node absent from a *successful* `List`** (Nomad GC'd it) → **hard-delete the CR.** The representation tracks the real registry.
- **`List` *fails*** (cluster unreachable) → **prune nothing.** Absence only counts after a clean list. This is the safety rule that stops a transient control-plane blip from wiping every CR.
- **User `kubectl delete`s a CR** while the node still exists → operator **recreates it** on the next poll, re-seeded from observed state — exactly like deleting a Kubernetes `Node` object while the kubelet keeps running. Delete = "re-mirror from scratch," not "stop managing."
- **`NomadCluster` deleted** → every `NomadNode` carries an ownerReference to it, so Kubernetes garbage collection cascades and removes them. This closes the otherwise-permanent orphan (with the cluster gone, `List` never succeeds again, and the prune-nothing-on-error rule would leave the CRs stranded as `ClusterNotReady` forever). Transient unreachability is still handled by prune-nothing-on-error; *deletion* is handled by GC.

**Down-node retention window** = the cluster's effective `node_gc_threshold` (§5), automatically: because prune is driven by *absence from List*, not an operator-side timer, the CR for a down box lingers exactly as long as Nomad keeps the node before GC. No operator-side retention state is invented (that would be precisely the drift the representation model avoids).

**Degradation / conditions:**
- `clusterRef` not `Ready` / unreachable → the reflector sets `Reconciled=False, reason=ClusterNotReady` on each of that cluster's existing `NomadNode` CRs, and requeues; existing `status` is left stale (last-known), never wiped.
- `DuplicateNodeName` (§3.2) — two or more non-`down` stubs share a Name (after sanitization).

**Hand-authored CRs are out of scope for v1.** `NomadNode` is an operator-authored representation (you Read+Update; the reflector Creates+Deletes), so a user hand-writing a `NomadNode` that points at a nonexistent node is not a supported flow. The reflector reconciles only its own labelled CRs; a hand-authored CR is ignored (neither driven nor pruned). A `NodeNotFound`-style condition for that case is deliberately deferred (YAGNI) — add it only if hand-authoring turns out to be a real need.

**Teardown / finalizer: none.** Deleting a CR never touches the real node — prune-delete happens *because* the node is already gone, cascade-delete is Kubernetes GC, and a user-delete just re-mirrors next poll. There is no finalizer, and **deleting a `NomadNode` never drains or deregisters** the edge box. Draining is `spec.drain`; deregistering is Nomad's own GC — neither is tied to CR deletion.

---

## 4. Per-cluster Client construction & `internal/nomad` additions

Slice 2 builds a per-cluster client inside `NomadClusterReconciler.clientFor` (`nomadcluster_controller.go:180`), a **private** method returning the slice-2 `NomadOps` interface (`Ping/Leader/ServerHealthy/ACLBootstrap`) — none of which the node loop needs, so it cannot be reused verbatim. The genuinely shared knowledge is the per-cluster **`nomad.Config` construction** (endpoint, CA/client PEM from the cert Secret, token from the status Secret, `TLSServerName`). That is single-sourced (DRY / No-Wall):

- **Extract** the `nomad.Config` construction into one shared helper (e.g. a package function that takes the `NomadCluster` + the fetched cert/token Secrets and returns a `nomad.Config`), called by both `clientFor` and the new reconciler. This is a small, behavior-preserving refactor of merged slice-2 code.
- **`NomadNodeReconciler` defines its own** consumer-side ops interface (`ListNodes`, `NodeInfo`, `SetEligibility`, `UpdateDrain`) + factory, faked in envtest — following the repo's "interface defined at the consumer" convention. Slice-2's `NomadOps` is **not** widened (that would couple the two controllers' test seams).

Two additive `internal/nomad.Client` methods (`ListNodes`/`NodeInfo` already exist):

```go
// SetEligibility toggles scheduling eligibility for a node.
func (c *Client) SetEligibility(ctx context.Context, nodeID string, eligible bool) error
// UpdateDrain sets or cancels a node's drain. A nil spec cancels; markEligible
// applies on cancel.
func (c *Client) UpdateDrain(ctx context.Context, nodeID string, spec *api.DrainSpec, markEligible bool) error
```

Both wrap the real `api` calls and are exercised by the reconciler (and by unit/integration tests), satisfying the **existence-only-pin gotcha**: every newly-pinned symbol is backed by a real call, so signature drift breaks `go build`.

### 4.1 `contract.go` additions (backed by real calls)

The pin rule (from Foundation): only pin symbols the operator's own code names or a real call exercises.

- **Method pins** (each exercised by a real call in `Client.SetEligibility`/`UpdateDrain`): `(*api.Nodes).ToggleEligibility`, `(*api.Nodes).UpdateDrain`. Their signatures already cover the `NodeEligibilityUpdateResponse`/`NodeDrainUpdateResponse` return types.
- **Type pins** (named directly in our code): `api.DrainSpec` (`Client.UpdateDrain` parameter); `api.DrainMetadata` (read from `node.LastDrain` for `status.lastDrain`); `api.DrainStrategy` (read from `Nodes().Info` for the actively-draining-at-mint seed, §3.2 step 4).
- **Constant pins** (read by the drain-convergence predicate, §3.3): `api.DrainStatusComplete` (the only `DrainStatus` value the code names — the out-of-band-cancel path is the *absence* of in-progress/complete, so it reads no `Canceled` constant).
- **Already pinned** (Foundation/slice 2), reused here: `(*api.Nodes).List`, `(*api.Nodes).Info`, `api.Node`, `api.NodeListStub`, `api.NodeStatusInit/Ready/Down/Disconnected`, `api.NodeSchedulingEligible/Ineligible`.
- **Not pinned:** `Purge`, `ForceEvaluate`, `GC`, `Identity` — prune deletes the *Kubernetes CR* (never the Nomad node) and the imperative actions are out of scope. Pinning a symbol we don't call would reintroduce the existence-only-pin risk.

---

## 5. Companion slice-2 change — optional `NomadClusterSpec.nodeGCThreshold`

A small **optional** additive field on `NomadClusterSpec`, bundled here because the node-visibility model motivates it (the down-node retention window in §3.4):

- **Field:** `nodeGCThreshold` — `metav1.Duration`, **`+optional`, no default.** Rendered into the servers' `server { node_gc_threshold = "…" }` **only when set**. When unset, the operator emits nothing and Nomad uses its built-in default (**24h**).
- **No forced upgrade roll, no default-shift.** Because rendering is gated on the field being set, existing clusters' config bodies are unchanged on operator upgrade — no config-hash change, no StatefulSet roll, and no 24h→48h default shift. (This is the review-driven change from an earlier `default: 48h`.)
- **Mutable.** Setting or changing it rolls the StatefulSet through the existing config-hash pod annotation — no special handling.
- Validation: a duration-format pattern (confirmed at plan time). The exact setting name/default is verified against Nomad docs + the pinned api during planning (ripsheet §0).

The `NomadNode` reflector's prune window automatically tracks whatever Nomad's effective value is (§3.4), set or unset.

---

## 6. Reconcile with the roadmap & interactions

- **Depends on slice 2**: the per-cluster endpoint/token/cert wiring (now single-sourced via the §4 helper), and a `Ready` `NomadCluster` to list nodes from. The only slice-2 code changes are the additive optional `nodeGCThreshold` field + its gated rendering, and the behavior-preserving `nomad.Config`-construction extraction.
- **Enables slice 4 (`NomadPool`)**: pools resolve to node-ID sets; `NomadNode` gives a K8s-native, per-node handle those can reference. The pool→constraint model is unchanged and out of scope here.
- **`v1alpha1` still unreleased**: both the new CRD and the additive `NomadClusterSpec` field ship without a conversion webhook.

---

## 7. Explicitly not built (YAGNI)

- **`spec.meta` (dynamic node metadata).** A real server-mutable knob (`Nodes().Meta().Apply`) but orthogonal to the cordon/drain lifecycle that is this slice's point. Not surfaced at all in v1 — not even mirrored, since `NodeListStub` carries no `Meta` and mirroring would force an N+1 `Info` fan-out. Add `spec.meta` (and, if wanted, a mirrored `status.meta` via `Info`) additively when label-style scheduling control is actually needed.
- **Imperative actions** — `ForceEvaluate`, `GC`, `GcAlloc`, `Purge`, `Identity().Renew`. "Do X once" fights a declarative `spec`; the K8s-idiomatic home is a separate action/imperative resource, not this one. Left to the `nomad` CLI for now.
- **Writable `nodeClass` / `nodePool`** — set in the edge client's own config, not pushable via the server API. Mirror-only (from the `List` stub).
- **Configurable resync cadence, drain `force` mode, down-node tombstones** — deferred; each is additive.

---

## 8. Definition of Done

- `NomadNode` CRD + the cluster-keyed reflector reconciler implemented; `make manifests generate fmt vet` and `make test` green.
- A registered node is reflected into a `NomadNode` within one resync; `kubectl get nomadnodes` shows the fleet with correct `STATUS`/`ELIGIBLE`/`DRAINING`.
- Setting `spec.eligible: false` marks the node ineligible in Nomad; clearing it restores eligibility (compare-before-write; no redundant calls).
- Adding `spec.drain` drains the node (ineligible + migration) and then **converges** — no repeated `UpdateDrain` once `LastDrain.Status=complete`; removing it cancels with the right post-drain eligibility; editing `deadline` re-drains via the generation bump.
- A re-registered (same-Name, new-ID) node keeps being managed — no `DuplicateNodeName` freeze from a `down` straggler; `DuplicateNodeName` raised only for two-plus non-`down` same-Name stubs.
- CR names are sanitized to valid RFC 1123 and never fail to mint.
- A down node stays visible; a GC'd node's CR is pruned; a failed `List` prunes nothing; a user-deleted CR is recreated; deleting the `NomadCluster` cascades-deletes its `NomadNode` CRs.
- `NomadClusterSpec.nodeGCThreshold` renders into server config **only when set** (no roll on upgrade for existing clusters) and rolls the StatefulSet when set/changed.
- `contract.go` compiles against the pinned api with every new pin backed by a real call.
- envtest coverage (fake Nomad client) for: reflect/upsert, status mirror, eligibility reconcile, drain set/converge/cancel, re-registration disambiguation, prune-on-absence, prune-nothing-on-list-error, cascade-delete, name sanitization, cluster-not-ready. Runbook section added (incl. the spec-wins out-of-band-drain note).

---

## 9. Testing

- **Unit** (`internal/nomad`): `SetEligibility`/`UpdateDrain` argument mapping; drain cancel with `markEligible`.
- **envtest** (`internal/controller`): inject a fake Nomad client (the slice-2 `NewNomadClient` factory pattern) returning a scripted node list; assert reflect/upsert, status mirror, eligibility/drain reconcile **and drain convergence** (no re-issue after `complete`), the re-registration disambiguation (a `down` + a live same-Name stub binds to the live one), name sanitization, the four lifecycle rules (§3.4) incl. ownerRef cascade, `DuplicateNodeName`, and cluster-not-ready degradation. No real Nomad/pods needed.
- **Integration** (`-tags integration`, hermetic, real Nomad v2.0.4): register a node, toggle eligibility, drain and cancel, observe status + convergence — the same containerized method used to close Foundation open-item #1.

---

## 10. Open items / assumptions to verify at plan/implementation time

1. **`node_gc_threshold` exact name/default** — confirm against Nomad v2.0.4 docs + pinned api (assumed `server.node_gc_threshold`, built-in default `24h`) before rendering it (ripsheet §0).
2. **`DrainSpec.Deadline` zero/negative encoding** — confirm `0` = no-deadline and negative = force against real v2.0.4 before documenting the `deadline` field behavior.
3. **Generation-based drain issuance** — confirm the exact mechanism (persisting `status.drainObservedGeneration` and comparing to `metadata.generation`) behaves as intended across a spec edit and an out-of-band cancel; it is the load-bearing part of the §3.3 convergence fix.
4. **Name sanitization function** — pin the exact transform (lowercase, illegal-rune replacement, length cap, and post-sanitization collision behavior feeding the §3.2 `DuplicateNodeName` guard).
5. **Same-namespace `clusterRef`** — assumed same-namespace only (reflector creates CRs in the cluster's namespace, ownerRef requires same namespace); no cross-namespace reference in v1.
6. **Two controllers on `NomadCluster`** — confirm controller-runtime cleanly supports the slice-2 reconciler and the node reflector both using `For(&NomadCluster{})` (separate `Named(...)` controllers, independent workqueues) in `SetupWithManager`.
