# NomadCluster Restart Resilience Implementation Plan

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

**Goal:** Make a `NomadCluster` server-pod restart a well-understood, instrumented event — add an operator-side `advertise.rpc` drift guard, replace the fabricated `status.quorum` with real quorum + `status.members`, and document the verified restart behavior + a recovery runbook.

**Architecture:** Three independent deliverables on the existing slice-2 `NomadCluster` reconciler. (C) A new read method `internal/nomad.Client.ServerHealth` wrapping `Operator().AutopilotServerHealth` feeds real `status.members`/`status.quorum` in `bootstrapAndReady`, reusing the shipped-but-unpopulated `MemberStatus` type. (B) A pure-operator drift check in `Reconcile` compares the freshly-resolved external address to the last-persisted one and raises a config-aware `RaftAddressDrift` Condition + Event. (A) Docs.

**Tech Stack:** Go 1.26.4, kubebuilder v4, controller-runtime v0.23.3, k8s v0.35.0, Ginkgo/Gomega envtest, pinned `github.com/hashicorp/nomad/api` `v0.0.0-20260707172059-5b83b133998a` (== v2.0.4).

## Global Constraints

- **Design of record:** `docs/development/designs/2026-07-17-nomadcluster-restart-resilience-design.md` (SGE-Opus reviewed, amend-before-planning findings folded). This plan implements it verbatim.
- **No networking/advertise change.** `advertise.rpc` wiring, Services, Gateway/LB topology, per-ordinal ports, PDB, anti-affinity, and the Pending→Bootstrapping→Ready→Degraded phase machine are untouched.
- **REJECTED (do not build):** per-pod ClusterIP advertise; automatic `peers.json` recovery.
- **`contract.go` discipline:** every new `api` symbol used is pinned in `internal/nomad/contract.go`, backed by a real call (existence-only-pin rule).
- **No `api.*` types leak past `internal/nomad`.** The controller consumes the projected `nomad.NomadMember`, never `*api.ServerHealth`.
- **Gate per task:** `make manifests generate fmt vet && make test` green; zero regen drift; `go vet -tags integration ./...` clean.
- **Signed commits** need 1Password Touch ID; on a `failed to fill whole buffer` error STOP and ask — never disable `commit.gpgsign`.
- **Repo is LOCAL-ONLY** — do not push.

> **Amended 2026-07-17** after an independent sr-go-engineer *plan* review (Opus), verdict *amend-before-execution*, 1 blocking. Verified SOUND and not re-litigated: API correctness against the pin (`AutopilotServerHealth` arity/fields, contract-pin style), every edit anchor (`NomadOps` 47-52, struct 63-67, Reconcile overwrite 121-122, leader gate 200-215, Degraded-only-on-leader-loss), `NomadOps` isolation (only `fakeNomad` implements it; the other four fakes are separate interfaces, so adding `ServerHealth` breaks nothing), no existing test asserts the old `"3/3"` quorum, `record.FakeRecorder` event formatting matches the substring assertions, gate commands, design fidelity, and both rejections. Folded: **B1** — `driftTo` used the wrong Gateway name `name+"-gw"`; the real name is `names(nc).Gateway == nc.Name+"-gateway"` (names.go:37) → fixed in Task 4 Step 1. **M1** — Task 3 Step 4 now also deletes the stale `nomadcluster_controller.go:210-213` "deferred to slice 6" comment that C makes false. **M2** — Task 1's projection test goes in a NEW file `internal/nomad/client_projection_test.go` to avoid import-merge churn with the existing `client_test.go`.

---

## File Structure

