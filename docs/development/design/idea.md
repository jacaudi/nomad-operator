# SDD Ripsheet — Nomad 2.0 Accommodation (`nomad-operator`)

| | |
|---|---|
| **Work order** | Adopt Nomad 2.0 across the operator |
| **Component** | `nomad-operator` (Kubernetes operator; Nomad control plane on K8s, edge clients) |
| **Target runtime** | Nomad **2.0.x** (latest v2.0.3, Jun 2026); K8s 1.28+; Go 1.23+ |
| **Repo state at handoff** | Core readiness refactor APPLIED (see §5). Remaining items are TODO. |
| **Build gate** | `make nomad-pin && make generate && make verify` must pass |
| **Status** | Ready to begin |

---

## 0. Method — mandatory research & verification rounds (do not skip)

**The CR/CRD ↔ Nomad API relationship is the core of this design and must be
derived empirically, over several iterative rounds — never assumed, never
one-shot.** Each CRD field, status field, and reconcile action binds to a
specific Nomad API source or sink with specific semantics, and those semantics
must be confirmed against the **pinned `api` types** and a **live Nomad 2.0
cluster** before the binding is considered designed. Treat any surprise (a field
renamed, a status value you didn't expect, different behavior for system vs
service jobs, a 2.0 addition) as the trigger for *another* round.

Run this loop, repeatedly, until every binding in the table below is confirmed:

1. **Research** — read the actual `github.com/hashicorp/nomad/api` Go types for
   the endpoint in question (not memory, not the OpenAPI repo, not these notes),
   plus the official HTTP API docs for semantics/edge cases.
2. **Verify live** — exercise the endpoint against a real Nomad 2.0 cluster and
   inspect actual responses (empty states, transitional states, error shapes).
3. **Map** — write down exactly which CR/CRD field is populated from / pushed to
   which API field, and the update semantics (read-only mirror vs writable;
   registration-time vs runtime-mutable).
4. **Encode & re-verify** — reflect the binding in code and in
   `internal/nomad/contract.go`; rebuild (`make verify`); re-test live. If
   anything differs from round 1, **loop again**.

**No binding is "done" after a single pass.** Plan for at least 2–3 rounds per
binding; the node-lifecycle and deployment bindings (below) will likely need
more. Document each confirmed binding so the next round starts from evidence, not
re-derivation.

### Bindings that each require their own research/verify rounds

| CR / CRD surface | Nomad API source/sink | What must be verified |
|---|---|---|
| `NomadNode.status` (mirror) | `GET /v1/nodes`, `GET /v1/node/:id` | Exact stub vs full-node fields; 2.0 node **introduction/identity** lifecycle states; driver/meta shapes |
| `NomadNode.spec.eligible/drain` | eligibility + drain APIs | Which transitions are runtime-mutable; request/response shapes; idempotency |
| `NomadPool` → constraint | (no API; derived) → job `constraint` | That `node.unique.id` regexp matches as intended; that membership is NOT pushable to Nomad node pools |
| `NomadJob` render → register | `POST /v1/jobs/parse`, `POST /v1/jobs` | Parse canonicalization; register response fields; modify-index semantics |
| `NomadJob.status` (Ready) | `LatestDeployment`, allocations | Whether system-job deployments populate the same `DeploymentState` fields as service jobs; no-deployment fallback |
| `NomadJob` finalizer | `DELETE /v1/job/:id` | Purge vs deregister semantics; what happens to running allocs |

---

## 1. Context

The operator runs the Nomad **control plane** inside Kubernetes and models Nomad
as K8s-native CRs: `NomadNode` (read-mirror of registered clients + writable
eligibility/drain), `NomadPool` (K8s-side grouping = explicit members ∪ label
selector − exclude, resolved to a sorted node-ID set), and `NomadJob` (rendered
to HCL, parsed by Nomad, registered). Workloads run on **edge clients** (e.g.
TrueNAS) that only have a container runtime. The operator builds directly against
`github.com/hashicorp/nomad/api`; `internal/nomad/contract.go` is a compile-time
pin of the exact API surface used.

