# Design — `nomad-operator` Foundation Slice

| | |
|---|---|
| **Component** | `nomad-operator` — a Kubernetes operator that provisions and manages a Nomad control plane on K8s, with workloads on edge clients |
| **This document** | Design for the **Foundation slice** (slice 1 of 6) — the buildable, verifiable skeleton every later slice builds on |
| **Target runtime** | Nomad **v2.0.4**; K8s 1.28+; Go **1.26.4** |
| **Status** | Approved 2026-07-09 — ready for implementation plan |
| **Supersedes premises in** | `docs/development/design/idea.md` (the ripsheet) — see Appendix A for corrections |

---

## 1. Background & framing

The operator's end goal is to model Nomad as K8s-native custom resources: it **provisions a
3-node HA Nomad control plane** (a `NomadCluster` CRD → StatefulSet with Raft quorum) and then
**manages that running cluster** via `NomadNode` (read-mirror of registered clients + writable
eligibility/drain), `NomadPool` (K8s-side grouping resolved to a sorted node-ID set), and
`NomadJob` (rendered HCL → server-side parse → register → readiness → finalizer). Workloads run
on **edge clients** (e.g. TrueNAS) that only have a container runtime.

**Key reframe (established empirically — see Appendix A):** *Nomad 2.0 is a versioning-scheme
rename, not an API break.* The `github.com/hashicorp/nomad/api` types this operator binds to are
byte-identical between 1.11.3 and 2.0.4. System-job deployments and node identity shipped in
**1.11.0**; "2.0.0" is the next V.M.F milestone, not their debut. Building against 2.0.4 is, for
this operator, the same Go-API surface as current 1.11.x — this materially de-risks the project.

## 2. Decomposition roadmap (context — only slice 1 is designed here)

The full operator is too large for one spec. It is sliced into six sub-projects, each with its own
spec → plan → implementation cycle:

| # | Slice | Purpose |
|---|-------|---------|
| **1** | **Foundation** *(this doc)* | Scaffold, Nomad client + contract pin, toolchain, hermetic live read. No CRDs, no controllers. |
| 2 | Control plane (`NomadCluster`) | 3-server HA StatefulSet: Raft quorum, storage, TLS/ACL, external join surface for edge clients. |
| 3 | `NomadNode` | Reflector (read-mirror) + writable eligibility/drain. (Ripsheet W5 "node introduction status" is **void** — see Appendix A.) |
| 4 | `NomadPool` | Pure K8s-side set computation (members ∪ selector − exclude → sorted node-ID set) → job constraint. |
| 5 | `NomadJob` | Render HCL → `ParseHCL` → `Register` → readiness (deployment-first, alloc fallback) → finalizer. |
| 6 | Hardening | envtests, the readiness test matrix, docs, §0-style binding write-ups. |

## 3. Scope of the Foundation slice

**In scope.** A buildable operator skeleton that proves the hard, cross-cutting things before any
business logic exists:

1. The Kubebuilder toolchain runs (`generate`, `verify`).
2. The pinned Nomad `api` module compiles against a **compile-time contract** that breaks the build
   on signature drift.
3. The operator can construct a Nomad client and **read from a live Nomad 2.0.4 instance** — proven
   by a hermetic test that boots its own ephemeral Nomad.

**Out of scope (YAGNI boundary).** All business CRDs (`NomadCluster`, `NomadNode`, `NomadPool`,
`NomadJob`); any controllers; **any Nomad writes**; Secret-watching; admission webhooks; leader
election tuning; multi-region; envtest (deferred to slice 3, where a controller first exists).

## 4. Design

### 4.1 Project scaffold (Kubebuilder)

- `kubebuilder init` with:
  - module `github.com/jacaudi/nomad-operator`
  - domain `operator.io`, group `nomad`, version `v1alpha1` → **API group `nomad.operator.io`**
    (later CRDs read `nomadnode.nomad.operator.io`, `nomadjob.nomad.operator.io`,
    `nomadcluster.nomad.operator.io`)