- `internal/nomad/client.go` — **modify**: add `NomadMember` type + `ServerHealth` method (Task 1).
- `internal/nomad/contract.go` — **modify**: pin `Operator`, `AutopilotServerHealth`, `OperatorHealthReply`, `ServerHealth` (Task 1).
- `internal/controller/nomadcluster_controller.go` — **modify**: `NomadOps` gains `ServerHealth` (Task 1); `bootstrapAndReady` populates members/quorum (Task 3); `Reconcile` + new `checkAddressDrift` (Task 4); `Recorder` field + `SetupWithManager` wiring (Task 4).
- `internal/nomad/client_projection_test.go` — **create**: unit test for the `toMembers` projection (Task 1).
- `internal/controller/fake_nomad_test.go` — **modify**: fake `ServerHealth` (Task 1).
- `api/v1alpha1/nomadcluster_types.go` — **modify**: `MemberStatus.Voter` field (Task 2); `CondRaftAddressDrift` constant (Task 4).
- `internal/controller/status_members.go` — **create**: `toMemberStatus` + `quorumString` helpers (Task 3).
- `internal/controller/nomadcluster_controller_test.go` — **modify**: members/quorum spec (Task 3), drift-guard specs (Task 4).
- `config/crd/bases/nomad.operator.io_nomadclusters.yaml`, `api/v1alpha1/zzz_generated.deepcopy.go` — **regenerated** (Task 2).
- `docs/runbooks/nomadcluster.md`, `docs/development/issues/known-issues.md` — **modify** (Task 5).

---

### Task 1: `internal/nomad.ServerHealth` + `NomadMember` + `NomadOps` method

**Files:**
- Modify: `internal/nomad/client.go`
- Modify: `internal/nomad/contract.go`
- Modify: `internal/controller/nomadcluster_controller.go:47-52` (the `NomadOps` interface)
- Modify: `internal/controller/fake_nomad_test.go`
- Create: `internal/nomad/client_projection_test.go` (new file — avoids import-merge churn with the existing `client_test.go`), plus the contract pin compiles

**Interfaces:**
- Produces: `nomad.NomadMember{Name, Addr, Status string; Leader, Voter bool}` and `nomad.Client.ServerHealth(ctx context.Context) ([]nomad.NomadMember, error)`; `NomadOps.ServerHealth(ctx) ([]nomad.NomadMember, error)`; `fakeNomad.ServerHealth`. Task 3 consumes these.

- [ ] **Step 1: Write the failing test** for the projection mapping in a NEW file `internal/nomad/client_projection_test.go` (the existing `client_test.go` already imports `testing`+`api` and defines an httptest `fakeNomad` helper — a new file sidesteps any import-merge collision).

There is no live Nomad in unit tests, so test the pure projection helper. First extract the mapping into a package-level function so it is unit-testable without a server. Write the test:

```go
package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/api"
)

func TestToMembers_MapsServerHealthFields(t *testing.T) {
	in := []api.ServerHealth{
		{Name: "srv-0", Address: "10.0.0.5:14647", SerfStatus: "alive", Leader: true, Voter: true},
		{Name: "srv-1", Address: "10.0.0.6:24647", SerfStatus: "failed", Leader: false, Voter: false},
	}
	got := toMembers(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0] != (NomadMember{Name: "srv-0", Addr: "10.0.0.5:14647", Status: "alive", Leader: true, Voter: true}) {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1] != (NomadMember{Name: "srv-1", Addr: "10.0.0.6:24647", Status: "failed", Leader: false, Voter: false}) {
		t.Errorf("got[1] = %+v", got[1])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/nomad/ -run TestToMembers -v`
Expected: FAIL — `undefined: toMembers` / `undefined: NomadMember`.

- [ ] **Step 3: Add the type, the mapping helper, and the method** in `internal/nomad/client.go` (place after `ServerHealthy`, ~line 120):

```go
// NomadMember is the operator's projection of a Nomad server's autopilot health,
// decoupled from *api.ServerHealth so no api type leaks past this package.
type NomadMember struct {
	Name   string // ServerHealth.Name (node name)
	Addr   string // ServerHealth.Address (advertise.rpc, "ip:port")
	Status string // ServerHealth.SerfStatus ("alive"/"failed"/"left")
	Leader bool   // ServerHealth.Leader
	Voter  bool   // ServerHealth.Voter
}

func toMembers(in []api.ServerHealth) []NomadMember {
	out := make([]NomadMember, 0, len(in))
	for _, s := range in {
		out = append(out, NomadMember{
			Name:   s.Name,
			Addr:   s.Address,
			Status: s.SerfStatus,
			Leader: s.Leader,
			Voter:  s.Voter,
		})
	}
	return out
}

// ServerHealth returns the per-server autopilot health for the cluster's
// servers (name, advertise.rpc address, serf status, leader, voter). It reads
// Operator().AutopilotServerHealth (GET /v1/operator/autopilot/health), an
// operator endpoint that is not namespace-scoped.
func (c *Client) ServerHealth(ctx context.Context) ([]NomadMember, error) {
	reply, _, err := c.api.Operator().AutopilotServerHealth(queryOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("nomad: autopilot server health: %w", err)
	}
	return toMembers(reply.Servers), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/nomad/ -run TestToMembers -v`
