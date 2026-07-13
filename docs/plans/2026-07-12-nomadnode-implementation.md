# NomadNode (slice 3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **For Claude:** REQUIRED EXECUTION WORKFLOW (follow in order):
> 1. `superpowers:using-git-worktrees` — Isolate work in a dedicated worktree
> 2. `superpowers:subagent-driven-development` — Dispatch a fresh subagent per task
> 3. `superpowers:test-driven-development` — All subagents use TDD
> 4. `superpowers:verification-before-completion` — Verify all tests pass per task
> 5. `superpowers:requesting-code-review` — Code review after each task (built in)
> 6. After all tasks: comprehensive code review on full diff from branch point (automatic)
> 7. `superpowers:finishing-a-development-branch` — Complete the branch
> 8. `modern-go-guidelines:use-modern-go` — subagents apply modern Go for Go 1.26.4
>
> Skills carry their own model and effort settings. Do not override them.

**Goal:** Add a `NomadNode` CRD that reflects self-registered Nomad edge clients into Kubernetes as representations, and lets a user drive scheduling eligibility and drain onto them — plus an optional `NomadClusterSpec.nodeGCThreshold` companion field.

**Architecture:** A second controller keyed on `NomadCluster` polls each `Ready` cluster's node list (one `Nodes().List()` per resync) via a per-cluster Nomad client, and for each node upserts a `NomadNode` CR (existence + status operator-owned), reconciling the user-owned `spec.eligible`/`spec.drain` onto Nomad. The representation model mirrors the Kubernetes `Node` object: operator owns Create+Delete, user owns Read+Update. Design: `docs/designs/2026-07-12-nomadnode-design.md`.

**Tech Stack:** Go 1.26.4, kubebuilder v4, controller-runtime v0.23.3, k8s v0.35.0, Ginkgo/Gomega envtest, `github.com/hashicorp/nomad/api` (pinned `5b83b133998a` == v2.0.4).

## Global Constraints

- **Go 1.26.4**; nomad `api` pinned at commit `5b83b133998a` (no `/v2`, no semver tags). Never rewrite the import to `/v2`.
- **Per-endpoint Nomad client only.** Build every client from a per-cluster `nomad.Config` via the shared `clusterNomadConfig` helper (Task 2). Never `api.DefaultConfig()`, never a process-wide singleton.
- **`contract.go` pins must be backed by a real call.** Every newly-pinned `api` symbol must be exercised by concrete code in the same or an earlier task (existence-only-pin gotcha: an uncalled method pin can have its signature reshape without breaking the build).
- **API group** `nomad.operator.io/v1alpha1`. `v1alpha1` is unreleased — additive CRD changes need no conversion webhook.
- **No new module dependency.** Everything here uses `api`, controller-runtime, and k8s libs already required.
- **Signed commits** need the user's 1Password Touch ID. If `git commit` fails with `1Password: failed to fill whole buffer`, stop and ask the user to unlock — do NOT disable `commit.gpgsign`.
- **Build gate** (run at the end of every task): `make manifests generate fmt vet && make test`. Integration tests are `-tags integration` and run via `make test-integration`.

---

## File Structure

**New files:**
- `api/v1alpha1/nomadnode_types.go` — `NomadNode` CRD Go types + kubebuilder markers (Task 3).
- `internal/controller/nomadclient.go` — shared `clusterNomadConfig` helper (Task 2).
- `internal/controller/nomadnode_names.go` — `sanitizeNodeName` (Task 4).
- `internal/controller/nomadnode_ops.go` — `NomadNodeOps` interface, factory, default (Task 4).
- `internal/controller/nomadnode_controller.go` — the cluster-keyed reflector reconciler (Tasks 5–8).
- `internal/controller/fake_nomadnode_test.go` — envtest fake for `NomadNodeOps` (Task 4).
- `internal/controller/nomadnode_names_test.go` — sanitize unit tests (Task 4).
- `internal/controller/nomadnode_controller_test.go` — reconciler envtest specs (Tasks 5–8).
- `docs/runbooks/nomadnode.md` — operator runbook (Task 10).

**Modified files:**
- `internal/nomad/client.go` — add `SetEligibility`, `UpdateDrain` (Task 1).
- `internal/nomad/contract.go` — add node write pins (Tasks 1, 5, 7).
- `internal/nomad/client_test.go` — extend write-method signature test (Task 1).
- `internal/nomad/client_write_integration_test.go` — live eligibility/drain exercise (Task 10).
- `internal/controller/nomadcluster_controller.go` — `clientFor` delegates to `clusterNomadConfig` (Task 2).
- `api/v1alpha1/nomadcluster_types.go` — optional `nodeGCThreshold` field (Task 9).
- `internal/controller/config_render.go` — gated `node_gc_threshold` render + hash (Task 9).
- `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/*.yaml`, `config/rbac/role.yaml` — regenerated (Tasks 3, 5, 9).

**Generated (never hand-edit; regenerate via `make manifests generate`):** `zz_generated.deepcopy.go`, `config/crd/bases/`, `config/rbac/role.yaml`.

---

## Task 1: `internal/nomad` — `SetEligibility` + `UpdateDrain` + contract pins

**Files:**
- Modify: `internal/nomad/client.go` (append two methods after `ACLBootstrap`)
- Modify: `internal/nomad/contract.go` (method + type pins)
- Modify: `internal/nomad/client_test.go:66` (`TestWriteMethodSignaturesErrorWithoutServer`)

**Interfaces:**
- Produces: `func (c *Client) SetEligibility(ctx context.Context, nodeID string, eligible bool) error`; `func (c *Client) UpdateDrain(ctx context.Context, nodeID string, spec *api.DrainSpec, markEligible bool) error`.

- [ ] **Step 1: Write the failing test** — extend `TestWriteMethodSignaturesErrorWithoutServer` in `internal/nomad/client_test.go` (add inside the existing function, after the `ACLBootstrap` check):

```go
	if err := c.SetEligibility(ctx, "abc123", false); err == nil {
		t.Error("SetEligibility() with no server: want error, got nil")
	}
	if err := c.UpdateDrain(ctx, "abc123", &api.DrainSpec{Deadline: time.Hour}, false); err == nil {
		t.Error("UpdateDrain() with no server: want error, got nil")
	}
```

Add imports `"time"` and `"github.com/hashicorp/nomad/api"` to the test file's import block.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/nomad/ -run TestWriteMethodSignaturesErrorWithoutServer -v`
Expected: FAIL — compile error `c.SetEligibility undefined` / `c.UpdateDrain undefined`.

- [ ] **Step 3: Write minimal implementation** — append to `internal/nomad/client.go`:

```go
// SetEligibility toggles a node's scheduling eligibility (Nomad's cordon knob).
func (c *Client) SetEligibility(ctx context.Context, nodeID string, eligible bool) error {
	if _, err := c.api.Nodes().ToggleEligibility(nodeID, eligible, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: set eligibility %q: %w", nodeID, err)
	}
	return nil
}

// UpdateDrain sets or cancels a node's drain. A nil spec cancels an active
// drain; markEligible marks the node eligible when canceling.
func (c *Client) UpdateDrain(ctx context.Context, nodeID string, spec *api.DrainSpec, markEligible bool) error {
	if _, err := c.api.Nodes().UpdateDrain(nodeID, spec, markEligible, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: update drain %q: %w", nodeID, err)
	}
	return nil
}
```

- [ ] **Step 4: Add contract pins** — in `internal/nomad/contract.go`, add to the method-pin `var` block:

```go
	_ = (*api.Nodes).ToggleEligibility
	_ = (*api.Nodes).UpdateDrain
```

and to the type-pin `var` block:

```go
	_ api.DrainSpec
```

(`DrainMetadata`, `DrainStrategy`, and the `DrainStatus*` constants are pinned in Tasks 5 and 7, where real reads back them.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/nomad/ -v`
Expected: PASS (all existing + the two new signature checks). `go build ./...` clean.

- [ ] **Step 6: Commit**

```bash
git add internal/nomad/client.go internal/nomad/contract.go internal/nomad/client_test.go
git commit -m "feat(nomad): add SetEligibility + UpdateDrain node write methods"
```

---

## Task 2: Shared per-cluster `nomad.Config` construction (`clusterNomadConfig`)

Extract the per-cluster config construction from `NomadClusterReconciler.clientFor` so both the cluster and node reconcilers single-source it (design §4). Behavior-preserving for slice 2.

**Files:**
- Create: `internal/controller/nomadclient.go`
- Modify: `internal/controller/nomadcluster_controller.go:180-207` (`clientFor` delegates)
- Test: `internal/controller/nomadclient_test.go`

**Interfaces:**
- Produces: `func clusterNomadConfig(ctx context.Context, c client.Client, nc *nomadv1alpha1.NomadCluster) (nomad.Config, error)`.
- Consumes (Task 1): nothing; uses existing `names()`, `portHTTP`.

- [ ] **Step 1: Write the failing test** — `internal/controller/nomadclient_test.go`:

```go
package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

func TestClusterNomadConfig(t *testing.T) {
	nc := &nomadv1alpha1.NomadCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ns"},
		Spec: nomadv1alpha1.NomadClusterSpec{
			Region: "global",
			TLS:    nomadv1alpha1.TLSSpec{CertSecretRef: "cert"},
		},
	}
	cert := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cert", Namespace: "ns"},
		Data:       map[string][]byte{"ca.crt": []byte("CA"), "tls.crt": []byte("CRT"), "tls.key": []byte("KEY")},
	}
	tok := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: names(nc).TokenSecret, Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("t0ken")},
	}
	c := fake.NewClientBuilder().WithObjects(cert, tok).Build()

	cfg, err := clusterNomadConfig(context.Background(), c, nc)
	if err != nil {
		t.Fatalf("clusterNomadConfig: %v", err)
	}
	if cfg.TLSServerName != "server.global.nomad" {
		t.Errorf("TLSServerName = %q", cfg.TLSServerName)
	}
	if string(cfg.TLS.CACertPEM) != "CA" || string(cfg.TLS.ClientKeyPEM) != "KEY" {
		t.Errorf("PEM material not wired: %+v", cfg.TLS)
	}
	if cfg.Token != "t0ken" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.Address == "" {
		t.Error("Address empty")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestClusterNomadConfig -v`
Expected: FAIL — `clusterNomadConfig` undefined.

- [ ] **Step 3: Create `internal/controller/nomadclient.go`**

```go
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// clusterNomadConfig builds the per-cluster nomad.Config both reconcilers use:
// endpoint is the in-cluster API Service, TLS material is PEM bytes from the
// cert-manager Secret (never files), the token (if bootstrapped) comes from the
// token Secret, and TLSServerName is the Nomad role name. This is the single
// source of the per-cluster client-construction contract (design §4).
func clusterNomadConfig(ctx context.Context, c client.Client, nc *nomadv1alpha1.NomadCluster) (nomad.Config, error) {
	n := names(nc)
	endpoint := fmt.Sprintf("https://%s.%s.svc:%d", n.APISvc, nc.Namespace, portHTTP)

	var certSec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: nc.Spec.TLS.CertSecretRef, Namespace: nc.Namespace}, &certSec); err != nil {
		return nomad.Config{}, err
	}
	cfg := nomad.Config{
		Address:       endpoint,
		Region:        nc.Spec.Region,
		TLSServerName: "server." + nc.Spec.Region + ".nomad",
		TLS: nomad.TLSConfig{
			CACertPEM:     certSec.Data["ca.crt"],
			ClientCertPEM: certSec.Data["tls.crt"],
			ClientKeyPEM:  certSec.Data["tls.key"],
		},
	}
	var tokenSec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: n.TokenSecret, Namespace: nc.Namespace}, &tokenSec); err == nil {
		cfg.Token = string(tokenSec.Data["token"])
	}
	return cfg, nil
}
```

- [ ] **Step 4: Delegate `clientFor` to it** — replace the body of `clientFor` in `internal/controller/nomadcluster_controller.go` (lines 180–207) with:

```go
func (r *NomadClusterReconciler) clientFor(ctx context.Context, nc *nomadv1alpha1.NomadCluster) (NomadOps, error) {
	cfg, err := clusterNomadConfig(ctx, r.Client, nc)
	if err != nil {
		return nil, err
	}
	return r.NewNomadClient(cfg)
}
```

Remove now-unused imports from `nomadcluster_controller.go` if `go build` flags any (e.g. `corev1`/`types` may still be used elsewhere in the file — only remove if the compiler says so).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestClusterNomadConfig|TestControllers' -v 2>&1 | tail -20`
Expected: PASS — the new unit test passes and the existing envtest specs (which exercise `clientFor`) still pass, proving behavior preserved.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/nomadclient.go internal/controller/nomadclient_test.go internal/controller/nomadcluster_controller.go
git commit -m "refactor(controller): extract clusterNomadConfig from clientFor"
```

---

## Task 3: `NomadNode` CRD types + CEL + manifests

**Files:**
- Create: `api/v1alpha1/nomadnode_types.go`
- Test: `internal/controller/nomadnode_crd_test.go`
- Regenerate: `zz_generated.deepcopy.go`, `config/crd/bases/nomad.operator.io_nomadnodes.yaml`

**Interfaces:**
- Produces (Go types every later task consumes): `NomadNode`, `NomadNodeList`, `NomadNodeSpec{ClusterRef NodeReference; NodeName string; Eligible bool; Drain *NodeDrainSpec}`, `NodeReference{Name string}`, `NodeDrainSpec{Deadline metav1.Duration; IgnoreSystemJobs bool}`, `NomadNodeStatus{...}`, `LastDrainStatus{...}`; condition/const names `NomadNodeCondReconciled`, `ReasonClusterNotReady`, `ReasonDuplicateNodeName`, `ReasonNodeNotFound`.

- [ ] **Step 1: Create `api/v1alpha1/nomadnode_types.go`**

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

// NomadNode condition types and reasons.
const (
	NomadNodeCondReconciled = "Reconciled"

	ReasonClusterNotReady   = "ClusterNotReady"
	ReasonDuplicateNodeName = "DuplicateNodeName"
	ReasonNodeNotFound      = "NodeNotFound"
	ReasonReconciled        = "Reconciled"
)

// NodeReference names a NomadCluster in the same namespace.
type NodeReference struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// NodeDrainSpec mirrors github.com/hashicorp/nomad/api.DrainSpec. Its presence
// on NomadNodeSpec means "drain this node"; its absence means "do not drain".
type NodeDrainSpec struct {
	// Deadline is how long remaining allocations may take to migrate before
	// they are force-stopped. Zero means no deadline; the operator substitutes
	// 1h when unset. +optional
	// +optional
	Deadline metav1.Duration `json:"deadline,omitempty"`
	// +optional
	IgnoreSystemJobs bool `json:"ignoreSystemJobs,omitempty"`
}

// NomadNodeSpec is the desired state of a NomadNode. clusterRef + nodeName are
// the immutable identity; eligible + drain are the user's control surface.
//
// +kubebuilder:validation:XValidation:rule="self.nodeName == oldSelf.nodeName",message="nodeName is immutable"
// +kubebuilder:validation:XValidation:rule="self.clusterRef.name == oldSelf.clusterRef.name",message="clusterRef.name is immutable"
type NomadNodeSpec struct {
	// +kubebuilder:validation:Required
	ClusterRef NodeReference `json:"clusterRef"`
	// NodeName is the exact Nomad node Name this CR represents (the match key).
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`
	// Eligible is the scheduling-eligibility target when the node is not
	// actively draining. Defaults true; the reflector seeds it from observed
	// state at first mint.
	// +kubebuilder:default=true
	Eligible bool `json:"eligible,omitempty"`
	// Drain, when set, requests a drain; remove it to cancel.
	// +optional
	Drain *NodeDrainSpec `json:"drain,omitempty"`
}

