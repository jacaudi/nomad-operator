# External Access Modes (Gateway + LoadBalancer) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **For Claude:** REQUIRED EXECUTION WORKFLOW (follow in order):
> 1. `superpowers:using-git-worktrees` — Isolate work in a dedicated worktree
> 2. `superpowers:subagent-driven-development` — Dispatch a fresh subagent per task
> 3. `superpowers:test-driven-development` — All subagents use TDD
> 4. `superpowers:verification-before-completion` — Verify all tests pass per task
> 5. `superpowers:requesting-code-review` — Code review after each task (built in)
> 6. After all tasks: comprehensive code review on full diff from branch point (automatic)
> 7. `superpowers:finishing-a-development-branch` — Complete the branch
>
> Skills carry their own model and effort settings. Do not override them.

**Goal:** Restructure `spec.gateway` into a discriminated `spec.externalAccess { mode: Gateway | LoadBalancer }` union and add a single-VIP `LoadBalancer` external-access mode scoped to `servers: 1`.

**Architecture:** A pure-refactor task re-homes the existing Gateway surface under `externalAccess.gateway` (build stays green, behavior identical) and renames the mode-specific status field to a neutral one. A second task adds the LoadBalancer building blocks (Service builder, address gate, and the nil-guards/port-synthesis the optional gateway pointer now requires) as unit-tested pieces. A third task wires mode dispatch into `Reconcile`, partitions the provisioning block (shared / Gateway-only / LB-only), and proves LoadBalancer mode end-to-end in envtest.

**Tech Stack:** Go 1.26.4, kubebuilder v4, controller-runtime v0.23.3, k8s v0.35.0, `sigs.k8s.io/gateway-api` v1.2.1 (experimental channel, already vendored), Ginkgo/Gomega envtest + stdlib `testing` unit tests, CEL x-kubernetes-validations.

## Global Constraints

- Go toolchain **1.26.4**; Nomad target **v2.0.4** (`api` pinned at commit `5b83b133998a`, no `/v2`). — one line each, copied from prior slices.
- **`v1alpha1` is unreleased** → the breaking `spec.gateway → spec.externalAccess` rename ships with **no conversion webhook**. Do not add back-compat machinery.
- **Only new external dep is none** — LoadBalancer mode uses plain `core/v1.Service`; add no dependency.
- **`internal/nomad.Client` stays per-endpoint** (built from an explicit `api.Config`, never `api.DefaultConfig()`); this plan does not touch `internal/nomad`.
- **`apply()` is Server-Side Apply** (`client.Patch` + `client.Apply` + `FieldOwner("nomad-operator")` + `ForceOwnership`); reuse it for the LB Service — never Get+Update (clears the immutable Service `clusterIP`... note: a `type: LoadBalancer` Service still has a `clusterIP`, so SSA is mandatory here too).
- **Naming goes through `names(nc)`** — never format an object name inline.
- **Signed commits** need the user's 1Password Touch ID; if `git commit` fails with "1Password: failed to fill whole buffer", stop and ask the user to unlock — do not disable `commit.gpgsign`.
- After every task: `make manifests generate fmt vet && make test` must be green before the task is complete.

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `api/v1alpha1/nomadcluster_types.go` | New `ExternalAccessSpec`/`ExternalAccessMode`/`LoadBalancerSpec` types; re-homed `GatewaySpec`; all CEL; renamed status field + condition. | 1 |
| `api/v1alpha1/zz_generated.deepcopy.go` | Regenerated deepcopy for the new types. | 1 |
| `config/crd/bases/nomad.operator.io_nomadclusters.yaml` | Regenerated CRD schema. | 1 |
| `internal/controller/config_render.go` | `rpcAdvertisePorts` helper (single source for per-ordinal RPC ports); hash reads it. | 1 (rename) / 2 (synthesis) |
| `internal/controller/resources_workload.go` | `buildConfigMap` reads `rpcAdvertisePorts`. | 1 (rename) / 2 (synthesis) |
| `internal/controller/resources_gateway.go` | Six Gateway-only builders re-homed to `spec.externalAccess.gateway`. | 1 |
| `internal/controller/resources_loadbalancer.go` | **New:** `buildLoadBalancerService` + `ensureLoadBalancer`. | 2 |
| `internal/controller/names.go` | Add `LBService` name. | 2 |
| `internal/controller/nomadcluster_controller.go` | Re-home reads + status rename (T1); `gatewayToClusters` nil-guard (T2); mode dispatch + step-3 partition (T3). | 1, 2, 3 |
| `docs/runbooks/nomadcluster.md` | LoadBalancer-mode section + `-tls-server-name` HTTP note + provider-required note. | 3 |
| test files (`*_test.go`) | Re-home fixtures (T1); unit tests for pieces (T2); LB envtest (T3). | 1, 2, 3 |

---

## Task 1: Restructure the external-access surface (behavior unchanged)

Atomic refactor: introduce the `externalAccess` union, re-home every `spec.gateway` read-site and test fixture, add all CEL, rename the mode-specific status field/condition, and regenerate. Mode defaults to `Gateway`; every existing Gateway-mode behavior and test must pass unchanged under the new path. This task must land whole — a partial rename leaves the build broken.