Expected: PASS.

- [ ] **Step 5: Pin the new api symbols** in `internal/nomad/contract.go`.

Add to the type-pins `var (...)` block (after `_ api.JobListStub`):

```go
	_ api.OperatorHealthReply
	_ api.ServerHealth
```

Add to the method-pins `var (...)` block (after `_ = (*api.Jobs).Summary`):

```go
	_ = (*api.Client).Operator
	_ = (*api.Operator).AutopilotServerHealth
```

- [ ] **Step 6: Add `ServerHealth` to the `NomadOps` interface** in `internal/controller/nomadcluster_controller.go` (the interface at lines 47-52):

```go
type NomadOps interface {
	Ping(ctx context.Context) error
	Leader(ctx context.Context) (string, error)
	ServerHealthy(ctx context.Context) (bool, error)
	ACLBootstrap(ctx context.Context, bootstrapToken string) (string, error)
	ServerHealth(ctx context.Context) ([]nomad.NomadMember, error)
}
```

(`nomad` is already imported in this file.)

- [ ] **Step 7: Implement the fake** in `internal/controller/fake_nomad_test.go`. Add fields to `fakeNomad` and a method:

```go
	// members/memberErr drive ServerHealth for status.members/quorum tests.
	members   []nomad.NomadMember
	memberErr error
```

```go
func (f *fakeNomad) ServerHealth(context.Context) ([]nomad.NomadMember, error) {
	return f.members, f.memberErr
}
```

(`nomad` is already imported in this file.)

- [ ] **Step 8: Verify the whole tree builds and existing tests pass**

Run: `go build ./... && make test`
Expected: build clean (`*nomad.Client` and `fakeNomad` both satisfy `NomadOps`); existing suites PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/nomad/client.go internal/nomad/client_projection_test.go internal/nomad/contract.go internal/controller/nomadcluster_controller.go internal/controller/fake_nomad_test.go
git commit -m "feat(nomad): ServerHealth read + NomadMember projection + NomadOps method

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: CRD — add `MemberStatus.Voter`

**Files:**
- Modify: `api/v1alpha1/nomadcluster_types.go:182-188` (the `MemberStatus` struct)
- Regenerate: `config/crd/bases/nomad.operator.io_nomadclusters.yaml`, `api/v1alpha1/zzz_generated.deepcopy.go`

**Interfaces:**
- Produces: `MemberStatus.Voter bool` (json `voter`). Task 3 sets it.

- [ ] **Step 1: Add the field** to `MemberStatus` in `api/v1alpha1/nomadcluster_types.go` (existing struct at line 182):

```go
// MemberStatus reports the observed state of a single Nomad server member.
type MemberStatus struct {
	Name   string `json:"name"`
	Addr   string `json:"addr"`
	Status string `json:"status"`
	Leader bool   `json:"leader"`
	// Voter reports whether this server is a raft voter.
	// +optional
	Voter bool `json:"voter,omitempty"`
}
```

- [ ] **Step 2: Regenerate manifests + deepcopy**

Run: `make manifests generate`
Expected: `config/crd/bases/nomad.operator.io_nomadclusters.yaml` now shows a `voter` boolean under `status.members.items.properties`; `zzz_generated.deepcopy.go` unchanged in shape (bool needs no deep copy). No other diffs.

- [ ] **Step 3: Verify build + no stray drift**

Run: `make manifests generate fmt vet && git status --porcelain`
Expected: only the two regenerated files + the types file are modified; re-running `make manifests generate` produces no further diff.

- [ ] **Step 4: Commit**

