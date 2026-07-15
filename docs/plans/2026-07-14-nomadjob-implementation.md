# NomadJob Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **REQUIRED EXECUTION WORKFLOW (follow in order):**
> 1. `superpowers:using-git-worktrees` — isolate work in a dedicated worktree
> 2. `superpowers:subagent-driven-development` — dispatch a fresh subagent per task
> 3. `superpowers:test-driven-development` — all subagents use TDD
> 4. `modern-go-guidelines:use-modern-go` — modern Go for the project's Go version (1.26.4)
> 5. `superpowers:verification-before-completion` — verify all tests pass per task
> 6. `superpowers:requesting-code-review` — per-task review (built in)
> 7. After all tasks: independent comprehensive review on the full diff from branch point
> 8. `superpowers:finishing-a-development-branch` — complete the branch
>
> Skills carry their own model and effort settings. Do not override them.

**Goal:** Add a `NomadJob` CRD that declaratively manages a Nomad job on a `NomadCluster` — the user authors a Nomad jobspec in the CR (`spec.job` = the `api.Job` structure as schemaless YAML), the operator submits it via `Jobs().Register` (upsert, drift-gated by `Plan`) and removes it via `Jobs().Deregister(purge=true)`.

**Architecture:** A managed-lifecycle CRD (source of truth = the CR), reconciled by a `NomadJob`-keyed controller that builds a per-cluster Nomad client from the shared `clusterNomadConfig` helper, **strict-decodes** `spec.job` into `api.Job`, injects the authoritative `job.ID`/`job.Region`, **drift-gates** the register via `Plan`, and uses a finalizer to confirm deletion. Follows the slice-4 `NomadPool` seam exactly: a **new** `NomadJobOps` consumer interface + factory + fake, with no widening of `NomadOps`/`NomadNodeOps`/`NomadPoolOps`, and the finalizer reuses NomadPool's cluster-gone-or-going short-circuit.

**Tech Stack:** Go 1.26.4, kubebuilder v4, controller-runtime v0.23.3, k8s v0.35.0, `github.com/hashicorp/nomad/api` pinned at `v0.0.0-20260707172059-5b83b133998a` (== v2.0.4), `sigs.k8s.io/yaml` v1.6.0 (already vendored — but decode uses stdlib `encoding/json` on `RawExtension.Raw`, which is already JSON), envtest, Ginkgo/Gomega for controller tests, plain-Go `testing`+`httptest` for `internal/nomad`.

**Design:** `docs/designs/2026-07-14-nomadjob-design.md` (amended after Fable SGE review).

> **Amended 2026-07-14** after an independent Fable sr-go-engineer *plan* review (verdict *amend-before-execution*; no rework — all API signatures/types, the six Task-5 decode assertions, CRD markers, contract pins, finalizer reuse, and harness accuracy verified sound against the pinned `api` source + a live decode program). Folded: **I-1** (Task 7) — the status test used `f.planChanged = true`, so the fake's `RegisterJob` overwrote the seeded `f.jobs["web"]` with the statusless *desired* job and `GetJob` returned no status → the test now sets `f.planChanged = false` (no Register, seed survives). **M-1** (Task 10) — the integration snippet called a non-existent `newIntegrationClient`; it now uses the real `devAgentWithNode(t)` harness from `client_write_integration_test.go`. **M-2** (Task 1) — `job_test.go` now reuses the existing `newTestClient` from `nodepool_test.go` instead of redefining it. **M-3 (note):** the controller test file accrues `errors` (Task 8) and `apierrors` (Task 8 `assertGoneJob`) imports across tasks — `goimports`/the build gate resolves them; don't be surprised running a single task's tests in isolation. **M-4 (confirmed sound, no change):** the client methods rely on `nomad.New` mapping `cfg.Region → api.Config.Region` (client default) plus `job.Region` injection rather than setting `WriteOptions.Region` per call — functionally equivalent to design §3.7 and DRY-cleaner.

## Global Constraints

- **Per-endpoint Nomad client only.** Build via `clusterNomadConfig` (`internal/controller/nomadclient.go`); never a singleton, never `api.DefaultConfig()`. `nomad.New` maps `cfg.Region`→`api.Config.Region`, so read/write requests default to the cluster region automatically; the reconciler *additionally* injects `job.Region` (SGE M-3).
- **Own consumer ops interface.** Define `NomadJobOps` in the controller package; do **not** widen `NomadOps` (slice 2), `NomadNodeOps` (slice 3), or `NomadPoolOps` (slice 4).
- **Reuse, don't redefine.** `IsNotFound` already exists in `internal/nomad/errors.go` (slice 4) — reuse it for `GetJob`/`DeregisterJob` 404 handling; do not add a second copy. The reason constants `ReasonRegistered`, `ReasonClusterNotFound` (nomadpool_types.go) and `ReasonClusterNotReady` (nomadnode_types.go) already exist in package `v1alpha1` — reuse them; declaring duplicates is a compile error.
- **contract.go pins must be backed by real calls.** Every newly pinned `api` symbol must be exercised by concrete operator code in the same task (existence-only-pin gotcha). All job pins land in Task 1, backed by the five `Client` methods.
- **Hand-authored CRD ⇒ manually wire kustomization.** After generating the CRD base, add `- bases/nomad.operator.io_nomadjobs.yaml` to `config/crd/kustomization.yaml` `resources:` (controller-gen regenerates the base but not the list; `make deploy` silently omits the CRD otherwise — the slice-3 `6c3e0c1` lesson). Do this in Task 2.
- **Build gate (run per task before commit):** `make manifests generate fmt vet && make test`. Zero regen drift (a dirty tree after `make manifests generate` fails the task).
- **v1alpha1 is unreleased** — additive CRD changes need no conversion webhook.
- **Signed commits** use the user's 1Password Touch ID. If `git commit` fails with `1Password: failed to fill whole buffer`, stop and ask the user to unlock — do **not** disable `commit.gpgsign`.
- **Verified Nomad v2.0.4 Jobs facts** (grounded via `go doc` + a live round-trip + the Fable SGE review; use verbatim):
  - `Client.Jobs()` → `Info(id,q) (*Job,_,err)`, `Register(*Job,w) (*JobRegisterResponse,_,err)` (**upsert**), `Deregister(id string, purge bool, w) (evalID string,_,err)`, `Plan(*Job, diff bool, w) (*JobPlanResponse,_,err)`, `Summary(id,q) (*JobSummary,_,err)`.
  - `api.Job` fields are `*T` pointers (`ID`, `Name`, `Type`, `Region`, `Version *uint64`, `Status *string`) with `hcl:` tags and no usable `json:` tags → JSON is **PascalCase**, but `encoding/json` matches keys **case-insensitively**, so camelCase authoring works. **`time.Duration` fields decode as integer nanoseconds and reject HCL strings like `"10s"`** (SGE I-1).
  - `JobPlanResponse.Diff *JobDiff`, `JobDiff.Type string` ∈ {`"None"`,`"Added"`,`"Edited"`,`"Deleted"`}; a not-yet-registered job plans as `"Added"` (SGE-confirmed).
  - `JobRegisterResponse.Warnings string`; `JobSummary.Summary map[string]TaskGroupSummary` with `Running/Starting/Queued/Failed/Complete/Lost/Unknown int`.
  - `Info`/`Deregister` on a missing job → `api.UnexpectedResponseError` with `StatusCode()==404` (reuse `IsNotFound`).
  - `PlanOpts` returns `errors.New("job is missing ID")` if `job.ID == nil` — the reconciler injects `job.ID` before `Plan` (SGE M-4).
  - Job-ID regex (Nomad server-side): assume `^[a-zA-Z0-9-_.]{1,128}$`; **verify exact v2.0.4 pattern at implementation time** (design §6.1).

---

## File Structure

| File | Responsibility | Task |
|------|----------------|------|
| `internal/nomad/client.go` (modify) | 5 job `Client` methods | 1 |
| `internal/nomad/contract.go` (modify) | job `api` pins (methods + types) | 1 |
| `internal/nomad/job_test.go` (create) | httptest unit tests for the 5 methods | 1 |
| `api/v1alpha1/nomadjob_types.go` (create) | CRD types, CEL, printer columns, condition consts | 2 |
| `config/crd/kustomization.yaml` (modify) | add nomadjobs base | 2 |
| `internal/controller/nomadjob_ops.go` (create) | `NomadJobOps` interface + factory | 3 |
| `internal/controller/fake_nomadjob_test.go` (create) | scriptable fake | 3 |
| `internal/controller/nomadjob_controller.go` (create) | reconciler + `decodeJob` | 4–8 |
| `internal/controller/nomadjob_decode_test.go` (create) | plain-Go decode unit tests | 5 |
| `internal/controller/nomadjob_controller_test.go` (create) | Ginkgo envtest suite | 4,6,7,8 |
| `internal/controller/nomadjob_crd_test.go` (create) | CEL behavioral tests | 9 |
| `cmd/main.go` (modify) | wire `NomadJobReconciler` | 10 |
| `docs/runbooks/nomadjob.md` (create) | runbook | 10 |
| `internal/nomad/job_integration_test.go` (create) | live register/replan/deregister + self-dedup spike | 10 |

---