**Files:**
- Modify: `api/v1alpha1/nomadcluster_types.go`
- Regenerate: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/nomad.operator.io_nomadclusters.yaml`
- Modify: `internal/controller/nomadcluster_controller.go`, `internal/controller/resources_gateway.go`, `internal/controller/resources_workload.go`, `internal/controller/config_render.go`
- Modify (fixtures): `internal/controller/nomadcluster_controller_test.go`, `internal/controller/resources_gateway_test.go`, `internal/controller/gatewaywatch_test.go`, `internal/controller/config_render_test.go`

**Interfaces:**
- Produces: `ExternalAccessSpec{ Mode ExternalAccessMode; Gateway *GatewaySpec; LoadBalancer *LoadBalancerSpec }`; `ExternalAccessMode` consts `ExternalAccessGateway="Gateway"`, `ExternalAccessLoadBalancer="LoadBalancer"`; `LoadBalancerSpec{ LoadBalancerClass string; Annotations map[string]string }`; `NomadClusterSpec.ExternalAccess ExternalAccessSpec` (replaces `.Gateway`); `NomadClusterStatus.ExternalAddress string` (replaces `.GatewayAddress`); condition const `CondExternalAccessReady="ExternalAccessReady"` (replaces `CondGatewayReady`); helper `rpcAdvertisePorts(nc) []int32` (Task 1 returns `nc.Spec.ExternalAccess.Gateway.RPCPorts`; Task 2 adds the LB branch).

- [ ] **Step 1: Write failing CEL tests for the new union (envtest)**

Add to `internal/controller/nomadcluster_controller_test.go`, inside the existing `Describe("NomadCluster reconcile skeleton", ...)` block (these run against the real CRD in envtest). They reference the not-yet-existing `ExternalAccess` shape, so they fail to compile first — that is the failing state.

```go
	It("rejects LoadBalancer mode with servers: 3 (LB requires servers: 1)", func() {
		ctx := context.Background()
		nc := minimalCluster("lb-multi", "default")
		nc.Spec.Servers = 3
		nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{
			Mode:         nomadv1alpha1.ExternalAccessLoadBalancer,
			LoadBalancer: &nomadv1alpha1.LoadBalancerSpec{},
		}
		Expect(k8s.Create(ctx, nc)).NotTo(Succeed())
	})

	It("accepts LoadBalancer mode with servers: 1 and no gateway block", func() {
		ctx := context.Background()
		nc := singleServerCluster("lb-single", "default")
		nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{
			Mode:         nomadv1alpha1.ExternalAccessLoadBalancer,
			LoadBalancer: &nomadv1alpha1.LoadBalancerSpec{},
		}
		Expect(k8s.Create(ctx, nc)).To(Succeed())
	})

	It("rejects a gateway block set under LoadBalancer mode (union exclusivity)", func() {
		ctx := context.Background()
		nc := singleServerCluster("lb-badunion", "default")
		nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{
			Mode:    nomadv1alpha1.ExternalAccessLoadBalancer,
			Gateway: &nomadv1alpha1.GatewaySpec{Mode: nomadv1alpha1.GatewayModeManaged, ClassName: "cilium", RPCPorts: []int32{14647}, HTTPHostname: "nomad.example.com"},
		}
		Expect(k8s.Create(ctx, nc)).NotTo(Succeed())
	})

	It("rejects mutation of the immutable externalAccess.mode field", func() {
		ctx := context.Background()
		nc := singleServerCluster("mode-immut", "default")
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		nc.Spec.ExternalAccess.Mode = nomadv1alpha1.ExternalAccessLoadBalancer
		Expect(k8s.Update(ctx, nc)).NotTo(Succeed())
	})
```

- [ ] **Step 2: Run tests to confirm they fail to compile**

Run: `go build ./... 2>&1 | head`
Expected: compile errors referencing `nomadv1alpha1.ExternalAccessSpec` / `ExternalAccessLoadBalancer` undefined.

- [ ] **Step 3: Define the new types + CEL in `nomadcluster_types.go`**

Add these types (place after `GatewaySpec`). Keep `GatewayMode`, `GatewaySpec`, `GatewayRef` exactly as they are — only their *home* moves:

```go
// ExternalAccessMode selects how a NomadCluster's control plane is exposed to
// out-of-cluster edge agents.
type ExternalAccessMode string

const (
	// ExternalAccessGateway exposes the control plane via Gateway API objects
	// (one Gateway, per-server RPC listeners). Supports servers 1/3/5.
	ExternalAccessGateway ExternalAccessMode = "Gateway"
	// ExternalAccessLoadBalancer exposes a single-node control plane via one
	// type: LoadBalancer Service (north-south only; scoped to servers: 1).
	ExternalAccessLoadBalancer ExternalAccessMode = "LoadBalancer"
)