- `go.mod` toolchain: **`go 1.26.4`** (the `api` module requires ≥ 1.25).
- Layout (Kubebuilder-standard, matches the ripsheet's assumptions):
  `api/v1alpha1/`, `internal/controller/`, `internal/nomad/`, `cmd/main.go`, `Makefile`, `config/`.
- `cmd/main.go` starts a controller-runtime manager with health/ready probes and
  **zero controllers registered**. It comes up clean and idle; the first controller arrives in slice 3.

### 4.2 Nomad client — `internal/nomad/client.go`

- A thin `Client` wrapping `*api.Client`, **constructed per-endpoint** (not a global singleton) so
  that slice 2's `NomadCluster` reconciler can build one client per cluster from the CR. This is the
  No-Wall seam: it costs nothing now and makes slice 2 an additive caller.
- Constructor input:

  ```go
  type Config struct {
      Address string           // e.g. http://127.0.0.1:4646
      Region  string           // optional
      Token   string           // ACL token; empty in dev mode
      TLS     TLSConfig         // CACert, ClientCert, ClientKey, Insecure
  }
  ```

- **Read-only surface only** in Foundation (no writes): list nodes, get node, and an
  agent-self/health read for connectivity sanity. Concrete `api` calls: `Nodes().List`,
  `Nodes().Info`, `Agent().Self` (or equivalent health read).

### 4.3 Compile-time contract — `internal/nomad/contract.go`

A file that references **every `api` symbol the operator uses**, so that a version bump changing any
signature **breaks `go build`** rather than failing silently at runtime. Foundation covers the
read surface; each later slice extends it. Confirmed symbols for the Foundation surface (from
Appendix A):

- Types: `api.Client`, `api.Config`, `api.QueryOptions`, `api.NodeListStub`, `api.Node`,
  `api.DriverInfo`.
- Methods: `Nodes().List`, `Nodes().Info`, `Agent().Self`.
- Constants (referenced to pin their existence/values):
  `api.NodeStatusInit` (`"initializing"`), `api.NodeStatusReady` (`"ready"`),
  `api.NodeStatusDown` (`"down"`), `api.NodeStatusDisconnected` (`"disconnected"`),
  `api.NodeSchedulingEligible`, `api.NodeSchedulingIneligible`.

### 4.4 Toolchain — `Makefile`

- **`make nomad-pin`** — pins the `api` submodule. **The `api` module has no semver tags; it is
  consumable only by commit / pseudo-version.** The target therefore takes a *commit-ish*, not a
  bare semver:

  ```
  make nomad-pin NOMAD_API_REF=5b83b133998a   # == release v2.0.4
  ```

  It runs `go get github.com/hashicorp/nomad/api@$(NOMAD_API_REF)` (resolving to pseudo-version
  `v0.0.0-20260707172059-5b83b133998a`) then `go mod tidy`. The default `NOMAD_API_REF` is pinned to
  **v2.0.4 / `5b83b133998a`**, and the Makefile documents the `v2.0.4 → 5b83b13` mapping in a comment.
- **`make generate`** — Kubebuilder codegen (CRD manifests, deepcopy). Wired now; effectively a
  no-op until CRDs exist in slice 2+.
- **`make verify`** — the build gate: `go build ./... && go vet ./... && go test ./...`.

### 4.5 Config plumbing

The `Client` is built from an **explicit `Config`** — never `api.DefaultConfig()`, which would
absorb the operator process's `NOMAD_*` environment into a per-endpoint client and cross-contaminate
clusters in slice 2. Foundation constructs `Config` directly (the hermetic test does so; the future
`NomadCluster` reconciler will build one per cluster). Flag/env wiring into `cmd/main.go` is
deferred to the `NomadNode` slice, where `main.go` first consumes a `Client` (no consumer today =
YAGNI); when added, env is resolved to a `Config` explicitly in one place, not via `DefaultConfig`'s
implicit partial inheritance. A `SecretRef` source is the later idiomatic path. KISS: **no
Secret-watching machinery** in Foundation.

### 4.6 Testing & hermetic live sanity

- **Unit tests:** `Config` construction/validation and client wiring.
- **Hermetic integration test** (opt-in behind a build tag / env guard, skips when unset): boots a
  throwaway **`nomad agent -dev`** at v2.0.4 (combined server+client, no ACL/TLS), constructs the
  `Client`, and reads nodes off it — proving pin + client + wire format end-to-end with **no
  external cluster and no credentials**. This is the Nomad analogue of controller-runtime's envtest
  (which downloads `kube-apiserver`/`etcd`).
  - **Test prerequisite:** a pinned **`nomad` v2.0.4 binary** present in the dev/CI environment.
  - This test also begins closing two of the live-cluster open items (§6): it observes the actual
    `Node.Status` value the dev agent reports, and exercises the real read path.
- **envtest** (K8s API-server tests) is **deferred to the `NomadNode` slice**, where a controller
  first exists to test.

## 5. Definition of Done

- `make nomad-pin && make generate && make verify` is green against the v2.0.4 pin
  (`api` at `5b83b133998a`), toolchain `go 1.26.4`.
- `internal/nomad/contract.go` compiles against the pinned `api` with no edits (or edits are
  intentional and documented).
- `cmd/main.go` manager starts idle with working health/ready probes.
- The hermetic dev-agent integration test passes: the `Client` reads at least one node
  (the dev agent's self-registered node, status `ready`) from a live ephemeral Nomad 2.0.4.
- Unit tests for `Config`/client wiring pass.

## 6. Open items requiring a live Nomad 2.0.4 cluster (tracked, not blocking Foundation)

These are carried forward for slices 3–5; Foundation's dev-agent test starts closing #1 and #3:

1. `nomad node status -json` sweep — confirm the actual `Node.Status` value set (verify no
   undocumented introduction/pending value beyond `initializing/ready/down/disconnected`).
2. Register a trivial **system** job → `Jobs().LatestDeployment` + `Deployments().Info` — capture
   real `DeploymentState` field population (`DesiredTotal`, `HealthyAllocs`, `PlacedAllocs`, canary
   fields) to lock the readiness rollup design.
3. Confirm `/v1/job/<id>/deployment` returns 200 + empty body → client `(nil, meta, nil)` for a job
   with no deployment on 2.0.4.

---

## Appendix A — Ripsheet (`docs/development/design/idea.md`) corrections, established empirically

Verified by reading the real `api` source at v2.0.4 (`5b83b133998a`) and diffing against v1.11.3.

| Ripsheet claim | Verdict | Correct fact |
|---|---|---|
| `api` imported as `github.com/hashicorp/nomad/api`, **no `/v2`** | ✅ Confirmed | Main module keeps the non-`/v2` path even at v2.0.4; the `api` submodule is pseudo-versioned; `go 1.25`+ required. |
| Pin via `NOMAD_API_VERSION=v2.0.x` | ❌ Wrong mechanism | The `api` submodule has **zero semver tags** — pin by commit/pseudo-version (`@5b83b133998a` → `v0.0.0-20260707172059-5b83b133998a`). |
| Latest is v2.0.3 | ❌ Stale | Latest is **v2.0.4**. |
| Go 1.23+ | ❌ Wrong | `api/go.mod` requires **`go 1.25`**; this project uses **1.26.4**. |
| Node "introduction/identity" is a new `Node.Status` value to surface (W5) | ❌ Wrong | Introduction is an **ACL JWT-token** mechanism (`ACLIdentity().CreateClientIntroductionToken`) + identity **claims** (`Nodes().Identity()`); `Node.Status` remains `initializing/ready/down/disconnected`. **W5 as written is void.** |
| System jobs get deployments on 2.0 | ✅ Correct (nuance) | True, but shipped in **1.11.0**, not new in 2.0. `DeploymentState` is shared with service jobs; exact field population for system jobs is **unverified-live** (open item #2). |
| `LatestDeployment` returns `(nil, nil)` when absent | ✅ Correct at client level | `var resp *Deployment` stays nil with nil error on empty body; code defensively. Server 200-empty-body contract is **unverified-live** (open item #3). |
| Field names `HealthyAllocs`/`HealthyAllocations` (ambiguous) | ▶ Resolved | Real names: **`HealthyAllocs`**, **`PlacedAllocs`**, `DesiredTotal`, `UnhealthyAllocs`; register modify index is **`JobModifyIndex`**. |
| Use `jobspec2` for local parse (heavy dep, §9-Q2) | ▶ Reinforced default | `jobspec2` pulls the heavyweight main module (~346 modules) with an api version-skew trap → **server-side `ParseHCL` is firmly correct**; avoid importing `jobspec2`. |
| `hashicorp/nomad-openapi` is stale | ✅ Confirmed (worse) | Repo is **archived** (last push 2023-09-18); do not build on it. |

## Appendix B — Confirmed `api` field references (for later slices)

- **`NodeListStub`** (from `Nodes().List`): `ID, Name, Address, Datacenter, NodeClass, NodePool,
  Version, Drain bool, SchedulingEligibility, Status, StatusDescription, Drivers, LastDrain,
  CreateIndex, ModifyIndex`. **Lacks** `Meta`, `DrainStrategy`, `Events` — call `Nodes().Info` for those.
- **`Node`** (from `Nodes().Info`): superset adding `Meta map[string]string`,
  `DrainStrategy *DrainStrategy`, `NodeMaxAllocs`, `Events`, host volumes/networks, `StatusUpdatedAt`.
- **`Deployment.TaskGroups map[string]*DeploymentState`**; `DeploymentState{ DesiredTotal,
  PlacedAllocs, HealthyAllocs, UnhealthyAllocs, DesiredCanaries, PlacedCanaries, Promoted, ... }`.
  Deployment status set: `running, paused, failed, successful, cancelled, pending, blocked, unblocking`.
- **Per-node alloc rollup:** `Nodes().Allocations(nodeID)` → `[]*Allocation`;
  `AllocDeploymentStatus.Healthy *bool` is **tri-state** (nil = not yet evaluated). Client status set:
  `pending, running, complete, failed, lost, unknown`.
- **Register:** `Jobs().Register(job, q) → *JobRegisterResponse{ EvalID, EvalCreateIndex,
  JobModifyIndex, Warnings }`.
