# NomadNamespace Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `NomadNamespace` CRD that manages a Nomad namespace on a `NomadCluster`, and thread the Nomad namespace through the existing `NomadJob` reconciler so non-`default`-namespace jobs are registered, observed, and deleted correctly (fixing the live-e2e orphan-on-delete bug at the root).

**Architecture:** `NomadNamespace` is a managed-lifecycle CRD mirroring the merged `NomadPool` almost line-for-line — user owns CRUD; the operator upserts via `Namespaces().Register` (read-modify-write, preserving unmanaged server fields), deletes via a block-until-empty finalizer, detects duplicate names, and surfaces a bounded status. A new `NomadNamespaceOps` consumer interface + fake is added; no existing interface except `NomadJobOps` is touched. `NomadJob` gains an immutable `spec.nomadNamespace` (default `default`) injected authoritatively into `job.Namespace`, and the namespace is threaded as a per-call parameter through the three job-ID-addressed client methods; the finalizer reads `spec.nomadNamespace` directly so deletion never orphans.

**Tech Stack:** Go 1.26, kubebuilder v4 / controller-runtime v0.23.3, `github.com/hashicorp/nomad/api` (pinned `v0.0.0-20260707172059-5b83b133998a` == v2.0.4), Ginkgo/Gomega envtest, `net/http/httptest` unit tests, build tag `integration` for the live spike.

**Design:** `docs/development/designs/2026-07-16-nomadnamespace-design.md` (HEAD `6615695`).

> **For Claude:** REQUIRED EXECUTION WORKFLOW (follow in order):
> 1. `superpowers:using-git-worktrees` — Isolate work in a dedicated worktree
> 2. `superpowers:subagent-driven-development` — Dispatch a fresh subagent per task
> 3. `superpowers:test-driven-development` — All subagents use TDD
> 4. `superpowers:verification-before-completion` — Verify all tests pass per task
> 5. `superpowers:requesting-code-review` — Code review after each task (built in)
> 6. After all tasks: comprehensive code review on full diff from branch point (automatic)
> 7. `superpowers:finishing-a-development-branch` — Complete the branch
> 8. `modern-go-guidelines:use-modern-go` — All Go conforms to the project's Go version
>
> Skills carry their own model and effort settings. Do not override them.

> **Amended 2026-07-17** after an independent sr-go-engineer *plan* review (Fable model), verdict *amend-before-execution* — mechanical/localized fixes, no architectural change. Folded: **C1** — `Namespaces().Register` PUTs to the fixed `/v1/namespace` path with the name in the JSON body (not `/v1/namespace/<name>`), so the T1 `UpsertNamespace` test asserts the path `/v1/namespace` + body `Name` (was a TDD-deadlocking wrong assertion). **I1/M2** — T10 must also update `internal/nomad/job_integration_test.go` (behind `//go:build integration`, invisible to untagged `make test`) and the real client call at `internal/nomad/../nomadjob_controller_test.go` (`DeregisterJob(ctx, "web", true)`), and name all four `job_test.go` call sites; T10's gate adds `go vet -tags integration ./...`. **I2** — the T11 live spike reuses the real harness helper `devAgentWithNode(t)` (`internal/nomad/client_write_integration_test.go`), not a nonexistent `startDevAgentClient`. **M1** — T7 builds the not-empty error via the existing httptest-4xx→`nomad.New` pattern (as in `nomadjob_controller_test.go`), NOT a new production `nomad.*ForTest()` seam. The reviewer confirmed SOUND: `k8s`/`readyCluster` helpers, reused reason constants, API routes/fields (`Namespace.Quota`, `Jobs().List` namespace scoping), all `contract.go` pins, NomadPool mirror fidelity, and that no task except this integration-build gap leaves a red build.

## Global Constraints

- **Go standard-library-only for the nomad boundary** where the pinned `api` already covers it; never `api.DefaultConfig` (absorbs `NOMAD_*` env). New client methods take `context.Context` first and wrap errors with `fmt.Errorf("nomad: <op> %q: %w", ...)`.
- **`contract.go` pins only symbols a real call exercises** — every new pin must be backed by an actual `Client` method call in the same task (Foundation "existence-only-pin" gotcha).
- **Per-endpoint client only** via `clusterNomadConfig`; never a singleton, never a namespace in the shared cluster-scoped `clusterNomadConfig` (namespace is job-scoped, passed per-call).
- **No widening** of `NomadOps`/`NomadNodeOps`/`NomadPoolOps`. `NomadJobOps` IS deliberately changed (namespace parameter) — that is the one intended interface change.
- **v1alpha1 is unreleased** — additive CRD changes need no conversion webhook.
- **Nomad namespace name** treated as `^[a-zA-Z0-9-_]{1,128}$`, reserved name `default` (verify exact regex + reserved set in the T11 spike; use the string literal `"default"` — the `api` package exports no default-namespace constant).
- **Build gate per task:** `make manifests generate fmt vet && make test` green, **zero** regen drift (`git diff --exit-code` on generated files).
- **Signed commits** need the user's 1Password Touch ID; on a `failed to fill whole buffer` error STOP and ask (never disable `commit.gpgsign`).
- **Every Nomad-domain claim** already verified against the pinned `api` in the design; do not re-derive from training.

---

## File Structure

**Part A — NomadNamespace CRD**
- `internal/nomad/namespace.go` (create) — `GetNamespace`/`UpsertNamespace`/`DeleteNamespace`/`CountNamespaceJobs`.
- `internal/nomad/namespace_test.go` (create) — httptest unit tests.
- `internal/nomad/errors.go` (modify) — add `IsNamespaceNotEmpty`.
- `internal/nomad/errors_test.go` (modify) — add its unit test.
- `internal/nomad/contract.go` (modify) — pin the namespace + `Jobs().List` symbols.
- `api/v1alpha1/nomadnamespace_types.go` (create) — CRD types + reasons + CEL.
- `config/crd/kustomization.yaml` (modify) — add the generated base to `resources:`.
- `internal/controller/nomadnamespace_ops.go` (create) — `NomadNamespaceOps` interface + factory.
- `internal/controller/fake_nomadnamespace_test.go` (create) — the fake.
- `internal/controller/nomadnamespace_controller.go` (create) — the reconciler.
- `internal/controller/nomadnamespace_controller_test.go` (create) — envtest.

**Part B — NomadJob threading**
- `api/v1alpha1/nomadjob_types.go` (modify) — add `spec.nomadNamespace` + CEL + `ReasonNamespaceMismatch`.
- `internal/controller/nomadjob_controller.go` (modify) — `decodeJob` injection/mismatch; caller namespace args.
- `internal/nomad/client.go` (modify) — namespace params on the 5 job methods.
- `internal/controller/nomadjob_ops.go` (modify) — interface signature change.
- `internal/controller/fake_nomadjob_test.go` (modify) — fake signature change + recording.
- `internal/nomad/job_test.go` (modify) — namespace-passing unit assertions.

**Part C — wiring / docs / live spike**
- `cmd/main.go` (modify) — register `NomadNamespaceReconciler`.
- `docs/runbooks/nomadnamespace.md` (create) — runbook.
- `internal/nomad/namespace_integration_test.go` (create) — live spike.
- `Makefile` (modify) — add `TestNamespaceLifecycleLive` to the `test-integration` `-run` filter.

---

## Task 1: `internal/nomad` namespace client methods + `IsNamespaceNotEmpty` + pins

**Files:**
- Create: `internal/nomad/namespace.go`
- Create: `internal/nomad/namespace_test.go`
- Modify: `internal/nomad/errors.go`
- Modify: `internal/nomad/errors_test.go`
- Modify: `internal/nomad/contract.go`

**Interfaces:**
- Produces: `(*Client).GetNamespace(ctx, name string) (*api.Namespace, error)` (`(nil,nil)` on 404); `(*Client).UpsertNamespace(ctx, ns *api.Namespace) error`; `(*Client).DeleteNamespace(ctx, name string) error`; `(*Client).CountNamespaceJobs(ctx, name string) (int, error)`; `IsNamespaceNotEmpty(err error) bool`.

- [ ] **Step 1: Write the failing unit tests** (`internal/nomad/namespace_test.go`). `newTestClient` lives in `nodepool_test.go` (same package) — reuse it.

```go
package nomad

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hashicorp/nomad/api"
)

func TestGetNamespace_NotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "namespace not found", http.StatusNotFound)
	})
	ns, err := c.GetNamespace(t.Context(), "missing")
	if err != nil || ns != nil {
		t.Fatalf("GetNamespace 404 = (%v, %v), want (nil, nil)", ns, err)
	}
}

func TestGetNamespace_Found(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Name":"team-a","Description":"d"}`))
	})
	ns, err := c.GetNamespace(t.Context(), "team-a")
	if err != nil || ns == nil || ns.Name != "team-a" {
		t.Fatalf("GetNamespace = (%v, %v), want name team-a", ns, err)
	}
}

func TestUpsertNamespace_PostsName(t *testing.T) {
	var gotPath, gotName string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var ns api.Namespace
		_ = json.NewDecoder(r.Body).Decode(&ns)
		gotName = ns.Name
		w.WriteHeader(http.StatusOK)
	})
	if err := c.UpsertNamespace(t.Context(), &api.Namespace{Name: "team-a"}); err != nil {
		t.Fatalf("UpsertNamespace: %v", err)
	}
	// Register PUTs to the FIXED /v1/namespace path with the name in the JSON
	// body (not /v1/namespace/<name>).
	if gotPath != "/v1/namespace" || gotName != "team-a" {
		t.Fatalf("Register = path %q name %q, want /v1/namespace + team-a", gotPath, gotName)
	}
}