// LoadBalancerSpec configures the type: LoadBalancer Service used in
// LoadBalancer external-access mode. Kept intentionally lean (KISS): cloud LB
// behavior is driven by annotations, which already cover static-IP requests.
type LoadBalancerSpec struct {
	// +optional
	LoadBalancerClass string `json:"loadBalancerClass,omitempty"`
	// Annotations are applied verbatim to the LoadBalancer Service (cloud-LB
	// config). Untyped by design.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ExternalAccessSpec is a discriminated union selecting the external-access
// mechanism. gateway is set iff mode==Gateway; loadBalancer is optional and may
// be set only when mode==LoadBalancer.
//
// +kubebuilder:validation:XValidation:rule="self.mode == oldSelf.mode",message="externalAccess.mode is immutable"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Gateway' || has(self.gateway)",message="externalAccess.gateway is required when mode is Gateway"
// +kubebuilder:validation:XValidation:rule="self.mode != 'LoadBalancer' || !has(self.gateway)",message="externalAccess.gateway must be absent when mode is LoadBalancer"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Gateway' || !has(self.loadBalancer)",message="externalAccess.loadBalancer must be absent when mode is Gateway"
type ExternalAccessSpec struct {
	// +kubebuilder:validation:Enum=Gateway;LoadBalancer
	// +kubebuilder:default=Gateway
	Mode ExternalAccessMode `json:"mode,omitempty"`
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`
	// +optional
	LoadBalancer *LoadBalancerSpec `json:"loadBalancer,omitempty"`
}
```

In `NomadClusterSpec`: replace the `Gateway GatewaySpec` field and move the root cross-field CEL. Replace the existing root marker

```go
// +kubebuilder:validation:XValidation:rule="size(self.gateway.rpcPorts) == self.servers",message="gateway.rpcPorts length must equal servers"
```

with (guarded implications so CEL never dereferences an absent gateway block):

```go
// +kubebuilder:validation:XValidation:rule="self.externalAccess.mode != 'Gateway' || size(self.externalAccess.gateway.rpcPorts) == self.servers",message="externalAccess.gateway.rpcPorts length must equal servers"
// +kubebuilder:validation:XValidation:rule="self.externalAccess.mode != 'LoadBalancer' || self.servers == 1",message="LoadBalancer external-access mode requires servers: 1"
```

and replace the field

```go
	// +kubebuilder:validation:Required
	Gateway GatewaySpec `json:"gateway"`
```

with

```go
	// +kubebuilder:validation:Required
	ExternalAccess ExternalAccessSpec `json:"externalAccess"`
```

Update the `TLSSpec.CertSecretRef` doc comment (`SANs must include ... spec.gateway.httpHostname ...`) to read `... spec.externalAccess.gateway.httpHostname (Gateway mode) ...`.

Rename the status field and condition:
- In `NomadClusterStatus`: `GatewayAddress string` json `gatewayAddress` → `ExternalAddress string` json `externalAddress`.
- In the condition consts: `CondGatewayReady = "GatewayReady"` → `CondExternalAccessReady = "ExternalAccessReady"`.

The `GatewaySpec` XValidation markers (`className required when Managed`, `ref required when Existing`) and its `RPCPorts` markers (`MinItems=1`, `self == oldSelf` immutable) stay verbatim on `GatewaySpec` — they only reference `self` within that struct.

- [ ] **Step 4: Regenerate deepcopy + CRD**

Run: `make manifests generate`
Expected: `zz_generated.deepcopy.go` gains `ExternalAccessSpec`/`LoadBalancerSpec` DeepCopy funcs; the CRD YAML shows `spec.externalAccess` with the new rules and drops `spec.gateway`.

- [ ] **Step 5: Re-home every `spec.gateway` read-site (production code)**

Introduce the single-source helper in `config_render.go` (Task 1 body; Task 2 extends it):

```go
// rpcAdvertisePorts returns the per-ordinal RPC advertise ports for the active
// external-access mode. Gateway mode uses the user's gateway.rpcPorts.
func rpcAdvertisePorts(nc *nomadv1alpha1.NomadCluster) []int32 {
	return nc.Spec.ExternalAccess.Gateway.RPCPorts
}
```

Then change every `nc.Spec.Gateway...` reference to route through `spec.externalAccess.gateway` (all sites, exhaustive — confirm with `grep -rn 'Spec.Gateway' internal/ api/` afterward):
- `config_render.go:35` — hash term `fmt.Sprint(nc.Spec.Gateway.RPCPorts)` → `fmt.Sprint(rpcAdvertisePorts(nc))`.
- `resources_workload.go:26` — `for _, p := range nc.Spec.Gateway.RPCPorts` → `for _, p := range rpcAdvertisePorts(nc)`.
- `resources_gateway.go` — `buildManagedGateway` (`:34` `HTTPHostname`, `:37` `RPCPorts`, `:47` `ClassName`), `parentGateway` (`:59-61` `Mode`/`Ref`), `buildTLSRoute` (`:81` `HTTPHostname`), `ensureGateway` (`:137` `Mode`), `ensureExistingGateway` (`:151` `Ref`, `:172` `HTTPHostname`, `:176` `RPCPorts`): every `nc.Spec.Gateway.X` → `nc.Spec.ExternalAccess.Gateway.X`.
- `nomadcluster_controller.go` — status write `:114` `nc.Status.GatewayAddress = gwAddr` → `nc.Status.ExternalAddress = gwAddr`; the two `CondGatewayReady` sets (`:111`, `:115`) → `CondExternalAccessReady`; `gatewayToClusters` (`:255-256`) `nc.Spec.Gateway.Ref`/`.Mode` → `nc.Spec.ExternalAccess.Gateway.Ref`/`.Mode`. (The nil-guard for `gatewayToClusters` is added in Task 2, where LB clusters can first exist — Task 1 keeps it a mechanical field move; every cluster is Gateway mode here.)

- [ ] **Step 6: Re-home every test fixture**

- `nomadcluster_controller_test.go` `minimalCluster` (`:28-31`): replace the `Gateway: nomadv1alpha1.GatewaySpec{...}` field with:

```go
			ExternalAccess: nomadv1alpha1.ExternalAccessSpec{
				Mode: nomadv1alpha1.ExternalAccessGateway,
				Gateway: &nomadv1alpha1.GatewaySpec{
					Mode: nomadv1alpha1.GatewayModeManaged, ClassName: "cilium",
					RPCPorts: []int32{14647, 24647, 34647}, HTTPHostname: "nomad.example.com",
				},
			},
```

- `singleServerCluster` (`:46-49`): same shape with `RPCPorts: []int32{14647}`.
- `nomadcluster_controller_test.go:91` (`nc.Spec.Gateway.RPCPorts = []int32{14647, 24647}`) → `nc.Spec.ExternalAccess.Gateway.RPCPorts = []int32{14647, 24647}`.
- `resources_gateway_test.go`: `:41` `nc.Spec.Gateway.RPCPorts[ordinal]` → `nc.Spec.ExternalAccess.Gateway.RPCPorts[ordinal]`; `:137-139` (`nc.Spec.Gateway.Mode`/`.ClassName`/`.Ref`) → `nc.Spec.ExternalAccess.Gateway.*`; the three `nc.Spec.Gateway = nomadv1alpha1.GatewaySpec{...}` reassignments (`:166`, `:191`, `:228`) → `nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{Mode: nomadv1alpha1.ExternalAccessGateway, Gateway: &nomadv1alpha1.GatewaySpec{...same fields...}}`; the two `CondGatewayReady` assertions (`:173`, `:198`, `:235`) → `CondExternalAccessReady`.
- `gatewaywatch_test.go` `existingModeCluster` (`:19-20`): `nc.Spec.Gateway.Mode`/`.ClassName`/`.Ref` → `nc.Spec.ExternalAccess.Gateway.*`.
- `config_render_test.go`: no `Spec.Gateway` reference — but `TestRenderConfigDeterministicHash` still exercises the address-changes-the-hash path; leave as-is (it calls `renderConfig(nc, addr)`).

- [ ] **Step 7: Run the full suite green**

Run: `make manifests generate fmt vet && make test`
Expected: PASS. All prior Gateway-mode specs pass under the re-homed path; the four new CEL specs from Step 1 pass. `grep -rn 'Spec.Gateway\b' internal/ api/` returns nothing (every site re-homed); `grep -rn 'GatewayAddress\|CondGatewayReady' internal/ api/` returns nothing.

- [ ] **Step 8: Commit**

```bash
git add api/ config/ internal/controller/
git commit -m "refactor(api): restructure spec.gateway into spec.externalAccess union

Re-home the Gateway surface under externalAccess.gateway (optional
pointer), add ExternalAccessSpec/LoadBalancerSpec types + CEL (mode
enum+immutable, LoadBalancer=>servers==1, guarded rpcPorts==servers,
union exclusivity), and rename status.gatewayAddress =>
status.externalAddress / GatewayReady => ExternalAccessReady. Behavior
unchanged; v1alpha1 unreleased so no conversion webhook.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: LoadBalancer building blocks (unit-tested, not yet wired)

Add the LB Service builder, the address gate, the `LBService` name, the `gatewayToClusters` nil-guard, and the LB-mode RPC-port synthesis. All pure functions / map-func / helpers — unit-testable in isolation. `Reconcile` does not dispatch to them yet (Task 3), so no envtest here.

> **Note:** `ensureLoadBalancer` is unreferenced until Task 3 wires it. This is fine — Go does not error on unused methods, and `go vet` (the repo's gate) does not flag them. If a stricter linter ("unused") is later enabled and complains, exercise `ensureLoadBalancer` from a fake-client unit test rather than deleting it; Task 3 makes it live either way.

**Files:**
- Create: `internal/controller/resources_loadbalancer.go`
- Create: `internal/controller/resources_loadbalancer_test.go`
- Modify: `internal/controller/names.go`
- Modify: `internal/controller/config_render.go` (extend `rpcAdvertisePorts`)
- Modify: `internal/controller/nomadcluster_controller.go` (`gatewayToClusters` nil-guard)
- Modify: `internal/controller/gatewaywatch_test.go` (nil-guard regression test)

**Interfaces:**
- Consumes: `names(nc).LBService`; `nc.Spec.ExternalAccess.{Mode,LoadBalancer}`; the `apply` method; `portRPC`/`portHTTP`.
- Produces: `buildLoadBalancerService(nc) *corev1.Service`; `(*NomadClusterReconciler).ensureLoadBalancer(ctx, nc) (addr string, ready bool, err error)`; `rpcAdvertisePorts` gains the LB branch returning `[]int32{portRPC}`.

- [ ] **Step 1: Add the `LBService` name (write failing test)**

Add to `internal/controller/names_test.go`:

```go
func TestLBServiceName(t *testing.T) {
	nc := singleServerCluster("edge", "nomad-system")
	if got := names(nc).LBService; got != "edge-lb" {
		t.Errorf("LBService = %q, want %q", got, "edge-lb")
	}
}
```

- [ ] **Step 2: Run it, confirm fail**

Run: `go test ./internal/controller/ -run TestLBServiceName 2>&1 | head`
Expected: compile error — `names(nc).LBService` undefined.

- [ ] **Step 3: Add the field**

In `names.go`, add `LBService string` to `resourceNames` (after `TLSRoute`), and in `names(nc)` add `LBService: nc.Name + "-lb",`.

- [ ] **Step 4: Run it, confirm pass**

Run: `go test ./internal/controller/ -run TestLBServiceName -v`
Expected: PASS.

- [ ] **Step 5: Write failing test for `buildLoadBalancerService`**

Create `internal/controller/resources_loadbalancer_test.go`:

```go
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

func lbCluster(name, ns string) *nomadv1alpha1.NomadCluster {
	nc := singleServerCluster(name, ns)
	nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{
		Mode:         nomadv1alpha1.ExternalAccessLoadBalancer,
		LoadBalancer: &nomadv1alpha1.LoadBalancerSpec{},
	}
	return nc
}

func TestBuildLoadBalancerService(t *testing.T) {
	nc := lbCluster("edge", "nomad-system")
	svc := buildLoadBalancerService(nc)

	if svc.Name != "edge-lb" {
		t.Errorf("name = %q, want edge-lb", svc.Name)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("type = %q, want LoadBalancer", svc.Spec.Type)
	}
	ports := map[string]int32{}
	for _, p := range svc.Spec.Ports {
		ports[p.Name] = p.Port
	}
	if ports["rpc"] != 4647 || ports["http"] != 4646 {
		t.Errorf("ports = %+v, want rpc:4647 http:4646", ports)
	}
	if svc.Spec.Selector["app.kubernetes.io/instance"] != "edge" {
		t.Errorf("selector = %+v, want instance=edge", svc.Spec.Selector)
	}
}

func TestBuildLoadBalancerServiceClassAndAnnotations(t *testing.T) {
	nc := lbCluster("edge", "nomad-system")
	nc.Spec.ExternalAccess.LoadBalancer = &nomadv1alpha1.LoadBalancerSpec{
		LoadBalancerClass: "service.k8s.aws/nlb",
		Annotations:       map[string]string{"foo": "bar"},
	}
	svc := buildLoadBalancerService(nc)

	if svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass != "service.k8s.aws/nlb" {
		t.Errorf("loadBalancerClass = %v, want service.k8s.aws/nlb", svc.Spec.LoadBalancerClass)
	}
	if svc.Annotations["foo"] != "bar" {
		t.Errorf("annotations = %+v, want foo=bar", svc.Annotations)
	}
}
```

- [ ] **Step 6: Run it, confirm fail**

Run: `go test ./internal/controller/ -run TestBuildLoadBalancerService 2>&1 | head`
Expected: compile error — `buildLoadBalancerService` undefined.

- [ ] **Step 7: Implement `buildLoadBalancerService` + `ensureLoadBalancer`**

Create `internal/controller/resources_loadbalancer.go`:

```go
package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

// buildLoadBalancerService builds the single type: LoadBalancer Service that
// fronts a single-node (servers: 1) control plane in LoadBalancer mode. It
// exposes RPC 4647 and HTTP 4646 and selects the server pods directly (no
// per-pod backend Services, no Gateway). North-south only — servers: 1 has no
// east-west Raft.
func buildLoadBalancerService(nc *nomadv1alpha1.NomadCluster) *corev1.Service {
	n := names(nc)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: n.LBService, Namespace: nc.Namespace, Labels: n.Labels()},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: n.Labels(),
			Ports: []corev1.ServicePort{
				{Name: "rpc", Port: portRPC, TargetPort: intstr.FromInt32(portRPC), Protocol: corev1.ProtocolTCP},
				{Name: "http", Port: portHTTP, TargetPort: intstr.FromInt32(portHTTP), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if lb := nc.Spec.ExternalAccess.LoadBalancer; lb != nil {
		if lb.LoadBalancerClass != "" {
			svc.Spec.LoadBalancerClass = &lb.LoadBalancerClass
		}
		if len(lb.Annotations) > 0 {
			svc.Annotations = lb.Annotations
		}
	}
	return svc
}

// ensureLoadBalancer applies the LoadBalancer Service and re-reads it to observe
// the ingress address the LB provider assigns (status.loadBalancer.ingress). It
// mirrors ensureManagedGateway: envtest runs no LB provider, so tests stub the
// ingress directly. Returns ready=false (never an error) until an address lands,
// so the reconciler surfaces ExternalAccessReady=False and requeues.
func (r *NomadClusterReconciler) ensureLoadBalancer(ctx context.Context, nc *nomadv1alpha1.NomadCluster) (string, bool, error) {
	svc := buildLoadBalancerService(nc)
	if err := r.apply(ctx, nc, svc); err != nil {
		return "", false, err
	}
	var current corev1.Service
	if err := r.Get(ctx, client.ObjectKeyFromObject(svc), &current); err != nil {
		return "", false, err
	}
	for _, ing := range current.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP, true, nil
		}
		if ing.Hostname != "" {
			return ing.Hostname, true, nil
		}
	}
	return "", false, nil
}
```

- [ ] **Step 8: Run it, confirm pass**

Run: `go test ./internal/controller/ -run TestBuildLoadBalancerService -v`
Expected: PASS (both cases).

- [ ] **Step 9: Write failing test for the `gatewayToClusters` nil-guard (crash-loop regression)**

Add to `internal/controller/gatewaywatch_test.go`, inside `TestGatewayToClusters` (after the `managed` fixture is built) — add an LB-mode cluster to the client and assert the map func does not panic:

```go
	lb := lbCluster("lb-edge", "default") // LoadBalancer mode: ExternalAccess.Gateway == nil
```

and include it in `WithObjects(referencer, otherGateway, managed, lb)`. The existing assertions (1 request for `shared-gw`, 0 for `nobody-refs-this`) must still hold — with the guard, the nil-gateway LB cluster is skipped, not dereferenced. (`lbCluster` is defined in `resources_loadbalancer_test.go`, same package.)

- [ ] **Step 10: Run it, confirm fail (panic)**

Run: `go test ./internal/controller/ -run TestGatewayToClusters 2>&1 | head -30`
Expected: FAIL — nil-pointer panic dereferencing `nc.Spec.ExternalAccess.Gateway.Mode` for the LB cluster.

- [ ] **Step 11: Add the nil-guard + the LB port synthesis**

In `nomadcluster_controller.go` `gatewayToClusters`, replace the loop body head:

```go
	for _, nc := range list.Items {
		gw := nc.Spec.ExternalAccess.Gateway
		if gw == nil || gw.Mode != nomadv1alpha1.GatewayModeExisting || gw.Ref == nil {
			continue
		}
		if gw.Ref.Name == obj.GetName() && gw.Ref.Namespace == obj.GetNamespace() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: nc.Namespace},
			})
		}
	}
