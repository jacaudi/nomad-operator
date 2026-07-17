# NomadCluster Restart Resilience — Design

**Type:** design · **Date:** 2026-07-17 · **Status:** proposed
**Feature:** make a `NomadCluster` server pod restart a well-understood, well-instrumented event rather than a silent failure mode. Concretely: (A) document the verified restart/raft-address behavior and a recovery runbook; (B) add an operator-side **`advertise.rpc` drift guard** that surfaces a Condition/Event instead of letting a single-node cluster silently wedge; (C) replace the fabricated `status.quorum = "N/N"` with **real quorum + `status.members`** read from the Nomad raft configuration (folding in a slice-2 deferral).

This is slice **6b** of the hardening slice (6a = `NomadNamespace`, done+merged; 6c = envtest/backlog). It originated from a live-kind end-to-end finding (2026-07-16) that a `servers: 1` cluster "does not survive a server-pod restart," originally root-caused as *"advertise uses the ephemeral `POD_IP`, which changes on restart and wedges raft."*

**That root cause was empirically disproven during this brainstorm.** A live Nomad **v2.0.4** spike (§2) shows the pod IP is irrelevant to raft; the wedge is caused **only** by the advertised **`rpc`** address *changing* across a restart — and the operator already advertises a **stable** address (the Gateway/LoadBalancer address, not `POD_IP`). So the shipping operator already survives a restart in any real deployment; the recorded failure was a bare-kind harness artifact (a manually-patched, non-durable fake ingress IP). This design therefore **rejects** the originally-proposed fix (per-pod ClusterIP advertise) and scopes 6b to detection, observability, and documentation of the one genuine residual failure mode: **`servers: 1` + external-address drift**.

Every Nomad-domain claim below is grounded either in the live v2.0.4 spike (§2) or in `go doc` against the pinned `github.com/hashicorp/nomad/api` (`v0.0.0-20260707172059-5b83b133998a`, == v2.0.4). The one new API dependency — `Operator().AutopilotServerHealth` → `GET /v1/operator/autopilot/health`, returning `OperatorHealthReply{Healthy bool, FailureTolerance int, Servers []ServerHealth{ID, Name, Address, SerfStatus, Leader, Voter, Healthy, ...}}` — was verified present in the pin (operator_autopilot.go:100,171,298).

> **Amended 2026-07-17** after an independent sr-go-engineer *design* review (Opus model), verdict *amend-before-planning* — no blocking issues. Verified SOUND and not re-litigated: the Nomad API surface, the `advertise.rpc` raft-keying, the stable-address sourcing, the drift-guard mechanics (`prevAddr` capture before overwrite; `prev==""` provisioning guard), that `status.members` cannot enter the rollout hash (the hash reads no status), the `EventRecorder` wiring mirroring `NomadPool`, the fabricated-quorum target, C's non-regression (Degraded is entered only on leader-lost), `contract.go` pin discipline, and both rejections (per-pod ClusterIP advertise, auto peers.json recovery). Folded corrections: **I-1** — deliverable C must **reuse the already-shipped `MemberStatus`/`status.members`** (dead slice-2 scaffolding at `nomadcluster_types.go:182-188,203`, present in the CRD but never populated), **not** introduce a parallel `NomadServerMember` type/`members` field (a DRY violation + JSON-key collision). The reviewer flagged that `MemberStatus.Status` (a required CRD field) has no source in `RaftGetConfiguration`; this design resolves that by sourcing C from **`AutopilotServerHealth`** instead, whose `ServerHealth.SerfStatus` fills `Status` **honestly** (real serf state — no re-fabrication) while also supplying `Leader`/`Voter`, in one namespace-agnostic operator read. **M-1** — the raft/health read must be placed **after** the leader gate (§6.3). **M-2** — projection field types clarified. **M-3** — the guard's `Ready`-gated *Warning* can under-warn a drift during the first post-bootstrap roll while still `Bootstrapping`; the Condition still fires, so it is not a correctness hole — noted in the runbook (§4) so operators do not rely on Warning severity alone.

---

## 1. Background & framing

### 1.1 How the operator advertises today

On each server pod, an init container (`internal/controller/resources_workload.go`) renders an `advertise` overlay:

```
advertise {
  http = "${GW}:4646"
  rpc  = "${GW}:${RPCPORT}"   # raft/peers.json key on THIS
  serf = "${POD_IP}"          # only serf uses the ephemeral pod IP
}
```