```bash
git add api/v1alpha1/nomadcluster_types.go config/crd/bases/nomad.operator.io_nomadclusters.yaml api/v1alpha1/zzz_generated.deepcopy.go
git commit -m "feat(api): add MemberStatus.Voter to NomadCluster status

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Populate real `status.members` + `status.quorum`

**Files:**
- Create: `internal/controller/status_members.go`
- Modify: `internal/controller/nomadcluster_controller.go` (`bootstrapAndReady`, ~lines 210-215; add `log` import)
- Test: `internal/controller/nomadcluster_controller_test.go`

**Interfaces:**
- Consumes: `NomadOps.ServerHealth` (Task 1), `MemberStatus.Voter` (Task 2).
- Produces: `toMemberStatus([]nomad.NomadMember) []nomadv1alpha1.MemberStatus`, `quorumString([]nomad.NomadMember) string`.

- [ ] **Step 1: Write the failing tests** in `internal/controller/nomadcluster_controller_test.go`. Add a new `It` inside the `Describe("Managed provisioning", ...)` block. It drives the existing Ready path but seeds the fake's `members` and asserts real status:

```go
	It("populates status.members and a real status.quorum from ServerHealth once Ready", func() {
		ctx := context.Background()
		ns := "mgd-members"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, "nomad-tls", ns)
		nc := minimalCluster("members", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		fake := &fakeNomad{
			leader:        "10.0.0.5:14647",
			serverHealthy: true,
			members: []nomad.NomadMember{
				{Name: "members-server-0", Addr: "10.0.0.5:14647", Status: "alive", Leader: true, Voter: true},
				{Name: "members-server-1", Addr: "10.0.0.6:24647", Status: "alive", Leader: false, Voter: true},
				{Name: "members-server-2", Addr: "10.0.0.7:34647", Status: "failed", Leader: false, Voter: false},
			},
		}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		reconcileOnce(r, "members", ns)
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).Gateway, Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
		reconcileOnce(r, "members", ns)

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "members", Namespace: ns}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))
		Expect(got.Status.Quorum).To(Equal("2/3")) // 2 voters of 3 members
		Expect(got.Status.Members).To(HaveLen(3))
		Expect(got.Status.Members[0]).To(Equal(nomadv1alpha1.MemberStatus{
			Name: "members-server-0", Addr: "10.0.0.5:14647", Status: "alive", Leader: true, Voter: true,
		}))
		Expect(got.Status.Members[2].Status).To(Equal("failed"))
		Expect(got.Status.Members[2].Voter).To(BeFalse())
	})

	It("does not flip Ready->Degraded when ServerHealth errors", func() {
		ctx := context.Background()
		ns := "mgd-members-err"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, "nomad-tls", ns)
		nc := minimalCluster("memberr", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true, memberErr: errors.New("transient: health read")}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		reconcileOnce(r, "memberr", ns)
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).Gateway, Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
		reconcileOnce(r, "memberr", ns)

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "memberr", Namespace: ns}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady)) // health-read error must NOT degrade
	})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `make test` (or the focused Ginkgo run)
Expected: FAIL — the first spec sees the fabricated `"3/3"` quorum and empty `Members`; second spec may already pass but must be kept.

- [ ] **Step 3: Add the helpers** in a new file `internal/controller/status_members.go`:

```go
package controller

import (
	"fmt"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// toMemberStatus projects the Nomad server health into the CRD status shape.
func toMemberStatus(members []nomad.NomadMember) []nomadv1alpha1.MemberStatus {
	out := make([]nomadv1alpha1.MemberStatus, 0, len(members))
	for _, m := range members {
		out = append(out, nomadv1alpha1.MemberStatus{
			Name:   m.Name,
			Addr:   m.Addr,
			Status: m.Status,
			Leader: m.Leader,
			Voter:  m.Voter,
		})
	}
	return out
}

// quorumString reports "<voters>/<total>" from the observed member set.
func quorumString(members []nomad.NomadMember) string {
	voters := 0
	for _, m := range members {
		if m.Voter {
			voters++
		}
	}
	return fmt.Sprintf("%d/%d", voters, len(members))
}
```

