# NomadNode — Design

**Type:** design · **Date:** 2026-07-12 · **Status:** proposed
**Feature:** slice 3 — a `NomadNode` CRD that represents a registered edge client of a `NomadCluster` and gives the user a control surface over its scheduling eligibility and drain. Includes a companion `NomadClusterSpec.nodeGCThreshold` field.

Follows slice 2 (NomadCluster HA control plane, merged `a1e4d6a`), FR-1 (single-node `servers: 1`, `349f5cc`), and the external-access-modes follow-up (merged `b91df1a`). This is the first CRD that *manages* an existing cluster rather than *provisioning* one.

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

- **Node identity is ephemeral, Name is stable.** `Node.ID` is a UUID regenerated when a client re-registers (reinstall, data-dir wipe); `Node.Name` is client-configured and stable across re-registration. Nomad does **not** guarantee Name uniqueness.
- **Status axis** = `Node.Status` ∈ `init | ready | down | disconnected` (observed value on real v2.0.4 = `ready`), plus `Node.SchedulingEligibility` ∈ `eligible | ineligible` and `Node.Drain` (bool) / `Node.DrainStrategy` / `Node.LastDrain`.
- **`Nodes().List()` stubs carry everything the mirror needs** — `NodeListStub` has `ID, Name, Status, StatusDescription, SchedulingEligibility, Drain, NodeClass, NodePool, Datacenter, LastDrain`. Reflection is therefore **one `List` call per cluster per resync**, no N+1 `Info` fan-out.
- **Server-mutable control knobs** (the only ones the server API can set): `Nodes().ToggleEligibility(id, bool)`; `Nodes().UpdateDrain(id, *DrainSpec, markEligible)` where `DrainSpec = { Deadline, IgnoreSystemJobs }`; `Nodes().Meta().Apply(...)` (dynamic metadata). `NodeClass`/`NodePool`/`Datacenter` are set in the *client's* config on the edge box and are **not** pushable through the server API — mirror-only.
- **Imperative node actions exist but are not desired-state**: `ForceEvaluate`, `GC`, `GcAlloc`, `Purge`, `Identity().Renew`. They do not fit a declarative `spec` and are excluded from this CRD (see §7).

### 1.3 Retiring ripsheet W5 (the "node introduction status" item is VOID)

`docs/design/idea.md` W5 asks to surface a Nomad 2.0 "node introduction/identity" *status* into `NomadNodeStatus.Status`. Verified against the pinned `api`: node "introduction/identity" in 2.0 is `Nodes().Identity().Get/Renew` — a **JWT node-identity token** issued and renewed by the ACL/auth subsystem — **not** a `Node.Status` lifecycle value. There is no pending-introduction status to map. W5's premise is retired: `NomadNode` mirrors the *real* status axis (§1.2) and nothing bogus. The `nomadnode_reflector.go` filename W5 anticipated survives in spirit as this slice's reflector loop.

---

## 2. Scope of this slice

**In scope:**
- The `NomadNode` CRD (`nomad.operator.io/v1alpha1`, namespaced).
- A `NomadNodeReconciler` that reflects registered nodes into CRs and reconciles eligibility + drain onto Nomad, reusing the per-cluster `clientFor` seam.
- Companion `NomadClusterSpec.nodeGCThreshold` field (slice-2 amendment, §6).
- Two additive `internal/nomad.Client` methods + `contract.go` pins, backed by real calls.
- envtest coverage with an injected fake Nomad client (as in slice 2) + a runbook.

**Out of scope (YAGNI; additive later — see §7):**
- `spec.meta` (dynamic node metadata) — mirrored read-only now, made writable later if wanted.
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
  name: truenas-01              # named after the Nomad node Name
  namespace: nomad-system       # same namespace as its NomadCluster