## Test Harness (READ FIRST — the per-task snippets are behavior specs, not final code)

The controller tests run under the **existing Ginkgo/Gomega envtest suite** (`internal/controller/suite_test.go`: `TestControllers` → `RunSpecs`). Follow these facts exactly — they were confirmed against the tree and the slice-4 SGE plan review:

- **The envtest client global is `k8s`** (NOT `k8sClient`), a **bare** `client.New(cfg, …)` with **no cache**.
- **Write every controller *reconcile/CEL* test as a Ginkgo `Describe`/`It`** against `k8s`, mirroring `internal/controller/nomadpool_controller_test.go`. Use the Ginkgo node's `SpecContext`, not `context.Background()`. Reuse the existing `readyCluster(ctx, ns)` helper (returns a `*NomadCluster` named `prod`, Phase=Ready) and the existing `mustDelete`, `assertGonePool`→ add a `assertGoneJob` sibling, and `mustCreateTerminatingCluster(ctx, ns)` helpers. **Do NOT** write plain-Go `TestXxx(t)` reconcile tests — they never start envtest, so `k8s` is nil.
- **CRD/CEL tests (Task 9) and every reconcile test MUST use the real envtest apiserver (`k8s`)** — the `fake` client performs no CEL admission.
- **`internal/nomad` tests are plain-Go `testing`** (Task 1) using `httptest`, so a real `api.UnexpectedResponseError` / real JSON responses are exercised with **no Nomad binary**.
- **`decodeJob` is a pure function** — its unit tests (Task 5) are plain-Go `TestXxx(t)` in the controller package that call `decodeJob` directly and never touch `k8s`, so they coexist with the Ginkgo suite without envtest.
- Build the reconciler under test with the fake ops: `&NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}`, then call `Reconcile` with the job's `NamespacedName`.

Each per-task snippet specifies the **behavior to assert**; port it into a Ginkgo `It` using the suite's conventions and helper names.

---

## Task 1: `internal/nomad` job client methods + contract pins

**Files:**
- Modify: `internal/nomad/client.go` (append methods after `CountNodePoolJobs`)
- Modify: `internal/nomad/contract.go`
- Test: `internal/nomad/job_test.go` (create)

**Interfaces:**
- Produces:
  - `func (c *Client) GetJob(ctx context.Context, jobID string) (*api.Job, error)` — `(nil, nil)` on 404.
  - `func (c *Client) PlanJob(ctx context.Context, job *api.Job) (bool, error)` — `true` if applying would change anything (`Diff.Type != "None"`).
  - `func (c *Client) RegisterJob(ctx context.Context, job *api.Job) (string, error)` — returns server warnings.
  - `func (c *Client) DeregisterJob(ctx context.Context, jobID string, purge bool) error`
  - `func (c *Client) JobGroupSummary(ctx context.Context, jobID string) (map[string]api.TaskGroupSummary, error)`

- [ ] **Step 1: Write the failing unit tests**

Create `internal/nomad/job_test.go`. **Reuse the existing `newTestClient(t, h)` helper** already defined in `internal/nomad/nodepool_test.go` (same package — do NOT redefine it; SGE plan-review M-2). It points a **real** `*Client` at an `httptest.Server`:

```go
package nomad

import (
	"net/http"
	"testing"

	"github.com/hashicorp/nomad/api"
)

// newTestClient(t, h) lives in nodepool_test.go (same package) — reuse it.

func TestGetJob_NotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "job not found", http.StatusNotFound)
	})
	job, err := c.GetJob(t.Context(), "missing")
	if err != nil || job != nil {
		t.Fatalf("GetJob 404 = (%v, %v), want (nil, nil)", job, err)
	}
}

func TestPlanJob_ChangedAndNone(t *testing.T) {
	diffType := "Added"
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Diff":{"Type":"` + diffType + `"}}`))
	})
	id := "web"
	changed, err := c.PlanJob(t.Context(), &api.Job{ID: &id})
	if err != nil || !changed {
		t.Fatalf("PlanJob Added = (%v, %v), want (true, nil)", changed, err)
	}
	diffType = "None"
	changed, err = c.PlanJob(t.Context(), &api.Job{ID: &id})
	if err != nil || changed {
		t.Fatalf("PlanJob None = (%v, %v), want (false, nil)", changed, err)
	}
}

func TestRegisterJob_Warnings(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"EvalID":"e1","Warnings":"deprecated foo"}`))
	})
	id := "web"
	warn, err := c.RegisterJob(t.Context(), &api.Job{ID: &id})
	if err != nil || warn != "deprecated foo" {
		t.Fatalf("RegisterJob = (%q, %v), want (\"deprecated foo\", nil)", warn, err)
	}
}

func TestJobGroupSummary(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"JobID":"web","Summary":{"app":{"Running":2,"Starting":1}}}`))
	})
	got, err := c.JobGroupSummary(t.Context(), "web")
	if err != nil {
		t.Fatalf("JobGroupSummary: %v", err)
	}
	if got["app"].Running != 2 || got["app"].Starting != 1 {
		t.Fatalf("summary = %+v, want app{Running:2,Starting:1}", got)
	}
}

func TestDeregisterJob_OK(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"EvalID":"e1"}`))
	})
	if err := c.DeregisterJob(t.Context(), "web", true); err != nil {
		t.Fatalf("DeregisterJob: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/nomad/ -run 'TestGetJob_NotFound|TestPlanJob_ChangedAndNone|TestRegisterJob_Warnings|TestJobGroupSummary|TestDeregisterJob_OK' -v`
Expected: FAIL — `GetJob`/`PlanJob`/`RegisterJob`/`JobGroupSummary`/`DeregisterJob` undefined.

- [ ] **Step 3: Add the 5 client methods**

Append to `internal/nomad/client.go` (after `CountNodePoolJobs`). Add `const jobDiffNone = "None"` near the top-level consts (or just above `GetJob`):

```go
// jobDiffNone is Nomad's JobDiff.Type value when a Plan finds no changes.
const jobDiffNone = "None"

// GetJob returns the job by ID, or (nil, nil) if it does not exist.
func (c *Client) GetJob(ctx context.Context, jobID string) (*api.Job, error) {
	job, _, err := c.api.Jobs().Info(jobID, queryOpts(ctx))
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("nomad: get job %q: %w", jobID, err)
	}
	return job, nil
}

// PlanJob dry-runs a register and reports whether applying the job would change
// anything (JobDiff.Type != "None"). A not-yet-registered job plans as "Added".
// The job must have a non-nil ID (Nomad's PlanOpts requires it); the caller
// injects it.
func (c *Client) PlanJob(ctx context.Context, job *api.Job) (bool, error) {
	resp, _, err := c.api.Jobs().Plan(job, true, (&api.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return false, fmt.Errorf("nomad: plan job: %w", err)
	}
	if resp.Diff == nil {
		return true, nil // no diff computed → treat as changed (safe: at worst one extra Register)
	}
	return resp.Diff.Type != jobDiffNone, nil
}

// RegisterJob upserts the job (Nomad's Register is an upsert) and returns any
// server warnings (e.g. deprecation notices).
func (c *Client) RegisterJob(ctx context.Context, job *api.Job) (string, error) {
	resp, _, err := c.api.Jobs().Register(job, (&api.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("nomad: register job: %w", err)
	}
	return resp.Warnings, nil
}

// DeregisterJob stops and removes a job. purge=true fully removes the job record
// (vs leaving a queryable dead record that would collide with a re-create).
func (c *Client) DeregisterJob(ctx context.Context, jobID string, purge bool) error {
	if _, _, err := c.api.Jobs().Deregister(jobID, purge, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: deregister job %q: %w", jobID, err)
	}
	return nil
}

// JobGroupSummary returns per-task-group allocation summaries for the job.
func (c *Client) JobGroupSummary(ctx context.Context, jobID string) (map[string]api.TaskGroupSummary, error) {
	summary, _, err := c.api.Jobs().Summary(jobID, queryOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("nomad: job summary %q: %w", jobID, err)
	}
	return summary.Summary, nil
}
```

- [ ] **Step 4: Add contract.go pins (methods + types; all exercised above)**

In `internal/nomad/contract.go`, add to the type-pins block:

```go
	_ api.Job
	_ api.JobPlanResponse
	_ api.JobDiff
	_ api.JobRegisterResponse
	_ api.JobSummary
	_ api.TaskGroupSummary
```

and to the method-pins block:

```go
	_ = (*api.Client).Jobs
	_ = (*api.Jobs).Info
	_ = (*api.Jobs).Plan
	_ = (*api.Jobs).Register
	_ = (*api.Jobs).Deregister
	_ = (*api.Jobs).Summary
```

> Do NOT pin `(*api.Jobs).Validate`/`api.JobValidateResponse` (dropped — SGE M-1: `Plan` validates), `api.JobListStub` (`List` not used), `api.Deployment` (deferred status), or `api.RegisterOptions`/`api.DeregisterOptions` (plain `Register`/`Deregister` used). Each pin above is named by a real call: `Info`→`GetJob`, `Plan`+`JobPlanResponse`+`JobDiff`→`PlanJob`, `Register`+`JobRegisterResponse`→`RegisterJob`, `Deregister`→`DeregisterJob`, `Summary`+`JobSummary`+`TaskGroupSummary`→`JobGroupSummary`, `api.Job`→all.