func TestDeleteNamespace_DeletesByName(t *testing.T) {
	var gotMethod, gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	if err := c.DeleteNamespace(t.Context(), "team-a"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/namespace/team-a" {
		t.Fatalf("delete = %s %s, want DELETE /v1/namespace/team-a", gotMethod, gotPath)
	}
}

func TestCountNamespaceJobs_ScopesByNamespace(t *testing.T) {
	var gotNS string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotNS = r.URL.Query().Get("namespace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"ID":"a"},{"ID":"b"}]`))
	})
	n, err := c.CountNamespaceJobs(t.Context(), "team-a")
	if err != nil || n != 2 {
		t.Fatalf("CountNamespaceJobs = (%d, %v), want (2, nil)", n, err)
	}
	if gotNS != "team-a" {
		t.Fatalf("namespace query = %q, want team-a", gotNS)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/nomad/ -run 'Namespace' -v`
Expected: FAIL — `c.GetNamespace` etc. undefined.

- [ ] **Step 3: Implement the client methods** (`internal/nomad/namespace.go`)

```go
package nomad

import (
	"context"
	"fmt"

	"github.com/hashicorp/nomad/api"
)

// GetNamespace returns the namespace by name, or (nil, nil) if it does not exist.
func (c *Client) GetNamespace(ctx context.Context, name string) (*api.Namespace, error) {
	ns, _, err := c.api.Namespaces().Info(name, queryOpts(ctx))
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("nomad: get namespace %q: %w", name, err)
	}
	return ns, nil
}

// UpsertNamespace creates or updates a namespace (Nomad's Register is an upsert).
func (c *Client) UpsertNamespace(ctx context.Context, ns *api.Namespace) error {
	if _, err := c.api.Namespaces().Register(ns, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: upsert namespace %q: %w", ns.Name, err)
	}
	return nil
}

// DeleteNamespace deletes a namespace by name. Nomad refuses to delete a
// namespace that still has non-terminal jobs (see IsNamespaceNotEmpty).
func (c *Client) DeleteNamespace(ctx context.Context, name string) error {
	if _, err := c.api.Namespaces().Delete(name, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: delete namespace %q: %w", name, err)
	}
	return nil
}

// CountNamespaceJobs returns how many jobs exist in the namespace. Note:
// Jobs().List is unfiltered (includes terminal jobs); this is a raw total, used
// only for the informational status.jobCount (the delete gate is the Delete
// refusal, not this count).
func (c *Client) CountNamespaceJobs(ctx context.Context, name string) (int, error) {
	jobs, _, err := c.api.Jobs().List((&api.QueryOptions{Namespace: name}).WithContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("nomad: list namespace %q jobs: %w", name, err)
	}
	return len(jobs), nil
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/nomad/ -run 'Namespace' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing `IsNamespaceNotEmpty` test** (append to `internal/nomad/errors_test.go`)

```go
func TestIsNamespaceNotEmpty(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "namespace \"team-a\" has non-terminal jobs", http.StatusBadRequest)
	})
	err := c.DeleteNamespace(t.Context(), "team-a")
	if err == nil || !IsNamespaceNotEmpty(err) {
		t.Fatalf("IsNamespaceNotEmpty(%v) = false, want true", err)
	}
	if IsNamespaceNotEmpty(nil) {
		t.Fatal("IsNamespaceNotEmpty(nil) = true, want false")
	}
}
```

- [ ] **Step 6: Run to verify it fails**

Run: `go test ./internal/nomad/ -run 'IsNamespaceNotEmpty' -v`
Expected: FAIL — `IsNamespaceNotEmpty` undefined.

- [ ] **Step 7: Implement `IsNamespaceNotEmpty`** (append to `internal/nomad/errors.go`, after `IsNodePoolNotEmpty`)

```go
// namespaceNotEmptyTexts are the substrings Nomad's server embeds in the error
// body when a namespace cannot be deleted because it still has non-terminal
// jobs. The exact v2.0.4 wording is confirmed by the T11 integration spike;
// keep this list in sync with that finding. Used only to choose a friendlier
// DeleteBlocked reason — control flow keeps the finalizer on ANY Delete error.
var namespaceNotEmptyTexts = []string{"has non-terminal jobs", "has non-terminal"}

// IsNamespaceNotEmpty reports whether err is Nomad's refusal to delete a
// namespace that still has non-terminal jobs.
func IsNamespaceNotEmpty(err error) bool {
	ure, ok := errors.AsType[api.UnexpectedResponseError](err)
	if !ok {
		return false
	}
	body := ure.Body()
	for _, s := range namespaceNotEmptyTexts {
		if strings.Contains(body, s) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 8: Run to verify it passes**

Run: `go test ./internal/nomad/ -run 'IsNamespaceNotEmpty' -v`
Expected: PASS.

- [ ] **Step 9: Add the contract pins** (`internal/nomad/contract.go`). In the type-pin `var` block add `_ api.Namespace` and `_ api.JobListStub`; in the method-pin block add:

```go
	_ = (*api.Client).Namespaces
	_ = (*api.Namespaces).Info
	_ = (*api.Namespaces).Register
	_ = (*api.Namespaces).Delete
	_ = (*api.Jobs).List
```

- [ ] **Step 10: Run the package build + tests**

Run: `go build ./internal/nomad/... && go test ./internal/nomad/ -run 'Namespace|IsNamespaceNotEmpty' -v`
Expected: build OK, PASS.

- [ ] **Step 11: Commit**

```bash
git add internal/nomad/namespace.go internal/nomad/namespace_test.go internal/nomad/errors.go internal/nomad/errors_test.go internal/nomad/contract.go
git commit -m "feat(nomad): namespace client methods + IsNamespaceNotEmpty + contract pins"
```

---

## Task 2: `NomadNamespace` CRD types + CEL + kustomization base

**Files:**
- Create: `api/v1alpha1/nomadnamespace_types.go`
- Modify: `config/crd/kustomization.yaml`

**Interfaces:**
- Produces: `nomadv1alpha1.NomadNamespace{Spec: NomadNamespaceSpec{ClusterRef NamespaceClusterRef, NamespaceName string, Description string, Meta map[string]string}, Status: NomadNamespaceStatus{JobCount int, ObservedGeneration int64, Conditions []metav1.Condition}}`; constants `NomadNamespaceCondReady`, `NomadNamespaceCondDeleteBlocked`, `ReasonNamespaceNameConflict`, `ReasonReservedNamespace`, `ReasonNamespaceNotEmpty`.

- [ ] **Step 1: Write the types file** (`api/v1alpha1/nomadnamespace_types.go`)

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
)

// NomadNamespace condition types and reasons. ReasonRegistered/ReasonClusterNotFound/
// ReasonDeleteFailed (nomadpool_types.go) and ReasonClusterNotReady (nomadnode_types.go)
// are declared elsewhere in this package and reused here.
const (
	NomadNamespaceCondReady         = "Ready"
	NomadNamespaceCondDeleteBlocked = "DeleteBlocked"

	ReasonNamespaceNameConflict = "NamespaceNameConflict"
	ReasonReservedNamespace     = "ReservedNamespace"
	ReasonNamespaceNotEmpty     = "NamespaceNotEmpty"
)

// NamespaceClusterRef names a NomadCluster in the same namespace.
type NamespaceClusterRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// NomadNamespaceSpec is the desired state of a Nomad namespace (a Nomad-internal
// tenancy partition, NOT a Kubernetes namespace). clusterRef + namespaceName are
// the immutable identity; description + meta are the managed body. The CR is the
// source of truth: the operator upserts it onto Nomad (read-modify-write,
// preserving unmanaged server fields) and deletes it.
//
// +kubebuilder:validation:XValidation:rule="self.namespaceName == oldSelf.namespaceName",message="namespaceName is immutable"
// +kubebuilder:validation:XValidation:rule="self.clusterRef.name == oldSelf.clusterRef.name",message="clusterRef.name is immutable"
type NomadNamespaceSpec struct {
	// ClusterRef names the NomadCluster (same namespace) this namespace lives on.
	// +kubebuilder:validation:Required
	ClusterRef NamespaceClusterRef `json:"clusterRef"`
	// NamespaceName is the exact Nomad namespace name. It is separate from
	// metadata.name because Nomad namespace names may contain characters illegal
	// in a Kubernetes object name. The built-in "default" namespace always exists
	// and cannot be deleted, so it is rejected.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9-_]{1,128}$`
	// +kubebuilder:validation:XValidation:rule="self != 'default'",message="namespaceName 'default' is built-in and cannot be managed"
	NamespaceName string `json:"namespaceName"`
	// Description is a human-readable namespace description.
	// +optional
	Description string `json:"description,omitempty"`
	// Meta is a fully-managed key/value map on the namespace. spec.meta owns it
	// entirely; out-of-band Meta keys are overwritten.
	// +optional
	Meta map[string]string `json:"meta,omitempty"`
}