// LastDrainStatus summarizes Nomad's Node.LastDrain (DrainMetadata).
type LastDrainStatus struct {
	Status    string       `json:"status,omitempty"`
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`
}

// NomadNodeStatus is the observed state, all operator-owned (mirror/resolved).
type NomadNodeStatus struct {
	// +optional
	NodeID string `json:"nodeID,omitempty"`
	// +optional
	Status string `json:"status,omitempty"`
	// +optional
	SchedulingEligibility string `json:"schedulingEligibility,omitempty"`
	// +optional
	Draining bool `json:"draining,omitempty"`
	// DrainObservedGeneration records the spec generation at which the current
	// drain was issued, so a completed drain converges (design §3.3).
	// +optional
	DrainObservedGeneration int64 `json:"drainObservedGeneration,omitempty"`
	// +optional
	LastDrain *LastDrainStatus `json:"lastDrain,omitempty"`
	// +optional
	NodeClass string `json:"nodeClass,omitempty"`
	// +optional
	NodePool string `json:"nodePool,omitempty"`
	// +optional
	Datacenter string `json:"datacenter,omitempty"`
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
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.status`
// +kubebuilder:printcolumn:name="Eligible",type=string,JSONPath=`.status.schedulingEligibility`
// +kubebuilder:printcolumn:name="Draining",type=boolean,JSONPath=`.status.draining`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NomadNode is the Schema for the nomadnodes API.
type NomadNode struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec NomadNodeSpec `json:"spec"`
	// +optional
	Status NomadNodeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NomadNodeList contains a list of NomadNode.
type NomadNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NomadNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NomadNode{}, &NomadNodeList{})
}
```

- [ ] **Step 2: Generate deepcopy + CRD manifest**

Run: `make manifests generate`
Expected: creates `config/crd/bases/nomad.operator.io_nomadnodes.yaml` and adds `NomadNode*` deepcopy methods to `api/v1alpha1/zz_generated.deepcopy.go`; no errors.

- [ ] **Step 3: Write the failing CRD test** — `internal/controller/nomadnode_crd_test.go` (Ginkgo, uses the envtest `k8s` client from `suite_test.go`, which loads `config/crd/bases`):

```go
package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

var _ = Describe("NomadNode CRD", func() {
	It("defaults eligible to true and rejects nodeName mutation", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-crd-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "box-1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: "prod"},
				NodeName:   "box-1",
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		Expect(nn.Spec.Eligible).To(BeTrue(), "eligible should default true")

		nn.Spec.NodeName = "box-2"
		err := k8s.Update(ctx, nn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("nodeName is immutable"))

		Expect(k8s.Delete(ctx, nn)).To(Succeed())
		_ = client.IgnoreNotFound(nil)
	})
})
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test 2>&1 | tail -30` (envtest needs the regenerated CRD; run the full gate so manifests are current)
Expected: PASS — `NomadNode CRD` spec green.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/nomadnode_types.go api/v1alpha1/zz_generated.deepcopy.go config/crd/bases/ internal/controller/nomadnode_crd_test.go
git commit -m "feat(api): add NomadNode CRD types + CEL immutability"
```

---

## Task 4: Name sanitization + `NomadNodeOps` interface, factory, and fake

**Files:**
- Create: `internal/controller/nomadnode_names.go`
- Create: `internal/controller/nomadnode_ops.go`
- Create: `internal/controller/nomadnode_names_test.go`
- Create: `internal/controller/fake_nomadnode_test.go`

**Interfaces:**
- Produces: `func sanitizeNodeName(name string) string`; interface `NomadNodeOps { ListNodes(ctx)([]*api.NodeListStub,error); NodeInfo(ctx,string)(*api.Node,error); SetEligibility(ctx,string,bool)error; UpdateDrain(ctx,string,*api.DrainSpec,bool)error }`; `type NomadNodeClientFactory func(nomad.Config)(NomadNodeOps,error)`; `func DefaultNomadNodeClientFactory(nomad.Config)(NomadNodeOps,error)`.

- [ ] **Step 1: Write the failing sanitize test** — `internal/controller/nomadnode_names_test.go`:

```go
package controller

import "testing"

func TestSanitizeNodeName(t *testing.T) {
	cases := map[string]string{
		"truenas-01":      "truenas-01",
		"TrueNAS-01":      "truenas-01",
		"host.lan":        "host.lan",
		"weird_name!":     "weird-name",
		"--edge--":        "edge",
		"":                "node",
	}
	for in, want := range cases {
		if got := sanitizeNodeName(in); got != want {
			t.Errorf("sanitizeNodeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	if got := sanitizeNodeName(string(long)); len(got) > 253 {
		t.Errorf("sanitizeNodeName did not cap length: %d", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestSanitizeNodeName -v`
Expected: FAIL — `sanitizeNodeName` undefined.

- [ ] **Step 3: Create `internal/controller/nomadnode_names.go`**

```go
package controller

import "strings"

// sanitizeNodeName converts a Nomad node Name (which may contain uppercase or
// characters illegal in a Kubernetes object name) into a valid RFC 1123
// subdomain used as the NomadNode's metadata.name. The exact node Name is kept
// verbatim in spec.nodeName; this is only the object name. Post-sanitization
// collisions are surfaced by the reflector's duplicate guard (design §3.2).
func sanitizeNodeName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	// Collapse runs introduced by replacement so "weird_name!" -> "weird-name".
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 253 {
		out = strings.Trim(out[:253], "-.")
	}
	if out == "" {
		return "node"
	}
	return out
}
```

- [ ] **Step 4: Run sanitize test to verify it passes**

Run: `go test ./internal/controller/ -run TestSanitizeNodeName -v`
Expected: PASS.

- [ ] **Step 5: Create `internal/controller/nomadnode_ops.go`**

```go
package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// NomadNodeOps is the subset of the Nomad client the node reflector needs,
// defined at the consumer (Go convention). *nomad.Client satisfies it, and
// envtest injects a fake. It is intentionally separate from the cluster
// reconciler's NomadOps so the two controllers' test seams stay decoupled.
type NomadNodeOps interface {
	ListNodes(ctx context.Context) ([]*api.NodeListStub, error)
	NodeInfo(ctx context.Context, id string) (*api.Node, error)
	SetEligibility(ctx context.Context, nodeID string, eligible bool) error
	UpdateDrain(ctx context.Context, nodeID string, spec *api.DrainSpec, markEligible bool) error
}

// NomadNodeClientFactory builds a NomadNodeOps from a per-cluster Config.
type NomadNodeClientFactory func(cfg nomad.Config) (NomadNodeOps, error)

// DefaultNomadNodeClientFactory constructs the real client.
func DefaultNomadNodeClientFactory(cfg nomad.Config) (NomadNodeOps, error) {
	return nomad.New(cfg)
}
```

- [ ] **Step 6: Create `internal/controller/fake_nomadnode_test.go`**