`${GW}` is the ConfigMap key `gateway_address`, resolved by the reconciler per external-access mode:

- **LoadBalancer mode:** `gateway_address` = the LB Service ingress IP (`status.loadBalancer.ingress`), `${RPCPORT}` = `4647`.
- **Gateway mode:** `gateway_address` = the Gateway's assigned address (`status.addresses`), `${RPCPORT}` = the per-ordinal `gateway.rpcPorts[ordinal]` (distinct ports on one stable Gateway IP).

Both `gateway_address` sources live on a **Service/Gateway object**, not on a pod, so both are **stable across a backend pod restart**. `serf` uses `POD_IP` but self-reconverges via `retry_join` to the headless Service, so its ephemerality is inert.

### 1.2 Why raft cares only about `advertise.rpc`

A Nomad server's raft peer identity is `(node-id, advertise.rpc-address)`. The `node-id` is persisted on the PVC (`/var/lib/nomad/server/node-id`) and is stable across restarts. `peers.json`/the raft store key the peer's **address** on `advertise.rpc`. `serf` and `POD_IP` never enter the raft peer set. This is the crux the original root cause got wrong.

---

## 2. Evidence — live Nomad v2.0.4 restart spike

Two throwaway Docker harnesses (server-only Nomad 2.0.4 — server pods need no privileged/cgroups) established the behavior directly. Harnesses preserved at `/Users/user/.cache/claude-nomad-spike/` (`run.sh`, `run-ha.sh`).

### 2.1 Single-node (`servers: 1`)

A single server, `bootstrap_expect = 1`, persistent data volume, `advertise.rpc` set to a **fixed fake IP** (`10.99.99.99`) deliberately **unequal to the container's real IP**:

| Scenario | `advertise.rpc` | Result |
| --- | --- | --- |
| S1 bootstrap | `10.99.99.99` | healthy leader/voter |
| S2 restart, **same** advertise, fresh container (new real IP), same volume | `10.99.99.99` | **self-heals** — election won, leadership acquired, **zero** reconcile error |
| S3 restart, **changed** advertise, same volume/node-id | `10.99.99.100` | **wedges** |

S3's fatal log (the exact recorded symptom):
```
[ERROR] nomad: failed to reconcile member ... error="error removing server with duplicate ID
  \"<node-id>\": need at least one voter in configuration: {[]}"
```
`raft list-peers` after S3 shows the **stale** `10.99.99.99` address with `State: follower` even though the process is the leader — the wedge.

**Because the advertised `rpc` IP was fake/unreachable in all three runs yet S1/S2 were perfectly healthy, the container/pod IP is proven irrelevant to raft. The wedge is caused solely by `advertise.rpc` changing across the restart.**

### 2.2 HA (`servers: 3`)

Three servers, `bootstrap_expect = 3`, each advertising its own routable address:

| Scenario | Result |
| --- | --- |
| Test A — rolling-restart one node, **same** advertise, same volume | **self-heals** — 3 healthy voters, `FailureTolerance: 1` |
| Test B — replace one node at a **drifted** advertise (`.12`→`.22`), same node-id/volume, quorum held by the other two | **autopilot self-heals** |