// NomadNamespaceStatus is the observed state, operator-owned.
type NomadNamespaceStatus struct {
	// JobCount is the total number of jobs in the namespace (informational;
	// populated on the delete-blocked path and steady-state resync). It is a raw
	// count from Jobs().List — the delete gate is the Delete refusal, not this.
	// +optional
	JobCount int `json:"jobCount,omitempty"`
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
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespaceName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NomadNamespace is the Schema for the nomadnamespaces API.
type NomadNamespace struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec NomadNamespaceSpec `json:"spec"`
	// +optional
	Status NomadNamespaceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NomadNamespaceList contains a list of NomadNamespace.
type NomadNamespaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NomadNamespace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NomadNamespace{}, &NomadNamespaceList{})
}
```

- [ ] **Step 2: Generate manifests + deepcopy**

Run: `make manifests generate`
Expected: creates `config/crd/bases/nomad.operator.io_nomadnamespaces.yaml` and `zz_generated.deepcopy.go` entries; no errors.

- [ ] **Step 3: Add the base to kustomization** (`config/crd/kustomization.yaml`). Add under `resources:` (controller-gen does not update the list — the slice-3 `6c3e0c1` lesson):

```yaml
- bases/nomad.operator.io_nomadnamespaces.yaml
```

- [ ] **Step 4: Verify the build + no stray drift**

Run: `go build ./... && make manifests generate && git diff --exit-code config/crd/bases zz_generated* 2>/dev/null; go vet ./api/...`
Expected: build OK, second generate produces no new diff, vet clean.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/nomadnamespace_types.go config/crd/
git commit -m "feat(api): NomadNamespace CRD types + CEL + kustomization base"
```

---

## Task 3: `NomadNamespaceOps` interface + factory + fake

**Files:**
- Create: `internal/controller/nomadnamespace_ops.go`
- Create: `internal/controller/fake_nomadnamespace_test.go`

**Interfaces:**
- Produces: `NomadNamespaceOps` (methods `GetNamespace`/`UpsertNamespace`/`DeleteNamespace`/`CountNamespaceJobs`); `NomadNamespaceClientFactory func(cfg nomad.Config) (NomadNamespaceOps, error)`; `DefaultNomadNamespaceClientFactory`; test double `fakeNomadNamespaceOps` with `newFakeNamespaceOps()` and `.factory()`.

- [ ] **Step 1: Write the interface + factory** (`internal/controller/nomadnamespace_ops.go`)

```go
package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// NomadNamespaceOps is the subset of the Nomad client the namespace reconciler
// needs, defined at the consumer (Go convention). *nomad.Client satisfies it,
// and envtest injects a fake. It is intentionally separate from the other *Ops
// interfaces so the controllers' test seams stay decoupled.
type NomadNamespaceOps interface {
	GetNamespace(ctx context.Context, name string) (*api.Namespace, error)
	UpsertNamespace(ctx context.Context, ns *api.Namespace) error
	DeleteNamespace(ctx context.Context, name string) error
	CountNamespaceJobs(ctx context.Context, name string) (int, error)
}

// NomadNamespaceClientFactory builds a NomadNamespaceOps from a per-cluster Config.
type NomadNamespaceClientFactory func(cfg nomad.Config) (NomadNamespaceOps, error)

// DefaultNomadNamespaceClientFactory constructs the real client.
func DefaultNomadNamespaceClientFactory(cfg nomad.Config) (NomadNamespaceOps, error) {
	return nomad.New(cfg)
}
```

- [ ] **Step 2: Write the fake** (`internal/controller/fake_nomadnamespace_test.go`)

```go
package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// fakeNomadNamespaceOps is a scriptable NomadNamespaceOps for envtest. Set the
// fields to control behavior and inspect the recorded calls.
type fakeNomadNamespaceOps struct {
	namespaces map[string]*api.Namespace // seeded state, keyed by name
	jobCount   int

	getErr    error
	upsertErr error
	deleteErr error

	registered []*api.Namespace // every UpsertNamespace arg, in order
	deleted    []string         // every DeleteNamespace name, in order
}

func newFakeNamespaceOps() *fakeNomadNamespaceOps {
	return &fakeNomadNamespaceOps{namespaces: map[string]*api.Namespace{}}
}

func (f *fakeNomadNamespaceOps) GetNamespace(_ context.Context, name string) (*api.Namespace, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.namespaces[name], nil // nil == not found, matching the real 404 mapping
}

func (f *fakeNomadNamespaceOps) UpsertNamespace(_ context.Context, ns *api.Namespace) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	cp := *ns
	f.registered = append(f.registered, &cp)
	f.namespaces[ns.Name] = &cp
	return nil
}

func (f *fakeNomadNamespaceOps) DeleteNamespace(_ context.Context, name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, name)
	delete(f.namespaces, name)
	return nil
}

func (f *fakeNomadNamespaceOps) CountNamespaceJobs(_ context.Context, _ string) (int, error) {
	return f.jobCount, nil
}

// factory returns a NomadNamespaceClientFactory that always yields this fake.
func (f *fakeNomadNamespaceOps) factory() NomadNamespaceClientFactory {
	return func(_ nomad.Config) (NomadNamespaceOps, error) { return f, nil }
}

var _ NomadNamespaceOps = (*fakeNomadNamespaceOps)(nil)
```

- [ ] **Step 3: Verify it compiles** (the fake is `_test.go`, so build the test binary)

Run: `go vet ./internal/controller/ && go test ./internal/controller/ -run 'XXX_nonexistent' 2>&1 | head`
Expected: compiles (no "undefined" / interface-satisfaction errors); the `var _ NomadNamespaceOps` assertion holds.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/nomadnamespace_ops.go internal/controller/fake_nomadnamespace_test.go
git commit -m "feat(controller): NomadNamespaceOps interface + factory + fake"
```

---

## Task 4: NomadNamespace reconciler skeleton — cluster resolution, finalizer, ownerRef, wiring

**Files:**
- Create: `internal/controller/nomadnamespace_controller.go`
- Create: `internal/controller/nomadnamespace_controller_test.go`

**Interfaces:**
- Consumes: `NomadNamespaceOps` (Task 3), `clusterNomadConfig` (`internal/controller/nomadclient.go`), `nomadv1alpha1.NomadNamespace` (Task 2).
- Produces: `NomadNamespaceReconciler{Client, Scheme, NewNomadClient NomadNamespaceClientFactory, Recorder}` with `Reconcile`/`SetupWithManager`; helpers `setNamespaceCondition`, `dropNamespaceFinalizer`; `reconcileNamespace(ctx, nn, ops)` (stubbed here, filled in Tasks 5–6).

- [ ] **Step 1: Write the failing envtest — ClusterNotFound + finalizer** (`internal/controller/nomadnamespace_controller_test.go`). Reuse the suite's shared `k8s` client and `readyCluster` helper (defined in the pool/job tests, same package).

```go
package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