```go
package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// fakeNodeOps is a scriptable NomadNodeOps for envtest. list is returned by
// ListNodes; info maps node ID -> full node for NodeInfo; eligibility and
// drain calls are recorded for assertions.
type fakeNodeOps struct {
	list     []*api.NodeListStub
	info     map[string]*api.Node
	listErr  error
	eligCalls  []eligCall
	drainCalls []drainCall
}

type eligCall struct {
	id       string
	eligible bool
}
type drainCall struct {
	id           string
	spec         *api.DrainSpec
	markEligible bool
}

func (f *fakeNodeOps) ListNodes(context.Context) ([]*api.NodeListStub, error) {
	return f.list, f.listErr
}
func (f *fakeNodeOps) NodeInfo(_ context.Context, id string) (*api.Node, error) {
	return f.info[id], nil
}
func (f *fakeNodeOps) SetEligibility(_ context.Context, id string, eligible bool) error {
	f.eligCalls = append(f.eligCalls, eligCall{id, eligible})
	return nil
}
func (f *fakeNodeOps) UpdateDrain(_ context.Context, id string, spec *api.DrainSpec, markEligible bool) error {
	f.drainCalls = append(f.drainCalls, drainCall{id, spec, markEligible})
	return nil
}

func newFakeNodeFactory(f *fakeNodeOps) NomadNodeClientFactory {
	return func(nomad.Config) (NomadNodeOps, error) { return f, nil }
}
```

- [ ] **Step 7: Run tests + build to verify**

Run: `go test ./internal/controller/ -run TestSanitizeNodeName -v && go vet ./internal/controller/`
Expected: PASS; vet clean (the fake compiles even though unused until Task 5 — Go does not flag unused methods/types).

- [ ] **Step 8: Commit**

```bash
git add internal/controller/nomadnode_names.go internal/controller/nomadnode_ops.go internal/controller/nomadnode_names_test.go internal/controller/fake_nomadnode_test.go
git commit -m "feat(controller): NomadNode name sanitization + ops seam + fake"
```

---

## Task 5: Reflector reconciler — mint, seed-once, status mirror, ownerRef, wiring

Implements the reconcile loop with a **simple** binding (one stub per Name; disambiguation is Task 6), a no-op drive (Task 7) and no-op prune (Task 8), so this task is independently testable: given one node, a `NomadNode` is minted with seeded spec, mirrored status, and an ownerReference to its cluster.

**Files:**
- Create: `internal/controller/nomadnode_controller.go`
- Create: `internal/controller/nomadnode_controller_test.go`
- Regenerate: `config/rbac/role.yaml` (via `make manifests`)

**Interfaces:**
- Consumes: `clusterNomadConfig` (Task 2), `sanitizeNodeName`/`NomadNodeOps`/`NomadNodeClientFactory` (Task 4), `NomadNode*` types (Task 3), `fakeNodeOps`/`newFakeNodeFactory` (Task 4).
- Produces: `type NomadNodeReconciler struct { client.Client; Scheme *runtime.Scheme; NewNomadClient NomadNodeClientFactory }`; helpers `bindNodes(list []*api.NodeListStub) (map[string]*api.NodeListStub, map[string]bool)` (Task 6 replaces the body), `upsertNode`, `mirrorStatus`, `driveDesired` (Task 7), `pruneAbsent` (Task 8).

- [ ] **Step 1: Write the failing envtest** — `internal/controller/nomadnode_controller_test.go`:

```go
package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hashicorp/nomad/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

// readyCluster creates a NomadCluster already in Ready phase + a cert Secret,
// so the node reflector will build a client and list nodes.
func readyCluster(ctx SpecContext, ns string) *nomadv1alpha1.NomadCluster {
	nc := &nomadv1alpha1.NomadCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: ns},
		Spec: nomadv1alpha1.NomadClusterSpec{
			Image: "hashicorp/nomad:2.0.4", Servers: 1, Region: "global",
			Storage: nomadv1alpha1.StorageSpec{Size: "1Gi"},
			TLS:     nomadv1alpha1.TLSSpec{CertSecretRef: "cert"},
			ExternalAccess: nomadv1alpha1.ExternalAccessSpec{
				Mode:         nomadv1alpha1.ExternalAccessLoadBalancer,
				LoadBalancer: &nomadv1alpha1.LoadBalancerSpec{},
			},
		},
	}
	Expect(k8s.Create(ctx, nc)).To(Succeed())
	nc.Status.Phase = nomadv1alpha1.PhaseReady
	Expect(k8s.Status().Update(ctx, nc)).To(Succeed())
	cert := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cert", Namespace: ns},
		Data:       map[string][]byte{"ca.crt": []byte("CA"), "tls.crt": []byte("CRT"), "tls.key": []byte("KEY")},
	}
	Expect(k8s.Create(ctx, cert)).To(Succeed())
	return nc
}

var _ = Describe("NomadNode reflector: mint", func() {
	It("mints a NomadNode with seeded spec, mirrored status, and an ownerRef", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-mint-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "id-abc", Name: "TrueNAS-01", Status: "ready", SchedulingEligibility: "ineligible", NodePool: "default", NodeClass: "truenas", Datacenter: "dc1"},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var nn nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "truenas-01", Namespace: ns.Name}, &nn)).To(Succeed())
		Expect(nn.Spec.NodeName).To(Equal("TrueNAS-01"))
		Expect(nn.Spec.Eligible).To(BeFalse(), "seeded from observed ineligible")
		Expect(nn.Status.NodeID).To(Equal("id-abc"))
		Expect(nn.Status.Status).To(Equal("ready"))
		Expect(nn.Status.NodePool).To(Equal("default"))
		Expect(nn.OwnerReferences).To(HaveLen(1))
		Expect(nn.OwnerReferences[0].Name).To(Equal(nc.Name))
	})
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A3 'reflector: mint'`
Expected: FAIL — `NomadNodeReconciler` undefined (compile error).

- [ ] **Step 3: Create `internal/controller/nomadnode_controller.go`**