```

In `config_render.go`, extend `rpcAdvertisePorts` to synthesize the single LB port (this is the joint fix that keeps `buildConfigMap` and the `renderConfig` hash off the nil gateway pointer in LB mode):

```go
func rpcAdvertisePorts(nc *nomadv1alpha1.NomadCluster) []int32 {
	if nc.Spec.ExternalAccess.Mode == nomadv1alpha1.ExternalAccessLoadBalancer {
		return []int32{portRPC}
	}
	return nc.Spec.ExternalAccess.Gateway.RPCPorts
}
```

- [ ] **Step 12: Write failing test for LB-mode port synthesis**

Add to `internal/controller/config_render_test.go`:

```go
func TestRpcAdvertisePortsLoadBalancerMode(t *testing.T) {
	nc := lbCluster("edge", "nomad-system")
	got := rpcAdvertisePorts(nc)
	if len(got) != 1 || got[0] != 4647 {
		t.Errorf("rpcAdvertisePorts(LB) = %v, want [4647]", got)
	}
	// renderConfig must not panic on a nil gateway block in LB mode, and its
	// hash must fold in the LB address (so an LB-IP change rolls the pods).
	_, h1 := renderConfig(nc, "203.0.113.7")
	_, h2 := renderConfig(nc, "203.0.113.9")
	if h1 == h2 {
		t.Error("hash unchanged when LB address changed")
	}
}
```

- [ ] **Step 13: Run the package suite green**

Run: `make test`
Expected: PASS — `TestGatewayToClusters` no longer panics, `TestRpcAdvertisePortsLoadBalancerMode` passes, all prior tests unchanged.

- [ ] **Step 14: Commit**

```bash
git add internal/controller/
git commit -m "feat(controller): LoadBalancer external-access building blocks

