# NomadJob — Design

**Type:** design · **Date:** 2026-07-14 · **Status:** proposed
**Feature:** slice 5 — a `NomadJob` CRD that declaratively manages a Nomad **job** on a `NomadCluster`: the user authors a Nomad jobspec in the CR, the operator submits it (`Jobs().Register`, an upsert) and removes it (`Jobs().Deregister`).

Follows slice 2 (NomadCluster HA control plane), the external-access-modes follow-up, slice 3 (NomadNode), and slice 4 (NomadPool, merged `df1b23c` — current `main` HEAD). Slice 4's `NomadPool` manages the pools a job targets by name; this slice manages the jobs themselves. `NomadJob` reuses NomadPool's **managed-lifecycle** shape almost exactly — the meaningful divergence is the **spec surface** (§3.1) and a richer, runtime-observing **status** (§3.6).

Every Nomad-domain claim below is grounded in `go doc` against the pinned `github.com/hashicorp/nomad/api` (`v0.0.0-20260707172059-5b83b133998a`, == v2.0.4) and verified with a live marshal/unmarshal round-trip during brainstorming — not assumed from training.

> **Amended 2026-07-14** after an independent sr-go-engineer *design* review (Fable model), verdict *amend-before-planning* — no blocking API-fidelity errors (every Nomad-Jobs-API claim re-confirmed against **both** the pinned `api` source and a freshly-pulled HashiCorp reference; architecture, Option B, Plan-gating, finalizer reuse, and the `NomadJobOps` seam verified sound). Folded corrections: **I-1** — the §1.3 authoring claim was incomplete: `api.Job`'s `time.Duration` fields (`update.minHealthyTime`, task timeouts, reschedule/migrate intervals) decode as **nanosecond integers** and *reject* HCL-style strings like `10s` under strict decode (live-proven); the accepted-cost paragraph and the tests now call this out. **I-2** — strict-decode must propagate the raw decoder error into the `InvalidJobSpec` condition message (a typo and a wrong-scalar-type otherwise look identical), and the tests cover both classes. **M-1** — `ValidateJob` dropped: `Plan` (`/v1/job/<id>/plan`) already runs server-side validation, so a separate `Validate` call is a redundant round-trip (resolves open item §6.6). **M-2** — §3.4 now states drift keys on `Diff.Type` only and ignores `FailedTGAllocs` (an infeasible-but-changed job must still re-register). **M-3** — the operator injects `job.Region = nc.Spec.Region` alongside `job.ID` (resolves §6.7 and removes a spurious-`Edited` churn risk). The review also resolved open items §6.2 (Info 404 shape — reuse `errors.AsType`), §6.5 (`Plan` on an unregistered job returns `Added`, not an error), §6.6, and §6.7, shrinking the plan-time list to §6.1/§6.3/§6.4.

---

## 1. Background & framing

A Nomad **job** is the unit of work: a declarative desired-state document (task groups → tasks → drivers, resources, services, networks, constraints, update strategy, …) that Nomad schedules and runs. It is the most complex object in the Nomad data model. A user authors it, an admin submits it via `Jobs().Register`, and Nomad continuously reconciles the running allocations toward it.

### 1.1 Managed lifecycle, not representation (the model this rests on)

Like `NomadPool` (slice 4) and unlike `NomadNode` (slice 3), a `NomadJob` is a **managed lifecycle object**: the CR is the **single source of truth**, and the operator brings the declared job into being and tears it down.

| Verb | Owner | Mechanism |
|------|-------|-----------|
| **C**reate | user | write a `NomadJob` CR → operator `Register`s the job onto Nomad |
| **R**ead | user | `kubectl get nomadjobs` — declared jobs + live status/alloc counts |
| **U**pdate | user | edit `spec.job` → operator re-`Register`s (drift-gated, §3.4) |
| **D**elete | user | `kubectl delete` → operator `Deregister`s the Nomad job (finalizer-gated, §3.5) |

The operator owns nothing it did not declare: jobs submitted out-of-band (CLI/Terraform/CI) are invisible to the operator. A `NomadJob` was considered as a *representation* (mirror out-of-band jobs read-only) and as a *hybrid* (submit + deep runtime mirror); both were rejected in brainstorming. Representation is a weak fit (jobs are human-authored specs like pools, and read-only mirroring duplicates `nomad job status`); hybrid invites scope creep toward reimplementing `nomad job status`. Managed-lifecycle with a **bounded** runtime status (§3.6) is the chosen middle.