```go
package controller

import (
	"context"
	"time"

	"github.com/hashicorp/nomad/api"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

const nodeResync = 30 * time.Second

// NomadNodeReconciler reflects a NomadCluster's registered nodes into NomadNode
// CRs and drives eligibility/drain onto Nomad. Its primary object is the
// NomadCluster (a NomadNode-keyed reconciler could never mint the first CR),
// so each Reconcile handles one cluster's whole node set.
type NomadNodeReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NewNomadClient NomadNodeClientFactory
}

// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadnodes/status,verbs=get;update;patch

func (r *NomadNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var nc nomadv1alpha1.NomadCluster
	if err := r.Get(ctx, req.NamespacedName, &nc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if nc.Status.Phase != nomadv1alpha1.PhaseReady {
		return ctrl.Result{RequeueAfter: nodeResync}, nil // not listable yet
	}

	cfg, err := clusterNomadConfig(ctx, r.Client, &nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	ops, err := r.NewNomadClient(cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	stubs, err := ops.ListNodes(ctx)
	if err != nil {
		return ctrl.Result{RequeueAfter: nodeResync}, nil // transient: prune nothing
	}

	bound, dupes := bindNodes(stubs)
	for name, stub := range bound {
		if err := r.upsertNode(ctx, &nc, name, stub, ops); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.markDuplicates(ctx, &nc, dupes); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.pruneAbsent(ctx, &nc, bound); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: nodeResync}, nil
}

// bindNodes maps node Name -> the stub to manage. Task 5 uses a naive
// last-wins binding; Task 6 replaces the body with down-straggler
// disambiguation. dupes holds Names with genuine ambiguity.
func bindNodes(stubs []*api.NodeListStub) (map[string]*api.NodeListStub, map[string]bool) {
	bound := map[string]*api.NodeListStub{}
	for _, s := range stubs {
		bound[s.Name] = s
	}
	return bound, map[string]bool{}
}

// upsertNode creates-or-updates the NomadNode for one bound stub: sanitized
// metadata.name, ownerRef to the cluster, spec seeded ONCE at create, status
// mirrored every pass, then desired state driven onto Nomad.
func (r *NomadNodeReconciler) upsertNode(ctx context.Context, nc *nomadv1alpha1.NomadCluster, nodeName string, stub *api.NodeListStub, ops NomadNodeOps) error {
	objName := sanitizeNodeName(nodeName)
	var nn nomadv1alpha1.NomadNode
	err := r.Get(ctx, types.NamespacedName{Name: objName, Namespace: nc.Namespace}, &nn)
	switch {
	case apierrors.IsNotFound(err):
		nn = nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: objName, Namespace: nc.Namespace, Labels: names(nc).Labels()},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name},
				NodeName:   nodeName,
				Eligible:   stub.SchedulingEligibility != api.NodeSchedulingIneligible, // seed from observed
			},
		}
		if stub.Drain { // seed drain presence; fetch detail only for a draining node
			nn.Spec.Drain = r.seedDrain(ctx, stub.ID, ops)
		}
		if err := controllerutil.SetControllerReference(nc, &nn, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, &nn); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	case err != nil:
		return err
	}
	// Drive desired state onto Nomad (Task 7 fills driveDesired), then mirror.
	if err := r.driveDesired(ctx, &nn, stub, ops); err != nil {
		return err
	}
	return r.mirrorStatus(ctx, &nn, stub)
}

// seedDrain fetches the active drain spec of a node draining at first mint.
func (r *NomadNodeReconciler) seedDrain(ctx context.Context, id string, ops NomadNodeOps) *nomadv1alpha1.NodeDrainSpec {
	node, err := ops.NodeInfo(ctx, id)
	if err != nil || node == nil || node.DrainStrategy == nil {
		return &nomadv1alpha1.NodeDrainSpec{} // presence only; deadline defaults in driveDesired
	}
	return &nomadv1alpha1.NodeDrainSpec{
		Deadline:         metav1.Duration{Duration: node.DrainStrategy.Deadline},
		IgnoreSystemJobs: node.DrainStrategy.IgnoreSystemJobs,
	}
}

// driveDesired reconciles spec.eligible/drain onto Nomad. Task 7 implements it;
// Task 5 ships a no-op so mint/mirror are independently testable.
func (r *NomadNodeReconciler) driveDesired(_ context.Context, _ *nomadv1alpha1.NomadNode, _ *api.NodeListStub, _ NomadNodeOps) error {
	return nil
}

// mirrorStatus writes the observed node state into NomadNode.status.
func (r *NomadNodeReconciler) mirrorStatus(ctx context.Context, nn *nomadv1alpha1.NomadNode, stub *api.NodeListStub) error {
	nn.Status.NodeID = stub.ID
	nn.Status.Status = stub.Status
	nn.Status.SchedulingEligibility = stub.SchedulingEligibility
	nn.Status.Draining = stub.Drain
	nn.Status.NodeClass = stub.NodeClass
	nn.Status.NodePool = stub.NodePool
	nn.Status.Datacenter = stub.Datacenter
	nn.Status.ObservedGeneration = nn.Generation
	if stub.LastDrain != nil {
		nn.Status.LastDrain = &nomadv1alpha1.LastDrainStatus{
			Status:    string(stub.LastDrain.Status),
			StartedAt: &metav1.Time{Time: stub.LastDrain.StartedAt},
			UpdatedAt: &metav1.Time{Time: stub.LastDrain.UpdatedAt},
		}
	}
	setNodeCondition(nn, nomadv1alpha1.NomadNodeCondReconciled, metav1.ConditionTrue, nomadv1alpha1.ReasonReconciled, "reflected")
	return r.Status().Update(ctx, nn)
}

// markDuplicates / pruneAbsent are filled in Tasks 6 and 8; no-op stubs here.
func (r *NomadNodeReconciler) markDuplicates(_ context.Context, _ *nomadv1alpha1.NomadCluster, _ map[string]bool) error {
	return nil
}
func (r *NomadNodeReconciler) pruneAbsent(_ context.Context, _ *nomadv1alpha1.NomadCluster, _ map[string]*api.NodeListStub) error {
	return nil
}

// clusterForNode maps a NomadNode event to its owning cluster's reconcile key.
func (r *NomadNodeReconciler) clusterForNode(_ context.Context, obj client.Object) []reconcile.Request {
	nn, ok := obj.(*nomadv1alpha1.NomadNode)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: nn.Spec.ClusterRef.Name, Namespace: nn.Namespace}}}
}

func (r *NomadNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewNomadClient == nil {
		r.NewNomadClient = DefaultNomadNodeClientFactory
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nomadv1alpha1.NomadCluster{}). // primary object is the cluster
		Owns(&nomadv1alpha1.NomadNode{}).   // minted CRs carry the cluster ownerRef
		Watches(&nomadv1alpha1.NomadNode{}, handler.EnqueueRequestsFromMapFunc(r.clusterForNode)).
		Named("nomadnode").
		Complete(r)
}
```

- [ ] **Step 4: Add the condition helper** — append to `internal/controller/nomadnode_controller.go` (mirrors the existing `setCondition` for `NomadCluster`):

```go
func setNodeCondition(nn *nomadv1alpha1.NomadNode, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta := metav1.Condition{Type: condType, Status: status, Reason: reason, Message: msg, ObservedGeneration: nn.Generation}
	for i, c := range nn.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				meta.LastTransitionTime = metav1.Now()
			} else {
				meta.LastTransitionTime = c.LastTransitionTime
			}
			nn.Status.Conditions[i] = meta
			return
		}
	}
	meta.LastTransitionTime = metav1.Now()
	nn.Status.Conditions = append(nn.Status.Conditions, meta)
}
```

- [ ] **Step 5: Pin the newly-read `api` symbol** — in `internal/nomad/contract.go`, add to the type-pin block (backed by `mirrorStatus` reading `stub.LastDrain`):

```go
	_ api.DrainMetadata
```

- [ ] **Step 6: Regenerate RBAC + run tests**

Run: `make manifests && make test 2>&1 | tail -30`
Expected: PASS — the `reflector: mint` spec is green; `config/rbac/role.yaml` gains the `nomadnodes` rules.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/nomadnode_controller.go internal/controller/nomadnode_controller_test.go internal/nomad/contract.go config/rbac/role.yaml
git commit -m "feat(controller): NomadNode reflector — mint, seed, mirror, ownerRef, wiring"
```

---

## Task 6: Re-registration disambiguation

Replace `bindNodes` so a `down` straggler (same Name, old ID, from a re-registered box) does not freeze the healthy node, and `DuplicateNodeName` fires only on genuine ambiguity (design §3.2).

**Files:**
- Modify: `internal/controller/nomadnode_controller.go` (`bindNodes`, `markDuplicates`)
- Modify: `internal/controller/nomadnode_controller_test.go` (add spec)

**Interfaces:**
- Consumes: `NomadNode*`, `setNodeCondition`, `sanitizeNodeName` (Tasks 3–5).

- [ ] **Step 1: Write the failing test** — add to `internal/controller/nomadnode_controller_test.go`:

```go
var _ = Describe("NomadNode reflector: disambiguation", func() {
	It("binds the live stub when a down straggler shares its Name", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-dis-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "old", Name: "box", Status: "down", CreateIndex: 1},
			{ID: "new", Name: "box", Status: "ready", SchedulingEligibility: "eligible", CreateIndex: 9},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var nn nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "box", Namespace: ns.Name}, &nn)).To(Succeed())
		Expect(nn.Status.NodeID).To(Equal("new"), "bound to the live, not the down straggler")
		Expect(nn.Status.Status).To(Equal("ready"))
	})

	It("flags DuplicateNodeName when two non-down stubs share a Name", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-dup-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "a", Name: "twin", Status: "ready", CreateIndex: 1},
			{ID: "b", Name: "twin", Status: "ready", CreateIndex: 2},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var nn nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "twin", Namespace: ns.Name}, &nn)).To(Succeed())
		cond := meta.FindStatusCondition(nn.Status.Conditions, nomadv1alpha1.NomadNodeCondReconciled)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonDuplicateNodeName))
	})
})
```

Add `"k8s.io/apimachinery/pkg/api/meta"` to the test imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A3 disambiguation`
Expected: FAIL — the `down` straggler binds last-wins (`box` may be `down`), and no `DuplicateNodeName` reason is set.