Add buildLoadBalancerService + ensureLoadBalancer (single type:
LoadBalancer Service, RPC 4647 + HTTP 4646, address from
status.loadBalancer.ingress), the <name>-lb name, the gatewayToClusters
nil-guard (skip LB-mode clusters whose gateway block is nil), and the
LB-mode rpcAdvertisePorts synthesis ([]int32{4647}) shared by
buildConfigMap and the renderConfig rollout hash. Not yet wired into
Reconcile.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Wire mode dispatch + partition Reconcile + LB envtest + runbook

Dispatch the address gate on `externalAccess.mode`, partition the step-3 provisioning block into shared / Gateway-only / LB-only, prove LoadBalancer mode end-to-end in envtest, and document it.

**Files:**
- Modify: `internal/controller/nomadcluster_controller.go` (`Reconcile`)
- Create: `internal/controller/resources_loadbalancer_envtest_test.go` (Ginkgo specs; the `_test.go` in the envtest suite)
- Modify: `docs/runbooks/nomadcluster.md`

**Interfaces:**
- Consumes: `ensureLoadBalancer`, `ensureGateway`, `buildLoadBalancerService`, `rpcAdvertisePorts`, `CondExternalAccessReady`, `ExternalAddress`, `lbCluster` fixture, `makeCertSecret`, `reconcileOnce`, `fakeNomad`, `newFakeFactory`.