var _ = Describe("NomadNamespace reconciler: cluster resolution", func() {
	It("sets ClusterNotFound and adds the finalizer when the cluster is missing", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-notfound-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef:    nomadv1alpha1.NamespaceClusterRef{Name: "missing"},
				NamespaceName: "team-a",
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotFound))
		Expect(controllerutil.ContainsFinalizer(&got, nomadNamespaceFinalizer)).To(BeTrue(), "finalizer not added")
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
	})

	It("sets ClusterNotReady and registers nothing when the cluster is not Ready", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-notready-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nc := readyCluster(ctx, ns.Name)
		nc.Status.Phase = nomadv1alpha1.PhaseDegraded
		Expect(k8s.Status().Update(ctx, nc)).To(Succeed())

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef:    nomadv1alpha1.NamespaceClusterRef{Name: nc.Name},
				NamespaceName: "team-a",
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotReady))
		Expect(f.registered).To(BeEmpty(), "must not Register when cluster not Ready")
	})
})
```

- [ ] **Step 2: Run to verify it fails**

Run: `make test` (or `go test ./internal/controller/ -run 'TestControllers' -v` if the suite entrypoint is named so)
Expected: FAIL — `NomadNamespaceReconciler`, `nomadNamespaceFinalizer` undefined.

- [ ] **Step 3: Implement the reconciler skeleton** (`internal/controller/nomadnamespace_controller.go`). This mirrors `nomadpool_controller.go` exactly (cluster resolution, finalizer, ownerRef, setup); `reconcileNamespace` is a Ready-only stub filled in Tasks 5–6.

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
	namespaceResync         = 60 * time.Second
	nomadNamespaceFinalizer = "nomad.operator.io/nomadnamespace-cleanup"
)

// NomadNamespaceReconciler manages Nomad namespaces declared as NomadNamespace
// CRs. The CR is the source of truth: the operator upserts the namespace onto
// Nomad and deletes it (finalizer-gated).
type NomadNamespaceReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NewNomadClient NomadNamespaceClientFactory
	Recorder       record.EventRecorder
}

// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadnamespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadnamespaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadnamespaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *NomadNamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var nn nomadv1alpha1.NomadNamespace
	if err := r.Get(ctx, req.NamespacedName, &nn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !nn.DeletionTimestamp.IsZero() {
		return r.finalizeNamespace(ctx, &nn)
	}

	if !controllerutil.ContainsFinalizer(&nn, nomadNamespaceFinalizer) {
		controllerutil.AddFinalizer(&nn, nomadNamespaceFinalizer)
		if err := r.Update(ctx, &nn); err != nil {
			return ctrl.Result{}, err
		}
	}

	var nc nomadv1alpha1.NomadCluster
	err := r.Get(ctx, types.NamespacedName{Name: nn.Spec.ClusterRef.Name, Namespace: nn.Namespace}, &nc)
	if apierrors.IsNotFound(err) {
		setNamespaceCondition(&nn, nomadv1alpha1.NomadNamespaceCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotFound, "referenced NomadCluster does not exist")
		nn.Status.ObservedGeneration = nn.Generation
		if err := r.Status().Update(ctx, &nn); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: namespaceResync}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if nc.Status.Phase != nomadv1alpha1.PhaseReady {
		setNamespaceCondition(&nn, nomadv1alpha1.NomadNamespaceCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotReady, "referenced NomadCluster is not Ready")
		nn.Status.ObservedGeneration = nn.Generation
		if err := r.Status().Update(ctx, &nn); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: namespaceResync}, nil
	}

	// Set the ownerReference for GC cascade, writing only when it changes.
	orig := nn.DeepCopy()
	if err := controllerutil.SetControllerReference(&nc, &nn, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if !equality.Semantic.DeepEqual(orig.OwnerReferences, nn.OwnerReferences) {
		if err := r.Update(ctx, &nn); err != nil {
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
	return r.reconcileNamespace(ctx, &nn, ops)
}

// reconcileNamespace applies the declared namespace onto Nomad and derives
// status. Reserved-name guard + upsert (Task 5) and conflict detection (Task 6)
// are layered in; this stub only marks Ready so the skeleton is exercisable.
func (r *NomadNamespaceReconciler) reconcileNamespace(ctx context.Context, nn *nomadv1alpha1.NomadNamespace, _ NomadNamespaceOps) (ctrl.Result, error) {
	setNamespaceCondition(nn, nomadv1alpha1.NomadNamespaceCondReady, metav1.ConditionTrue, nomadv1alpha1.ReasonRegistered, "namespace registered onto Nomad")
	nn.Status.ObservedGeneration = nn.Generation
	if err := r.Status().Update(ctx, nn); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: namespaceResync}, nil
}

// finalizeNamespace is filled in Task 7; here it only drops the finalizer so the
// skeleton compiles and a delete does not hang.
func (r *NomadNamespaceReconciler) finalizeNamespace(ctx context.Context, nn *nomadv1alpha1.NomadNamespace) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(nn, nomadNamespaceFinalizer) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.dropNamespaceFinalizer(ctx, nn)
}

func (r *NomadNamespaceReconciler) dropNamespaceFinalizer(ctx context.Context, nn *nomadv1alpha1.NomadNamespace) error {
	controllerutil.RemoveFinalizer(nn, nomadNamespaceFinalizer)
	return r.Update(ctx, nn)
}

func (r *NomadNamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewNomadClient == nil {
		r.NewNomadClient = DefaultNomadNamespaceClientFactory
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("nomadnamespace")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nomadv1alpha1.NomadNamespace{}).
		Watches(&nomadv1alpha1.NomadCluster{}, handler.EnqueueRequestsFromMapFunc(r.namespacesForCluster)).
		Named("nomadnamespace").
		Complete(r)
}

// namespacesForCluster maps a NomadCluster event to the reconcile keys of every
// NomadNamespace that targets it (so a cluster going Ready reconciles pending ones).
func (r *NomadNamespaceReconciler) namespacesForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	nc, ok := obj.(*nomadv1alpha1.NomadCluster)
	if !ok {
		return nil
	}
	var list nomadv1alpha1.NomadNamespaceList
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

// setNamespaceCondition upserts a status condition, preserving LastTransitionTime
// when the status is unchanged (mirrors setPoolCondition).
func setNamespaceCondition(nn *nomadv1alpha1.NomadNamespace, condType string, status metav1.ConditionStatus, reason, msg string) {
	c := metav1.Condition{Type: condType, Status: status, Reason: reason, Message: msg, ObservedGeneration: nn.Generation}
	for i, existing := range nn.Status.Conditions {
		if existing.Type == condType {
			if existing.Status != status {
				c.LastTransitionTime = metav1.Now()
			} else {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			nn.Status.Conditions[i] = c
			return
		}
	}
	c.LastTransitionTime = metav1.Now()
	nn.Status.Conditions = append(nn.Status.Conditions, c)
}
```

- [ ] **Step 4: Run to verify the two tests pass**

Run: `make test`
Expected: PASS (the new cluster-resolution specs green; existing suites unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadnamespace_controller.go internal/controller/nomadnamespace_controller_test.go
git commit -m "feat(controller): NomadNamespace reconciler skeleton (cluster resolution, finalizer, wiring)"
```

---

## Task 5: Apply — reserved-name guard, read-modify-write upsert, compare-before-write, status jobCount

**Files:**
- Modify: `internal/controller/nomadnamespace_controller.go` (replace the `reconcileNamespace` stub; add `desiredNamespace`)
- Modify: `internal/controller/nomadnamespace_controller_test.go` (add specs)

**Interfaces:**
- Consumes: `NomadNamespaceOps.GetNamespace/UpsertNamespace/CountNamespaceJobs`.
- Produces: real `reconcileNamespace`; `desiredNamespace(existing *api.Namespace, nn *nomadv1alpha1.NomadNamespace) *api.Namespace`.

- [ ] **Step 1: Write the failing specs** (append to `nomadnamespace_controller_test.go`)

```go
var _ = Describe("NomadNamespace reconciler: apply", func() {
	It("registers on create and preserves unmanaged fields on update", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-apply-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef:    nomadv1alpha1.NamespaceClusterRef{Name: nc.Name},
				NamespaceName: "team-a",
				Description:   "Team A",
				Meta:          map[string]string{"owner": "team-a"},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		f := newFakeNamespaceOps()
		// Seed an existing namespace carrying an unmanaged Quota to prove preservation.
		f.namespaces["team-a"] = &api.Namespace{Name: "team-a", Quota: "q1", Description: "old"}
		f.jobCount = 3
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(f.registered).To(HaveLen(1))
		Expect(f.registered[0].Description).To(Equal("Team A"))
		Expect(f.registered[0].Meta).To(Equal(map[string]string{"owner": "team-a"}))
		Expect(f.registered[0].Quota).To(Equal("q1"), "unmanaged Quota must be preserved")

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.Status.JobCount).To(Equal(3))
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("does not re-register when description and meta are unchanged", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-nodrift-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: nc.Name}, NamespaceName: "team-a",
				Description: "same", Meta: map[string]string{"k": "v"},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		f := newFakeNamespaceOps()
		f.namespaces["team-a"] = &api.Namespace{Name: "team-a", Description: "same", Meta: map[string]string{"k": "v"}}
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.registered).To(BeEmpty(), "no Register when nothing drifted")
	})
})
```

- [ ] **Step 2: Run to verify it fails**

Run: `make test`
Expected: FAIL — the stub Registers nothing / preserves nothing (assertions on `f.registered` fail).

- [ ] **Step 3: Replace `reconcileNamespace` + add `desiredNamespace`** (`internal/controller/nomadnamespace_controller.go`). Add imports `"maps"` and `"github.com/hashicorp/nomad/api"`.

```go
// reconcileNamespace applies the declared namespace onto Nomad (read-modify-write,
// preserving unmanaged fields) and derives status. Conflict detection (Task 6) is
// layered in.
func (r *NomadNamespaceReconciler) reconcileNamespace(ctx context.Context, nn *nomadv1alpha1.NomadNamespace, ops NomadNamespaceOps) (ctrl.Result, error) {
	// Defense-in-depth guard: CEL already rejects "default" at admission, but
	// never Register/Delete it even if one reaches here.
	if nn.Spec.NamespaceName == "default" {
		setNamespaceCondition(nn, nomadv1alpha1.NomadNamespaceCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonReservedNamespace, "the default namespace is built-in and cannot be managed")
		nn.Status.ObservedGeneration = nn.Generation
		return ctrl.Result{}, r.Status().Update(ctx, nn)
	}

	existing, err := ops.GetNamespace(ctx, nn.Spec.NamespaceName)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := desiredNamespace(existing, nn)
	if existing == nil || existing.Description != desired.Description || !maps.Equal(existing.Meta, desired.Meta) {
		if err := ops.UpsertNamespace(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
	}
	count, err := ops.CountNamespaceJobs(ctx, nn.Spec.NamespaceName)
	if err != nil {
		return ctrl.Result{}, err
	}
	nn.Status.JobCount = count

	setNamespaceCondition(nn, nomadv1alpha1.NomadNamespaceCondReady, metav1.ConditionTrue, nomadv1alpha1.ReasonRegistered, "namespace registered onto Nomad")
	nn.Status.ObservedGeneration = nn.Generation
	if err := r.Status().Update(ctx, nn); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: namespaceResync}, nil
}