- [ ] **Step 3: Replace `bindNodes` + implement `markDuplicates`** in `internal/controller/nomadnode_controller.go`:

```go
// bindNodes groups stubs by Name and, per Name, binds the single non-down stub
// (tie-breaking on highest CreateIndex if several non-down share a Name).
// A down straggler from a re-registered box is ignored, not treated as a
// duplicate. dupes[name]=true marks Names with two-or-more NON-down stubs
// (genuine ambiguity) — those are surfaced, not bound.
func bindNodes(stubs []*api.NodeListStub) (map[string]*api.NodeListStub, map[string]bool) {
	byName := map[string][]*api.NodeListStub{}
	for _, s := range stubs {
		byName[s.Name] = append(byName[s.Name], s)
	}
	bound := map[string]*api.NodeListStub{}
	dupes := map[string]bool{}
	for name, group := range byName {
		var live []*api.NodeListStub
		for _, s := range group {
			if s.Status != api.NodeStatusDown {
				live = append(live, s)
			}
		}
		switch len(live) {
		case 0:
			// all down — bind the freshest so the box stays visible until GC
			best := group[0]
			for _, s := range group {
				if s.CreateIndex > best.CreateIndex {
					best = s
				}
			}
			bound[name] = best
		case 1:
			bound[name] = live[0]
		default:
			dupes[name] = true // genuine ambiguity: refuse to guess
		}
	}
	return bound, dupes
}

// markDuplicates sets DuplicateNodeName on the CR for each ambiguous Name
// (creating a minimal CR if none exists yet) without binding it to any node.
func (r *NomadNodeReconciler) markDuplicates(ctx context.Context, nc *nomadv1alpha1.NomadCluster, dupes map[string]bool) error {
	for name := range dupes {
		objName := sanitizeNodeName(name)
		var nn nomadv1alpha1.NomadNode
		err := r.Get(ctx, types.NamespacedName{Name: objName, Namespace: nc.Namespace}, &nn)
		if apierrors.IsNotFound(err) {
			nn = nomadv1alpha1.NomadNode{
				ObjectMeta: metav1.ObjectMeta{Name: objName, Namespace: nc.Namespace, Labels: names(nc).Labels()},
				Spec:       nomadv1alpha1.NomadNodeSpec{ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: name},
			}
			if err := controllerutil.SetControllerReference(nc, &nn, r.Scheme); err != nil {
				return err
			}
			if err := r.Create(ctx, &nn); err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
		} else if err != nil {
			return err
		}
		setNodeCondition(&nn, nomadv1alpha1.NomadNodeCondReconciled, metav1.ConditionFalse, nomadv1alpha1.ReasonDuplicateNodeName, "two or more non-down nodes share this Name")
		if err := r.Status().Update(ctx, &nn); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Pin the newly-read constant** — `internal/nomad/contract.go` already pins `NodeStatusDown` (Foundation). No change needed; confirm `go build ./...` clean.

- [ ] **Step 5: Run tests to verify they pass**

Run: `make test 2>&1 | tail -30`
Expected: PASS — both disambiguation specs green; the Task 5 mint spec still green.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/nomadnode_controller.go internal/controller/nomadnode_controller_test.go
git commit -m "feat(controller): NomadNode re-registration disambiguation + DuplicateNodeName"
```

---

## Task 7: Drive eligibility + drain with convergence

Implement `driveDesired`: reconcile `spec.eligible` via `SetEligibility` (compare-before-write) and `spec.drain` via `UpdateDrain` with the drain-satisfied predicate + generation-based issuance (design §3.3), so a completed drain converges instead of re-issuing every resync.

**Files:**
- Modify: `internal/controller/nomadnode_controller.go` (`driveDesired`)
- Modify: `internal/controller/nomadnode_controller_test.go` (add specs)
- Modify: `internal/nomad/contract.go` (pin `DrainStrategy`, `DrainStatus*`)

**Interfaces:**
- Consumes: `NomadNodeOps.SetEligibility/UpdateDrain`, `fakeNodeOps.eligCalls/drainCalls`.

- [ ] **Step 1: Write the failing tests** — add to `internal/controller/nomadnode_controller_test.go`:

```go
var _ = Describe("NomadNode reflector: drive", func() {
	It("toggles eligibility only on mismatch", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-elig-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		// Pre-create a NomadNode whose spec wants ineligible; Nomad reports eligible.
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNodeSpec{ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "e1", Eligible: false},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "e1id", Name: "e1", Status: "ready", SchedulingEligibility: "eligible"}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.eligCalls).To(HaveLen(1))
		Expect(fake.eligCalls[0]).To(Equal(eligCall{"e1id", false}))
	})

	It("does not re-issue a completed drain (converges)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-drain-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "d1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "d1",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		// record drainObservedGeneration == current generation, node already drained
		nn.Status.DrainObservedGeneration = nn.Generation
		Expect(k8s.Status().Update(ctx, nn)).To(Succeed())

		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "d1id", Name: "d1", Status: "ready", SchedulingEligibility: "ineligible", Drain: false,
				LastDrain: &api.DrainMetadata{Status: api.DrainStatusComplete}},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(BeEmpty(), "completed drain must not re-issue")
	})

	It("issues a drain when unsatisfied", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-drain2-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "d2", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "d2",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "d2id", Name: "d2", Status: "ready", SchedulingEligibility: "eligible", Drain: false}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(HaveLen(1))
		Expect(fake.drainCalls[0].spec).NotTo(BeNil())
		Expect(fake.drainCalls[0].markEligible).To(BeFalse())
	})
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A3 'reflector: drive'`
Expected: FAIL — `driveDesired` is a no-op, so `eligCalls`/`drainCalls` stay empty.

- [ ] **Step 3: Implement `driveDesired`** — replace the no-op in `internal/controller/nomadnode_controller.go`:

```go
const defaultDrainDeadline = time.Hour

// driveDesired reconciles spec.eligible/drain onto Nomad. Drain, when present,
// transiently dominates eligibility (Nomad forces a draining node ineligible),
// so eligibility is only driven when no drain is desired.
func (r *NomadNodeReconciler) driveDesired(ctx context.Context, nn *nomadv1alpha1.NomadNode, stub *api.NodeListStub, ops NomadNodeOps) error {
	if nn.Spec.Drain != nil {
		if drainSatisfied(nn, stub) {
			return nil // converged
		}
		spec := &api.DrainSpec{
			Deadline:         nn.Spec.Drain.Deadline.Duration,
			IgnoreSystemJobs: nn.Spec.Drain.IgnoreSystemJobs,
		}
		if spec.Deadline == 0 {
			spec.Deadline = defaultDrainDeadline
		}
		if err := ops.UpdateDrain(ctx, stub.ID, spec, false); err != nil {
			return err
		}
		nn.Status.DrainObservedGeneration = nn.Generation
		return nil
	}

	// No drain desired. If the node is still draining, cancel it, marking it
	// eligible per spec.eligible.
	if stub.Drain {
		return ops.UpdateDrain(ctx, stub.ID, nil, nn.Spec.Eligible)
	}
	// Otherwise reconcile eligibility directly, compare-before-write.
	want := api.NodeSchedulingEligible
	if !nn.Spec.Eligible {
		want = api.NodeSchedulingIneligible
	}
	if stub.SchedulingEligibility != want {
		return ops.SetEligibility(ctx, stub.ID, nn.Spec.Eligible)
	}
	return nil
}

// drainSatisfied reports whether the node has completed the drain requested at
// the current spec generation (design §3.3): drain removed, node ineligible,
// last drain complete, and issued at this generation.
func drainSatisfied(nn *nomadv1alpha1.NomadNode, stub *api.NodeListStub) bool {
	return !stub.Drain &&
		stub.SchedulingEligibility == api.NodeSchedulingIneligible &&
		stub.LastDrain != nil && stub.LastDrain.Status == api.DrainStatusComplete &&
		nn.Status.DrainObservedGeneration == nn.Generation
}
```

- [ ] **Step 4: Pin the newly-read `api` symbols** — in `internal/nomad/contract.go`, add to the type-pin block:

```go
	_ api.DrainStrategy
```

and add a constant-pin `var` block (or extend the existing constant block):

```go
	_ = api.DrainStatusComplete
	_ = api.DrainStatusCanceled
```

(Backed by real reads: `seedDrain` reads `node.DrainStrategy`; `drainSatisfied` reads `api.DrainStatusComplete`.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `make test 2>&1 | tail -30`
Expected: PASS — all three drive specs green; earlier specs still green; `go build ./...` clean.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/nomadnode_controller.go internal/controller/nomadnode_controller_test.go internal/nomad/contract.go
git commit -m "feat(controller): drive eligibility + convergent drain onto Nomad"
```

---

## Task 8: Prune + cascade

Implement `pruneAbsent`: hard-delete CRs whose node is absent from a successful list; never prune on list error (already handled — the reconcile returns before prune); confirm cluster-delete cascades via the ownerRef from Task 5.

**Files:**
- Modify: `internal/controller/nomadnode_controller.go` (`pruneAbsent`)
- Modify: `internal/controller/nomadnode_controller_test.go` (add specs)

- [ ] **Step 1: Write the failing tests** — add to `internal/controller/nomadnode_controller_test.go`:

```go
var _ = Describe("NomadNode reflector: prune + cascade", func() {
	It("deletes a CR whose node is absent from a successful list", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-prune-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		// First pass: node present -> CR minted.
		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "g1", Name: "ghost", Status: "ready", SchedulingEligibility: "eligible"}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "ghost", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})).To(Succeed())

		// Second pass: empty list -> CR pruned.
		fake.list = nil
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		err = k8s.Get(ctx, types.NamespacedName{Name: "ghost", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("does not prune when the list fails", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-noprune-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "k1", Name: "keep", Status: "ready", SchedulingEligibility: "eligible"}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		fake.listErr = errors.New("unreachable")
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "keep", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})).To(Succeed(), "must survive a list error")
	})
})
```

Add `"errors"` to the test imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A3 'prune'`
Expected: FAIL — `pruneAbsent` is a no-op, so `ghost` is not deleted.

- [ ] **Step 3: Implement `pruneAbsent`** — replace the no-op in `internal/controller/nomadnode_controller.go`:

```go
// pruneAbsent deletes this cluster's NomadNode CRs whose node Name is absent
// from the successful list (Nomad GC'd them). It is only reached after a
// successful ListNodes (the reconcile returns earlier on list error), so a
// transient outage never prunes.
func (r *NomadNodeReconciler) pruneAbsent(ctx context.Context, nc *nomadv1alpha1.NomadCluster, bound map[string]*api.NodeListStub) error {
	present := map[string]bool{}
	for name := range bound {
		present[sanitizeNodeName(name)] = true
	}
	var list nomadv1alpha1.NomadNodeList
	if err := r.List(ctx, &list, client.InNamespace(nc.Namespace), client.MatchingLabels(names(nc).Labels())); err != nil {
		return err
	}
	for i := range list.Items {
		nn := &list.Items[i]
		if nn.Spec.ClusterRef.Name != nc.Name {
			continue
		}
		if !present[nn.Name] {
			if err := r.Delete(ctx, nn); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `make test 2>&1 | tail -30`
Expected: PASS — prune + no-prune-on-error specs green; all earlier specs green.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadnode_controller.go internal/controller/nomadnode_controller_test.go
git commit -m "feat(controller): prune GC'd NomadNodes; never prune on list error"
```

---

## Task 9: Optional `NomadClusterSpec.nodeGCThreshold` + gated rendering

**Files:**
- Modify: `api/v1alpha1/nomadcluster_types.go` (add field)
- Modify: `internal/controller/config_render.go` (gated render + hash)
- Modify: `internal/controller/config_render_test.go` (add cases)
- Regenerate: `zz_generated.deepcopy.go`, `config/crd/bases/nomad.operator.io_nomadclusters.yaml`

- [ ] **Step 1: Add the field** — in `api/v1alpha1/nomadcluster_types.go`, add to `NomadClusterSpec` (after `Datacenters`):

```go
	// NodeGCThreshold sets the servers' node_gc_threshold — how long a node
	// must stay in a terminal (down) state before Nomad garbage-collects it.
	// Optional with NO default: when unset, the operator emits nothing and
	// Nomad uses its built-in default (24h). The NomadNode reflector's
	// down-node retention window tracks whatever this resolves to.
	// +optional
	NodeGCThreshold *metav1.Duration `json:"nodeGCThreshold,omitempty"`
```

- [ ] **Step 2: Write the failing render test** — add to `internal/controller/config_render_test.go`:

```go
func TestRenderConfigNodeGCThreshold(t *testing.T) {
	base := &nomadv1alpha1.NomadCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec: nomadv1alpha1.NomadClusterSpec{
			Servers: 1, Region: "global",
			ExternalAccess: nomadv1alpha1.ExternalAccessSpec{
				Mode: nomadv1alpha1.ExternalAccessGateway,
				Gateway: &nomadv1alpha1.GatewaySpec{Mode: nomadv1alpha1.GatewayModeManaged, RPCPorts: []int32{14647}, HTTPHostname: "h"},
			},
		},
	}
	unsetBody, unsetHash := renderConfig(base, "1.2.3.4")
	if strings.Contains(unsetBody, "node_gc_threshold") {
		t.Error("unset: node_gc_threshold must not render")
	}

	set := base.DeepCopy()
	set.Spec.NodeGCThreshold = &metav1.Duration{Duration: 48 * time.Hour}
	setBody, setHash := renderConfig(set, "1.2.3.4")
	if !strings.Contains(setBody, `node_gc_threshold = "48h0m0s"`) {
		t.Errorf("set: expected node_gc_threshold in body, got:\n%s", setBody)
	}
	if setHash == unsetHash {
		t.Error("setting node_gc_threshold must change the rollout hash")
	}
}
```