- [ ] **Step 1: Write the failing LB reconcile envtest**

Create `internal/controller/resources_loadbalancer_envtest_test.go`:

```go
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

var _ = Describe("LoadBalancer external-access mode", func() {
	It("provisions an LB Service, gates on ingress, and reaches Ready with no Gateway objects", func() {
		ctx := context.Background()
		ns := "lbmode"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, "nomad-tls", ns)
		nc := lbCluster("edge", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		fake := &fakeNomad{leader: "203.0.113.7:4647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		// First reconcile creates the LB Service (no ingress yet) → Pending.
		reconcileOnce(r, "edge", ns)
		var svc corev1.Service
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).LBService, Namespace: ns}, &svc)).To(Succeed())
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))

		var afterFirst nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "edge", Namespace: ns}, &afterFirst)).To(Succeed())
		Expect(afterFirst.Status.Phase).To(Equal(nomadv1alpha1.PhasePending))

		// Stub the ingress address (envtest runs no LB provider).
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}
		Expect(k8s.Status().Update(ctx, &svc)).To(Succeed())

		// Second reconcile provisions the shared workloads and reaches Ready.
		reconcileOnce(r, "edge", ns)

		var afterSecond nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "edge", Namespace: ns}, &afterSecond)).To(Succeed())
		Expect(afterSecond.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))
		Expect(afterSecond.Status.ExternalAddress).To(Equal("203.0.113.7"))

		// Shared workloads exist; the ConfigMap advertises rpc_ports "4647".
		var ss appsv1.StatefulSet
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).StatefulSet, Namespace: ns}, &ss)).To(Succeed())
		var cm corev1.ConfigMap
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).ConfigMap, Namespace: ns}, &cm)).To(Succeed())
		Expect(cm.Data["rpc_ports"]).To(Equal("4647"))
		Expect(cm.Data["gateway_address"]).To(Equal("203.0.113.7"))

		// No Gateway-mode objects were created.
		var gw gwapiv1.Gateway
		errGw := k8s.Get(ctx, types.NamespacedName{Name: names(nc).Gateway, Namespace: ns}, &gw)
		Expect(errGw).To(HaveOccurred())
		var podSvc corev1.Service
		errPod := k8s.Get(ctx, types.NamespacedName{Name: names(nc).PodSvc(0), Namespace: ns}, &podSvc)
		Expect(errPod).To(HaveOccurred())
	})
})
```