// desiredNamespace builds the Namespace to Register: managed fields (Description,
// Meta) from spec, unmanaged fields (Quota, Capabilities, NodePoolConfiguration,
// Vault/Consul config, extra-claims) preserved from the existing namespace.
func desiredNamespace(existing *api.Namespace, nn *nomadv1alpha1.NomadNamespace) *api.Namespace {
	var d api.Namespace
	if existing != nil {
		d = *existing // preserve Quota, Capabilities, config blocks, indexes
	}
	d.Name = nn.Spec.NamespaceName
	d.Description = nn.Spec.Description
	d.Meta = nn.Spec.Meta
	return &d
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `make test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadnamespace_controller.go internal/controller/nomadnamespace_controller_test.go
git commit -m "feat(controller): NomadNamespace apply — reserved guard, read-modify-write, jobCount"
```

---

## Task 6: Duplicate-name conflict detection

**Files:**
- Modify: `internal/controller/nomadnamespace_controller.go` (add conflict check to `reconcileNamespace`; add helpers)
- Modify: `internal/controller/nomadnamespace_controller_test.go`

**Interfaces:**
- Produces: `namespaceClusterKey(nn) string`; `(r).hasNamespaceNameConflict(ctx, nn) (bool, error)`.

- [ ] **Step 1: Write the failing spec** (append)

```go
var _ = Describe("NomadNamespace reconciler: conflict", func() {
	It("sets NamespaceNameConflict and skips Register when a live sibling shares the name", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-conflict-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		first := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a-1", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: nc.Name}, NamespaceName: "team-a"},
		}
		Expect(k8s.Create(ctx, first)).To(Succeed())
		second := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a-2", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: nc.Name}, NamespaceName: "team-a"},
		}
		Expect(k8s.Create(ctx, second)).To(Succeed())

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a-2", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a-2", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonNamespaceNameConflict))
		Expect(f.registered).To(BeEmpty())
	})
})
```

- [ ] **Step 2: Run to verify it fails**

Run: `make test`
Expected: FAIL — second CR Registers (no conflict guard), assertion on `f.registered` fails.

- [ ] **Step 3: Add the conflict check** at the top of `reconcileNamespace`, right after the reserved-name guard:

```go
	conflict, err := r.hasNamespaceNameConflict(ctx, nn)
	if err != nil {
		return ctrl.Result{}, err
	}
	if conflict {
		setNamespaceCondition(nn, nomadv1alpha1.NomadNamespaceCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonNamespaceNameConflict, "another NomadNamespace targets this namespaceName on this cluster; skipping Register")
		r.Recorder.Event(nn, "Warning", nomadv1alpha1.ReasonNamespaceNameConflict, "duplicate namespaceName on the same cluster; not registering to avoid churn")
		nn.Status.ObservedGeneration = nn.Generation
		if err := r.Status().Update(ctx, nn); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: namespaceResync}, nil
	}
```

- [ ] **Step 4: Add the helpers** (append to `nomadnamespace_controller.go`)

```go
// namespaceClusterKey is the composite collision key for a namespace CR.
func namespaceClusterKey(nn *nomadv1alpha1.NomadNamespace) string {
	return nn.Spec.ClusterRef.Name + "/" + nn.Spec.NamespaceName
}

// hasNamespaceNameConflict reports whether another live NomadNamespace in this
// K8s namespace targets the same namespaceName on the same cluster. A Terminating
// sibling does not count (it is being replaced/GC'd and must not block a
// successor). Plain namespaced List + in-Go filter — no field indexer.
func (r *NomadNamespaceReconciler) hasNamespaceNameConflict(ctx context.Context, nn *nomadv1alpha1.NomadNamespace) (bool, error) {
	var list nomadv1alpha1.NomadNamespaceList
	if err := r.List(ctx, &list, client.InNamespace(nn.Namespace)); err != nil {
		return false, err
	}
	key := namespaceClusterKey(nn)
	for i := range list.Items {
		if list.Items[i].Name != nn.Name && list.Items[i].DeletionTimestamp.IsZero() && namespaceClusterKey(&list.Items[i]) == key {
			return true, nil
		}
	}
	return false, nil
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `make test`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/nomadnamespace_controller.go internal/controller/nomadnamespace_controller_test.go
git commit -m "feat(controller): NomadNamespace duplicate-name conflict detection"
```

---

## Task 7: Finalizer delete — block-until-empty + cluster-gone-or-going short-circuit

**Files:**
- Modify: `internal/controller/nomadnamespace_controller.go` (replace the `finalizeNamespace` stub)
- Modify: `internal/controller/nomadnamespace_controller_test.go`

**Interfaces:**
- Consumes: `NomadNamespaceOps.DeleteNamespace/CountNamespaceJobs`, `nomad.IsNotFound`, `nomad.IsNamespaceNotEmpty`.

- [ ] **Step 1: Write the failing specs** (append). Covers: happy delete, not-empty blocked, already-gone drop, cluster-gone short-circuit.

```go
var _ = Describe("NomadNamespace reconciler: finalize", func() {
	newNS := func(ctx SpecContext, k8sNS, cluster string, del bool) *nomadv1alpha1.NomadNamespace {
		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: k8sNS, Finalizers: []string{nomadNamespaceFinalizer}},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: cluster}, NamespaceName: "team-a"},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		Expect(k8s.Delete(ctx, nn)).To(Succeed()) // sets DeletionTimestamp; finalizer holds it
		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: k8sNS}, &got)).To(Succeed())
		return &got
	}

	It("deletes the namespace and drops the finalizer when the cluster is Ready and it is empty", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-fin-ok-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := newNS(ctx, ns.Name, nc.Name, true)

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nn.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deleted).To(ConsistOf("team-a"))
		// CR should now be gone (finalizer dropped → GC).
		var got nomadv1alpha1.NomadNamespace
		Expect(apierrors.IsNotFound(k8s.Get(ctx, types.NamespacedName{Name: nn.Name, Namespace: ns.Name}, &got))).To(BeTrue())
	})

	It("holds with NamespaceNotEmpty when Delete is refused for non-terminal jobs", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-fin-busy-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := newNS(ctx, ns.Name, nc.Name, true)

		f := newFakeNamespaceOps()
		f.jobCount = 2
		f.deleteErr = notEmptyErr() // a genuine "not empty" api.UnexpectedResponseError the classifier matches
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nn.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: nn.Name, Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondDeleteBlocked)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonNamespaceNotEmpty))
		Expect(got.Status.JobCount).To(Equal(2))
		Expect(controllerutil.ContainsFinalizer(&got, nomadNamespaceFinalizer)).To(BeTrue())
	})

	It("short-circuits (drops finalizer, no Delete) when the cluster is gone", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-fin-gone-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nn := newNS(ctx, ns.Name, "missing-cluster", true)

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nn.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deleted).To(BeEmpty(), "must not call Delete when cluster is gone")
		var got nomadv1alpha1.NomadNamespace
		Expect(apierrors.IsNotFound(k8s.Get(ctx, types.NamespacedName{Name: nn.Name, Namespace: ns.Name}, &got))).To(BeTrue())
	})
})
```

Add this helper near the top of the test file — it produces a **real** `api.UnexpectedResponseError` the classifier matches, using the same httptest→`nomad.New` round-trip the job controller tests already use (SGE plan-review M1; do NOT add a production `nomad.*ForTest()` seam). Add the imports `"context"`, `"net/http"`, `"net/http/httptest"`, and `"github.com/jacaudi/nomad-operator/internal/nomad"` to the test file.

```go
// notEmptyErr returns a genuine "namespace has non-terminal jobs" error by
// round-tripping a 400 through a throwaway nomad client — the proven pattern the
// job controller tests use to obtain a real api.UnexpectedResponseError. No
// production test-seam is added.
func notEmptyErr() error {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `namespace "team-a" has non-terminal jobs`, http.StatusBadRequest)
	}))
	DeferCleanup(srv.Close)
	c, err := nomad.New(nomad.Config{Address: srv.URL})
	Expect(err).NotTo(HaveOccurred())
	return c.DeleteNamespace(context.Background(), "team-a")
}
```

> **Implementer note:** this mirrors `internal/controller/nomadjob_controller_test.go` (the existing httptest-4xx→`nomad.New` pattern for a real `api.UnexpectedResponseError`). `DeferCleanup` is Ginkgo's; it runs after the spec. If wiring a real error proves heavy in review, the acceptable fallback is to assert the not-empty *classification* in the `internal/nomad` suite (Task 1's `TestIsNamespaceNotEmpty` already does) and here assert only the `DeleteFailed`-vs-blocked control flow with a plain `errors.New` — but prefer the real error above so the `NamespaceNotEmpty` reason is exercised end-to-end.

- [ ] **Step 2: Run to verify it fails**

Run: `make test`
Expected: FAIL — stub `finalizeNamespace` just drops the finalizer; no Delete recorded, no blocked condition.

- [ ] **Step 3: Replace `finalizeNamespace`** (`internal/controller/nomadnamespace_controller.go`). Add imports `apierrors` (already present) and `"github.com/jacaudi/nomad-operator/internal/nomad"`.