### 1.2 Nomad Jobs-API facts this design rests on (verified against the pinned `api`)

- **The endpoint is `Client.Jobs() *Jobs`**, with: `Register(job *Job, w) (*JobRegisterResponse, *WriteMeta, error)` (an **upsert** — the same call creates and updates; there is no separate Create/Update), `Deregister(jobID string, purge bool, w) (evalID string, *WriteMeta, error)`, `Info(jobID string, q) (*Job, *QueryMeta, error)`, `Plan(job *Job, diff bool, w) (*JobPlanResponse, *WriteMeta, error)` (dry-run — computes what a `Register` *would* change, writes nothing), `Summary(jobID string, q) (*JobSummary, *QueryMeta, error)`, and `List`/`Validate`/`ParseHCL`.
- **`api.Job` is a large, deeply-nested struct** — `ID`, `Name`, `Type`, `Datacenters`, `NodePool`, `TaskGroups []*TaskGroup` (each with `Tasks []*Task`, `Networks`, `Services`, …), `Update`, `Constraints`, `Meta`, plus server-set fields `Status`, `Version`, `ModifyIndex`, `JobModifyIndex`, `SubmitTime`, `Stable`. Its fields carry **218 `hcl:` tags and effectively no usable `json:` tags**, so it JSON-marshals to **PascalCase** Go field names — this is exactly what `Register` POSTs to `/v1/jobs` and what `nomad job run -output` emits.
- **`Register` submits the `*Job` struct as JSON** — a structured JSON/YAML representation that unmarshals to `api.Job` can be Registered with **no parse step and no server round-trip to build it**. (`ParseHCL` — the HCL path — is **server-side**, PUTing `/v1/jobs/parse`; it is *not* used by this design. See §5 "HCL input".)
- **`Job.Canonicalize()`** fills server defaults client-side (`Region="global"`, `Namespace="default"`, `Priority=50`, per-group/task defaults).
- **`JobPlanResponse.Diff *JobDiff` carries `Type string`** (`"None"`/`"Added"`/`"Edited"`/`"Deleted"`) — the drift signal for compare-before-write (§3.4).
- **`JobSummary.Summary map[string]TaskGroupSummary`** (per task-group `Running`/`Starting`/`Queued`/`Failed`/`Complete`/`Lost`/`Unknown` counts) — the universal, all-job-types source for runtime status (§3.6).

### 1.3 The spec-surface decision (the load-bearing choice, brainstormed)

How the user expresses the jobspec inside the CR was the pivotal decision. Three shapes were weighed:

- **(A) Raw HCL string** — `spec.jobHCL` holds native HCL2; operator parses via server-side `ParseHCL`. Full fidelity, native authoring, but the spec is an opaque string, parse needs a reachable cluster, and it carries the HCL parse coupling.
- **(B) Structured `api.Job` passthrough, authored as YAML** — **chosen.** `spec.job` is the `api.Job` structure as YAML; the operator unmarshals it directly to `api.Job` and `Register`s. Full fidelity, no hand-synced mirror, no HCL, no server-side parse.
- **(C) Curated structured subset** — hand-typed Go structs mirroring a chosen slice of `api.Job`. K8s-native (CEL, structural schema) but a perpetual DRY/maintenance wall and low fidelity as `api.Job` evolves. Rejected.

A generic "YAML→HCL" library was investigated and rejected: HCL2 is a *language* (block labels carry job/group/task identity; expressions, interpolation, variables) that no schema-blind converter can emit as valid Nomad HCL — owning a Nomad-schema-aware translator *is* option C's wall relocated. The industry norm (Terraform `nomad_job`, `nomad job run`, Levant, Ansible) is **string passthrough of the native jobspec**; option B is the structured, K8s-file-native form of the same idea.

**Two facts verified by live round-trip make B strong** (both re-checked by the SGE review):
1. **camelCase authoring works.** `encoding/json` (through `sigs.k8s.io/yaml`, already vendored at v1.6.0) matches struct fields **case-insensitively**, so a user writes natural `taskGroups:` / `nodePool:` / `memoryMB:` and it maps to the PascalCase `api.Job` fields — deeply nested (`taskGroups[].networks[].dynamicPorts[].label`, `tasks[].resources.cpu`) all resolve. The feared "ugly PascalCase authoring" cost does not materialize.
2. **The residual cost — silent field-drop on a typo — is mitigable.** `encoding/json` silently ignores unknown keys, so `taskGropus:` (typo) drops with no error. **`DisallowUnknownFields()` catches exactly this** (`json: unknown field "taskGropus"`), so the operator strict-decodes and surfaces typos as a condition (§3.3).