- [ ] **Step 2: Run it, confirm fail**

Run: `make test 2>&1 | tail -30`
Expected: FAIL — the LB cluster reaches the Gateway path (or panics), because `Reconcile` doesn't dispatch on mode yet; `svc` (the LB Service) is never created.

- [ ] **Step 3: Dispatch on mode + partition step 3 in `Reconcile`**

In `nomadcluster_controller.go`, replace the section-2 gateway block (`:104-115`) with a mode dispatch:

```go
	// 2. External access: resolve the advertised address for the active mode.
	var extAddr string
	var extReady bool
	switch nc.Spec.ExternalAccess.Mode {
	case nomadv1alpha1.ExternalAccessLoadBalancer:
		extAddr, extReady, err = r.ensureLoadBalancer(ctx, &nc)
	default:
		extAddr, extReady, err = r.ensureGateway(ctx, &nc)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !extReady {
		nc.Status.Phase = nomadv1alpha1.PhasePending
		setCondition(&nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionFalse, "WaitingForAddress", "external address not assigned")
		return r.finish(ctx, &nc, ctrl.Result{RequeueAfter: requeueShort})
	}
	nc.Status.ExternalAddress = extAddr
	setCondition(&nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionTrue, "Assigned", "external address assigned")
```

Replace the step-3 block (`:117-146`) so the Gateway-only objects (per-pod Services + routes) are fenced behind Gateway mode; shared objects and the StatefulSet/PDB always apply. Rename `gwAddr` → `extAddr` in the `renderConfig`/`buildConfigMap`/`bootstrapAndReady` calls:

```go
	// 3. Render config + provision workloads.
	_, configHash := renderConfig(&nc, extAddr)
	if err := r.apply(ctx, &nc, buildConfigMap(&nc, extAddr)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, &nc, buildHeadlessService(&nc)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, &nc, buildAPIService(&nc)); err != nil {
		return ctrl.Result{}, err
	}
	// Gateway-only: per-pod RPC Services + routes. LoadBalancer mode selects the
	// server pod directly through the LB Service and needs none of these.
	if nc.Spec.ExternalAccess.Mode == nomadv1alpha1.ExternalAccessGateway {
		for ordinal := range int(nc.Spec.Servers) {
			if err := r.apply(ctx, &nc, buildPodService(&nc, ordinal)); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := r.apply(ctx, &nc, buildTLSRoute(&nc)); err != nil {
			return ctrl.Result{}, err
		}
		for _, rt := range buildTCPRoutes(&nc) {
			if err := r.apply(ctx, &nc, rt); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if err := r.apply(ctx, &nc, buildStatefulSet(&nc, configHash)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, &nc, buildPDB(&nc)); err != nil {
		return ctrl.Result{}, err
	}
```