```go
// finalizeNamespace deletes the Nomad namespace when the CR is deleted, gated so
// it never deadlocks a cascade: if the cluster is gone OR going, drop the
// finalizer without calling Delete (control plane gone/going ⇒ namespace too).
// Closes both background and foreground cascade (design §3.5).
func (r *NomadNamespaceReconciler) finalizeNamespace(ctx context.Context, nn *nomadv1alpha1.NomadNamespace) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(nn, nomadNamespaceFinalizer) {
		return ctrl.Result{}, nil
	}

	var nc nomadv1alpha1.NomadCluster
	err := r.Get(ctx, types.NamespacedName{Name: nn.Spec.ClusterRef.Name, Namespace: nn.Namespace}, &nc)
	clusterGoneOrGoing := apierrors.IsNotFound(err) || (err == nil && !nc.DeletionTimestamp.IsZero())
	if clusterGoneOrGoing {
		return ctrl.Result{}, r.dropNamespaceFinalizer(ctx, nn)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if nc.Status.Phase != nomadv1alpha1.PhaseReady {
		setNamespaceCondition(nn, nomadv1alpha1.NomadNamespaceCondDeleteBlocked, metav1.ConditionTrue, nomadv1alpha1.ReasonClusterNotReady, "cluster not Ready; cannot confirm namespace deletion")
		if uerr := r.Status().Update(ctx, nn); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: namespaceResync}, nil
	}

	cfg, err := clusterNomadConfig(ctx, r.Client, &nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	ops, err := r.NewNomadClient(cfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	if derr := ops.DeleteNamespace(ctx, nn.Spec.NamespaceName); derr != nil && !nomad.IsNotFound(derr) {
		// Delete failed for a reason other than "already gone". Keep the
		// finalizer and requeue. Surface a friendly reason when the namespace is
		// non-empty; fetch the job count so the user sees what holds it.
		reason := nomadv1alpha1.ReasonDeleteFailed
		if nomad.IsNamespaceNotEmpty(derr) {
			reason = nomadv1alpha1.ReasonNamespaceNotEmpty
		}
		if jobs, e := ops.CountNamespaceJobs(ctx, nn.Spec.NamespaceName); e == nil {
			nn.Status.JobCount = jobs
		}
		setNamespaceCondition(nn, nomadv1alpha1.NomadNamespaceCondDeleteBlocked, metav1.ConditionTrue, reason, derr.Error())
		if uerr := r.Status().Update(ctx, nn); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: namespaceResync}, nil
	}
	// Delete succeeded OR the namespace is already gone (404) — drop the finalizer.
	return ctrl.Result{}, r.dropNamespaceFinalizer(ctx, nn)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `make test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadnamespace_controller.go internal/controller/nomadnamespace_controller_test.go
git commit -m "feat(controller): NomadNamespace finalizer — block-until-empty + cluster short-circuit"
```

---

## Task 8: CEL behavioral tests (immutability, reject default, name pattern)

**Files:**
- Modify: `internal/controller/nomadnamespace_controller_test.go` (add a CEL Describe block)

These run against the real CRD schema loaded by the envtest suite, so they exercise the CEL `XValidation` rules from Task 2.

- [ ] **Step 1: Write the CEL specs** (append)

```go
var _ = Describe("NomadNamespace CEL", func() {
	It("rejects namespaceName 'default'", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-cel-default-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: "c"}, NamespaceName: "default"},
		}
		Expect(k8s.Create(ctx, nn)).NotTo(Succeed())
	})

	It("rejects a namespaceName with illegal characters", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-cel-pattern-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: "c"}, NamespaceName: "has spaces!"},
		}
		Expect(k8s.Create(ctx, nn)).NotTo(Succeed())
	})

	It("rejects mutating namespaceName", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-cel-immutable-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: "c"}, NamespaceName: "team-a"},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		nn.Spec.NamespaceName = "team-b"
		Expect(k8s.Update(ctx, nn)).NotTo(Succeed())
	})
})
```

- [ ] **Step 2: Run**

Run: `make test`
Expected: PASS (CEL rejects all three; the valid create in the immutability test succeeds).

- [ ] **Step 3: Commit**

```bash
git add internal/controller/nomadnamespace_controller_test.go
git commit -m "test(controller): NomadNamespace CEL immutability + reserved-name + pattern"
```

---

## Task 9: NomadJob `spec.nomadNamespace` field + decode injection + NamespaceMismatch

**Files:**
- Modify: `api/v1alpha1/nomadjob_types.go`
- Modify: `internal/controller/nomadjob_controller.go` (`decodeJob`, reason mapping in `reconcileJob`)
- Modify: `internal/controller/nomadjob_controller_test.go` (decode unit test — `decodeJob` is a package function)

**Interfaces:**
- Produces: `NomadJobSpec.NomadNamespace string`; `ReasonNamespaceMismatch = "NamespaceMismatch"`; `errNamespaceMismatch`; `decodeJob` now injects `job.Namespace = &spec.NomadNamespace` and rejects a disagreeing blob namespace.

> **Note:** this task threads the value into `job.Namespace` (so `RegisterJob`/`PlanJob`, which read the job body, already target the right namespace) but does **not** yet thread it through the ID-addressed client methods — that is Task 10, which completes the orphan fix.

- [ ] **Step 1: Add the field + reason** (`api/v1alpha1/nomadjob_types.go`). Add `ReasonNamespaceMismatch = "NamespaceMismatch"` to the const block. Add a third struct-level CEL rule and the field:

Add to the `// +kubebuilder:validation:XValidation` rules above `NomadJobSpec`:
```go
// +kubebuilder:validation:XValidation:rule="self.nomadNamespace == oldSelf.nomadNamespace",message="nomadNamespace is immutable"
```
Add the field to `NomadJobSpec` (after `JobID`):
```go
	// NomadNamespace is the Nomad namespace (a Nomad-internal tenancy partition,
	// NOT the Kubernetes namespace) this job is placed into. Immutable because
	// Nomad job identity is (namespace, jobID). The named namespace must already
	// exist (via a NomadNamespace CR, out-of-band, or the always-present
	// "default"); the operator injects it as the authoritative job.Namespace, and
	// a differing namespace inside spec.job is rejected.
	// +optional
	// +kubebuilder:default="default"
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9-_]{1,128}$`
	NomadNamespace string `json:"nomadNamespace,omitempty"`
```

- [ ] **Step 2: Regenerate + verify**

Run: `make manifests generate && go build ./...`
Expected: CRD + deepcopy updated; build OK.

- [ ] **Step 3: Write the failing decode unit test** (append to `nomadjob_controller_test.go`, in the `controller` package). If the existing decode tests live in a table, add these cases there.

```go
var _ = Describe("decodeJob namespace injection", func() {
	It("injects spec.nomadNamespace into job.Namespace", func() {
		spec := nomadv1alpha1.NomadJobSpec{
			JobID:          "web",
			NomadNamespace: "team-a",
			Job:            runtime.RawExtension{Raw: []byte(`{"type":"service"}`)},
		}
		job, err := decodeJob(spec, "global")
		Expect(err).NotTo(HaveOccurred())
		Expect(job.Namespace).NotTo(BeNil())
		Expect(*job.Namespace).To(Equal("team-a"))
	})

	It("rejects a blob namespace that disagrees with spec.nomadNamespace", func() {
		spec := nomadv1alpha1.NomadJobSpec{
			JobID:          "web",
			NomadNamespace: "team-a",
			Job:            runtime.RawExtension{Raw: []byte(`{"namespace":"team-b"}`)},
		}
		_, err := decodeJob(spec, "global")
		Expect(err).To(MatchError(errNamespaceMismatch))
	})

	It("accepts a blob namespace equal to spec.nomadNamespace", func() {
		spec := nomadv1alpha1.NomadJobSpec{
			JobID:          "web",
			NomadNamespace: "team-a",
			Job:            runtime.RawExtension{Raw: []byte(`{"namespace":"team-a"}`)},
		}
		job, err := decodeJob(spec, "global")
		Expect(err).NotTo(HaveOccurred())
		Expect(*job.Namespace).To(Equal("team-a"))
	})
})
```

Ensure the test file imports `"k8s.io/apimachinery/pkg/runtime"` (add if missing).

- [ ] **Step 4: Run to verify it fails**

Run: `make test`
Expected: FAIL — `errNamespaceMismatch` undefined; `job.Namespace` not injected.

- [ ] **Step 5: Update `decodeJob` + add the sentinel** (`internal/controller/nomadjob_controller.go`). Add the sentinel near `errJobIDMismatch`:

```go
// errNamespaceMismatch is returned by decodeJob when spec.job carries an explicit
// namespace that disagrees with spec.nomadNamespace (spec.nomadNamespace is authoritative).
var errNamespaceMismatch = errors.New("job.namespace does not match spec.nomadNamespace")
```

In `decodeJob`, after the jobID-mismatch check and before injecting identity:

```go
	if job.Namespace != nil && *job.Namespace != "" && *job.Namespace != spec.NomadNamespace {
		return nil, fmt.Errorf("%w: job.namespace=%q spec.nomadNamespace=%q", errNamespaceMismatch, *job.Namespace, spec.NomadNamespace)
	}
	job.ID = &spec.JobID
	job.Region = &region
	job.Namespace = &spec.NomadNamespace
	return &job, nil