The accepted, un-mitigated costs of B:
- **Opaque to the CRD structural schema** — no per-field CEL, `kubectl explain spec.job` shows nothing. Validation is reconcile-time (strict decode, plus `Plan`'s server-side validation in §3.4), not admission-time.
- **Duration fields must be nanoseconds, not HCL strings (SGE I-1).** `api.Job`'s `time.Duration` fields — `update.minHealthyTime`, task `timeouts`, `reschedule`/`migrate` intervals — decode through `encoding/json` as integer **nanoseconds**. So a user writes `minHealthyTime: 10000000000`, not the native Nomad `10s`; under `DisallowUnknownFields` strict decode, `10s` is actively **rejected** (`cannot unmarshal string into … time.Duration`, live-proven). This is the one place the "natural authoring" of Option B genuinely leaks the Go struct — it is documented (§3.1 note, runbook) and exercised by a test (§8), not papered over.

This is the deliberate trade for full fidelity with zero mirror maintenance.

---

## 2. Scope of this slice

**In scope:**
- The `NomadJob` CRD (`nomad.operator.io/v1alpha1`, namespaced), managed-lifecycle, **Option B** spec surface (`spec.job` = `api.Job` as a schemaless `RawExtension`).
- A `NomadJob`-keyed reconciler that strict-decodes `spec.job`, drift-gates via `Plan`, `Register`s/`Deregister`s the job, and derives a bounded runtime status.
- A finalizer that `Deregister`s (`purge=true`) on CR delete, with the NomadPool §3.4 cluster-gone-or-going short-circuit reused verbatim.
- Additive `internal/nomad.Client` job methods + `contract.go` pins, each backed by a real call (§4).
- Reuse of the slice-3 `clusterNomadConfig` helper; a **new** `NomadJobOps` consumer interface + fake (§4).
- envtest coverage with an injected fake job client + a runbook.

**Out of scope (YAGNI; additive later — §5):**
- HCL input (Option A), HCL2 variables, JSON-string input.
- Parameterized/dispatch jobs (`Dispatch`), `Scale`/autoscaling, `Revert`, canary **deployment promotion**, multiregion.
- Deep runtime mirroring (per-allocation state, deployment progress subtree) — the *hybrid* status.
- Vault/Consul token injection, `spec.nomadNamespace`, configurable resync cadence, optimistic-concurrency via `RegisterOptions.EnforceIndex`.

---

## 3. Design

### 3.1 CRD — `nomad.operator.io/v1alpha1`, kind `NomadJob` (namespaced)

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadJob
metadata:
  name: web                        # user-authored, RFC 1123
  namespace: nomad-system          # same namespace as its NomadCluster
  ownerReferences:                 # set by the operator to its NomadCluster (§3.5)
    - {apiVersion: nomad.operator.io/v1alpha1, kind: NomadCluster, name: prod}
spec:
  clusterRef:
    name: prod                     # NomadCluster in this namespace (immutable)
  jobID: web                       # EXACT Nomad job ID (immutable); see below
  job:                             # api.Job as YAML (camelCase or PascalCase); schemaless
    type: service
    datacenters: [dc1]
    nodePool: gpu-workers
    update:
      maxParallel: 1
      minHealthyTime: 10000000000  # NANOSECONDS (= 10s) — time.Duration decodes as int ns, NOT "10s" (SGE I-1)
    taskGroups:
      - name: app
        count: 3
        tasks:
          - name: server
            driver: docker
            config: {image: nginx:1.27, ports: [http]}
            resources: {cpu: 200, memoryMB: 128}
status:
  jobStatus: running               # observed — Info().Status
  jobVersion: 4                    # observed — Info().Version
  groups:                          # from Summary().Summary + desired counts from spec
    app: {running: 3, desired: 3}
  observedGeneration: 7
  conditions:
    - {type: Ready, status: "True"}          # job registered onto Nomad
    - {type: DeleteBlocked, status: "False"} # present only during a finalizer-blocked delete
```

**Field-level decisions:**

- **`spec.jobID` is a separate immutable top-level field (D1).** CEL cannot reference *into* a schemaless `RawExtension` (structural schema required for CEL), so job identity and its immutability must live at top level — mirroring NomadPool's `poolName`/`metadata.name` split. `spec.jobID` is the exact Nomad job ID; the operator **injects it authoritatively** (`job.ID = spec.jobID`) after decoding, so a `job.id` inside the blob is unnecessary and, if present and mismatched, is rejected (§3.3). This gives an admission-visible, immutable identity and a printer column without decoding the blob at admission.
- **`spec.job` is a schemaless `runtime.RawExtension`** (`+kubebuilder:validation:Type=object`, `+kubebuilder:pruning:PreserveUnknownFields`, `+kubebuilder:validation:Schemaless`). The API server stores it as JSON; the operator decodes `RawExtension.Raw` to `api.Job` (§3.3).
- **`spec.clusterRef`** names a `NomadCluster` in the same namespace (immutable); one CR = one job on one cluster; +ownerReference for GC cascade (§3.5). Same as NomadPool.
- **No `phase` enum.** A `Ready` condition + `observedGeneration` + the observed `jobStatus` suffice (follows NomadPool/NomadNode, not NomadCluster's phase machine).
- **Printer columns:** `NAME`, `CLUSTER` (`spec.clusterRef.name`), `JOB` (`spec.jobID`), `STATUS` (`status.jobStatus`), `READY` (`Ready` condition), `AGE`.

**CEL validation** (only top-level fields — the blob is schemaless):
- `spec.jobID` immutable (`self == oldSelf`).
- `spec.clusterRef.name` immutable.
- `spec.jobID` matches Nomad's job-ID pattern (assumed `^[a-zA-Z0-9-_.]{1,...}$`; **exact regex verified at plan time**, §6).

### 3.2 Reconciler — the `NomadJob`-keyed managed loop

`SetupWithManager`: `For(&NomadJob{})`, `Watches(&NomadCluster{}, → jobs with that clusterRef)` (a cluster becoming `Ready` re-reconciles its pending jobs), `Named("nomadjob")`, `RequeueAfter: 60s` steady-state resync. Coexists with the slice-2 `For(&NomadCluster{})` reconciler, slice-3 reflector, and slice-4 pool controller as an independent `Named` controller (workqueue-isolated). Mirrors `nomadpool_controller.go`.

**Normal reconcile** (CR not being deleted; `clusterRef` resolves and is `Ready`):
1. **Ensure finalizer**; set the ownerReference to the `NomadCluster` (write only when it changes — the NomadPool M-1 pattern).
2. **Build the client** via the shared `clusterNomadConfig` helper; set `WriteOptions.Region = nc.Spec.Region` on all calls (§3.7).
3. **Decode + validate** `spec.job` → `desired api.Job` (§3.3). On failure → `Ready=False, reason=InvalidJobSpec`; requeue.
4. **Drift-gate + apply** (§3.4) — `Plan(desired)`; `Register` only if the plan shows changes. `Ready=True` on success.
5. **Derive status** (§3.6) — `Info(jobID)` → `jobStatus`, `jobVersion`; `Summary(jobID)` → per-group running counts; `observedGeneration`.

**Cluster not resolvable / not Ready** (identical to NomadPool):
- `clusterRef` **NotFound** → `Ready=False, reason=ClusterNotFound`; requeue; existing `status` left stale.
- `clusterRef` exists but **not `Ready`** → `Ready=False, reason=ClusterNotReady`; requeue (the API can't be reached, so no `Register`).

### 3.3 Decode & validation — strict, reconcile-time (D2)

`spec.job` is opaque to the API server, so validation happens in the reconciler:

- **Strict decode (D2):** unmarshal `spec.job.Raw` (JSON) into `api.Job` with `DisallowUnknownFields()`. Two failure classes flow through this one path: an **unknown key** (typo, e.g. `taskGropus`) and a **wrong scalar type** (e.g. `count: "three"`, or the I-1 duration case `minHealthyTime: 10s`). Both surface as `Ready=False, reason=InvalidJobSpec`, and the condition **message carries the decoder's `err.Error()` verbatim** (SGE I-2) so the user can tell a typo from a type mismatch. Strict decode is the sole guard against the silent-field-drop a plain unmarshal allows (§1.3). Because the operator's `api.Job` is pinned to the same Nomad version the cluster runs, strict decoding rejects genuine typos without rejecting valid fields.
- **Identity & region injection (D1, SGE M-3):** set `job.ID = spec.jobID` and `job.Region = nc.Spec.Region` (both `*string`, set as pointers). If the decoded blob carried a non-empty `id` that differs from `spec.jobID` → `Ready=False, reason=JobIDMismatch` (invalid states unrepresentable — `spec.jobID` is authoritative). Injecting `job.Region` in the same step (rather than only `WriteOptions.Region`, §3.7) keeps the stored and submitted region identical, so `Plan` (§3.4) never reports a spurious `Edited` from a region default drift.
- **Server-side validation is via `Plan`, not a separate `Validate` call (SGE M-1).** `Plan` (§3.4, `/v1/job/<id>/plan`) already runs Nomad's scheduler-side validation as part of the dry-run, surfacing semantic errors (bad driver config, constraint typos) with Nomad's own diagnostics. A distinct `Jobs().Validate` call before every `Plan` would be a redundant round-trip, so it is **not** in the client seam (§4).

### 3.4 Apply rule — drift-gated `Register` via `Plan` (labeled decision, D3)

`Register` is an upsert over the whole `*Job`. Re-registering an unchanged job every 60s resync would risk churning `Version`/spawning evaluations. Rather than hand-diff the giant `api.Job` struct (fragile, and it would diff server-set fields like `ModifyIndex`), **delegate drift detection to Nomad**:

- `Plan(desired, diff=true)` → inspect `JobPlanResponse.Diff.Type`. `"None"` → no `Register` (in sync). `"Added"`/`"Edited"` → `Register(desired)`. On a not-yet-registered job `Plan` returns `Diff.Type="Added"` (SGE-confirmed against a recent reference — not an error), so create and update are drift-gated by the same predicate.
- **Drift keys on `Diff.Type` only; `FailedTGAllocs` is ignored (SGE M-2).** `Plan` runs a scheduler dry-run, so a *changed but currently-unplaceable* job (e.g. a `nodePool` with zero eligible nodes, or a resource-infeasible edit) populates `JobPlanResponse.FailedTGAllocs` while still returning a valid `Diff.Type`. The operator must **not** gate `Register` on feasibility — the user may be editing the job precisely to fix the infeasibility, and refusing the write would wedge them. Feasibility is Nomad's runtime concern, surfaced later via `status.jobStatus`/group counts, never a reason to withhold the declared spec.
- `PlanJob`/`Plan` require a non-nil `job.ID` (`PlanOpts` errors `"job is missing ID"` on a nil ID — SGE M-4); the reconcile order guarantees this since identity injection (§3.3) runs before `Plan`, and it sets the `*string` pointer.
- `Register`'s `JobRegisterResponse.Warnings` (deprecations, etc.), when non-empty, are surfaced as a `Normal` event so the user sees them.

This is the No-Wall/DRY choice: Nomad owns the canonical job comparison, so the operator never maintains a struct differ. Cost: one read-only `Plan` call per resync.

**Plan-time spike (mirrors NomadPool I-2).** Whether `Register` *already* self-dedups an identical job (no new `Version`/eval on an unchanged submit) is **server-side and not derivable from the pinned `api`**. Resolve it with a real-Nomad spike *before* planning: if `Register` reliably no-ops identical jobs, the `Plan` gate can be dropped for KISS (always `Register`, let the server dedup); if it churns, `Plan`-gating stays. The design assumes `Plan`-gating is needed and treats its removal as a spike-driven simplification.

**Alternative (not chosen):** optimistic concurrency via `RegisterOptions.EnforceIndex + ModifyIndex` (reject the write if the server job moved under us). Deferred — it guards against concurrent out-of-band edits, which the managed model already discourages; `Plan`-gating covers the steady-state drift case without index bookkeeping.

### 3.5 Lifecycle, finalizer & deletion (D4)

Deleting a `NomadJob` has a real external side-effect (stopping a running job) that must be confirmed, so — like `NomadPool` — it uses a **finalizer**. The **entire finalizer path reuses NomadPool §3.4 verbatim** (that logic is already merged and battle-tested; DRY — same shape, different verb):

- `clusterRef` **NotFound, or the `NomadCluster` is being deleted** (`DeletionTimestamp != nil`) → **drop the finalizer without calling `Deregister`.** Control plane gone/going ⇒ the job is gone/going too. This single predicate makes the ownerReference cascade safe under **both** background and foreground cascade (the NomadPool foreground-deadlock fix).
- `clusterRef` present, not deleting, but **unreachable/not `Ready`** → keep the finalizer; `DeleteBlocked=True, reason=ClusterNotReady`; requeue (don't orphan on a blip).
- `clusterRef` **`Ready`** → `Deregister(jobID, purge=true)`:
  - success, or job **already absent** (404, via `nomad.IsNotFound`) → **drop the finalizer.**
  - other transient error → keep the finalizer; `DeleteBlocked=True, reason=DeregisterFailed`; requeue.

**`purge=true` (D4).** The CR going away means the job **should not exist**. `Deregister(purge=false)` leaves a `dead` job *record* (visible in `nomad status`, GC'd later) whose ID would collide with a future re-create of the same `NomadJob`; `purge=true` removes it cleanly. Unlike a node pool, a Nomad job Deregister is **never refused for being "non-empty"** — stopping a job stops its allocations — so there is no `PoolNotEmpty`-style blocked branch; `DeleteBlocked` here only ever means "cluster unreachable" or "transient Deregister error."

**Orphan premise (acknowledged).** The short-circuit rests on "cluster CR gone/going ⇒ control plane gone," which is false under `--cascade=orphan` or a manual CR removal while the StatefulSet runs (the running job is then orphaned — non-destructive, re-appliable). This is the **same premise NomadNode/NomadPool already rely on** — a consistent project stance.

**Conditions:** `Ready`, `DeleteBlocked` (reasons `ClusterNotReady`, `DeregisterFailed`), `ClusterNotFound`/`ClusterNotReady` (normal reconcile), `InvalidJobSpec`/`JobIDMismatch` (decode, §3.3).

### 3.6 Status — bounded runtime observation (D5)

Managed-lifecycle, *not* hybrid: surface enough to answer "is my job up?" without reimplementing `nomad job status`.

- `status.jobStatus` — `Info().Status` (`running`/`pending`/`dead`).
- `status.jobVersion` — `Info().Version` (observed server version; distinct from `observedGeneration`, which tracks the CR).
- `status.groups[name] = {running, desired}` — `running` from `Summary().Summary[name].Running`; `desired` from the decoded `desired.TaskGroups[i].Count`. `Summary` is the universal source (all job types), avoiding per-allocation enumeration.
- `status.observedGeneration`, `status.conditions[Ready]`.

Deferred (YAGNI): per-allocation state, deployment progress (`LatestDeployment`), per-task health, eval history — the deep mirror belongs to `nomad job status`, and the user chose *managed*, not *hybrid*.

### 3.7 Region & Nomad namespace

`Canonicalize()` defaults `Region="global"`. The operator sets `WriteOptions.Region = nc.Spec.Region` on every `Register`/`Deregister`/`Plan`/`Info`/`Summary` call (consistent with `clusterNomadConfig`'s region-scoped `TLSServerName`) **and** injects `job.Region = nc.Spec.Region` during identity injection (§3.3, SGE M-3), so the job targets the cluster's region regardless of the blob's `region` and the stored/submitted regions never diverge (no spurious `Plan` `Edited`). The Nomad **namespace** (distinct from the K8s namespace) is left to the blob (`default` if unset); a `spec.nomadNamespace` override is deferred (§5).

---

## 4. Per-cluster client, `internal/nomad` additions & `contract.go` pins

**Client seam.** The reconciler defines its **own** consumer-side ops interface — `NomadOps` (slice 2), `NomadNodeOps` (slice 3), and `NomadPoolOps` (slice 4) are **not** widened (keeps the controllers' test seams decoupled):

```go
// NomadJobOps is the job reconciler's consumer interface (defined in the controller pkg).
type NomadJobOps interface {
    GetJob(ctx context.Context, jobID string) (*api.Job, error)               // Info; nil,nil on 404
    PlanJob(ctx context.Context, job *api.Job) (changed bool, err error)      // Plan(diff) → Diff.Type != "None"
    RegisterJob(ctx context.Context, job *api.Job) (warnings string, err error) // Register
    DeregisterJob(ctx context.Context, jobID string, purge bool) error        // Deregister
    JobGroupSummary(ctx context.Context, jobID string) (map[string]api.TaskGroupSummary, error) // Summary → .Summary
}
```

Built by a `NewNomadJobClient` factory (faked in envtest). Config via the existing `clusterNomadConfig` helper (DRY). No `ValidateJob` — `Plan` covers server-side validation (SGE M-1, §3.3).

**Additive `internal/nomad.Client` methods**, each backed by a real `api` call, mapping 1:1 to the ops interface: `GetJob` (`Jobs().Info`; `(nil,nil)` on 404 via the `errors.AsType` pattern — SGE-confirmed the same shape `internal/nomad/errors.go` `IsNotFound` already handles), `PlanJob` (`Jobs().Plan`, reads `Diff.Type`), `RegisterJob` (`Jobs().Register`, returns `.Warnings`), `DeregisterJob` (`Jobs().Deregister`), `JobGroupSummary` (`Jobs().Summary`, returns `.Summary`).

### 4.1 `contract.go` additions (backed by real calls)

Pin rule (from Foundation): only pin symbols a real call exercises.

- **Accessor pin:** `(*api.Client).Jobs`.
- **Method pins:** `(*api.Jobs).Info`, `.Plan`, `.Register`, `.Deregister`, `.Summary`.
- **Type pins:** `api.Job` (named in `GetJob`/`PlanJob`/`RegisterJob`), `api.JobPlanResponse` + `api.JobDiff` (named reading `Plan(...).Diff.Type`), `api.JobRegisterResponse` (named reading `Register(...).Warnings`), `api.JobSummary` + `api.TaskGroupSummary` (named in `JobGroupSummary`).
- **Not pinned:** `(*api.Jobs).Validate`/`api.JobValidateResponse` (dropped — SGE M-1), `api.JobListStub` (`List` not used), `api.Deployment` (deferred status), `api.RegisterOptions`/`api.DeregisterOptions` (plain `Register`/`Deregister` used), `ParseHCL`/`JobsParseRequest` (Option A not built). Pinning any would reintroduce the existence-only-pin risk.

**`config/crd/kustomization.yaml`.** Manually add `- bases/nomad.operator.io_nomadjobs.yaml` to `resources:` (the slice-3 `6c3e0c1` lesson — `controller-gen` regenerates the base but not the list; `make deploy` silently omits it otherwise).

**`cmd/main.go`.** Register the `NomadJobReconciler`.

---

## 5. Explicitly not built (YAGNI)

- **HCL input (Option A)** — `spec.jobHCL` + server-side `ParseHCL`. Additive later as an alternative input field; the reconcile path downstream of "have an `api.Job`" is unchanged, so it is a clean seam, not a rewrite.
- **HCL2 variables / JSON-string input** — no present consumer under Option B.
- **Parameterized & dispatch jobs** — a parameterized job needs `Dispatch` to actually run; a different lifecycle (payload, idempotency token) with no present requirement.
- **`Scale` / autoscaling, `Revert`, deployment promotion (canary)** — imperative actions; the CR is declarative desired-state. Additive as explicit sub-resources or spec fields later.
- **Multiregion, `RegisterOptions.EnforceIndex` optimistic concurrency** — deferred (§3.4 alternative).
- **Deep runtime status (hybrid)** — per-alloc/deployment mirror (§3.6).
- **`spec.nomadNamespace`, Vault/Consul token injection, configurable resync** — niche; additive.
- **Periodic/batch jobs** register through the same path (no special handling), but their *status nuances* (periodic child launches, batch completion semantics) are not specially surfaced in v1 — `jobStatus` reflects whatever `Info` reports.

---

## 6. Open items / assumptions to verify at plan/implementation time

Still open (verify at plan/implementation time):

1. **Job-ID regex** — confirm Nomad v2.0.4's exact validation pattern for the `spec.jobID` CEL rule (not validated client-side in the `api` package — it is server-side in `structs` — so it stays a plan-time item).
3. **`Register` self-dedup (pre-plan spike — load-bearing for §3.4).** Does re-registering an identical job create a new `Version`/eval, or no-op? Determines whether `Plan`-gating is required or can be dropped for KISS. Resolve on a real Nomad before planning (the NomadPool I-2 spike pattern); resolved in the §8 integration test.
4. **`Deregister` on a missing job** — confirm it returns success or a 404 (`nomad.IsNotFound`), not an opaque error, so the finalizer's already-gone branch is reliable.

Resolved by the SGE review (no longer open):

2. ~~`Jobs().Info` 404 shape~~ — **confirmed** `api.UnexpectedResponseError{StatusCode: 404}`, the exact shape `internal/nomad/errors.go` `IsNotFound` already handles; `GetJob` returns `(nil, nil)` reliably.
5. ~~`Plan` on a not-yet-registered job~~ — **confirmed** returns `Diff.Type="Added"` (create), not an error; the create path is drift-gated uniformly (§3.4).
6. ~~`Validate` vs `Plan` overlap~~ — **confirmed** `Plan` runs server-side validation; `ValidateJob` dropped (SGE M-1, §3.3/§4).
7. ~~`WriteOptions.Region` vs `job.Region`~~ — **resolved** by injecting `job.Region = nc.Spec.Region` (SGE M-3, §3.3/§3.7), removing the divergence entirely.

---

## 7. Definition of Done

- `NomadJob` CRD + the managed reconciler implemented; `make manifests generate fmt vet` and `make test` green (zero regen drift).
- Creating a `NomadJob` decodes `spec.job` and `Register`s the job onto Nomad; `kubectl get nomadjobs` shows `STATUS`/`READY` and per-group `running/desired`.
- camelCase **and** PascalCase `spec.job` authoring both round-trip to the same `api.Job`; a typo'd/unknown key is rejected with `InvalidJobSpec` (strict decode); a `job.id` that disagrees with `spec.jobID` is rejected with `JobIDMismatch`.
- Editing `spec.job` re-`Register`s only when `Plan` shows a change (no redundant `Register` when unchanged); `Register` warnings are surfaced as an event.
- `spec.jobID`/`clusterRef` are immutable (CEL); `jobID` matches the job-ID pattern.
- Deleting a `NomadJob` `Deregister`s (`purge=true`) and completes; already-absent job → completes; cluster unreachable during delete → holds in `Terminating` with `DeleteBlocked` until reachable.
- Under **both** background and foreground cascade, deleting the `NomadCluster` cascade-deletes its `NomadJob` CRs without a stuck-`Terminating` (the cluster-gone-or-going short-circuit); a transient cluster blip during a standalone `NomadJob` delete does not orphan.
- `contract.go` compiles against the pinned `api` with every new pin backed by a real call.
- envtest coverage (fake job client) for: decode (camel/pascal/strict-reject/id-mismatch), drift-gated register (Plan None→skip, Edited→register), status derivation (jobStatus/version/group counts), `Ready`/`ClusterNotFound`/`ClusterNotReady`, finalizer success / already-gone / cluster-gone-or-going short-circuit (both modes) / cluster-unreachable-blocked, CEL immutability + jobID pattern. Runbook section added.
- `config/crd/kustomization.yaml` lists the `nomadjobs` base; `cmd/main.go` wires the reconciler.

---

## 8. Testing

- **Unit** (`internal/nomad`): each `Client` job method's argument mapping; `GetJob` 404 → `(nil, nil)`; `PlanJob` `Diff.Type` → `changed`; `RegisterJob` warnings passthrough. Plus a decode test proving: camelCase/PascalCase equivalence (byte-identical `api.Job`); `DisallowUnknownFields` **unknown-key** rejection (`taskGropus` → error, SGE I-2); **wrong-scalar-type** rejection (`count: "three"` → error) and that the raw `err.Error()` is what the caller surfaces; and the **duration** contract (SGE I-1) — a job with `update.minHealthyTime` decodes only in integer-nanosecond form and an HCL-style `"10s"` is rejected.
- **envtest** (`internal/controller`): inject a fake `NomadJobOps` returning scripted plan/info/summary data; assert the DoD behaviors. No real Nomad/pods.
- **Integration** (`-tags integration`, hermetic, real Nomad v2.0.4): register a service job, edit it (Plan shows Edited → re-register), delete it (Deregister purge); the containerized harness used to close Foundation open-item #1. Live run deferred if no `nomad` binary is present (as in slices 3/4). This integration test is also where the §6.3 `Register` self-dedup spike is resolved.

---

## 9. Reconcile with the roadmap

- **Depends on slices 2–4:** a `Ready` `NomadCluster` and the per-cluster client wiring (`clusterNomadConfig`). The only cross-slice reuse is that helper + the NomadPool finalizer shape; no slice-2/3/4 interface is widened.
- **Relates to slice 4:** a job targets a pool by name (`job.nodePool`); a managed `NomadPool` gives that reference a K8s-native object, but the coupling is by-name only (no CR-level dependency enforced in v1).
- **Completes the 6-slice roadmap's core CRDs:** Cluster → Node → Pool → Job. Slice 6 (per the roadmap) is follow-up/hardening (e.g. the deferred `status.quorum` real peer count, live integration runs).
- **`v1alpha1` still unreleased:** the new CRD ships without a conversion webhook.