- [ ] **Step 5: Run tests + build gate**

Run: `go test ./internal/nomad/ -run 'TestGetJob|TestPlanJob|TestRegisterJob|TestJobGroupSummary|TestDeregisterJob' -v && go build ./...`
Expected: PASS; build clean (contract.go compiles — every new pin is exercised).

- [ ] **Step 6: Commit**

```bash
git add internal/nomad/client.go internal/nomad/contract.go internal/nomad/job_test.go
git commit -m "feat(nomad): job client methods (Info/Plan/Register/Deregister/Summary) + contract pins"
```

---

## Task 2: `NomadJob` CRD types + generated manifests + kustomization

**Files:**
- Create: `api/v1alpha1/nomadjob_types.go`
- Modify: `config/crd/kustomization.yaml`
- Generated (by `make manifests generate`): `config/crd/bases/nomad.operator.io_nomadjobs.yaml`, `api/v1alpha1/zz_generated.deepcopy.go`

**Interfaces:**
- Produces: `NomadJob`, `NomadJobSpec`, `NomadJobStatus`, `NomadJobGroupStatus`, `JobClusterRef` types; condition-type/reason constants `NomadJobCondReady`, `NomadJobCondDeleteBlocked`, `ReasonInvalidJobSpec`, `ReasonJobIDMismatch`, `ReasonDeregisterFailed`. (Reuses `ReasonRegistered`, `ReasonClusterNotFound`, `ReasonClusterNotReady` from the same package.)

- [ ] **Step 1: Write the CRD types**

Create `api/v1alpha1/nomadjob_types.go`:

```go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NomadJob condition types and reasons. ReasonRegistered/ReasonClusterNotFound
// (nomadpool_types.go) and ReasonClusterNotReady (nomadnode_types.go) are
// declared elsewhere in this package and reused here.
const (
	NomadJobCondReady         = "Ready"
	NomadJobCondDeleteBlocked = "DeleteBlocked"

	ReasonInvalidJobSpec   = "InvalidJobSpec"
	ReasonJobIDMismatch    = "JobIDMismatch"
	ReasonDeregisterFailed = "DeregisterFailed"
)

// JobClusterRef names a NomadCluster in the same namespace.
type JobClusterRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// NomadJobSpec is the desired state of a Nomad job. clusterRef + jobID are the
// immutable identity; job is the full Nomad jobspec. The CR is the source of
// truth: the operator strict-decodes spec.job into api.Job, injects the
// authoritative ID/Region, and Registers it (drift-gated by Plan).
//
// +kubebuilder:validation:XValidation:rule="self.jobID == oldSelf.jobID",message="jobID is immutable"
// +kubebuilder:validation:XValidation:rule="self.clusterRef.name == oldSelf.clusterRef.name",message="clusterRef.name is immutable"
type NomadJobSpec struct {
	// ClusterRef names the NomadCluster (same namespace) this job runs on.
	// +kubebuilder:validation:Required
	ClusterRef JobClusterRef `json:"clusterRef"`
	// JobID is the exact Nomad job ID. It is a separate top-level field (not read
	// from spec.job) because CEL cannot validate into a schemaless RawExtension,
	// so identity and immutability must live here. The operator injects it as the
	// authoritative job.ID; a differing job.id inside spec.job is rejected.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9-_.]{1,128}$`
	JobID string `json:"jobID"`
	// Job is the Nomad jobspec expressed as the api.Job structure (camelCase or
	// PascalCase; keys match api.Job case-insensitively). It is schemaless: the
	// CRD does not model its fields, so there is no per-field validation — the
	// operator strict-decodes it (unknown/typo'd keys are rejected). NOTE:
	// time.Duration fields (e.g. update.minHealthyTime) must be integer
	// nanoseconds, not "10s" strings.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	Job runtime.RawExtension `json:"job"`
}

// NomadJobGroupStatus is the observed vs desired allocation count for one task
// group.
type NomadJobGroupStatus struct {
	// +optional
	Running int `json:"running,omitempty"`
	// +optional
	Desired int `json:"desired,omitempty"`
}