```
(Remove the old `job.ID = &spec.JobID; job.Region = &region; return &job, nil` tail — it is replaced by the block above.)

- [ ] **Step 6: Map the sentinel to the reason** in `reconcileJob`, extending the existing decode-error branch:

```go
	desired, err := decodeJob(nj.Spec, region)
	if err != nil {
		reason := nomadv1alpha1.ReasonInvalidJobSpec
		if errors.Is(err, errJobIDMismatch) {
			reason = nomadv1alpha1.ReasonJobIDMismatch
		}
		if errors.Is(err, errNamespaceMismatch) {
			reason = nomadv1alpha1.ReasonNamespaceMismatch
		}
		// ... unchanged: setJobCondition(..., reason, err.Error()); status update; requeue
```

- [ ] **Step 7: Run to verify it passes**

Run: `make test`
Expected: PASS (new decode specs green; existing NomadJob specs unaffected — injecting `job.Namespace="default"` by default is inert for existing tests).

- [ ] **Step 8: Commit**

```bash
git add api/v1alpha1/nomadjob_types.go internal/controller/nomadjob_controller.go internal/controller/nomadjob_controller_test.go config/crd/
git commit -m "feat(nomadjob): spec.nomadNamespace field + decode injection + NamespaceMismatch"
```

---

## Task 10: Thread the Nomad namespace through the job client + finalizer (root orphan fix)

**Files:**
- Modify: `internal/nomad/client.go` (job method signatures)
- Modify: `internal/nomad/job_test.go` (namespace-passing assertions)
- Modify: `internal/controller/nomadjob_ops.go` (interface signature)
- Modify: `internal/controller/fake_nomadjob_test.go` (fake signature + recording)
- Modify: `internal/controller/nomadjob_controller.go` (caller namespace args)
- Modify: `internal/controller/nomadjob_controller_test.go` (orphan-regression envtest; fix the real client call at the `nomadClient.DeregisterJob(ctx, "web", true)` site)
- Modify: `internal/nomad/job_integration_test.go` (behind `//go:build integration` — its `GetJob`/`DeregisterJob`/`JobGroupSummary` calls break too, and untagged `make test` will NOT catch them — SGE I1)

**Interfaces:**
- Produces: `GetJob(ctx, namespace, jobID)`, `DeregisterJob(ctx, namespace, jobID, purge)`, `JobGroupSummary(ctx, namespace, jobID)`; `PlanJob`/`RegisterJob` set `WriteOptions.Namespace` from `job.Namespace` (nil-guarded). `NomadJobOps` reflects the three new signatures.

- [ ] **Step 1: Write the failing unit test** (append to `internal/nomad/job_test.go`) — asserts the namespace reaches the wire.

```go
func TestGetJob_ScopesByNamespace(t *testing.T) {
	var gotNS string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotNS = r.URL.Query().Get("namespace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ID":"web"}`))
	})
	if _, err := c.GetJob(t.Context(), "team-a", "web"); err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if gotNS != "team-a" {
		t.Fatalf("namespace = %q, want team-a", gotNS)
	}
}

func TestDeregisterJob_ScopesByNamespace(t *testing.T) {
	var gotNS string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotNS = r.URL.Query().Get("namespace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"EvalID":"e"}`))
	})
	if err := c.DeregisterJob(t.Context(), "team-a", "web", true); err != nil {
		t.Fatalf("DeregisterJob: %v", err)
	}
	if gotNS != "team-a" {
		t.Fatalf("namespace = %q, want team-a", gotNS)
	}
}

func TestRegisterJob_ScopesByJobNamespace(t *testing.T) {
	var gotNS string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotNS = r.URL.Query().Get("namespace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"EvalID":"e"}`))
	})
	id, nsName := "web", "team-a"
	if _, err := c.RegisterJob(t.Context(), &api.Job{ID: &id, Namespace: &nsName}); err != nil {
		t.Fatalf("RegisterJob: %v", err)
	}
	if gotNS != "team-a" {
		t.Fatalf("namespace = %q, want team-a", gotNS)
	}
}
```

Also update **all four** existing `internal/nomad/job_test.go` call sites that now need a namespace argument (SGE M2): `TestGetJob_NotFound` (`c.GetJob(t.Context(), "default", "missing")`), the `TestPlanJob_*` cases (signature unchanged — `PlanJob` still takes just the job — but confirm they compile), `TestJobGroupSummary` (`c.JobGroupSummary(t.Context(), "default", ...)`), and `TestDeregisterJob_OK` (`c.DeregisterJob(t.Context(), "default", ...)`).

- [ ] **Step 2: Run to verify it fails / does not compile**

Run: `go test ./internal/nomad/ -run 'Job' -v`
Expected: build error — `GetJob`/`DeregisterJob` signatures don't match; new tests undefined behavior.

- [ ] **Step 3: Update the client methods** (`internal/nomad/client.go`). Add a nil-safe helper and change the five job methods:

```go
// jobWriteOpts builds WriteOptions carrying the job's namespace (nil-safe). The
// controller injects job.Namespace before Plan/Register, but the deref is
// guarded so a nil Namespace from any caller cannot panic.
func jobWriteOpts(ctx context.Context, job *api.Job) *api.WriteOptions {
	w := &api.WriteOptions{}
	if job != nil && job.Namespace != nil {
		w.Namespace = *job.Namespace
	}
	return w.WithContext(ctx)
}

// nsQueryOpts builds QueryOptions scoped to a Nomad namespace.
func nsQueryOpts(ctx context.Context, namespace string) *api.QueryOptions {
	return (&api.QueryOptions{Namespace: namespace}).WithContext(ctx)
}
```

```go
func (c *Client) GetJob(ctx context.Context, namespace, jobID string) (*api.Job, error) {
	job, _, err := c.api.Jobs().Info(jobID, nsQueryOpts(ctx, namespace))
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("nomad: get job %q: %w", jobID, err)
	}
	return job, nil
}

func (c *Client) PlanJob(ctx context.Context, job *api.Job) (bool, error) {
	resp, _, err := c.api.Jobs().Plan(job, true, jobWriteOpts(ctx, job))
	if err != nil {
		return false, fmt.Errorf("nomad: plan job: %w", err)
	}
	if resp.Diff == nil {
		return true, nil
	}
	return resp.Diff.Type != jobDiffNone, nil
}

func (c *Client) RegisterJob(ctx context.Context, job *api.Job) (string, error) {
	resp, _, err := c.api.Jobs().Register(job, jobWriteOpts(ctx, job))
	if err != nil {
		return "", fmt.Errorf("nomad: register job: %w", err)
	}
	return resp.Warnings, nil
}

func (c *Client) DeregisterJob(ctx context.Context, namespace, jobID string, purge bool) error {
	if _, _, err := c.api.Jobs().Deregister(jobID, purge, (&api.WriteOptions{Namespace: namespace}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: deregister job %q: %w", jobID, err)
	}
	return nil
}

func (c *Client) JobGroupSummary(ctx context.Context, namespace, jobID string) (map[string]api.TaskGroupSummary, error) {
	summary, _, err := c.api.Jobs().Summary(jobID, nsQueryOpts(ctx, namespace))
	if err != nil {
		return nil, fmt.Errorf("nomad: job summary %q: %w", jobID, err)
	}
	return summary.Summary, nil
}
```

- [ ] **Step 4: Update the ops interface** (`internal/controller/nomadjob_ops.go`)

```go
type NomadJobOps interface {
	GetJob(ctx context.Context, namespace, jobID string) (*api.Job, error)
	PlanJob(ctx context.Context, job *api.Job) (bool, error)
	RegisterJob(ctx context.Context, job *api.Job) (string, error)
	DeregisterJob(ctx context.Context, namespace, jobID string, purge bool) error
	JobGroupSummary(ctx context.Context, namespace, jobID string) (map[string]api.TaskGroupSummary, error)
}
```

- [ ] **Step 5: Update the fake** (`internal/controller/fake_nomadjob_test.go`) — new signatures + record the namespace on deregister for the orphan assertion:

```go
	deregisteredNS []string // the namespace for each DeregisterJob, in order
```
```go
func (f *fakeNomadJobOps) GetJob(_ context.Context, _ /*namespace*/ string, jobID string) (*api.Job, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.jobs[jobID], nil
}

func (f *fakeNomadJobOps) DeregisterJob(_ context.Context, namespace, jobID string, purge bool) error {
	if f.deregisterErr != nil {
		return f.deregisterErr
	}
	f.deregistered = append(f.deregistered, jobID)
	f.deregisteredNS = append(f.deregisteredNS, namespace)
	f.purged = append(f.purged, purge)
	delete(f.jobs, jobID)
	return nil
}

func (f *fakeNomadJobOps) JobGroupSummary(_ context.Context, _ /*namespace*/ string, _ string) (map[string]api.TaskGroupSummary, error) {
	if f.summaryErr != nil {
		return nil, f.summaryErr
	}
	return f.summary, nil
}
```

- [ ] **Step 6: Update the controller callers** (`internal/controller/nomadjob_controller.go`). In `reconcileJob`:
```go
	info, err := ops.GetJob(ctx, nj.Spec.NomadNamespace, nj.Spec.JobID)
	...
	summary, err := ops.JobGroupSummary(ctx, nj.Spec.NomadNamespace, nj.Spec.JobID)
```
In `finalizeJob`:
```go
	if derr := ops.DeregisterJob(ctx, nj.Spec.NomadNamespace, nj.Spec.JobID, true); derr != nil && !nomad.IsNotFound(derr) {
```

- [ ] **Step 7: Add the orphan-regression envtest** (append to `nomadjob_controller_test.go`) — proves the finalizer deregisters in the job's namespace.

```go
var _ = Describe("NomadJob finalizer namespace threading", func() {
	It("deregisters in spec.nomadNamespace, not default", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-ns-del-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name, Finalizers: []string{nomadJobFinalizer}},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef:     nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:          "web",
				NomadNamespace: "team-a",
				Job:            runtime.RawExtension{Raw: []byte(`{"type":"service"}`)},
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())
		Expect(k8s.Delete(ctx, nj)).To(Succeed())
		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())

		f := newFakeJobOps()
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deregistered).To(ConsistOf("web"))
		Expect(f.deregisteredNS).To(ConsistOf("team-a"), "must deregister in the job's namespace, not default")
	})
})
```

- [ ] **Step 8: Fix every other caller the signature change broke — including `_test.go` and integration-tagged files** (SGE I1). Search WITH test files:

Run: `grep -rn "GetJob(\|DeregisterJob(\|JobGroupSummary(" internal/`
Expected break sites to update: `nomadjob_controller.go` (the three, done in Step 6); `nomadjob_controller_test.go` (the real `nomadClient.DeregisterJob(ctx, "web", true)` call — add the namespace arg); `job_test.go` (the four from Step 1); `job_integration_test.go` (its `GetJob`/`DeregisterJob`/`JobGroupSummary` calls, behind `//go:build integration`).
Then vet BOTH build configurations (untagged `make test` does NOT compile the integration files):