Update the `bootstrapAndReady(ctx, &nc, gwAddr)` call (`:154`) → `bootstrapAndReady(ctx, &nc, extAddr)`.

- [ ] **Step 4: Run the LB envtest + full suite green**

Run: `make test`
Expected: PASS — the LB spec reaches Ready, `rpc_ports == "4647"`, no Gateway/per-pod-Service objects; all Gateway-mode specs still pass.

- [ ] **Step 5: Write the runbook section**

Append to `docs/runbooks/nomadcluster.md` a `## External access modes` section documenting:
- `spec.externalAccess.mode: Gateway | LoadBalancer`; Gateway supports servers 1/3/5, LoadBalancer requires servers: 1 (single-VIP, north-south only — no east-west Raft to serve).
- LoadBalancer mode provisions one `type: LoadBalancer` Service (`<name>-lb`, RPC 4647 + HTTP 4646); the cluster stays `Pending` until the LB provider assigns `status.loadBalancer.ingress` (same failure shape as a missing Gateway controller — requires a cloud LB / metallb / Cilium LBIPAM).
- **HTTP/UI over the LB needs `-tls-server-name server.<region>.nomad`** (or `NOMAD_TLS_SERVER_NAME`): Nomad RPC is role-verified so edge agents join over 4647 with no extra flag, but HTTP is verified against the dialed address and the LB address is not in the cert SANs. The operator's own in-cluster client is unaffected.
- `mode` is immutable; `servers` is immutable, so a 3/5 cluster can never be LoadBalancer.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/ docs/runbooks/nomadcluster.md
git commit -m "feat(controller): wire LoadBalancer mode into Reconcile

Dispatch the external-access address gate on externalAccess.mode
(Gateway=>ensureGateway, LoadBalancer=>ensureLoadBalancer) and partition
step-3 provisioning: shared workloads always apply; per-pod RPC Services
and TLS/TCP routes are fenced behind Gateway mode. LB-mode envtest:
servers:1 cluster applies the LB Service, stays Pending until
status.loadBalancer.ingress is stubbed, then reaches Ready with
advertise rpc_ports=4647 and no Gateway objects. Runbook documents the
mode + the -tls-server-name HTTP caveat.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review

**1. Spec coverage** (design §§ → task):
- §5.1 shape (union, gateway pointer, loadBalancer block) → T1 Step 3. ✅
- §5.2 all five CEL rules (mode enum+immutable, LB⇒servers==1, guarded rpcPorts==servers, union exclusivity both directions) → T1 Step 3 + tests Step 1. ✅
- §4 LoadBalancer Service (type, ports, class, annotations, address from `status.loadBalancer.ingress`) → T2 Steps 5-8. ✅
- §4 advertise reuse via synthesized `rpc_ports="4647"` → T2 Step 11 + T3 envtest asserts `rpc_ports=="4647"`. ✅
- §4 cert/HTTP `-tls-server-name` → T3 runbook Step 5. ✅
- §7 mode dispatch + step-3 partition + nil-guards + status rename + no generation predicate → T1 (status rename, re-home), T2 (nil-guard), T3 (dispatch/partition). `SetupWithManager` already `Owns(&corev1.Service{})` with no predicate — untouched, so LB-ingress reactivity holds (no step needed; do NOT add a predicate). ✅
- §8 migration (types, regen, read-sites, fixtures) → T1 Steps 3-7 (incl. the exhaustive `grep Spec.Gateway` safeguard) + TLSSpec SAN comment fix in Step 3. ✅
- §9 testing (CEL cases, LB reconcile envtest, buildLoadBalancerService unit, crash-loop guard, regression) → T1 Step 1, T2 Steps 5/9/12, T3 Step 1. ✅
- §11 deferred typed LB fields — intentionally NOT built (YAGNI); documented in design, no task. ✅

**2. Placeholder scan:** No TBD/TODO/"handle edge cases"/"similar to Task N"; every code step shows full code; every test step shows the assertion. ✅

**3. Type consistency:** `ExternalAccessSpec`/`ExternalAccessMode`/`ExternalAccessGateway`/`ExternalAccessLoadBalancer`/`LoadBalancerSpec`/`ExternalAddress`/`CondExternalAccessReady`/`rpcAdvertisePorts`/`buildLoadBalancerService`/`ensureLoadBalancer`/`names(nc).LBService`/`lbCluster` are named identically across T1→T3. `ensureLoadBalancer` returns `(string, bool, error)` matching `ensureGateway`'s signature so the `Reconcile` dispatch assigns both from the same `extAddr, extReady, err`. ✅