Nomad 2.0 is now GA. This work order makes the operator correct and idiomatic on
2.0 without regressing older lines.

## 2. Objective

The operator behaves correctly against Nomad 2.0.x, with job readiness using
2.0's system-job deployments where available, verified by `make verify` plus
envtests, and with 2.0-specific node lifecycle states surfaced rather than
silently mismapped.

## 3. Scope

**In scope:** readiness logic for 2.0 system-job deployments; pin/verify against
2.0.x; node-introduction/identity status surfacing; envtests for the readiness
divergence cases; docs.

**Out of scope:** changing the pool→constraint model; multi-region; Nomad
Enterprise-only features; rewriting the render path to structured `api.Job`
(we keep HCL + server-side parse).

## 4. Environment & pinning facts (verified mid-2026)

- `api` submodule import path is **`github.com/hashicorp/nomad/api`** — **no
  `/v2`** — despite the main module being v2. **Do not** rewrite imports to `/v2`.
- Latest Nomad: **v2.0.3**. 1.10.x is the last LTS; 1.11.x also active.
- Do **not** generate a client from `hashicorp/nomad-openapi` (experimental,
  derived from `api`, stale).
- `jobspec2` (local HCL parsing) lives in the heavyweight main module; only pull
  it if §9-Q2 is chosen. Default stays on server-side `api.Jobs().ParseHCL`.

## 5. Design decisions

1. **Readiness branches on "has a deployment," not on job type.** 2.0 gives system
   jobs deployments; pre-2.0 system jobs and any job without an update strategy
   have none. Prefer the deployment rollup when present; fall back to per-node
   allocation health otherwise. *(APPLIED.)*
2. **Per-node allocation health is always recorded** (`status.nodeHealth`) for
   visibility, independent of which signal drives `Ready`. *(APPLIED.)*
3. **Two readiness axes stay separate**: `Reconciled` (operator applied spec) vs
   `Ready` (Nomad reports healthy). Unchanged by this work.
4. **No import-path or contract changes** for 2.0 (path confirmed in §4). The
   compile-time contract in `internal/nomad/contract.go` already covers
   `LatestDeployment`.

## 6. Work items

> Legend: ☑ applied in this handoff · ☐ TODO for the agent

| ID | File(s) | Change | Acceptance |
|----|---------|--------|------------|
| W1 ☑ | `internal/controller/nomadjob_controller.go` | `refreshNomadStatus` rewritten: deployment-first with alloc-health fallback; reasons `DeploymentSuccessful/Running/Failed/…`, `AllAllocsHealthy`, `AllocsNotHealthy`, `NoAllocations`. | `Ready` is correct for a 2.0 system job with an update strategy (deployment) **and** a bare system job (alloc fallback). |
| W2 ☑ | `internal/nomad/client.go` | `LatestDeployment` doc updated; now called for all job types. | Returns rollup for system jobs on 2.0; `(nil,nil)` when no deployment. |
| W3 ☑ | `go.mod`, `README.md` | Note confirmed `api` path (no `/v2`); 2.0 readiness behavior. | Docs match code. |
| W4 ☐ | repo root | `make nomad-pin NOMAD_API_VERSION=<2.0.x>` then `make generate && make verify`. Fix any signature drift surfaced by `contract.go`. | `go build ./...` and `go vet ./...` clean. |
| W5 ☐ | `internal/nomad/client.go`, `api/v1alpha1/nomadnode_types.go`, `internal/controller/nomadnode_reflector.go` | Surface 2.0 **node introduction/identity** lifecycle. Confirm the node status values 2.0 reports (e.g. a pending-introduction state) and map them into `NomadNodeStatus.Status` rather than letting them read as `down`/unknown. Add a `status` field if a new dimension is needed. | A client awaiting introduction shows a distinct, documented status, and pools correctly treat it as not-yet-schedulable. |
| W6 ☐ | `internal/render/render.go` | *(Decision-gated, see §9-Q1)* optionally emit an `update {}` stanza for `AllNodes` jobs so system jobs actually produce a 2.0 deployment. | With the stanza, a system job yields a deployment and W1's deployment path drives `Ready`. |
| W7 ☐ | `internal/controller/*_test.go` (new) | Envtests for: (a) register failure keeps `Ready` untouched; (b) deployment `failed` ⇒ `Reconciled=True, Ready=False`; (c) bare system job uses alloc fallback; (d) 2.0 system-job deployment drives `Ready`. Use a fake Nomad client. | Tests pass in CI; cover the divergence matrix in §8. |
| W8 ☐ | `internal/nomad/client.go` | Audit remaining 2.0 `api` deltas the operator touches (job submission response shape, node stub fields). Reconcile any change against `contract.go`. | `contract.go` compiles against pinned 2.0.x with no edits, or edits are intentional + documented. |