Run: `go vet ./... && go vet -tags integration ./...`
Expected: both clean — no "not enough arguments" / signature errors.

- [ ] **Step 9: Run the full gate (both build configs)**

Run: `go build -tags integration ./internal/nomad/... && make test`
Expected: integration files compile; PASS (nomad unit tests + both controller suites green).

- [ ] **Step 10: Commit**

```bash
git add internal/nomad/client.go internal/nomad/job_test.go internal/controller/nomadjob_ops.go internal/controller/fake_nomadjob_test.go internal/controller/nomadjob_controller.go internal/controller/nomadjob_controller_test.go
git commit -m "fix(nomadjob): thread Nomad namespace through job client + finalizer (orphan fix)"
```

---

## Task 11: Wiring, runbook, and the live integration spike

**Files:**
- Modify: `cmd/main.go`
- Create: `docs/runbooks/nomadnamespace.md`
- Create: `internal/nomad/namespace_integration_test.go`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `NomadNamespaceReconciler` (Task 4).

- [ ] **Step 1: Wire the reconciler** (`cmd/main.go`). After the `NomadJobReconciler` block and before `// +kubebuilder:scaffold:builder`:

```go
	if err := (&controller.NomadNamespaceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "nomadnamespace")
		os.Exit(1)
	}
```

- [ ] **Step 2: Verify the build**

Run: `go build ./... && make manifests generate && git diff --exit-code config/ 2>/dev/null`
Expected: build OK; RBAC role regenerated to include `nomadnamespaces` (commit that diff); no unexpected drift.

- [ ] **Step 3: Write the runbook** (`docs/runbooks/nomadnamespace.md`). Follow the structure of `docs/runbooks/nomadpool.md`. Cover: what the CRD does (manage a Nomad namespace); the `spec` fields (`clusterRef`, `namespaceName` immutable, `description`/`meta` managed); the `Ready`/`DeleteBlocked` conditions and every reason (`Registered`, `ClusterNotFound`, `ClusterNotReady`, `ReservedNamespace`, `NamespaceNameConflict`, `NamespaceNotEmpty`, `DeleteFailed`); the block-until-empty delete behavior (a namespace with non-terminal jobs holds in `Terminating`); the read-modify-write preservation of `Quota`/`Capabilities`/config; and the NomadJob `spec.nomadNamespace` linkage (by name; the namespace must exist; `NamespaceMismatch` when the blob disagrees). Note the deferred items (cross-namespace `clusterRef`, managing `Quota`/`Capabilities`) per design §5.

- [ ] **Step 4: Write the live integration spike** (`internal/nomad/namespace_integration_test.go`). Mirror `internal/nomad/nodepool_integration_test.go` (build tag, dev-agent harness). It resolves the design §6 spikes: exact not-empty wording, and namespace-scoped job count.

```go
//go:build integration

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/api"
)

// TestNamespaceLifecycleLive exercises the namespace client against a real
// Nomad v2.0.4 dev agent: create a namespace, register a job into it, confirm
// DeleteNamespace is refused while the job is non-terminal (and that
// IsNamespaceNotEmpty matches the exact v2.0.4 wording — closes design §6.1),
// then deregister the job and delete the namespace. Skips if no nomad binary.
func TestNamespaceLifecycleLive(t *testing.T) {
	c, _ := devAgentWithNode(t) // the real live-agent bootstrap TestJobLifecycleLive reuses (client_write_integration_test.go); do NOT invent a new harness
	const nsName = "it-team-a"

	if err := c.UpsertNamespace(t.Context(), &api.Namespace{Name: nsName, Description: "it"}); err != nil {
		t.Fatalf("UpsertNamespace: %v", err)
	}
	// Register a minimal service job (raw_exec, ≥1 group+task — Nomad rejects a
	// job with no task groups) into the namespace.
	id := "it-web"
	job := &api.Job{ID: &id, Namespace: &nsName /* + Type, TaskGroups per the job spike harness */}
	// Build the job body from the same helper TestJobLifecycleLive uses; set
	// job.Namespace = nsName so it lands in the namespace.
	if _, err := c.RegisterJob(t.Context(), job); err != nil {
		t.Fatalf("RegisterJob: %v", err)
	}

	// Delete must be refused while the job is non-terminal.
	err := c.DeleteNamespace(t.Context(), nsName)
	if err == nil {
		t.Fatal("DeleteNamespace succeeded with a live job; want not-empty refusal")
	}
	if !IsNamespaceNotEmpty(err) {
		t.Fatalf("IsNamespaceNotEmpty(%q) = false; update namespaceNotEmptyTexts to the real v2.0.4 wording", err.Error())
	}

	n, err := c.CountNamespaceJobs(t.Context(), nsName)
	if err != nil || n < 1 {
		t.Fatalf("CountNamespaceJobs = (%d, %v), want ≥1", n, err)
	}

	if err := c.DeregisterJob(t.Context(), nsName, id, true); err != nil {
		t.Fatalf("DeregisterJob: %v", err)
	}
	if err := c.DeleteNamespace(t.Context(), nsName); err != nil {
		t.Fatalf("DeleteNamespace after empty: %v", err)
	}
}
```

> **Implementer note:** reuse the existing dev-agent bootstrap `devAgentWithNode(t) (*Client, string)` (`internal/nomad/client_write_integration_test.go`) and the job-body builder `TestJobLifecycleLive` (`internal/nomad/job_integration_test.go`) already uses — do NOT reinvent the harness (Global Constraints). The job MUST have ≥1 task group + task (Nomad rejects `"Missing job task groups"`), using the `raw_exec` driver that is healthy under `nomad agent -dev`; set `job.Namespace = &nsName` so it lands in the namespace.

- [ ] **Step 5: Add the test to the Makefile filter** (`Makefile`, line ~279). Append `|TestNamespaceLifecycleLive` to the `-run` regex:

```make
	go test -tags integration ./internal/nomad/... -run 'TestDevAgent|TestACLBootstrapAndLeaderLive|TestNodeEligibilityAndDrainLive|TestNodePoolLifecycleLive|TestJobLifecycleLive|TestNamespaceLifecycleLive' -v
```

- [ ] **Step 6: Verify integration compiles (build tag), full gate**

Run: `go build -tags integration ./internal/nomad/... && make manifests generate fmt vet && make test`
Expected: integration test compiles; full unit/envtest gate PASS; zero regen drift. (Live run deferred if no `nomad` binary — as in slices 3–5.)

- [ ] **Step 7: Commit**

```bash
git add cmd/main.go docs/runbooks/nomadnamespace.md internal/nomad/namespace_integration_test.go Makefile config/
git commit -m "feat: wire NomadNamespace reconciler, runbook, live namespace integration spike"
```

---

## Self-Review (completed by plan author)

**Spec coverage:** §3.1 CRD → T2; §3.2 reconciler → T4; §3.3 conflict → T6; §3.4 read-modify-write → T5; §3.5 finalizer → T7; §3.6 jobCount → T5; §3.7 NomadJob threading → T9 (field/decode) + T10 (client/finalizer); §4 client methods + `NomadNamespaceOps` + `IsNamespaceNotEmpty` → T1/T3; §4.1 pins + kustomization → T1/T2; §6 spikes → T11 integration; §7 DoD wiring/runbook → T11; §8 testing → unit (T1/T9/T10), envtest (T4–T8, T10), integration (T11). No uncovered requirement.

**Placeholder scan:** the only deferrals are the two explicitly-marked implementer notes in T7 (constructing a real not-empty error — with a concrete fallback) and T11 (reuse the existing dev-agent harness) — both point at existing code, neither is a "TODO fill in." All code steps carry real code.

**Type consistency:** `NomadNamespaceOps` methods (T3) match the `*Client` methods (T1) and the fake (T3). `NomadJobOps` new signatures (T10) match `*Client` (T10), the fake (T10), and the callers (T10). `spec.nomadNamespace` (T9) is consumed with the same name in T10. Reason constants (`ReasonReservedNamespace`/`ReasonNamespaceNameConflict`/`ReasonNamespaceNotEmpty`/`ReasonNamespaceMismatch`) are defined once (T2/T9) and referenced consistently.
