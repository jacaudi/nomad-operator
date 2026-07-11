# Known Issues — deferred non-blocking items

These are non-blocking follow-ups deferred out of the slice-2 (NomadCluster) merge
(`a1e4d6a`). Both the per-task reviews and the independent whole-branch review agreed
none block merge. Each entry is written to be portable to a GitHub issue verbatim once
`github.com/jacaudi/nomad-operator` exists as a repository.

Source: slice-2 whole-branch review, 2026-07-11.

---

## 1. `status.quorum` is fabricated `N/N`, not measured

- **Severity:** Minor · **Area:** reconciler / status
- **Location:** `internal/controller/nomadcluster_controller.go` (`bootstrapAndReady`, the
  `nc.Status.Quorum = fmt.Sprintf("%d/%d", servers, servers)` line, ~:219)
- **Problem:** `status.quorum` is set to `"<servers>/<servers>"` (e.g. `3/3`) whenever a
  leader exists, without counting healthy peers. A leader-with-2-of-3-up cluster still
  reports `3/3`, so the field is misleading.
- **Why deferred:** real peer counting requires `Status().Peers()` and `status.members`,
  which the design explicitly defers to slice 6 (hardening). The DoD only requires
  `leader`/`quorum` be populated.
- **Proposed fix:** in slice 6, populate `status.quorum` from the actual peer set
  (`Status().Peers()`), alongside `status.members` and the friendly-leader-name mapping.

## 2. Two `golangci-lint` findings (prealloc, unparam)

- **Severity:** Minor · **Area:** lint / cleanup
- **Locations:**
  - `internal/controller/resources_gateway.go:30` — `prealloc`: `listeners` slice should be
    preallocated with capacity `1 + len(nc.Spec.Gateway.RPCPorts)`.
  - `internal/controller/security_test.go:58` — `unparam`: `makeCertSecret`'s `name`
    parameter always receives `"nomad-tls"`.
- **Problem:** `make lint` reports these two findings. Lint is not part of the acceptance
  build gate (`make manifests generate fmt vet` + `make test`), so they did not block merge.
- **Proposed fix:** preallocate the `listeners` slice; drop or use the `makeCertSecret`
  `name` parameter. Trivial, no behavior change.

## 3. Unused `NomadOps` interface methods (`Ping`, `ServerHealthy`)

- **Severity:** Minor · **Area:** API surface / YAGNI
- **Location:** `internal/controller/nomadcluster_controller.go` (`NomadOps` interface, ~:42-44)
- **Problem:** `NomadOps.Ping` and `NomadOps.ServerHealthy` are never called by the
  reconciler. They are dead interface surface.
- **Note:** `(*nomad.Client).ServerHealthy` (`internal/nomad/client.go`) must remain — it
  backs the `(*api.Agent).Health` `contract.go` pin via a real call. Only the *interface*
  members are unused; trimming the interface is safe, the concrete method stays.
- **Proposed fix:** trim `NomadOps` to what the reconciler uses (`Leader`, `ACLBootstrap`);
  re-add methods when a consumer actually calls them.

## 4. Redundant gossip Secret mount on the main container

- **Severity:** Minor · **Area:** workload builder
- **Location:** `internal/controller/resources_workload.go` (main `nomad` container volume
  mounts, ~:212)
- **Problem:** the `gossip` Secret is mounted read-only at `/nomad/gossip` on the main
  `nomad` container, but the encrypt key is baked into `overlay.hcl` by the **init**
  container; the main container never reads `/nomad/gossip`. Harmless but dead.
- **Proposed fix:** remove the gossip mount from the main container (keep it on the init
  container). Add/confirm a builder test that the main container has no `/nomad/gossip` mount.

## 5. `Ready`→`Pending` flap on a transient cert/gateway read

- **Severity:** Minor · **Area:** reconciler robustness
- **Location:** `internal/controller/nomadcluster_controller.go` (cert gate ~:92-96 and
  gateway gate ~:103-107)
- **Problem:** the cert and gateway gates set `Phase = Pending` and return early if the cert
  Secret or gateway address read momentarily fails — even for a `Ready`/`Degraded` cluster.
  In Existing mode a shared-Gateway blip could flap a healthy cluster to `Pending`.
- **Proposed fix:** don't demote a `Ready`/`Degraded` cluster to `Pending` on a transient
  read; distinguish "never provisioned" from "already provisioned, transient dependency
  blip" (e.g. only gate to `Pending` when phase is empty/`Pending`, mirroring the
  Bootstrapping-seed guard added for the Ready→Degraded fix).