// NomadJobStatus is the observed state, operator-owned.
type NomadJobStatus struct {
	// JobStatus is Nomad's job status (running/pending/dead) from Info.
	// +optional
	JobStatus string `json:"jobStatus,omitempty"`
	// JobVersion is the server-observed job version from Info (distinct from
	// observedGeneration, which tracks the CR).
	// +optional
	JobVersion int64 `json:"jobVersion,omitempty"`
	// Groups maps each task-group name to its running/desired allocation counts.
	// +optional
	Groups map[string]NomadJobGroupStatus `json:"groups,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.spec.jobID`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.jobStatus`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NomadJob is the Schema for the nomadjobs API.
type NomadJob struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec NomadJobSpec `json:"spec"`
	// +optional
	Status NomadJobStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NomadJobList contains a list of NomadJob.
type NomadJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NomadJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NomadJob{}, &NomadJobList{})
}
```

- [ ] **Step 2: Generate manifests + deepcopy**

Run: `make manifests generate`
Expected: creates `config/crd/bases/nomad.operator.io_nomadjobs.yaml` (the `job` property carries `x-kubernetes-preserve-unknown-fields: true` and no nested schema) and regenerates `api/v1alpha1/zz_generated.deepcopy.go` with `NomadJob*` deepcopy funcs (controller-gen deep-copies `runtime.RawExtension` correctly). No errors.

- [ ] **Step 3: Wire the CRD base into kustomization**

Edit `config/crd/kustomization.yaml` — add the nomadjobs base under `resources:` (keep existing entries):

```yaml
resources:
- bases/nomad.operator.io_nomadclusters.yaml
- bases/nomad.operator.io_nomadnodes.yaml
- bases/nomad.operator.io_nomadpools.yaml
- bases/nomad.operator.io_nomadjobs.yaml
# +kubebuilder:scaffold:crdkustomizeresource
```

- [ ] **Step 4: Verify build + no regen drift**

Run: `make manifests generate fmt vet && go build ./... && git status --porcelain config/ api/`
Expected: build clean; re-running generate produces no further diff (only the intended new/changed files).

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/nomadjob_types.go api/v1alpha1/zz_generated.deepcopy.go config/crd/bases/nomad.operator.io_nomadjobs.yaml config/crd/kustomization.yaml
git commit -m "feat(api): NomadJob CRD types (schemaless spec.job), CEL, printer columns; wire kustomization"
```

---

## Task 3: `NomadJobOps` interface + factory + fake

**Files:**
- Create: `internal/controller/nomadjob_ops.go`
- Create: `internal/controller/fake_nomadjob_test.go`

**Interfaces:**
- Consumes: the Task-1 `*nomad.Client` methods.
- Produces: `NomadJobOps` interface; `NomadJobClientFactory` + `DefaultNomadJobClientFactory`; `fakeNomadJobOps` test double.

- [ ] **Step 1: Write the ops interface + factory**

Create `internal/controller/nomadjob_ops.go`:

```go
package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// NomadJobOps is the subset of the Nomad client the job reconciler needs,
// defined at the consumer (Go convention). *nomad.Client satisfies it, and
// envtest injects a fake. It is intentionally separate from NomadOps (slice 2),
// NomadNodeOps (slice 3), and NomadPoolOps (slice 4) so the controllers' test
// seams stay decoupled.
type NomadJobOps interface {
	GetJob(ctx context.Context, jobID string) (*api.Job, error)
	PlanJob(ctx context.Context, job *api.Job) (bool, error)
	RegisterJob(ctx context.Context, job *api.Job) (string, error)
	DeregisterJob(ctx context.Context, jobID string, purge bool) error
	JobGroupSummary(ctx context.Context, jobID string) (map[string]api.TaskGroupSummary, error)
}

// NomadJobClientFactory builds a NomadJobOps from a per-cluster Config.
type NomadJobClientFactory func(cfg nomad.Config) (NomadJobOps, error)

// DefaultNomadJobClientFactory constructs the real client.
func DefaultNomadJobClientFactory(cfg nomad.Config) (NomadJobOps, error) {
	return nomad.New(cfg)
}
```

- [ ] **Step 2: Write the fake**

Create `internal/controller/fake_nomadjob_test.go`:

```go
package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// fakeNomadJobOps is a scriptable NomadJobOps for envtest. Set the fields to
// control behavior and inspect the recorded calls.
type fakeNomadJobOps struct {
	jobs    map[string]*api.Job                // seeded Info state, keyed by jobID
	summary map[string]api.TaskGroupSummary    // JobGroupSummary result
	planChanged bool                           // PlanJob result
	warnings    string                         // RegisterJob warnings

	getErr, planErr, registerErr, deregisterErr, summaryErr error

	registered   []*api.Job // every RegisterJob arg, in order
	deregistered []string   // every DeregisterJob jobID, in order
	purged       []bool     // the purge flag for each DeregisterJob, in order
}

func newFakeJobOps() *fakeNomadJobOps {
	return &fakeNomadJobOps{jobs: map[string]*api.Job{}, summary: map[string]api.TaskGroupSummary{}}
}

func (f *fakeNomadJobOps) GetJob(_ context.Context, jobID string) (*api.Job, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.jobs[jobID], nil // nil == not found, matching the real 404 mapping
}

func (f *fakeNomadJobOps) PlanJob(_ context.Context, _ *api.Job) (bool, error) {
	if f.planErr != nil {
		return false, f.planErr
	}
	return f.planChanged, nil
}

func (f *fakeNomadJobOps) RegisterJob(_ context.Context, job *api.Job) (string, error) {
	if f.registerErr != nil {
		return "", f.registerErr
	}
	cp := *job
	f.registered = append(f.registered, &cp)
	if cp.ID != nil {
		f.jobs[*cp.ID] = &cp
	}
	return f.warnings, nil
}

func (f *fakeNomadJobOps) DeregisterJob(_ context.Context, jobID string, purge bool) error {
	if f.deregisterErr != nil {
		return f.deregisterErr
	}
	f.deregistered = append(f.deregistered, jobID)
	f.purged = append(f.purged, purge)
	delete(f.jobs, jobID)
	return nil
}

func (f *fakeNomadJobOps) JobGroupSummary(_ context.Context, _ string) (map[string]api.TaskGroupSummary, error) {
	if f.summaryErr != nil {
		return nil, f.summaryErr
	}
	return f.summary, nil
}

// factory returns a NomadJobClientFactory that always yields this fake.
func (f *fakeNomadJobOps) factory() NomadJobClientFactory {
	return func(_ nomad.Config) (NomadJobOps, error) { return f, nil }
}

var _ NomadJobOps = (*fakeNomadJobOps)(nil)
```

- [ ] **Step 3: Verify it compiles + satisfies the interface**

Run: `go build ./... && go vet ./internal/controller/`
Expected: clean (the fake satisfies `NomadJobOps`; `*nomad.Client` satisfies it via Task 1).

- [ ] **Step 4: Commit**

```bash
git add internal/controller/nomadjob_ops.go internal/controller/fake_nomadjob_test.go
git commit -m "feat(controller): NomadJobOps consumer interface, factory, and fake"
```

---

## Task 4: Reconciler skeleton — cluster resolution, finalizer, ownerRef, SetupWithManager

**Files:**
- Create: `internal/controller/nomadjob_controller.go`
- Test: `internal/controller/nomadjob_controller_test.go` (create)

**Interfaces:**
- Consumes: Task-2 types, Task-3 `NomadJobOps`/factory, `clusterNomadConfig`, `PhaseReady`.
- Produces:
  - `NomadJobReconciler{ client.Client; Scheme *runtime.Scheme; NewNomadClient NomadJobClientFactory; Recorder record.EventRecorder }`
  - `func (r *NomadJobReconciler) SetupWithManager(mgr ctrl.Manager) error`
  - `const jobResync`, `const nomadJobFinalizer`
  - `func setJobCondition(np *NomadJob, condType string, status metav1.ConditionStatus, reason, msg string)`
  - `func (r *NomadJobReconciler) jobsForCluster(ctx, obj) []reconcile.Request`
  - `func (r *NomadJobReconciler) dropJobFinalizer(ctx, nj) error`
  - stubs `reconcileJob(ctx, nj, ops, region)` and `finalizeJob(ctx, nj)` filled in Tasks 6–8.

- [ ] **Step 1: Write the failing tests (cluster resolution + finalizer)**

Create `internal/controller/nomadjob_controller_test.go`. Per the **Test Harness** section, write as Ginkgo `Describe`/`It` against `k8s`, mirroring `nomadpool_controller_test.go`. Cover:
- **ClusterNotFound** — a `NomadJob` referencing a missing cluster; reconcile; assert `Ready=False/ClusterNotFound`, finalizer added, `observedGeneration == generation`.
- **ClusterNotReady** — a `readyCluster` flipped to `PhaseDegraded`; assert `Ready=False/ClusterNotReady`, `f.registered` empty, `observedGeneration` advanced.
- **ownerRef + requeue** — with a `readyCluster`, after reconcile the `NomadJob` carries a controller ownerReference to the cluster and the result is `ctrl.Result{RequeueAfter: jobResync}`.
- **`jobsForCluster` mapper** — two jobs referencing `prod`, one referencing `other`; `r.jobsForCluster(ctx, prodCluster)` returns exactly the two `prod` job keys.

A `spec.job` value is required (CRD marks it Required). Use a minimal valid one in these tests, e.g.:

```go
minimalJobRaw := runtime.RawExtension{Raw: []byte(`{"type":"service","taskGroups":[{"name":"app","count":1}]}`)}
```

Example `It` (ClusterNotFound), mirroring the NomadPool suite's structure:

```go
It("sets ClusterNotFound and adds the finalizer when the referenced cluster is missing", func(ctx SpecContext) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-notfound-"}}
	Expect(k8s.Create(ctx, ns)).To(Succeed())

	nj := &nomadv1alpha1.NomadJob{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
		Spec: nomadv1alpha1.NomadJobSpec{
			ClusterRef: nomadv1alpha1.JobClusterRef{Name: "missing"},
			JobID:      "web",
			Job:        runtime.RawExtension{Raw: []byte(`{"type":"service","taskGroups":[{"name":"app","count":1}]}`)},
		},
	}
	Expect(k8s.Create(ctx, nj)).To(Succeed())

	f := newFakeJobOps()
	r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
	Expect(err).NotTo(HaveOccurred())

	var got nomadv1alpha1.NomadJob
	Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
	cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadJobCondReady)
	Expect(cond).NotTo(BeNil())
	Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotFound))
	Expect(controllerutil.ContainsFinalizer(&got, nomadJobFinalizer)).To(BeTrue())
	Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `make test`
Expected: FAIL — `NomadJobReconciler` / `nomadJobFinalizer` undefined.

- [ ] **Step 3: Write the reconciler skeleton**

Create `internal/controller/nomadjob_controller.go`:

```go
package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

const (
	jobResync         = 60 * time.Second
	nomadJobFinalizer = "nomad.operator.io/nomadjob-cleanup"
)

// NomadJobReconciler manages Nomad jobs declared as NomadJob CRs. The CR is the
// source of truth: the operator strict-decodes spec.job, Registers it onto Nomad
// (drift-gated by Plan), and Deregisters it (finalizer-gated).
type NomadJobReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NewNomadClient NomadJobClientFactory
	Recorder       record.EventRecorder
}

// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *NomadJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var nj nomadv1alpha1.NomadJob
	if err := r.Get(ctx, req.NamespacedName, &nj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !nj.DeletionTimestamp.IsZero() {
		return r.finalizeJob(ctx, &nj)
	}

	// Ensure finalizer before any external side-effect.
	if !controllerutil.ContainsFinalizer(&nj, nomadJobFinalizer) {
		controllerutil.AddFinalizer(&nj, nomadJobFinalizer)
		if err := r.Update(ctx, &nj); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Resolve the cluster.
	var nc nomadv1alpha1.NomadCluster
	err := r.Get(ctx, types.NamespacedName{Name: nj.Spec.ClusterRef.Name, Namespace: nj.Namespace}, &nc)
	if apierrors.IsNotFound(err) {
		setJobCondition(&nj, nomadv1alpha1.NomadJobCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotFound, "referenced NomadCluster does not exist")
		nj.Status.ObservedGeneration = nj.Generation
		if err := r.Status().Update(ctx, &nj); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: jobResync}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if nc.Status.Phase != nomadv1alpha1.PhaseReady {
		setJobCondition(&nj, nomadv1alpha1.NomadJobCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotReady, "referenced NomadCluster is not Ready")
		nj.Status.ObservedGeneration = nj.Generation
		if err := r.Status().Update(ctx, &nj); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: jobResync}, nil
	}

	// Set the ownerReference for GC cascade, writing only when it actually changes.
	orig := nj.DeepCopy()
	if err := controllerutil.SetControllerReference(&nc, &nj, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if !equality.Semantic.DeepEqual(orig.OwnerReferences, nj.OwnerReferences) {
		if err := r.Update(ctx, &nj); err != nil {
			return ctrl.Result{}, err
		}
	}

	cfg, err := clusterNomadConfig(ctx, r.Client, &nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	ops, err := r.NewNomadClient(cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcileJob(ctx, &nj, ops, nc.Spec.Region)
}

// reconcileJob decodes spec.job, applies it onto Nomad, and derives status.
// Filled in Tasks 6–7.
func (r *NomadJobReconciler) reconcileJob(ctx context.Context, nj *nomadv1alpha1.NomadJob, ops NomadJobOps, region string) (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: jobResync}, nil
}

// finalizeJob handles CR deletion. Filled in Task 8.
func (r *NomadJobReconciler) finalizeJob(ctx context.Context, nj *nomadv1alpha1.NomadJob) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(nj, nomadJobFinalizer)
	return ctrl.Result{}, r.Update(ctx, nj)
}

// dropJobFinalizer removes the cleanup finalizer, allowing GC of the CR.
func (r *NomadJobReconciler) dropJobFinalizer(ctx context.Context, nj *nomadv1alpha1.NomadJob) error {
	controllerutil.RemoveFinalizer(nj, nomadJobFinalizer)
	return r.Update(ctx, nj)
}

func (r *NomadJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewNomadClient == nil {
		r.NewNomadClient = DefaultNomadJobClientFactory
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("nomadjob")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nomadv1alpha1.NomadJob{}).
		Watches(&nomadv1alpha1.NomadCluster{}, handler.EnqueueRequestsFromMapFunc(r.jobsForCluster)).
		Named("nomadjob").
		Complete(r)
}

// jobsForCluster maps a NomadCluster event to the reconcile keys of every
// NomadJob that targets it (so a cluster going Ready reconciles pending jobs).
func (r *NomadJobReconciler) jobsForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	nc, ok := obj.(*nomadv1alpha1.NomadCluster)
	if !ok {
		return nil
	}
	var list nomadv1alpha1.NomadJobList
	if err := r.List(ctx, &list, client.InNamespace(nc.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.ClusterRef.Name == nc.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name, Namespace: list.Items[i].Namespace}})
		}
	}
	return reqs
}

// setJobCondition upserts a status condition, preserving LastTransitionTime when
// the status is unchanged (mirrors setPoolCondition).
func setJobCondition(nj *nomadv1alpha1.NomadJob, condType string, status metav1.ConditionStatus, reason, msg string) {
	c := metav1.Condition{Type: condType, Status: status, Reason: reason, Message: msg, ObservedGeneration: nj.Generation}
	for i, existing := range nj.Status.Conditions {
		if existing.Type == condType {
			if existing.Status != status {
				c.LastTransitionTime = metav1.Now()
			} else {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			nj.Status.Conditions[i] = c
			return
		}
	}
	c.LastTransitionTime = metav1.Now()
	nj.Status.Conditions = append(nj.Status.Conditions, c)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `make test`
Expected: PASS — ClusterNotFound/ClusterNotReady/ownerRef/mapper/requeue green. (The `reconcileJob` stub returns `RequeueAfter: jobResync` with no Register, satisfying the ownerRef/requeue tests; ClusterNotReady asserts `f.registered` empty, which holds since `reconcileJob` is never reached for a non-Ready cluster.)

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadjob_controller.go internal/controller/nomadjob_controller_test.go
git commit -m "feat(controller): NomadJob reconciler skeleton, cluster resolution, finalizer, ownerRef"
```