Test B leader log (the decisive contrast with §2.1):
```
RemoveServer server-id=<id>           # SUCCEEDS — quorum held by the other two voters
removed server with duplicate ID: id=<id>   # INFO here, not the fatal ERROR of servers:1
AddNonvoter    server-addr=172.32.0.22:4647
autopilot: Promoting server           address=172.32.0.22:4647
AddVoter       server-addr=172.32.0.22:4647
```
The very `remove duplicate ID` operation that is **fatal** at `servers: 1` (can't remove the sole voter) **succeeds** at `servers: 3` because quorum survives on the other voters, after which autopilot re-adds and promotes the drifted node.

### 2.3 Complete truth table (all live-verified)

| Config | Restart, **stable** `advertise.rpc` | Restart, **drifted** `advertise.rpc` |
| --- | --- | --- |
| **`servers: 1`** | ✅ self-heals (S2) | ❌ **permanent wedge**, manual recovery (S3) |
| **`servers: 3/5`** | ✅ self-heals (Test A) | ✅ autopilot remove→re-add→promote (Test B) |

The operator provides a **stable** `advertise.rpc` in any real deployment (§1.1), so **restart is already safe for both configs.** The single genuine failure mode is the top-right cell: **`servers: 1` + external-address drift.**

---

## 3. Root cause & non-goals

### 3.1 Corrected root cause

The recorded root cause ("ephemeral `POD_IP` wedges raft") is wrong on both counts: `POD_IP` is not the raft address, and the operator already advertises a stable `rpc` address. The **true** failure mode is: **`advertise.rpc` changing across a restart while raft cannot remove the stale voter** — which only happens at `servers: 1` (no quorum survivor) and only when the external address *drifts* (LB reassignment, Service delete/recreate, switching `loadBalancerClass`, or a non-durable manually-patched ingress IP as in the bare-kind harness).

### 3.2 Rejected: per-pod ClusterIP advertise

The original handoff proposed giving each pod a stable ClusterIP Service and advertising **that** for `rpc`. Rejected as both unnecessary and harmful:

- **Unnecessary** — `advertise.rpc` is already stable (§1.1, §2.3).
- **Harmful** — `advertise.rpc` is single-valued and dual-purpose: external client agents dial it for north-south RPC **and** raft/serf peers use it east-west (verified networking constraint, prior slice-2 work). Advertising an internal ClusterIP would make servers **unreachable to external client agents**, breaking the external-access design. There is no way to give raft an internal address while giving external clients an external one, because Nomad has a single `advertise.rpc`.

### 3.3 Rejected: automatic `peers.json` recovery

Auto-detecting the wedge and rewriting `peers.json` / triggering raft recovery is heavy, risky, and only benefits a dev-only `servers: 1` mode. Rejected as YAGNI (explicit user decision). The recovery is documented (deliverable A), not automated.

---

## 4. Deliverable A — documentation & recovery runbook

*No code.* Corrects the record and gives operators a recovery path for the one real failure mode.

1. **`docs/runbooks/nomadcluster.md`** — new section "Restart resilience & raft address stability":
   - The §2.3 truth table.
   - The invariant: *raft integrity depends on a stable external `advertise.rpc`; the operator supplies one from the Gateway/LB address, so a normal pod restart self-heals. A drifting external address wedges `servers: 1` (single-node/dev mode) because raft cannot remove its sole voter; HA self-heals via autopilot.*
   - How to recognize the wedge: `RaftAddressDrift` Condition (§5), the `need at least one voter in configuration` log, `raft list-peers` showing a stale address in `follower` state on the leader process. **Note (M-3):** the guard's *Warning* is gated on `Phase == Ready`, so a drift during the very first post-bootstrap roll (still `Bootstrapping`) surfaces the Condition but at `Normal` severity — do not rely on Warning severity alone; check the `RaftAddressDrift` Condition directly.
2. **Recovery runbook** (scoped to the `servers: 1` + drift wedge), two paths:
   - **Preserve state** — surgical `peers.json` recovery: write `/var/lib/nomad/server/raft/peers.json` on the pod with the current node-id + the *new* advertised address, restart the agent (Nomad consumes and deletes `peers.json` on boot).
   - **Simplest (dev-acceptable)** — delete the server pod's PVC and let the StatefulSet re-bootstrap a clean single-node raft at the new address (loses Nomad state; acceptable for the non-HA/dev tier).
   - Guidance: prefer HA (`servers: 3`) for any cluster where address drift is plausible, since it self-heals.
3. **`docs/known-issues.md`** — correct/close the prior "servers:1 does not survive restart" entry: it was a bare-kind harness artifact (non-durable patched ingress IP), not a shipping defect; the real, narrow failure mode is `servers: 1` + address drift, now covered by the runbook + drift guard.

---

## 5. Deliverable B — operator-side `advertise.rpc` drift guard

*Operator-only; no Nomad API call.* Detects the exact juncture at which the operator would otherwise **self-inflict** the wedge and surfaces it.

### 5.1 Why the operator triggers the wedge

`renderConfig`'s rollout hash includes `gateway_address`; `buildConfigMap` writes it. When the resolved `extAddr` changes, the config-hash pod annotation changes → the StatefulSet rolls → pods reboot advertising the **new** `rpc` address while raft still holds the **old** one → the §2.1 S3 wedge (at `servers: 1`). The signal is therefore already in hand, purely operator-side: **the freshly-resolved `extAddr` differs from the last-persisted `status.externalAddress`.**

### 5.2 Mechanism

In `Reconcile`, capture the previously-persisted address **before** it is overwritten (today `nc.Status.ExternalAddress = extAddr` at controller step 2). Since `nc` is freshly `Get`-read at the top of reconcile, `nc.Status.ExternalAddress` still holds the prior value:

```go
prevAddr := nc.Status.ExternalAddress
// ... resolve extAddr for the active mode ...
drifted := prevAddr != "" && prevAddr != extAddr
```

On `drifted`, emit a **config-aware** signal:

| `spec.servers` | Condition | Event | Message |
| --- | --- | --- | --- |
| `1` | `RaftAddressDrift = True`, reason `AddressChanged` | `Warning` | external address changed `<prev>`→`<new>`; single-node raft will wedge on the ensuing roll — see recovery runbook |
| `≥ 3` | `RaftAddressDrift = True`, reason `AddressChangedHA` | `Normal` | external address changed; HA autopilot will self-heal |

- **Does not block the roll.** If the external address truly changed, the old one is dead regardless; the guard **detects and guides**, it does not prevent (a hold would not restore reachability, and preventing the roll would strand external clients on a dead address). This keeps the guard simple and side-effect-free on the happy path.
- **New CRD condition type** `CondRaftAddressDrift` (mirrors the existing `setCondition` pattern). It is set `False`/absent when `!drifted`.
- **New `EventRecorder`** on `NomadClusterReconciler` (the struct has none today; `NomadPool`/`NomadNode` reconcilers already carry one — same wiring via `mgr.GetEventRecorderFor`).

### 5.3 False-fire guards

- Initial provisioning: `prevAddr == ""` → no drift.
- Pending→Bootstrapping (raft not yet persisted at the old address): a change here is benign; the guard's *Warning severity* is only load-bearing once the cluster has been `Ready`. The Warning is therefore **gated on `nc.Status.Phase == Ready`**; the Condition itself is set regardless (harmless earlier). Consequence (M-3): a drift during the first post-bootstrap roll surfaces the Condition at `Normal` severity — documented in the runbook (§4).

---

## 6. Deliverable C — real `status.quorum` + `status.members`

*Folds in a slice-2 deferral.* Today `bootstrapAndReady` fabricates `status.Quorum = fmt("%d/%d", spec.Servers, spec.Servers)` (`nomadcluster_controller.go:215`), and slice-2 shipped a `MemberStatus`/`status.members` field that is **declared but never populated** (dead scaffolding). This replaces the fabrication with truth read from Nomad, and **reuses the existing `MemberStatus` type** — it does not introduce a parallel type (I-1).

The read is justified **independently of the guard** (deliverable B stays operator-side per §5); the guard does not consume it. The consolidation is: one operator-endpoint read powers both `status.quorum` and `status.members`.

**API choice.** The existing `MemberStatus{Name, Addr, Status, Leader}` has a `Status` field that clearly intends a *serf/member liveness* value — which `RaftGetConfiguration` (raft peer set) cannot supply. `Operator().AutopilotServerHealth` (`GET /v1/operator/autopilot/health`) supplies **all** required fields from one namespace-agnostic read: `ServerHealth{Name, Address, SerfStatus, Leader, Voter, Healthy, ...}`, so `Status` is filled **honestly** (real serf state) rather than re-fabricated as a constant — which is the whole point of C.

### 6.1 Client surface

New method on the reconciler's Nomad client + `NomadOps` interface:

```go
// ServerHealth returns the observed per-server autopilot health.
ServerHealth(ctx context.Context) ([]NomadMember, error)
```

wrapping `client.Operator().AutopilotServerHealth(qopts)` and projecting each `*api.ServerHealth` to an internal struct (avoid leaking `*api.ServerHealth` past the client boundary, matching the existing `internal/nomad` pattern):

```go
type NomadMember struct {
    Name    string // ServerHealth.Name
    Addr    string // ServerHealth.Address (advertise.rpc)
    Status  string // ServerHealth.SerfStatus ("alive"/"failed"/"left")
    Leader  bool   // ServerHealth.Leader
    Voter   bool   // ServerHealth.Voter
}
```

A `contract.go` pin backs `AutopilotServerHealth` + `OperatorHealthReply.Servers` + `ServerHealth.{Name,Address,SerfStatus,Leader,Voter}` with a **real call** (existence-only-pin discipline). The envtest fake `NomadOps` gains a `ServerHealth` stub.

### 6.2 Status shape

**Reuse** the shipped `MemberStatus`/`Members` field (`nomadcluster_types.go:182-188,203`); the only CRD change is adding one field:

```go
type MemberStatus struct {
    Name   string `json:"name"`
    Addr   string `json:"addr"`
    Status string `json:"status"`            // ← now populated from SerfStatus (was never populated)
    Leader bool   `json:"leader"`
    Voter  bool   `json:"voter,omitempty"`   // NEW — additive
}
```

Mapping: `NomadMember.{Name,Addr,Status,Leader,Voter}` → `MemberStatus.{Name,Addr,Status,Leader,Voter}` (a direct 1:1). `status.quorum` becomes `fmt("%d/%d", voters, len(members))` computed from the read (`voters` = count of `Voter == true`). Adding `Voter` is an additive CRD change — regen the manifest (`config/crd/bases/...`) + deepcopy. No field need change from required→optional, since `Status` is now always populated with a real serf state.

### 6.3 Population point

In `bootstrapAndReady`, **after the leader gate** (i.e. after the `ops.Leader` non-empty check at `nomadcluster_controller.go:209`, never before — M-1), call `ops.ServerHealth`; on success populate `status.Members` + real `status.Quorum`. On error, **do not fail the reconcile** — log and leave the prior `status.Members`/a best-effort `quorum`. `Degraded` is entered **only** on leader-lost (`nomadcluster_controller.go:202-206`), so a health-read failure structurally **cannot** flap `Ready`→`Degraded`. This keeps the existing phase machine intact.

---

## 7. What this design deliberately does **not** change

- **No networking/advertise change.** `advertise.rpc` wiring, Services, Gateway/LB topology, per-ordinal ports, PDB, anti-affinity, and the Pending→Bootstrapping→Ready→Degraded phase machine are untouched. The guard is read-only observation; C is a status read.
- **No new external-access surface.** Deliverable B adds a Condition + Event; C adds a status field.
- **No auto-recovery** (§3.3).

The design keeps the next addition additive: HA-specific hardening (if ever needed) slots in beside the guard; the autopilot-health read in C is the seam a future richer health/observability feature (e.g. surfacing `FailureTolerance`/`Healthy`) would reuse.

---

## 8. Testing strategy

- **Deliverable B (guard):** envtest — drive a reconcile to `Ready` with `status.externalAddress = A`, then re-resolve with address `B` (stub the LB/Gateway read to return `B`); assert `RaftAddressDrift = True` with the `servers: 1` reason + a Warning event; a parallel `servers: 3` case asserts the `Normal`/HA reason. A no-drift reconcile asserts the condition is absent. Initial-provisioning (`prev == ""`) asserts no fire.
- **Deliverable C (status):** envtest with the injected fake `ServerHealth` returning a 1- and 3-member set; assert `status.members` mapping (incl. `Status` from `SerfStatus` and `Voter`) and `status.quorum` = `voters/total`; assert a `ServerHealth` error does **not** flip `Ready`→`Degraded`.
- **`contract.go`:** the `AutopilotServerHealth` pin runs against a real (dev-agent) call under `-tags integration`, consistent with prior slices; the live restart behavior itself is already established by the §2 spike and does not need re-encoding as a gated test.
- **No regression** to the existing slice-2 reconcile/gateway/loadbalancer/teardown tests.

---

## 9. Plan-time spikes / notes

- **S-1 (contract pin):** confirm the `AutopilotServerHealth` call shape + `ServerHealth.{Name,Address,SerfStatus,Leader,Voter}` fields against a real Nomad v2.0.4 dev agent when the pin is written (existence-only-pin rule; the fields are already read from the pinned source — `operator_autopilot.go:100,298` — in this design).
- **S-2 (phase-gating):** decide whether the guard's *Warning* is gated on `Phase == Ready` (Condition may be set earlier harmlessly) — a small plan-time call, not an architectural one.
- **N-1:** `status.members` is observed state; it does not participate in any comparator/rollout hash (must not, or it would cause reconcile churn).
- **N-2:** the `EventRecorder` addition must be wired in `SetupWithManager` (`mgr.GetEventRecorderFor("nomadcluster")`) and injected in envtest, mirroring `NomadNode`/`NomadPool`.

---

## 10. Slice decomposition recap

- **6a — `NomadNamespace`** ✅ done + merged (local `main`).
- **6b — restart resilience** ← *this design* (A docs/runbook + B drift guard + C real quorum/members).
- **6c — hardening/envtest + backlog** (remaining slice-2 items now reduced by C; slice-3 test gaps; full `make test-integration` live run; the 6a deferred nits).