## 6. Existing-mode `GatewayReady=False` reason is imprecise

- **Severity:** Minor · **Area:** Existing-mode diagnostics
- **Location:** `internal/controller/nomadcluster_controller.go` (gateway gate condition,
  ~:105) and `ensureExistingGateway` in `internal/controller/resources_gateway.go`
- **Problem:** all Existing-mode verification failures (Gateway not found, missing/misnamed
  listener, namespace not admitted, no address yet) collapse into a single generic
  `GatewayReady=False` / `"WaitingForAddress"` reason. Operators can't tell which prerequisite
  failed from status alone.
- **Mitigation in place:** documented as a manual diagnosis checklist in
  `docs/runbooks/nomadcluster.md` §6.
- **Why deferred:** a precise per-failure reason requires threading a reason string through
  the fixed `ensureGateway`/`ensureExistingGateway` `(string, bool, error)` signature — a
  design change beyond the slice.
- **Proposed fix:** return a typed verification result (reason enum + message) from
  `ensureExistingGateway` and surface it in the `GatewayReady` condition.

---

# Feature Requests

## FR-1. Support a single-node (`servers: 1`) control plane

- **Type:** Feature request · **Area:** CRD validation / topology · **Requested by:** user, 2026-07-11
- **Current behavior:** `spec.servers` is `Enum=3;5` (immutable), so the minimum control
  plane is 3 Raft servers across 3 nodes (hard `kubernetes.io/hostname` anti-affinity). A
  single-node control plane cannot be expressed.
- **Request:** allow `spec.servers: 1` for non-HA / edge / dev / small deployments.
- **Rationale:** running on Kubernetes, a failed control-plane pod is rescheduled by the
  StatefulSet controller, so the downtime from a single control-plane node is minimal — full
  3-node Raft quorum HA is not always required. The operator should let the user opt into a
  1-server control plane and accept the (small, reschedule-bounded) downtime tradeoff.
- **Scope (small — the rest already scales with `servers`):**
  - Relax the CEL enum `Enum=3;5` → `Enum=1;3;5` on `NomadClusterSpec.Servers` (keep it odd;
    even counts remain disallowed for split-brain safety). Regenerate the CRD.
  - No anti-affinity change needed: with 1 server there is a single pod, so hard
    per-node anti-affinity is moot (it only constrains 2+ pods).
  - `bootstrap_expect` already renders from `servers` (→ 1); a 1-server Raft bootstraps
    immediately.
  - `PDB minAvailable = servers - 1 = 0` already permits the single pod to be
    rescheduled/drained (which is exactly the intended "minimal downtime via reschedule"
    behavior) — no change required.
  - `gateway.rpcPorts` length must equal `servers`, so a `servers: 1` cluster uses exactly
    one RPC port / one per-server TCPRoute.
- **Tradeoff to document:** `servers: 1` = NO Raft HA. A pod reschedule (node failure,
  upgrade, eviction) is a brief control-plane outage; running workloads on edge clients keep
  running, but new scheduling/API is unavailable until the server is back. Recommend `3`/`5`
  for production HA; `1` for edge/dev/single-node.

## 7. Existing mode: operator does not watch the referenced Gateway

- **Severity:** Minor · **Area:** reconciler / Existing-mode gateway · **Found:** local single-node e2e test, 2026-07-11
- **Location:** `internal/controller/nomadcluster_controller.go` `SetupWithManager` (watch set) +
  `ensureExistingGateway` in `internal/controller/resources_gateway.go`
- **Problem:** in `gateway.mode: Existing`, the operator reads the referenced Gateway's
  `status.addresses` but does not set up a Watch on that Gateway. If the user's Gateway gets
  its address assigned (or changed) AFTER the operator's reconcile, the operator won't react
  until its periodic resync or the next NomadCluster change — so the cluster can sit at
  `Pending`/stale `gatewayAddress` longer than necessary. Observed directly: patching the
  Gateway's `status.addresses` did not trigger a reconcile; a manual CR annotation was needed.
- **Proposed fix:** add a `Watches` on `gatewayapi.Gateway` in `SetupWithManager` mapping the
  referenced Gateway back to the owning NomadCluster(s) (Existing mode), OR watch the operator's
  own Routes on that Gateway. Managed mode is unaffected (the operator owns and watches its
  Gateway via ownerRef).