---

## Task 5: `decodeJob` — strict decode, identity/region injection (pure function + unit tests)

**Files:**
- Modify: `internal/controller/nomadjob_controller.go` (add `decodeJob` + `errJobIDMismatch`)
- Test: `internal/controller/nomadjob_decode_test.go` (create)

**Interfaces:**
- Produces:
  - `var errJobIDMismatch error` (sentinel)
  - `func decodeJob(spec nomadv1alpha1.NomadJobSpec, region string) (*api.Job, error)` — strict-decodes `spec.Job.Raw` into `api.Job`, rejects a blob `id` that disagrees with `spec.JobID` (wrapping `errJobIDMismatch`), injects `job.ID = spec.JobID` and `job.Region = region`.

- [ ] **Step 1: Write the failing unit tests**

Create `internal/controller/nomadjob_decode_test.go` (plain-Go — no envtest):

```go
package controller

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

func specWith(jobJSON string) nomadv1alpha1.NomadJobSpec {
	return nomadv1alpha1.NomadJobSpec{
		ClusterRef: nomadv1alpha1.JobClusterRef{Name: "prod"},
		JobID:      "web",
		Job:        runtime.RawExtension{Raw: []byte(jobJSON)},
	}
}

func TestDecodeJob_CamelAndPascalEquivalent(t *testing.T) {
	camel, err := decodeJob(specWith(`{"type":"service","taskGroups":[{"name":"app","count":3,"tasks":[{"name":"s","driver":"docker","resources":{"cpu":200,"memoryMB":128}}]}]}`), "global")
	if err != nil {
		t.Fatalf("camel decode: %v", err)
	}
	pascal, err := decodeJob(specWith(`{"Type":"service","TaskGroups":[{"Name":"app","Count":3,"Tasks":[{"Name":"s","Driver":"docker","Resources":{"CPU":200,"MemoryMB":128}}]}]}`), "global")
	if err != nil {
		t.Fatalf("pascal decode: %v", err)
	}
	if len(camel.TaskGroups) != 1 || camel.TaskGroups[0].Count == nil || *camel.TaskGroups[0].Count != 3 {
		t.Fatalf("camel not parsed: %+v", camel)
	}
	if len(pascal.TaskGroups) != 1 || pascal.TaskGroups[0].Count == nil || *pascal.TaskGroups[0].Count != 3 {
		t.Fatalf("pascal not parsed: %+v", pascal)
	}
	if pascal.TaskGroups[0].Tasks[0].Resources.MemoryMB == nil || *pascal.TaskGroups[0].Tasks[0].Resources.MemoryMB != 128 {
		t.Fatalf("nested resources not parsed")
	}
}

func TestDecodeJob_InjectsIDAndRegion(t *testing.T) {
	job, err := decodeJob(specWith(`{"type":"service"}`), "us-east")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.ID == nil || *job.ID != "web" {
		t.Fatalf("job.ID = %v, want web", job.ID)
	}
	if job.Region == nil || *job.Region != "us-east" {
		t.Fatalf("job.Region = %v, want us-east", job.Region)
	}
}

func TestDecodeJob_RejectsUnknownKey(t *testing.T) {
	_, err := decodeJob(specWith(`{"type":"service","taskGropus":[]}`), "global")
	if err == nil {
		t.Fatal("unknown key must be rejected (strict decode)")
	}
	if errors.Is(err, errJobIDMismatch) {
		t.Fatal("unknown-key error must not be a jobID mismatch")
	}
}

func TestDecodeJob_RejectsWrongScalarType(t *testing.T) {
	if _, err := decodeJob(specWith(`{"taskGroups":[{"name":"app","count":"three"}]}`), "global"); err == nil {
		t.Fatal("count as string must be rejected")
	}
}

func TestDecodeJob_DurationMustBeNanoseconds(t *testing.T) {
	// HCL-style "10s" is rejected; integer nanoseconds decode (SGE I-1).
	if _, err := decodeJob(specWith(`{"update":{"minHealthyTime":"10s"}}`), "global"); err == nil {
		t.Fatal(`update.minHealthyTime:"10s" must be rejected (time.Duration wants int ns)`)
	}
	if _, err := decodeJob(specWith(`{"update":{"minHealthyTime":10000000000}}`), "global"); err != nil {
		t.Fatalf("minHealthyTime in ns must decode: %v", err)
	}
}

func TestDecodeJob_JobIDMismatch(t *testing.T) {
	_, err := decodeJob(specWith(`{"id":"other","type":"service"}`), "global")
	if err == nil || !errors.Is(err, errJobIDMismatch) {
		t.Fatalf("mismatched blob id must wrap errJobIDMismatch, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/controller/ -run 'TestDecodeJob' -v`
Expected: FAIL — `decodeJob` / `errJobIDMismatch` undefined.

- [ ] **Step 3: Implement `decodeJob`**

Add to `internal/controller/nomadjob_controller.go`. Add imports `"bytes"`, `"encoding/json"`, `"errors"`, `"fmt"`, and `"github.com/hashicorp/nomad/api"`:

```go
// errJobIDMismatch is returned by decodeJob when spec.job carries an explicit
// id that disagrees with spec.jobID (spec.jobID is authoritative).
var errJobIDMismatch = errors.New("job.id does not match spec.jobID")

// decodeJob strict-decodes spec.job (JSON in RawExtension.Raw) into an api.Job,
// then injects the authoritative identity and region. DisallowUnknownFields
// turns a typo'd/unknown key or a wrong-scalar-type (incl. an HCL-style duration
// string, which time.Duration rejects — it wants integer nanoseconds) into an
// error the reconciler surfaces as InvalidJobSpec. A blob id that disagrees with
// spec.jobID is rejected (JobIDMismatch); otherwise spec.jobID wins.
func decodeJob(spec nomadv1alpha1.NomadJobSpec, region string) (*api.Job, error) {
	dec := json.NewDecoder(bytes.NewReader(spec.Job.Raw))
	dec.DisallowUnknownFields()
	var job api.Job
	if err := dec.Decode(&job); err != nil {
		return nil, fmt.Errorf("decode spec.job: %w", err)
	}
	if job.ID != nil && *job.ID != "" && *job.ID != spec.JobID {
		return nil, fmt.Errorf("%w: job.id=%q spec.jobID=%q", errJobIDMismatch, *job.ID, spec.JobID)
	}
	id := spec.JobID
	job.ID = &id
	reg := region
	job.Region = &reg
	return &job, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestDecodeJob' -v`
Expected: PASS — camel/pascal equivalent, ID/Region injected, unknown-key + wrong-type + duration-string rejected, ns duration accepted, jobID mismatch wraps the sentinel.

- [ ] **Step 5: Build gate + commit**

Run: `go build ./... && go vet ./internal/controller/`

```bash
git add internal/controller/nomadjob_controller.go internal/controller/nomadjob_decode_test.go
git commit -m "feat(controller): strict decodeJob with identity/region injection + duration-ns unit tests"
```

---

## Task 6: Apply — decode conditions + Plan-gated Register + warnings event