Add `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`, `"strings"`, `"time"` to the test imports if absent.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestRenderConfigNodeGCThreshold -v`
Expected: FAIL — body never contains `node_gc_threshold`.

- [ ] **Step 4: Implement gated render** — in `internal/controller/config_render.go`, inside `renderConfig`, after the `server { ... }` block is written (after line 30) and before the `acl {` block, add:

```go
	if nc.Spec.NodeGCThreshold != nil {
		fmt.Fprintf(&b, "node_gc_threshold = %q\n\n", nc.Spec.NodeGCThreshold.Duration.String())
	}
```

The rendered value is part of `body`, which already feeds the SHA-256 hash — so setting/changing it rolls the StatefulSet automatically; no separate hash change needed.

- [ ] **Step 5: Regenerate + run tests**

Run: `make manifests generate && make test 2>&1 | tail -30`
Expected: PASS — the render test + all envtest specs green; `nomadclusters` CRD gains the optional `nodeGCThreshold` property.

- [ ] **Step 6: Commit**

```bash
git add api/v1alpha1/nomadcluster_types.go api/v1alpha1/zz_generated.deepcopy.go config/crd/bases/ internal/controller/config_render.go internal/controller/config_render_test.go
git commit -m "feat(api): optional NomadClusterSpec.nodeGCThreshold, gated render"
```

---

## Task 10: Live integration exercise + runbook

**Files:**
- Modify: `internal/nomad/client_write_integration_test.go` (add a node eligibility/drain live check)
- Create: `docs/runbooks/nomadnode.md`
- Modify: `cmd/main.go` (register `NomadNodeReconciler` with the manager)

**Interfaces:**
- Consumes: `SetEligibility`/`UpdateDrain` (Task 1), `NomadNodeReconciler.SetupWithManager` (Task 5).

- [ ] **Step 1: Wire the reconciler into main** — in `cmd/main.go`, where `NomadClusterReconciler` is registered with the manager, add alongside it:

```go
	if err := (&controller.NomadNodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NomadNode")
		os.Exit(1)
	}
```

- [ ] **Step 2: Write the failing integration test** — add to `internal/nomad/client_write_integration_test.go` (build tag `//go:build integration` is already at the file top; this test execs a real `nomad agent -dev`, per the existing `TestACLBootstrapAndLeaderLive` pattern — reuse its dev-agent bootstrap helper):

```go
func TestNodeEligibilityAndDrainLive(t *testing.T) {
	c, nodeID := devAgentWithNode(t) // helper: boots nomad -dev, waits for a ready node, returns client + node ID
	ctx := t.Context()

	if err := c.SetEligibility(ctx, nodeID, false); err != nil {
		t.Fatalf("SetEligibility: %v", err)
	}
	node, err := c.NodeInfo(ctx, nodeID)
	if err != nil {
		t.Fatalf("NodeInfo: %v", err)
	}
	if node.SchedulingEligibility != "ineligible" {
		t.Errorf("eligibility = %q, want ineligible", node.SchedulingEligibility)
	}

	if err := c.UpdateDrain(ctx, nodeID, &api.DrainSpec{Deadline: time.Second}, false); err != nil {
		t.Fatalf("UpdateDrain: %v", err)
	}
	if err := c.UpdateDrain(ctx, nodeID, nil, true); err != nil { // cancel, mark eligible
		t.Fatalf("UpdateDrain cancel: %v", err)
	}
}
```

If `devAgentWithNode` does not already exist, add it modeled on the dev-agent bootstrap in `TestACLBootstrapAndLeaderLive` (boot `nomad agent -dev`, poll `ListNodes` until one reports `ready`, return the client + that node's ID). Reuse the existing helper if present.

- [ ] **Step 3: Run the integration test**

Run: `make test-integration 2>&1 | tail -30` (needs a `nomad` v2.0.4 binary; on macOS use the containerized method in `docs/runbooks/nomadcluster.md` §7)
Expected: PASS — eligibility flips to `ineligible`; drain set + cancel both succeed. If no `nomad` binary is available in the execution environment, the test compiles under `-tags integration` and is skipped; note this in the task report and defer the live run to the user.

- [ ] **Step 4: Write the runbook** — create `docs/runbooks/nomadnode.md`:

```markdown
# NomadNode runbook

`NomadNode` is a **representation** of a Nomad edge client — the operator
creates and deletes these CRs (reflecting Nomad's node registry); you read and
update them. It mirrors the Kubernetes `Node`/`cordon` model.

## Inspect the fleet
    kubectl get nomadnodes
    # NAME        CLUSTER   STATUS   ELIGIBLE     DRAINING   AGE

## Cordon a node (stop new placements, keep running allocs)
    kubectl patch nomadnode <name> --type=merge -p '{"spec":{"eligible":false}}'

## Drain a node (migrate allocs off, then it stays ineligible)
    kubectl patch nomadnode <name> --type=merge -p \
      '{"spec":{"drain":{"deadline":"1h","ignoreSystemJobs":true}}}'
    # Cancel a drain by removing the block:
    kubectl patch nomadnode <name> --type=json -p '[{"op":"remove","path":"/spec/drain"}]'

## Behavior notes
- **You cannot create or delete nodes here.** Deleting a NomadNode CR just
  re-mirrors on the next resync (~30s) if the node still exists; it never
  drains or deregisters the machine.
- **Spec wins over out-of-band changes.** If you run `nomad node drain -enable`
  from the CLI on a node whose CR has no `spec.drain`, the operator cancels it
  within one resync. Manage drains through the CR.
- **Down nodes stay visible** until Nomad garbage-collects them
  (`node_gc_threshold`, default 24h; set `NomadClusterSpec.nodeGCThreshold` to
  change it). Then the CR is pruned automatically.
- **Deleting the NomadCluster** cascades and removes all its NomadNode CRs.
```

- [ ] **Step 5: Run the full gate**

Run: `make manifests generate fmt vet && make test 2>&1 | tail -20`
Expected: PASS — all unit + envtest specs green; build clean.

- [ ] **Step 6: Commit**

```bash
git add internal/nomad/client_write_integration_test.go docs/runbooks/nomadnode.md cmd/main.go
git commit -m "test(nomad): live node eligibility/drain; wire reconciler + runbook"
```

---

## Self-Review Checklist (completed by plan author)

- **Spec coverage:** representation model + CRUD split (T3, T5); node identity/ephemeral-ID + sanitized name (T3, T4, T5); eligibility + drain composition with convergence (T7); reflector loop cluster-keyed (T5); disambiguation (T6); prune lifecycle incl. list-error safety + cascade (T8); clientFor extraction (T2); `internal/nomad` additions + pins backed by real calls (T1, T5, T7); optional `nodeGCThreshold` gated render (T9); W5 retirement (no code — nothing to build, verified in design); envtest with injected fake (T5–T8); integration + runbook (T10). All design §§1–10 map to a task.
- **Placeholder scan:** no TBD/TODO; every code step shows complete code; the one deferred item (a missing `devAgentWithNode` integration helper) has explicit construction instructions.
- **Type consistency:** `NomadNodeOps`, `NomadNodeClientFactory`, `bindNodes`, `upsertNode`, `driveDesired`, `drainSatisfied`, `pruneAbsent`, `sanitizeNodeName`, `clusterNomadConfig`, `setNodeCondition` used identically across tasks; `NodeDrainSpec.Deadline` is `metav1.Duration` throughout; contract pins (`ToggleEligibility`/`UpdateDrain`/`DrainSpec` in T1, `DrainMetadata` in T5, `DrainStrategy`/`DrainStatus*` in T7) each land in the task whose code backs them.
- **Verify-at-plan-time carried from design §10:** confirm `node_gc_threshold` name/default (T9 assumes `server.node_gc_threshold`, built-in 24h); confirm `DrainSpec.Deadline` 0/negative encoding (T1/T7 use `0 → default 1h`); the generation-based drain issuance is exercised by T7's convergence specs; two controllers on `NomadCluster` is validated by T5's envtest actually running.