- [ ] **Step 4: Populate status in `bootstrapAndReady`.** In `internal/controller/nomadcluster_controller.go`, replace the stale slice-6 deferral comment AND the fabricated quorum line. The current code (~lines 210-215) reads:

```go
	// status.leader carries the raw "ip:port" from Status().Leader(). Mapping it
	// to a friendly "<name>-server-N.<region>" and populating status.members from
	// Status().Peers() are DEFERRED to slice 6 (hardening) — noted so they are not
	// silently dropped; the DoD only requires leader/quorum be populated.
	nc.Status.Leader = leader
	nc.Status.Quorum = fmt.Sprintf("%d/%d", nc.Spec.Servers, nc.Spec.Servers)
```

Replace that whole block (comment + both assignments) with (the read is placed AFTER the leader gate at line 209 — per design M-1; the deferral comment is now false and must go — review M1):

```go
	// status.leader carries the raw "ip:port" from Status().Leader().
	nc.Status.Leader = leader
	// Real quorum + members from autopilot health. Placed after the leader
	// gate; a read error must not flip Ready->Degraded (Degraded is entered
	// only on leader-loss above), so on error we log and keep prior status.
	if members, err := ops.ServerHealth(ctx); err != nil {
		log.FromContext(ctx).Error(err, "reading server health for status.members; keeping prior status")
	} else if len(members) > 0 {
		nc.Status.Members = toMemberStatus(members)
		nc.Status.Quorum = quorumString(members)
	}
```

Add the import `"sigs.k8s.io/controller-runtime/pkg/log"` to the file's import block. (`fmt` is already imported and still used elsewhere.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `make test`
Expected: both new specs PASS; all existing specs still PASS (the pre-existing Ready specs use `fakeNomad` with no `members`, so `len(members)==0` leaves their `Quorum` at the last value — verify none asserted the old `"3/3"`; if one did, update it to not assert quorum or seed members).

- [ ] **Step 6: Verify full gate**

Run: `make manifests generate fmt vet && make test && go vet -tags integration ./...`
Expected: all green, no drift.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/status_members.go internal/controller/nomadcluster_controller.go internal/controller/nomadcluster_controller_test.go
git commit -m "feat(nomadcluster): real status.quorum + status.members from autopilot health

Replaces the fabricated N/N quorum with voters/total read from
Operator().AutopilotServerHealth after the leader gate; a health-read error
logs and keeps prior status (never degrades Ready). Closes known-issues #1.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: `advertise.rpc` drift guard

**Files:**
- Modify: `api/v1alpha1/nomadcluster_types.go:45-52` (add `CondRaftAddressDrift`)
- Modify: `internal/controller/nomadcluster_controller.go` (`Recorder` field; `SetupWithManager`; `Reconcile` capture + call; new `checkAddressDrift`)
- Test: `internal/controller/nomadcluster_controller_test.go`

**Interfaces:**
- Consumes: `setCondition`, `metav1ConditionTrue/False`, phase/reason constants.
- Produces: `CondRaftAddressDrift` condition type; `checkAddressDrift(nc, prev, cur string)`.

- [ ] **Step 1: Write failing specs** in `internal/controller/nomadcluster_controller_test.go`. Add a new `Describe`:

```go
var _ = Describe("advertise.rpc drift guard", func() {
	// driveToReady runs the two-reconcile Managed path to Ready at address A,
	// returning the reconciler (with a fake recorder) for a follow-up drift.
	driveToReady := func(ctx context.Context, name, ns, addrA string, servers int32, rpcPorts []int32) (*NomadClusterReconciler, *record.FakeRecorder) {
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, "nomad-tls", ns)
		nc := minimalCluster(name, ns)
		nc.Spec.Servers = servers
		nc.Spec.ExternalAccess.Gateway.RPCPorts = rpcPorts
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		rec := record.NewFakeRecorder(10)
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake), Recorder: rec}

		reconcileOnce(r, name, ns)
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).Gateway, Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: addrA}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
		reconcileOnce(r, name, ns)

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))
		return r, rec
	}

	driftTo := func(ctx context.Context, name, ns, addrB string, r *NomadClusterReconciler) nomadv1alpha1.NomadCluster {
		var gw gwapiv1.Gateway
		// names(nc).Gateway == nc.Name + "-gateway" (internal/controller/names.go:37).
		Expect(k8s.Get(ctx, types.NamespacedName{Name: name + "-gateway", Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: addrB}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
		reconcileOnce(r, name, ns)
		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got)).To(Succeed())
		return got
	}

	It("does not fire on a stable address (no drift)", func() {
		ctx := context.Background()
		r, rec := driveToReady(ctx, "stable", "drift-stable", "10.0.0.5", 3, []int32{14647, 24647, 34647})
		got := driftTo(ctx, "stable", "drift-stable", "10.0.0.5", r) // same address
		Expect(meta_IsStatusConditionFalse(got.Status.Conditions, nomadv1alpha1.CondRaftAddressDrift)).To(BeTrue())
		Consistently(rec.Events).ShouldNot(Receive())
	})

	It("raises a Warning + True condition on servers:1 drift while Ready", func() {
		ctx := context.Background()
		r, rec := driveToReady(ctx, "single", "drift-single", "10.0.0.5", 1, []int32{14647})
		got := driftTo(ctx, "single", "drift-single", "10.0.0.9", r)
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondRaftAddressDrift)).To(BeTrue())
		var ev string
		Eventually(rec.Events).Should(Receive(&ev))
		Expect(ev).To(ContainSubstring("Warning"))
		Expect(ev).To(ContainSubstring("wedge"))
	})

	It("raises a Normal (self-heal) event + True condition on HA drift", func() {
		ctx := context.Background()
		r, rec := driveToReady(ctx, "hadrift", "drift-ha", "10.0.0.5", 3, []int32{14647, 24647, 34647})
		got := driftTo(ctx, "hadrift", "drift-ha", "10.0.0.9", r)
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondRaftAddressDrift)).To(BeTrue())
		var ev string
		Eventually(rec.Events).Should(Receive(&ev))
		Expect(ev).To(ContainSubstring("Normal"))
		Expect(ev).To(ContainSubstring("self-heal"))
	})
})
```

Add `"k8s.io/client-go/tools/record"` to the test file's imports. `meta_IsStatusConditionTrue` already exists (used at line 73).

- [ ] **Step 2: Run specs to verify they fail**

Run: `make test`
Expected: FAIL — `Recorder` field undefined on `NomadClusterReconciler`; `CondRaftAddressDrift` undefined.

- [ ] **Step 3: Add the condition constant** in `api/v1alpha1/nomadcluster_types.go` (the `Condition types` const block at line 46):

```go
// Condition types.
const (
	CondReconciled          = "Reconciled"
	CondExternalAccessReady = "ExternalAccessReady"
	CondQuorumHealthy       = "QuorumHealthy"
	CondACLBootstrapped     = "ACLBootstrapped"
	CondReady               = "Ready"
	CondRaftAddressDrift    = "RaftAddressDrift"
)
```

- [ ] **Step 4: Add the `Recorder` field + wiring** in `internal/controller/nomadcluster_controller.go`. Extend the struct (lines 62-67):

```go
type NomadClusterReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NewNomadClient NomadClientFactory
	Recorder       record.EventRecorder
}
```

Add the import `"k8s.io/client-go/tools/record"`. In `SetupWithManager`, after the `NewNomadClient` nil-guard (mirrors `nomadpool_controller.go:294-296`):

```go
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("nomadcluster")
	}
```

- [ ] **Step 5: Capture prevAddr and call the guard** in `Reconcile`. The current lines 121-122:

```go
	nc.Status.ExternalAddress = extAddr
	setCondition(&nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionTrue, "Assigned", "external address assigned")
```

become:

```go
	prevAddr := nc.Status.ExternalAddress
	nc.Status.ExternalAddress = extAddr
	setCondition(&nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionTrue, "Assigned", "external address assigned")
	r.checkAddressDrift(&nc, prevAddr, extAddr)
```

- [ ] **Step 6: Implement `checkAddressDrift`** (add near the bottom of `internal/controller/nomadcluster_controller.go`):