**Files:**
- Modify: `internal/controller/nomadjob_controller.go` (fill `reconcileJob` apply body)
- Test: `internal/controller/nomadjob_controller_test.go`

**Interfaces:**
- Consumes: `decodeJob`, `errJobIDMismatch` (Task 5), `NomadJobOps.PlanJob`/`RegisterJob`, `r.Recorder`.

- [ ] **Step 1: Write failing tests (register-on-change, no-register-when-unchanged, invalid spec, id mismatch, warnings)**

Add as Ginkgo `It`s to `nomadjob_controller_test.go`. Behaviors to assert (build with `readyCluster(ctx, ns.Name)` and `newFakeJobOps()`):
- **Registers when Plan shows changes** — `f.planChanged = true`; after reconcile `f.registered` has 1 entry whose `*ID == "web"` and `*Region == "global"`; `Ready=True/Registered`.
- **Skips Register when Plan shows no change** — `f.planChanged = false`; `f.registered` empty; still `Ready=True/Registered`.
- **InvalidJobSpec** — `spec.job` = `{"taskGropus":[]}` (unknown key); reconcile; `Ready=False/InvalidJobSpec`, `f.registered` empty, `observedGeneration` advanced.
- **JobIDMismatch** — `spec.jobID="web"` but `spec.job` = `{"id":"other"}`; `Ready=False/JobIDMismatch`, no Register.
- **Register warnings → event** — `f.planChanged = true`, `f.warnings = "deprecated x"`; assert the `record.FakeRecorder` channel received an event containing `deprecated x`.

Example (register-on-change), abbreviated:

```go
It("registers the job when Plan reports a change and injects ID/Region", func(ctx SpecContext) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-apply-"}}
	Expect(k8s.Create(ctx, ns)).To(Succeed())
	nc := readyCluster(ctx, ns.Name) // Region "global"

	f := newFakeJobOps()
	f.planChanged = true

	nj := &nomadv1alpha1.NomadJob{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
		Spec: nomadv1alpha1.NomadJobSpec{
			ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
			JobID:      "web",
			Job:        runtime.RawExtension{Raw: []byte(`{"type":"service","taskGroups":[{"name":"app","count":1}]}`)},
		},
	}
	Expect(k8s.Create(ctx, nj)).To(Succeed())

	r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
	Expect(err).NotTo(HaveOccurred())

	Expect(f.registered).To(HaveLen(1))
	Expect(f.registered[0].ID).NotTo(BeNil())
	Expect(*f.registered[0].ID).To(Equal("web"))
	Expect(*f.registered[0].Region).To(Equal("global"))

	var got nomadv1alpha1.NomadJob
	Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
	cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadJobCondReady)
	Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonRegistered))
})
```

For the warnings event, drain the recorder channel:

```go
rec := record.NewFakeRecorder(10)
// ... reconcile with Recorder: rec ...
var got string
Eventually(rec.Events).Should(Receive(&got))
Expect(got).To(ContainSubstring("deprecated x"))
```

- [ ] **Step 2: Run to verify they fail**

Run: `make test`
Expected: FAIL — `reconcileJob` is the Task-4 stub (0 Registers, no conditions set).

- [ ] **Step 3: Implement the apply body**

Replace the `reconcileJob` stub in `nomadjob_controller.go`:

```go
// reconcileJob decodes spec.job, drift-gates the register via Plan, and derives
// status. Status derivation (Task 7) is layered in before the Ready write.
func (r *NomadJobReconciler) reconcileJob(ctx context.Context, nj *nomadv1alpha1.NomadJob, ops NomadJobOps, region string) (ctrl.Result, error) {
	desired, err := decodeJob(nj.Spec, region)
	if err != nil {
		reason := nomadv1alpha1.ReasonInvalidJobSpec
		if errors.Is(err, errJobIDMismatch) {
			reason = nomadv1alpha1.ReasonJobIDMismatch
		}
		setJobCondition(nj, nomadv1alpha1.NomadJobCondReady, metav1.ConditionFalse, reason, err.Error())
		nj.Status.ObservedGeneration = nj.Generation
		if uerr := r.Status().Update(ctx, nj); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: jobResync}, nil
	}

	// Drift-gate the register on Plan's Diff.Type (never on FailedTGAllocs — an
	// infeasible-but-changed job must still be registered so the user can fix it).
	changed, err := ops.PlanJob(ctx, desired)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		warnings, err := ops.RegisterJob(ctx, desired)
		if err != nil {
			return ctrl.Result{}, err
		}
		if warnings != "" {
			r.Recorder.Event(nj, "Normal", "RegisterWarnings", warnings)
		}
	}

	setJobCondition(nj, nomadv1alpha1.NomadJobCondReady, metav1.ConditionTrue, nomadv1alpha1.ReasonRegistered, "job registered onto Nomad")
	nj.Status.ObservedGeneration = nj.Generation
	if err := r.Status().Update(ctx, nj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: jobResync}, nil
}
```

- [ ] **Step 4: Run tests + build gate**