spec:
  clusterRef:
    name: prod                  # NomadCluster in this namespace
  nodeName: truenas-01          # the stable node Name this CR represents
  eligible: true                # eligibility target when NOT actively draining
  drain:                        # OPTIONAL — presence = drain, absence = don't/cancel
    deadline: 1h                # default 1h; 1:1 with api.DrainSpec.Deadline
    ignoreSystemJobs: true      # 1:1 with api.DrainSpec.IgnoreSystemJobs
status:
  nodeID: a1b2c3d4-…            # resolved ephemeral Nomad node ID
  status: ready                 # raw Node.Status: init|ready|down|disconnected
  schedulingEligibility: eligible
  draining: false               # Node.Drain
  lastDrain:                    # summarized Node.LastDrain, when present
    status: complete
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

- **CR key = node `Name`.** The CR is named after the node Name (stable, human-readable). The ephemeral `nodeID` lives in `status` and is re-resolved from the `List` every reconcile. Referencing by ID was rejected: a re-registered box would silently become a *different* `NomadNode`, orphaning the old one.
- **`clusterRef`** names a `NomadCluster` in the same namespace. `NomadNode` CRs are always co-located with their cluster (the reflector creates them there).
- **`spec.eligible`** is the eligibility target *when the node is not actively draining* (see §3.3).
- **`spec.drain`** is an optional block mirroring `api.DrainSpec` 1:1. Presence ⇒ drain; absence ⇒ no drain (cancel any active drain). `deadline` defaults to `1h` (the `nomad node drain` CLI default). `deadline > 0` force-stops stragglers after that long; `deadline: 0` drains gracefully with no deadline. The `-force`/immediate variant is deferred (would be an additive `force: bool` or negative-deadline encoding later).
- **No `phase` enum.** Nomad's own `status` field *is* the node-health axis; a parallel operator phase would duplicate it. The "desired applied" axis is the `Reconciled` condition instead (the ripsheet's two-axis Reconciled-vs-health split, §5 of `idea.md`).
- **Printer columns:** `NAME`, `CLUSTER` (`spec.clusterRef.name`), `STATUS` (`status.status`), `ELIGIBLE` (`status.schedulingEligibility`), `DRAINING` (`status.draining`), `AGE`.

**CEL validation:**
- `spec.nodeName` immutable (`self == oldSelf`) — it is the CR's identity; changing it would mean a different node.
- `spec.clusterRef.name` immutable — a node belongs to one cluster.
- (No cross-field eligible/drain rule is needed — the composition in §3.3 makes them non-conflicting by construction.)

### 3.2 Reconciler — the reflector loop

A new `NomadNodeReconciler`. Because Nomad is **not** a Kubernetes watch source, reflection is a **poll loop**, driven per `NomadCluster` that is `Ready`:

1. **Enumerate clusters.** For each `NomadCluster` in `Ready` phase, obtain a per-cluster client via the existing **`clientFor(cluster)`** seam (in-cluster API Service `https://…:4646`, CA/client PEM from the cert Secret, token from the cluster's status bootstrap-token Secret, `TLSServerName=server.<region>.nomad`). No global singleton, no `api.DefaultConfig()`. An un-`Ready`/unreachable cluster is simply not reflected — its nodes can't be listed.
2. **List once.** `Nodes().List()` — one call, all stubs.
3. **Upsert per node.** For each stub, upsert a `NomadNode` (named after `stub.Name`, in the cluster's namespace, `clusterRef` set). On *first creation*, seed `spec.eligible` from `stub.SchedulingEligibility` and `spec.drain` from `stub.Drain`/`DrainStrategy` (born matching reality). On subsequent passes, **never overwrite `spec`** — only `status`.
4. **Mirror status.** Populate `status` from the stub (`nodeID`, `status`, `schedulingEligibility`, `draining`, `lastDrain`, `nodeClass`, `nodePool`, `datacenter`).
5. **Reconcile desired → Nomad.** Drive `spec.eligible`/`spec.drain` onto the node (§3.3), setting the `Reconciled` condition on success.

**Resync cadence:** default **30s** `RequeueAfter` (steady-state). Not user-configurable in v1 (add later if a need appears).

**Reflection trigger wiring:** the reconciler watches `NomadNode` (`For`), and maps `NomadCluster` events to a re-list of that cluster's nodes (`Watches` on `NomadCluster` → enqueue). A periodic resync provides the steady poll for nodes appearing/disappearing between `NomadCluster` events. (Exact watch/enqueue mechanics — including whether a lightweight per-cluster lister requeue is cleaner than mapping every node — are a plan-time detail; the loop's *contract* is what this section fixes.)

**`DuplicateNodeName` safety:** if a single `List` returns two stubs with the same `Name`, the reflector refuses to guess — it sets a `DuplicateNodeName` condition and does not bind the CR to a random node. (A no-op in a fleet with unique names.)

### 3.3 Eligibility + drain composition

`spec.eligible` and `spec.drain` are independent user knobs whose interaction mirrors how `UpdateDrain`'s `markEligible` actually behaves — so they never fight:

- **`spec.drain` present** → `UpdateDrain(id, drainSpec, markEligible=false)`. The node is forced **ineligible** while draining and allocations migrate. `spec.eligible` is *dormant* during an active drain (the API enforces ineligibility, not the operator).
- **`spec.drain` removed** (was draining) → cancel the drain with `UpdateDrain(id, nil, markEligible=spec.eligible)` → the node returns to the eligibility the user asked for.
- **No drain** → reconcile eligibility straight to `spec.eligible` via `ToggleEligibility(id, spec.eligible)`.

So `spec.eligible` reads as *"eligibility when the node isn't actively draining,"* and drain transiently dominates it. This matches `kubectl cordon` (eligible) vs `kubectl drain` (drain) being independent verbs. No CEL conflict rule, no silent-override surprise.

### 3.4 Lifecycle & prune

CR existence tracks Nomad's node registry:

- **Node down but still present** (client offline, not yet GC'd) → **keep the CR**, mirror `status.status: down`. This is the fleet-visibility value: the dead box stays visible in `kubectl get nomadnodes`.
- **Node absent from a *successful* `List`** (Nomad GC'd it) → **hard-delete the CR.** The representation tracks the real registry.
- **`List` *fails*** (cluster unreachable) → **prune nothing.** Absence only counts after a clean list. This is the safety rule that stops a transient control-plane blip from wiping every CR.
- **User `kubectl delete`s a CR** while the node still exists → operator **recreates it** on the next poll, re-seeded from observed state — exactly like deleting a Kubernetes `Node` object while the kubelet keeps running. Delete = "re-mirror from scratch," not "stop managing."

**Down-node retention window** = the cluster's `node_gc_threshold` (§6), automatically: because prune is driven by *absence from List*, not an operator-side timer, the CR for a down box lingers exactly as long as Nomad keeps the node before GC. No operator-side retention state is invented (that would be precisely the drift the representation model avoids).

**Degradation / conditions:**
- `clusterRef` not `Ready` / unreachable → `Reconciled=False, reason=ClusterNotReady`; requeue; existing `status` left stale (last-known), never wiped.
- Node not in a successful `List` for a *reflector-created* CR → the node was GC'd → prune (above). The standing `NodeNotFound` condition only applies to the edge case of a hand-authored CR pointing at a name Nomad has never listed.
- `DuplicateNodeName` (§3.2).

**Teardown / finalizer: none.** Deleting a CR never touches the real node — prune-delete happens *because* the node is already gone, and a user-delete just re-mirrors next poll. There is no finalizer, and **deleting a `NomadNode` never drains or deregisters** the edge box. Draining is `spec.drain`; deregistering is Nomad's own GC — neither is tied to CR deletion.

---

## 4. Per-cluster Client reuse & `internal/nomad` additions

The reconciler reuses slice 2's `clientFor` seam verbatim — the same per-cluster construction (endpoint, PEM material, token, `TLSServerName`) already used by the `NomadCluster` bootstrap path. This slice adds **two** methods to `internal/nomad.Client` and the matching `NomadOps`-style consumer interface:

```go
// SetEligibility toggles scheduling eligibility for a node.
func (c *Client) SetEligibility(ctx context.Context, nodeID string, eligible bool) error
// UpdateDrain sets or cancels a node's drain. A nil spec cancels; markEligible
// applies on cancel.
func (c *Client) UpdateDrain(ctx context.Context, nodeID string, spec *api.DrainSpec, markEligible bool) error
```

`ListNodes`/`NodeInfo` already exist. Both new methods wrap the real `api` calls and are exercised by the reconciler (and by unit/integration tests), satisfying the **existence-only-pin gotcha**: every newly-pinned symbol is backed by a real call, so signature drift breaks `go build`.

### 4.1 `contract.go` additions (backed by real calls)

The pin rule (from Foundation): only pin symbols the operator's own code names or a real call exercises — never a bare pin for a symbol we don't touch, which reintroduces the existence-only-pin risk.

- **Method pins** (each exercised by a real call in `Client.SetEligibility`/`UpdateDrain`): `(*api.Nodes).ToggleEligibility`, `(*api.Nodes).UpdateDrain`. Their signatures already cover the `NodeEligibilityUpdateResponse`/`NodeDrainUpdateResponse` return types and the `*DrainSpec` parameter — no separate pin needed for those.
- **Type pins** (named directly in our code): `api.DrainSpec` (`Client.UpdateDrain`'s parameter type); `api.DrainMetadata` (read from `node.LastDrain` when summarizing `status.lastDrain`).
- **Already pinned** (Foundation/slice 2), reused here: `(*api.Nodes).List`, `(*api.Nodes).Info`, `api.Node`, `api.NodeListStub`, `api.NodeStatusInit/Ready/Down/Disconnected`, `api.NodeSchedulingEligible/Ineligible`.
- **Not pinned:** `Purge`, `ForceEvaluate`, `GC`, `Identity`, `DrainStrategy` — prune deletes the *Kubernetes CR* (never the Nomad node); the imperative actions are out of scope; and the mirror uses `stub.Drain`/`stub.LastDrain`, not `Node.DrainStrategy`. Pinning any of them would be a symbol we don't call.

---

## 5. Companion slice-2 change — `NomadClusterSpec.nodeGCThreshold`

A small additive field on `NomadClusterSpec`, bundled here because the node-visibility model motivates it (present consumer: the down-node retention window in §3.4):

- **Field:** `nodeGCThreshold` — Go duration string, **default `"48h"`**, rendered into the servers' `server { node_gc_threshold = "48h" }`.
- **Mutable** (unlike `servers`/`rpcPorts`). Changing it rolls the StatefulSet through the existing config-hash pod annotation — no special handling.
- **Deliberate behavior change:** today the operator emits no `node_gc_threshold`, so Nomad falls back to its built-in **24h**. Adding this field with a `48h` default shifts the effective default **24h → 48h** for every cluster. Intentional; acceptable while `v1alpha1` is unreleased.
- Validation: a duration-format pattern (confirmed at plan time). The exact setting name/default is verified against Nomad docs + the pinned api during planning (ripsheet §0).

The `NomadNode` reflector's prune window automatically tracks whatever this is set to (§3.4).

---

## 6. Reconcile with the roadmap & interactions

- **Depends on slice 2**: the `clientFor` seam, the per-cluster endpoint/token/cert wiring, and a `Ready` `NomadCluster` to list nodes from. No changes to slice-2 reconcile beyond the additive `nodeGCThreshold` field + its config rendering.
- **Enables slice 4 (`NomadPool`)**: pools resolve to node-ID sets; `NomadNode` gives a K8s-native, per-node handle those can reference. The pool→constraint model is unchanged and out of scope here.
- **`v1alpha1` still unreleased**: both the new CRD and the additive `NomadClusterSpec` field ship without a conversion webhook.

---

## 7. Explicitly not built (YAGNI)

- **`spec.meta` (dynamic node metadata).** A real server-mutable knob (`Nodes().Meta().Apply`) but orthogonal to the cordon/drain lifecycle that is this slice's point. Mirrored read-only into status now; add `spec.meta` additively when label-style scheduling control is actually wanted.
- **Imperative actions** — `ForceEvaluate`, `GC`, `GcAlloc`, `Purge`, `Identity().Renew`. "Do X once" fights a declarative `spec`; the K8s-idiomatic home is a separate action/imperative resource, not this one. Left to the `nomad` CLI for now.
- **Writable `nodeClass` / `nodePool`** — set in the edge client's own config, not pushable via the server API. Mirror-only.
- **Configurable resync cadence, drain `force` mode, down-node tombstones** — deferred; each is additive.

---

## 8. Definition of Done

- `NomadNode` CRD + `NomadNodeReconciler` implemented; `make manifests generate fmt vet` and `make test` green.
- A registered node is reflected into a `NomadNode` within one resync; `kubectl get nomadnodes` shows the fleet with correct `STATUS`/`ELIGIBLE`/`DRAINING`.
- Setting `spec.eligible: false` marks the node ineligible in Nomad; clearing it restores eligibility.
- Adding `spec.drain` drains the node (ineligible + migration); removing it cancels with the right post-drain eligibility.
- A down node stays visible; a GC'd node's CR is pruned; a failed `List` prunes nothing; a user-deleted CR is recreated.
- `NomadClusterSpec.nodeGCThreshold` renders into server config (default `48h`) and rolls the StatefulSet on change.
- `contract.go` compiles against the pinned api with every new pin backed by a real call.
- envtest coverage (fake Nomad client) for: reflect/upsert, eligibility reconcile, drain set/cancel, prune-on-absence, prune-nothing-on-list-error, duplicate-name, cluster-not-ready. Runbook section added.

---

## 9. Testing

- **Unit** (`internal/nomad`): `SetEligibility`/`UpdateDrain` argument mapping; drain cancel with `markEligible`.
- **envtest** (`internal/controller`): inject a fake Nomad client (the slice-2 `NewNomadClient` factory pattern) that returns a scripted node list; assert reflect/upsert, status mirror, eligibility/drain reconcile, the four lifecycle rules (§3.4), duplicate-name, and cluster-not-ready degradation. No real Nomad/pods needed.
- **Integration** (`-tags integration`, hermetic, real Nomad v2.0.4): register a node, toggle eligibility, drain and cancel, and observe status transitions — the same containerized method used to close Foundation open-item #1.

---

## 10. Open items / assumptions to verify at plan/implementation time

1. **`node_gc_threshold` exact name/default** — confirm against Nomad v2.0.4 docs + pinned api (assumed `server.node_gc_threshold`, default `24h`) before rendering it (ripsheet §0).
2. **`DrainSpec.Deadline` zero/negative semantics** — confirm `0` = no-deadline and negative = force against real v2.0.4 before documenting the `deadline` field behavior.
3. **Reflection watch wiring** — the precise `Watches`/enqueue shape (map `NomadCluster` → its nodes; steady resync for churn) vs a per-cluster lister requeue is a plan-time mechanism choice; §3.2 fixes the loop's contract, not its wiring.
4. **`NodeName` uniqueness** — assumed unique per fleet; the `DuplicateNodeName` guard makes the non-unique case safe (surfaced, not guessed) but does not resolve it.
5. **Cross-namespace `clusterRef`** — assumed same-namespace only (reflector creates CRs in the cluster's namespace); no cross-namespace reference in v1.