```go
// checkAddressDrift surfaces a change in the resolved external advertise
// address. Raft integrity depends on a stable advertise.rpc: at servers:1 a
// drift wedges the single-node raft (it cannot remove its sole voter) and needs
// manual recovery; HA self-heals via autopilot. This DETECTS and reports only —
// it does not block the ensuing StatefulSet roll (if the external address truly
// changed, the old one is dead regardless). The Condition is momentary (True on
// the reconcile that observes the change, else Stable); the emitted Event is the
// durable record. The Warning is gated on Ready (design M-3): a drift during the
// first post-bootstrap roll still sets the Condition but at Normal severity.
func (r *NomadClusterReconciler) checkAddressDrift(nc *nomadv1alpha1.NomadCluster, prev, cur string) {
	if prev == "" || prev == cur {
		setCondition(nc, nomadv1alpha1.CondRaftAddressDrift, metav1ConditionFalse, "Stable", "external advertise address stable")
		return
	}
	msg := fmt.Sprintf("external advertise address changed %s -> %s", prev, cur)
	if nc.Spec.Servers == 1 {
		setCondition(nc, nomadv1alpha1.CondRaftAddressDrift, metav1ConditionTrue, "AddressChanged",
			msg+"; single-node raft will wedge on the ensuing roll - see the restart-resilience recovery runbook")
		if nc.Status.Phase == nomadv1alpha1.PhaseReady && r.Recorder != nil {
			r.Recorder.Event(nc, "Warning", nomadv1alpha1.CondRaftAddressDrift, msg+"; servers:1 raft will wedge - see recovery runbook")
		}
		return
	}
	setCondition(nc, nomadv1alpha1.CondRaftAddressDrift, metav1ConditionTrue, "AddressChangedHA",
		msg+"; HA autopilot will self-heal")
	if r.Recorder != nil {
		r.Recorder.Event(nc, "Normal", nomadv1alpha1.CondRaftAddressDrift, msg+"; HA autopilot will self-heal")
	}
}
```

Note: `record.FakeRecorder` formats events as `"<Type> <Reason> <Message>"`, so the specs' `ContainSubstring("Warning")` / `"Normal"` / `"wedge"` / `"self-heal"` match.

- [ ] **Step 7: Run specs to verify they pass**

Run: `make test`
Expected: the three drift specs PASS; all existing specs still PASS.

- [ ] **Step 8: Full gate**

Run: `make manifests generate fmt vet && make test && go vet -tags integration ./...`
Expected: green, no drift. (No CRD change in this task — the condition is a freeform `status.conditions` entry.)

- [ ] **Step 9: Commit**

```bash
git add api/v1alpha1/nomadcluster_types.go internal/controller/nomadcluster_controller.go internal/controller/nomadcluster_controller_test.go
git commit -m "feat(nomadcluster): advertise.rpc drift guard (RaftAddressDrift condition + event)

Operator-side detection: compares the freshly-resolved external address to the
last-persisted one. On drift raises a config-aware RaftAddressDrift condition +
event (Warning/servers:1 gated on Ready, Normal/HA self-heals). No Nomad call;
does not block the roll. Adds an EventRecorder to the reconciler.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Documentation — restart resilience section, recovery runbook, known-issues correction

**Files:**
- Modify: `docs/runbooks/nomadcluster.md` (new section after §8)
- Modify: `docs/development/issues/known-issues.md` (§1 → resolved; correct the restart-artifact record)

**Interfaces:** none (docs). References the `RaftAddressDrift` condition (Task 4) and real `status.quorum`/`members` (Task 3).

- [ ] **Step 1: Add the runbook section.** Append to `docs/runbooks/nomadcluster.md` a new top-level section `## 9. Restart resilience & raft address stability` containing, verbatim in prose:
  - The verified truth table from the design §2.3 (servers:1 stable=self-heal / drift=wedge; servers:3 stable=self-heal / drift=autopilot self-heal).
  - The invariant: *raft integrity depends on a stable external `advertise.rpc`; the operator supplies one from the Gateway/LB address, so a normal pod restart self-heals. A drifting external address wedges `servers:1` because raft cannot remove its sole voter; HA self-heals via autopilot.*
  - How to recognize the wedge: the `RaftAddressDrift` Condition (note: momentary — True on the reconcile that observes the change; the **Warning Event** in `kubectl describe nomadcluster`/`kubectl get events` is the durable record, and per design M-3 a drift during the first post-bootstrap roll surfaces the Condition at Normal severity, so check the Condition/Event, not only Warning severity), the `need at least one voter in configuration` server log, and `nomad operator raft list-peers` showing a stale address in `follower` state on the leader process.
  - **Recovery (servers:1 + drift wedge only)**, two paths:
    - Preserve state — write `/var/lib/nomad/server/raft/peers.json` on the pod with the current node-id + the new advertised `ip:port`, then restart the agent (Nomad consumes and deletes `peers.json` on boot). Include an example `peers.json` array literal.
    - Simplest (dev-acceptable) — delete the server pod's `data-<sts>-0` PVC and let the StatefulSet re-bootstrap a clean single-node raft at the new address (loses Nomad state).
    - Guidance: prefer `servers: 3` for any cluster where external-address drift is plausible; HA self-heals.