Run: `make manifests generate fmt vet && make test`
Expected: PASS — registers on change, skips when unchanged, InvalidJobSpec/JobIDMismatch conditions set with no Register, warnings surfaced as an event.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadjob_controller.go internal/controller/nomadjob_controller_test.go
git commit -m "feat(controller): decode-gated + Plan-gated job register with warnings event"
```

---

## Task 7: Status — jobStatus, jobVersion, per-group running/desired counts

**Files:**
- Modify: `internal/controller/nomadjob_controller.go` (add status derivation to `reconcileJob`)
- Test: `internal/controller/nomadjob_controller_test.go`

**Interfaces:**
- Consumes: `NomadJobOps.GetJob`/`JobGroupSummary`, the decoded `desired.TaskGroups`.

- [ ] **Step 1: Write the failing test**

Add a Ginkgo `It`: seed `f.jobs["web"]` with a `*api.Job` whose `Status`/`Version` are set, and `f.summary["app"] = api.TaskGroupSummary{Running: 2}`; `spec.job` has group `app` with `count: 3`. After reconcile assert `status.jobStatus`, `status.jobVersion`, and `status.groups["app"] == {Running:2, Desired:3}`.

```go
It("mirrors jobStatus, jobVersion, and per-group running/desired into status", func(ctx SpecContext) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-status-"}}
	Expect(k8s.Create(ctx, ns)).To(Succeed())
	nc := readyCluster(ctx, ns.Name)

	f := newFakeJobOps()
	// planChanged=false so NO Register runs: the fake's RegisterJob would
	// overwrite f.jobs["web"] with the decoded desired job (which has no
	// server-set Status/Version), clobbering the seed below and making GetJob
	// return a statusless job (SGE plan-review I-1). With no Register, the seeded
	// job survives and GetJob returns running/4.
	f.planChanged = false
	status, ver := "running", uint64(4)
	f.jobs["web"] = &api.Job{Status: &status, Version: &ver}
	f.summary["app"] = api.TaskGroupSummary{Running: 2}

	nj := &nomadv1alpha1.NomadJob{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
		Spec: nomadv1alpha1.NomadJobSpec{
			ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
			JobID:      "web",
			Job:        runtime.RawExtension{Raw: []byte(`{"type":"service","taskGroups":[{"name":"app","count":3}]}`)},
		},
	}
	Expect(k8s.Create(ctx, nj)).To(Succeed())

	r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
	Expect(err).NotTo(HaveOccurred())

	var got nomadv1alpha1.NomadJob
	Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
	Expect(got.Status.JobStatus).To(Equal("running"))
	Expect(got.Status.JobVersion).To(Equal(int64(4)))
	Expect(got.Status.Groups["app"]).To(Equal(nomadv1alpha1.NomadJobGroupStatus{Running: 2, Desired: 3}))
})
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/controller/ -run TestControllers` (or the full `make test`)
Expected: FAIL — status fields stay zero.

- [ ] **Step 3: Implement status derivation**

In `reconcileJob`, insert **after** the Plan/Register block and **before** the final `setJobCondition(...Ready...)`:

```go
	// Derive bounded runtime status (managed, not a deep mirror).
	info, err := ops.GetJob(ctx, nj.Spec.JobID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if info != nil {
		if info.Status != nil {
			nj.Status.JobStatus = *info.Status
		}
		if info.Version != nil {
			nj.Status.JobVersion = int64(*info.Version)
		}
	}
	summary, err := ops.JobGroupSummary(ctx, nj.Spec.JobID)
	if err != nil {
		return ctrl.Result{}, err
	}
	groups := make(map[string]nomadv1alpha1.NomadJobGroupStatus, len(desired.TaskGroups))
	for _, g := range desired.TaskGroups {
		name := ""
		if g.Name != nil {
			name = *g.Name
		}
		gs := nomadv1alpha1.NomadJobGroupStatus{}
		if g.Count != nil {
			gs.Desired = *g.Count
		}
		if s, ok := summary[name]; ok {
			gs.Running = s.Running
		}
		groups[name] = gs
	}
	nj.Status.Groups = groups
```

- [ ] **Step 4: Run tests**

Run: `make test`
Expected: PASS — status mirrors jobStatus/jobVersion and per-group counts.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadjob_controller.go internal/controller/nomadjob_controller_test.go
git commit -m "feat(controller): mirror job status, version, and per-group running/desired counts"
```

---

## Task 8: Finalizer deletion path (Deregister purge=true + cluster-gone-or-going short-circuit)

**Files:**
- Modify: `internal/controller/nomadjob_controller.go` (fill `finalizeJob`)
- Test: `internal/controller/nomadjob_controller_test.go`

**Interfaces:**
- Consumes: `NomadJobOps.DeregisterJob`, `nomad.IsNotFound`, `clusterNomadConfig`.

- [ ] **Step 1: Write failing tests (deregister-success, cluster-going, cluster-not-ready-blocked, deregister-failed)**

Add Ginkgo `It`s (mirror the NomadPool `finalize` block; use `mustDelete`, `assertGoneJob`, `mustCreateTerminatingCluster`). Add an `assertGoneJob` helper next to `assertGonePool`:

```go
func assertGoneJob(ctx SpecContext, ns, name string) {
	var got nomadv1alpha1.NomadJob
	err := k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got)
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected job %s/%s to be gone, got: %v", ns, name, err)
}
```

Behaviors:
- **Deregister success** — Ready cluster, CR with the finalizer, `mustDelete`; after reconcile `f.deregistered == ["web"]`, `f.purged == [true]` (**purge=true**), and `assertGoneJob` (finalizer removed).
- **Cluster gone/going** — `mustCreateTerminatingCluster`; reconcile a deleting CR; `f.deregistered` empty (no Deregister), `assertGoneJob` (finalizer dropped without Deregister).
- **Cluster present but not Ready** — cluster flipped to `PhaseDegraded`; finalizer held, `DeleteBlocked=True/ClusterNotReady`, `f.deregistered` empty.
- **Deregister failed** — `f.deregisterErr = errors.New("boom")`; finalizer held, `DeleteBlocked=True/DeregisterFailed`.

Example (deregister success), abbreviated:

```go
It("deregisters with purge and removes the finalizer on success", func(ctx SpecContext) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-del-"}}
	Expect(k8s.Create(ctx, ns)).To(Succeed())
	nc := readyCluster(ctx, ns.Name)

	f := newFakeJobOps()
	f.jobs["web"] = &api.Job{}
	r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

	nj := &nomadv1alpha1.NomadJob{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name, Finalizers: []string{nomadJobFinalizer}},
		Spec: nomadv1alpha1.NomadJobSpec{
			ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
			JobID:      "web",
			Job:        runtime.RawExtension{Raw: []byte(`{"type":"service"}`)},
		},
	}
	Expect(k8s.Create(ctx, nj)).To(Succeed())
	mustDelete(ctx, nj)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
	Expect(err).NotTo(HaveOccurred())
	Expect(f.deregistered).To(Equal([]string{"web"}))
	Expect(f.purged).To(Equal([]bool{true}))
	assertGoneJob(ctx, ns.Name, "web")
})
```

- [ ] **Step 2: Run to verify they fail**

Run: `make test`
Expected: FAIL — `finalizeJob` is the Task-4 stub (drops the finalizer unconditionally, never Deregisters).

- [ ] **Step 3: Implement `finalizeJob`**

Replace the `finalizeJob` stub. Add import `"github.com/jacaudi/nomad-operator/internal/nomad"`:

```go
// finalizeJob deregisters the Nomad job when the CR is deleted, gated so it
// never deadlocks a cascade: if the cluster is gone OR going (DeletionTimestamp
// set), there is nothing to clean up (the control plane is gone/going too), so
// drop the finalizer without calling Deregister. This closes both background and
// foreground cascade (design §3.5, reusing the NomadPool pattern). A job
// Deregister is never refused for "non-emptiness" (stopping a job stops its
// allocations), so there is no PoolNotEmpty-style blocked branch.
func (r *NomadJobReconciler) finalizeJob(ctx context.Context, nj *nomadv1alpha1.NomadJob) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(nj, nomadJobFinalizer) {
		return ctrl.Result{}, nil
	}

	var nc nomadv1alpha1.NomadCluster
	err := r.Get(ctx, types.NamespacedName{Name: nj.Spec.ClusterRef.Name, Namespace: nj.Namespace}, &nc)
	clusterGoneOrGoing := apierrors.IsNotFound(err) || (err == nil && !nc.DeletionTimestamp.IsZero())
	if clusterGoneOrGoing {
		return ctrl.Result{}, r.dropJobFinalizer(ctx, nj)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if nc.Status.Phase != nomadv1alpha1.PhaseReady {
		// Cluster present, not deleting, but unreachable — do NOT orphan on a blip.
		setJobCondition(nj, nomadv1alpha1.NomadJobCondDeleteBlocked, metav1.ConditionTrue, nomadv1alpha1.ReasonClusterNotReady, "cluster not Ready; cannot confirm job deregistration")
		if uerr := r.Status().Update(ctx, nj); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: jobResync}, nil
	}

	cfg, err := clusterNomadConfig(ctx, r.Client, &nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	ops, err := r.NewNomadClient(cfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	// purge=true: the CR going away means the job should not exist; a lingering
	// dead record would collide with a future re-create of the same jobID.
	if derr := ops.DeregisterJob(ctx, nj.Spec.JobID, true); derr != nil && !nomad.IsNotFound(derr) {
		setJobCondition(nj, nomadv1alpha1.NomadJobCondDeleteBlocked, metav1.ConditionTrue, nomadv1alpha1.ReasonDeregisterFailed, derr.Error())
		if uerr := r.Status().Update(ctx, nj); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: jobResync}, nil
	}
	// Deregister succeeded OR the job was already gone (404) — drop the finalizer.
	return ctrl.Result{}, r.dropJobFinalizer(ctx, nj)
}
```

- [ ] **Step 4: Run tests**

Run: `make test`
Expected: PASS — deregister-success (purge=true, finalizer removed), cluster-going (no Deregister, finalizer dropped), not-ready (finalizer held, DeleteBlocked/ClusterNotReady), deregister-failed (finalizer held, DeleteBlocked/DeregisterFailed).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadjob_controller.go internal/controller/nomadjob_controller_test.go
git commit -m "feat(controller): finalizer deregister (purge) with cluster-gone-or-going short-circuit"
```

---

## Task 9: CEL behavioral tests (immutability + jobID pattern)

**Files:**
- Create: `internal/controller/nomadjob_crd_test.go`

**Interfaces:**
- Consumes: the installed CRD (from `config/crd/bases`, loaded by the envtest suite).

- [ ] **Step 1: Write the CEL tests**

Create `internal/controller/nomadjob_crd_test.go`. Write as Ginkgo `It`s against `k8s` (a valid `spec.job` is required for the object to be admitted; use a minimal one). Cover:
- **jobID immutable** — create a valid `NomadJob`, then change `spec.jobID` and `k8s.Update` → expect an error.
- **clusterRef.name immutable** — change `spec.clusterRef.name` → expect an error.
- **jobID pattern** — create with `spec.jobID: "has spaces"` → expect a create error (pattern rejects it).

```go
It("rejects a jobID change (immutable)", func(ctx SpecContext) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-immut-"}}
	Expect(k8s.Create(ctx, ns)).To(Succeed())
	nj := &nomadv1alpha1.NomadJob{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
		Spec: nomadv1alpha1.NomadJobSpec{
			ClusterRef: nomadv1alpha1.JobClusterRef{Name: "prod"},
			JobID:      "web",
			Job:        runtime.RawExtension{Raw: []byte(`{"type":"service"}`)},
		},
	}
	Expect(k8s.Create(ctx, nj)).To(Succeed())
	nj.Spec.JobID = "web2"
	Expect(k8s.Update(ctx, nj)).NotTo(Succeed(), "jobID must be immutable")
})

It("rejects a jobID that violates the name pattern", func(ctx SpecContext) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-badid-"}}
	Expect(k8s.Create(ctx, ns)).To(Succeed())
	nj := &nomadv1alpha1.NomadJob{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns.Name},
		Spec: nomadv1alpha1.NomadJobSpec{
			ClusterRef: nomadv1alpha1.JobClusterRef{Name: "prod"},
			JobID:      "has spaces",
			Job:        runtime.RawExtension{Raw: []byte(`{"type":"service"}`)},
		},
	}
	Expect(k8s.Create(ctx, nj)).NotTo(Succeed(), "jobID with spaces must be rejected by the pattern")
})
```

- [ ] **Step 2: Run the tests**

Run: `make test`
Expected: PASS — jobID/clusterRef immutable, bad jobID pattern rejected. (These exercise the CRD authored in Task 2; if any fails, fix the CEL markers in `nomadjob_types.go`, re-run `make manifests`, and re-test.)

- [ ] **Step 3: Commit**

```bash
git add internal/controller/nomadjob_crd_test.go
git commit -m "test(controller): CEL enforces jobID/clusterRef immutability and jobID pattern"
```

---

## Task 10: main.go wiring, runbook, and live job integration spike (resolves self-dedup)

**Files:**
- Modify: `cmd/main.go`
- Create: `docs/runbooks/nomadjob.md`
- Create: `internal/nomad/job_integration_test.go`

**Interfaces:**
- Consumes: `NomadJobReconciler` (Task 4), the Task-1 client methods.

- [ ] **Step 1: Wire the reconciler in main.go**

In `cmd/main.go`, after the `NomadPoolReconciler` block (before `// +kubebuilder:scaffold:builder`):