## 7. Definition of Done

- **Every binding in §0 has been through at least 2–3 research/verify rounds** and
  is documented with its confirmed Nomad-API source/sink and update semantics.
- `make nomad-pin && make generate && make verify` passes against a pinned 2.0.x.
- `kubectl get nomadjobs` shows correct `RECONCILED`/`READY`/`NOMAD` for: a service
  job, a 2.0 system job with deployment, and a bare system job.
- Node-introduction state is surfaced (W5), not silently mismapped.
- Envtests (W7) green; the §8 matrix is covered.
- README + this ripsheet reflect final behavior.

## 8. Test matrix (Reconciled × Ready)

| Scenario | Reconciled | Ready | Driven by |
|---|---|---|---|
| Service job, deployment successful | True | True | deployment |
| Service job, deployment failed | True | False | deployment |
| New generation fails to register, old version healthy | False | True (unchanged) | deployment (stale-but-true) |
| 2.0 system job + update strategy, all healthy | True | True | deployment |
| Bare system job, all allocs running | True | True | alloc fallback |
| Bare system job, one alloc failed | True | False | alloc fallback |
| Pool resolves to zero nodes | False (`PoolEmpty`) | unchanged | n/a |

## 9. Risks & open questions

- **Q1 (W6):** Do we want system jobs to *produce* deployments by emitting an
  `update` stanza? Pro: richer rollout/health signal on 2.0. Con: changes rollout
  semantics for edge daemons. **Default: no**, keep bare system jobs on the alloc
  fallback unless the user opts in.
- **Q2:** Local jobspec validation via `jobspec2` (heavy dep) vs current
  server-side `ParseHCL`. **Default: server-side**; revisit only if offline render
  tests become necessary.
- **Q3 (W5):** Exact node statuses 2.0 introduces for node introduction/identity
  must be confirmed against a live 2.0 cluster or the 2.0 `api` types before
  mapping — do not guess.
- **Q4:** Confirm `LatestDeployment` populates `DesiredTotal`/`HealthyAllocs` for
  system-job deployments the same way it does for service jobs; adjust the rollup
  in `client.go` if 2.0 uses different `DeploymentState` fields for system jobs.

## 10. Agent kickoff sequence

> Run every step under the **§0 research-and-verify loop**. Each binding you touch
> gets its own rounds of read-real-types → verify-live → map → re-verify; do not
> mark a binding done on the first pass.

1. `make nomad-pin NOMAD_API_VERSION=v2.0.3` (or `@latest`), then `make verify`.
   Resolve any `contract.go` compile breakage first — that is the signature gate.
2. W5 then W8 (confirm 2.0 API shapes against the pinned types **and a live 2.0
   cluster**, per §0).
3. W7 envtests to lock §8.
4. W6 only if Q1 is answered "yes."