- [ ] **Step 2: Correct `docs/development/issues/known-issues.md`.**
  - §1 (`status.quorum is fabricated N/N, not measured`): mark **Resolved (2026-07-17, slice 6b)** — `status.quorum` is now `voters/total` and `status.members` is populated from `Operator().AutopilotServerHealth` in `bootstrapAndReady`.
  - Add a short correction note (near the FR-1 / restart discussion): the earlier "servers:1 does not survive a server-pod restart" observation was a **bare-kind harness artifact** — the e2e manually patched a non-durable fake LB ingress IP that changed across the restart. With a real LB/Gateway the advertised address is stable and a restart self-heals; the genuine, narrow failure mode is `servers: 1` + external-address **drift**, covered by the runbook §9 recovery + the `RaftAddressDrift` guard.

- [ ] **Step 3: Verify no code/build impact**

Run: `git status --porcelain` (only the two docs files changed) and `make test` (unchanged — sanity).
Expected: docs-only diff; tests still green.

- [ ] **Step 4: Commit**

```bash
git add docs/runbooks/nomadcluster.md docs/development/issues/known-issues.md
git commit -m "docs: restart-resilience runbook + recovery + known-issues corrections

Adds runbook section 9 (verified restart truth table, RaftAddressDrift
recognition, servers:1 drift recovery). Marks known-issues #1 resolved and
corrects the servers:1 restart record (bare-kind harness artifact, not a
shipping defect).

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:**
- Design §4 (docs + runbook + known-issues) → Task 5. ✓
- Design §5 (drift guard: prevAddr capture, config-aware Condition+Event, Ready-gated Warning, no block, EventRecorder) → Task 4. ✓
- Design §6 (reuse `MemberStatus` + `AutopilotServerHealth`, add `Voter`, read after leader gate, error→log-not-Degraded, real quorum) → Tasks 1 (client), 2 (CRD field), 3 (populate). ✓
- Design §3 non-goals (no ClusterIP advertise, no auto-recovery) → Global Constraints. ✓
- Design §9 S-1 (contract pin backed by real call) → Task 1 Step 5 + the integration `contract.go` gate. ✓

**Placeholder scan:** every code step carries complete code; the only prose-described step is Task 5 (docs content), which is itemized point-by-point. No TBD/TODO. ✓

**Type consistency:** `ServerHealth(ctx) ([]nomad.NomadMember, error)` is identical in the real client (T1), the `NomadOps` interface (T1), and the fake (T1); `NomadMember{Name,Addr,Status,Leader,Voter}` (T1) maps 1:1 to `MemberStatus{Name,Addr,Status,Leader,Voter}` (T2) via `toMemberStatus` (T3). `CondRaftAddressDrift` defined in T4 and used only in T4/T5. `quorumString`/`toMemberStatus` defined and used in T3. ✓

**Ordering/deps:** T1, T2 independent; T3 depends on T1+T2; T4 independent of T1-3 (same file, ordered after T3); T5 depends on T3+T4. Seed TodoWrite in this order.