```go
	if err := (&controller.NomadJobReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "nomadjob")
		os.Exit(1)
	}
```

- [ ] **Step 2: Write the live integration spike (resolves design §6.3 self-dedup + §6.4 Deregister-missing)**

Create `internal/nomad/job_integration_test.go` (mirrors `client_write_integration_test.go`; `//go:build integration`). It resolves, against a real Nomad v2.0.4, whether `Register` self-dedups an identical job (design §6.3 — determines if `Plan`-gating is strictly required) and the `Deregister`-missing shape (§6.4):

```go
//go:build integration

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/api"
)

// TestJobLifecycleLive exercises the full job surface against a real Nomad
// (skips when no binary/endpoint is available, matching the other integration
// tests). It resolves the design's two remaining plan-time opens:
//   - §6.3: whether a second identical Register creates a new Version (if it
//     no-ops, Plan-gating is a KISS-optional optimization; if it churns, keep it).
//   - §6.4: Deregister on a missing job returns success/404, not an opaque error.
func TestJobLifecycleLive(t *testing.T) {
	// devAgentWithNode(t) is the existing harness in client_write_integration_test.go
	// (starts a real `nomad agent -dev`, registers a node, returns (*Client, nodeID),
	// and skips when no nomad binary is present). Reuse it — do NOT invent a helper
	// (SGE plan-review M-1).
	c, _ := devAgentWithNode(t)

	ctx := t.Context()
	id := "operator-it-job"
	region := "global"
	typ := "service"
	// A trivial always-runnable job is driver-dependent; if the dev agent lacks a
	// usable driver, keep the assertions to Register/Plan/Deregister acceptance
	// (scheduling health is out of scope for this client-level spike).
	job := &api.Job{ID: &id, Name: &id, Region: &region, Type: &typ}

	// First register.
	if _, err := c.RegisterJob(ctx, job); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = c.DeregisterJob(ctx, id, true) })

	// Info round-trips; capture the version.
	got, err := c.GetJob(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: job=%v err=%v", got, err)
	}
	v1 := got.Version

	// Plan an identical job → expect Diff.Type "None" (no change) OR document
	// what it reports.
	changed, err := c.PlanJob(ctx, job)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	t.Logf("§6.3 spike: Plan(identical).changed = %v", changed)

	// Second identical register → does Version advance? (§6.3)
	if _, err := c.RegisterJob(ctx, job); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got2, err := c.GetJob(ctx, id)
	if err != nil || got2 == nil {
		t.Fatalf("get2: %v", err)
	}
	t.Logf("§6.3 spike: Version before=%v after identical re-register=%v (equal ⇒ Register self-dedups ⇒ Plan-gating is optional)", deref(v1), deref(got2.Version))

	// Deregister with purge, then Deregister the now-missing job (§6.4).
	if err := c.DeregisterJob(ctx, id, true); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	if err := c.DeregisterJob(ctx, id, true); err != nil && !IsNotFound(err) {
		t.Fatalf("§6.4: deregister-missing must be nil or 404, got %v", err)
	}

	// GetJob on a missing job → (nil, nil).
	if p, err := c.GetJob(ctx, "does-not-exist-xyz"); err != nil || p != nil {
		t.Fatalf("missing get: job=%v err=%v (want nil,nil)", p, err)
	}
}

func deref(p *uint64) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}
```

> If the harness helper is named differently (e.g. the slice-2/3 integration tests may use a `newIntegrationClient` or an inline `New(Config{...})` against `NOMAD_ADDR`), match the existing pattern in `internal/nomad/client_write_integration_test.go` rather than inventing a new one.

- [ ] **Step 3: Run the build gate + integration (if a nomad binary is present)**

Run: `make manifests generate fmt vet && make test`
Expected: PASS; build clean; zero regen drift.
Run (best-effort): `make test-integration` — if no `nomad` v2.0.4 binary/endpoint, it skips (like slices 3/4). If it runs, **record the §6.3 outcome** in the design's open-items (whether `Register` self-dedups) and confirm §6.4.

- [ ] **Step 4: Write the runbook**

Create `docs/runbooks/nomadjob.md` covering: what a `NomadJob` is (a managed Nomad job); authoring `spec.job` as the `api.Job` structure in YAML (camelCase or PascalCase), and the **duration-nanoseconds caveat** (`minHealthyTime: 10000000000`, not `"10s"`); why `spec.jobID` is separate from `metadata.name` and immutable; how a typo surfaces (`InvalidJobSpec` condition with the decoder error); update semantics (edit `spec.job`; re-register only on `Plan` drift); deletion (finalizer deregisters with `purge=true`; a stuck `Terminating` shows `DeleteBlocked`); status fields (`jobStatus`/`jobVersion`/per-group `running`/`desired`); and cascade behavior (deleting the `NomadCluster` removes its jobs' CRs). Model the structure on `docs/runbooks/nomadpool.md`.

- [ ] **Step 5: Final gate + commit**

Run: `make manifests generate fmt vet && make test && go build ./...`
Expected: all green; zero regen drift.

```bash
git add cmd/main.go docs/runbooks/nomadjob.md internal/nomad/job_integration_test.go
git commit -m "feat: wire NomadJob reconciler, runbook, and live job integration spike"
```

---

## Self-Review

**1. Spec coverage** (design → task):
- Managed-lifecycle CRD, Option B schemaless `spec.job` (§1.1/§1.3/§3.1) → Task 2. ✔
- Job client methods (Info/Plan/Register/Deregister/Summary) + 404 reuse (§4) → Task 1. ✔
- Strict decode + camel/pascal + duration-ns + typo + JobIDMismatch (§1.3/§3.3, I-1/I-2) → Task 5. ✔
- Identity + region injection (D1, M-3, §3.3/§3.7) → Task 5 (`decodeJob`) + asserted in Task 6. ✔
- Plan-gated register on `Diff.Type` only, warnings event (§3.4, D3, M-2) → Task 6. ✔
- Bounded status: jobStatus/jobVersion/per-group counts (§3.6, D5) → Task 7. ✔
- Finalizer Deregister purge=true + cluster-gone-or-going short-circuit + not-ready-blocked (§3.5, D4) → Task 8. ✔
- No PoolNotEmpty-style branch (jobs never refused) (§3.5) → Task 8 (comment + absent branch). ✔
- CEL: jobID/clusterRef immutable, jobID pattern (§3.1) → Task 2 (rules) + Task 9 (behavior). ✔
- Own `NomadJobOps`, no widening; reuse `clusterNomadConfig` (§4) → Task 3 + Task 4. ✔
- contract.go pins backed by real calls; ValidateJob dropped (§4.1, M-1) → Task 1. ✔
- kustomization base (Global Constraints, 6c3e0c1 lesson) → Task 2. ✔
- main.go wiring + runbook (§7) → Task 10. ✔
- Register self-dedup spike (§6.3) + Deregister-missing (§6.4) → Task 10 integration. ✔

**2. Placeholder scan:** No "TBD"/"handle edge cases"/"similar to Task N" — every code step carries complete code. The one runtime-observed value (whether `Register` self-dedups) is captured by the Task-10 integration spike with `Plan`-gating as the working default, not a code placeholder.

**3. Type consistency:** `NomadJobOps` methods (`GetJob`/`PlanJob`/`RegisterJob`/`DeregisterJob`/`JobGroupSummary`) match the Task-1 `Client` methods verbatim; `decodeJob`/`errJobIDMismatch`/`reconcileJob(…, region)`/`finalizeJob`/`dropJobFinalizer`/`jobsForCluster`/`setJobCondition` are used consistently; condition types/reasons (`NomadJobCondReady`, `NomadJobCondDeleteBlocked`, `ReasonInvalidJobSpec`, `ReasonJobIDMismatch`, `ReasonDeregisterFailed`, reused `ReasonRegistered`/`ReasonClusterNotFound`/`ReasonClusterNotReady`) are declared in Task 2 and referenced thereafter; `NomadJobGroupStatus{Running,Desired}` is defined in Task 2 and consumed in Task 7. `api.Job` pointer fields (`ID`, `Region`, `Status`, `Version *uint64`, `TaskGroups[].Count *int`, `Name *string`) are dereferenced with nil-guards throughout.

**Deliberate non-inclusions (faithful to the design, not gaps):** no duplicate-`jobID` collision detection (unlike NomadPool §3.5 — the design omits it; `Plan`-gating naturally dampens identical duplicates, and divergent duplicates are a deferred edge case); no HCL input, no `Scale`/`Revert`/promotion, no deep runtime mirror (design §5 YAGNI).

**Note for the executor:** follow the **Test Harness** section — reconcile/CEL tests are Ginkgo `It`s against the bare envtest client `k8s`, mirroring `internal/controller/nomadpool_controller_test.go`; `decodeJob` tests are plain-Go in the same package (no envtest); `internal/nomad` tests are plain-Go with `httptest`. Reuse existing helpers (`readyCluster`, `mustDelete`, `mustCreateTerminatingCluster`); add only `assertGoneJob`.
