# Slice 6c — Hardening & Backlog Close-Out — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

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

**Goal:** Drive every deferred item accumulated across slices 2–6b to a terminal disposition (fix / test / documented won't-fix), and prove the full live integration suite green — clearing the deferred backlog so the repository can be published.

**Architecture:** Localized reconciler fixes across three CRDs (NomadNode, NomadCluster), plus test-gap backfill, a batched cleanup, and documentation. No new CRDs, no new spec surface, no new Nomad API dependency. Source of truth: `docs/development/designs/2026-07-17-slice-6c-hardening-design.md` (SGE-amended).

**Tech Stack:** Go 1.26, controller-runtime v0.23.3, kubebuilder v4, k8s v0.35.0, HashiCorp Nomad `api` pinned `v0.0.0-20260707172059-5b83b133998a` (== v2.0.4). Tests: Ginkgo v2 + Gomega on envtest.

> **Amended 2026-07-17** after an independent `sr-go-engineer` plan review (Fable), verdict *amend-before-execution* (no BLOCK). All production fixes and RED claims verified sound against `main`. Folded three compile-blocking test-snippet corrections: **T9/T11** — `driveToReady`/`driftTo` were local closures inside the drift-guard `Describe`, now hoisted to package scope (T9 Step 1); **T10** — the Existing-mode `Ref` type is `*GatewayRef` (pointer), not `GatewayReference`; **T8/D3** — `resources_workload_test.go` is a plain `testing.T` file, so the gossip-mount test is a `func TestGossipMountedOnlyOnInitContainer(t *testing.T)`, not a bare Ginkgo `It`. Hard-coded the two confirm-then-write facts: `r.apply` = SSA `Patch` on the ConfigMap first (T11 wrapper correct); `certSecretReady` returns `(false, nil)` on an incomplete Secret (T9 trigger correct). Minors: `for range 2`, explicit `client` import.

## Global Constraints

- **Package layout:** controller code is `package controller`; Nomad client code is `package nomad`; API types are `package v1alpha1`. Tests are **white-box** (same package as the code under test).
- **Test framework:** Ginkgo v2 + Gomega (`. "github.com/onsi/ginkgo/v2"`, `. "github.com/onsi/gomega"`) on **envtest** (real API server via the package-level `k8s client.Client` from `suite_test.go`). No testify. Specs use `func(ctx SpecContext)` or `context.Background()` per the surrounding file's convention.
- **No `api.*` type leaks past `internal/nomad`.** The controller consumes projections (e.g. `nomad.NomadMember`), never raw `*api.*` return types beyond the client boundary.
- **`contract.go` existence-pin discipline.** No 6c task adds a new Nomad API dependency, so `internal/nomad/contract.go` is **not** edited. Do not remove any pin.
- **Zero regen drift.** `make manifests generate` MUST produce **no** diff (no CRD schema change in 6c — `MemberStatus.Voter` already shipped in 6b). Any diff is a bug.
- **Build gate for every code task:** `make manifests generate fmt vet && make test`, plus `go vet -tags integration ./...` clean.
- **Reason strings** in the NomadCluster reconciler are free-form string literals (e.g. `"Assigned"`, `"WaitingForCert"`), NOT named consts — match that convention (do not introduce `Reason*` consts for NomadCluster).
- **Commits:** signed; trailer `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`. Repo is **local-only** — never push, open a PR, or create a remote.

---

## Task 1: A1 — full live `make test-integration` baseline

*Verification, no code.* Establishes the last open Foundation item and a baseline before any code change. Failures here become fix items appended to this plan.

**Files:** none (runs the existing `Makefile:278` target).

- [ ] **Step 1: Ensure the Docker harness image exists**

The `nomad` binary is Linux-only; run the suite in a container with a real `nomad` v2.0.4. Build the image if absent:

```bash
cat > /tmp/nomad-itest.Dockerfile <<'EOF'
FROM golang:1.26
COPY --from=hashicorp/nomad:2.0.4 /bin/nomad /usr/local/bin/nomad
EOF
docker build -t nomad-itest:local -f /tmp/nomad-itest.Dockerfile /tmp
```

- [ ] **Step 2: Run the full integration suite against one binary**

```bash
docker run --rm --privileged --cgroupns=host \
  -v "$PWD":/src -w /src nomad-itest:local \
  make test-integration
```

Expected: all six live tests PASS — `TestDevAgent`, `TestACLBootstrapAndLeaderLive`, `TestNodeEligibilityAndDrainLive`, `TestNodePoolLifecycleLive`, `TestJobLifecycleLive`, `TestNamespaceLifecycleLive`. (The in-container `docker driver: no docker.sock` warning is benign; `--privileged`/`--cgroupns=host` are required for the client cgroup-v2 subtree. Repo must be under `/Users` for Docker Desktop file-sharing.)

- [ ] **Step 3: Record the outcome**

Capture the summary line (pass counts) into the task notes. If any test FAILS, stop and add a fix task for it before continuing — do not sweep it. If all pass, the Foundation open item is closed (final evidence recorded in Task 12/13).

- [ ] **Step 4: Commit (evidence note only, if any file changed)**

No source change; nothing to commit here unless a harness note file is added. Proceed to Task 2.

---

## Task 2: C2 (L-1) — persist `DrainObservedGeneration` immediately

**Files:**
- Modify: `internal/controller/nomadnode_controller.go` (`driveDesired`, ~:196-215)
- Test: `internal/controller/nomadnode_controller_test.go`

**Interfaces:**
- Consumes: `fakeNodeOps` (`fake_nomadnode_test.go`), `readyCluster(ctx, ns)`, `newFakeNodeFactory(fake)`.
- Produces: nothing new; `driveDesired` keeps its signature `driveDesired(ctx, *NomadNode, *api.NodeListStub, NomadNodeOps) error`.

Current behavior: `driveDesired` sets `nn.Status.DrainObservedGeneration = nn.Generation` in memory only; it is persisted later by `mirrorStatus`'s `r.Status().Update`. A `mirrorStatus` failure loses the generation and the next pass re-issues the drain.

- [ ] **Step 1: Write the failing test** (white-box: calls `driveDesired` directly, without `mirrorStatus`)

Add under the existing `Describe("NomadNode reflector: drive", ...)` block in `nomadnode_controller_test.go`:

```go
	It("persists DrainObservedGeneration when it issues a drain, independent of mirrorStatus (L-1)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-l1-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "l1",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "l1", Namespace: ns.Name}, nn)).To(Succeed())

		fake := &fakeNodeOps{}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		stub := &api.NodeListStub{ID: "l1id", Name: "l1", Status: "ready", SchedulingEligibility: "eligible", Drain: false}

		// Call driveDesired directly — NOT the full Reconcile — so mirrorStatus never runs.
		Expect(r.driveDesired(ctx, nn, stub, fake)).To(Succeed())
		Expect(fake.drainCalls).To(HaveLen(1))

		var got nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "l1", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.Status.DrainObservedGeneration).To(Equal(nn.Generation),
			"driveDesired must persist the generation itself, not rely on mirrorStatus")
	})
```

- [ ] **Step 2: Run it — verify RED**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A3 "L-1"`
Expected: FAIL — `got.Status.DrainObservedGeneration` is `0`, not `nn.Generation` (pre-fix `driveDesired` does not persist).

- [ ] **Step 3: Implement the fix**

In `driveDesired`, persist the generation immediately after a successful `UpdateDrain`. Replace the drain-issue block:

```go
		spec := &api.DrainSpec{Deadline: deadline, IgnoreSystemJobs: nn.Spec.Drain.IgnoreSystemJobs}
		if err := ops.UpdateDrain(ctx, stub.ID, spec, false); err != nil {
			return err
		}
		nn.Status.DrainObservedGeneration = nn.Generation
		// Persist the generation NOW, decoupled from mirrorStatus: a later
		// status-write failure must not lose it and cause a re-issue (L-1).
		return r.Status().Update(ctx, nn)
```

(This replaces the previous `nn.Status.DrainObservedGeneration = nn.Generation` / `return nil`.)

- [ ] **Step 4: Run it — verify GREEN + no regression**

Run: `go test ./internal/controller/ -run TestControllers`
Expected: PASS (the new test and all existing drain/eligibility/prune specs).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadnode_controller.go internal/controller/nomadnode_controller_test.go
git commit -S -m "fix(nomadnode): persist DrainObservedGeneration in driveDesired (L-1)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: B1 — two-pass persisted-generation end-to-end test

*Test-only.* Depends on Task 2 (exercises the persisted path via full `Reconcile`, no manual generation seeding).

**Files:**
- Test: `internal/controller/nomadnode_controller_test.go`

- [ ] **Step 1: Write the test**

```go
	It("does not re-issue a drain across passes via the persisted generation (B1)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-b1-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "b1",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		// Pass 1: node not yet draining -> drain issued, generation persisted.
		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "b1id", Name: "b1", Status: "ready", SchedulingEligibility: "eligible", Drain: false},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(HaveLen(1))

		var mid nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "b1", Namespace: ns.Name}, &mid)).To(Succeed())
		Expect(mid.Status.DrainObservedGeneration).To(Equal(mid.Generation))

		// Pass 2: node now draining (in progress) -> must NOT re-issue.
		fake.list[0].Drain = true
		fake.list[0].SchedulingEligibility = "ineligible"
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(HaveLen(1), "pass 2 must not re-issue via the persisted generation")
	})
```

- [ ] **Step 2: Run it — verify PASS**

Run: `go test ./internal/controller/ -run TestControllers`
Expected: PASS. (This guards the end-to-end convergence built on Task 2's persistence.)

- [ ] **Step 3: Commit**

```bash
git add internal/controller/nomadnode_controller_test.go
git commit -S -m "test(nomadnode): two-pass persisted-generation drain convergence (B1)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: C3 (L-3) — no drain re-issue on adoption

**Files:**
- Modify: `internal/controller/nomadnode_controller.go` (`driveDesired`, drain branch)
- Test: `internal/controller/nomadnode_controller_test.go`

Current behavior: at first mint of an already-draining node, `spec.Drain` is seeded but `DrainObservedGeneration`(0) != `Generation`(1), so `drainHandledThisGeneration` is false and `UpdateDrain` is issued once — re-issuing an in-progress drain and restarting its deadline.

- [ ] **Step 1: Write the failing test**

```go
	It("adopts an already-draining node without re-issuing the drain (L-3)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-l3-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		// No pre-created CR: this pass MINTS it from an already-draining node.
		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "l3id", Name: "l3", Status: "ready", SchedulingEligibility: "ineligible", Drain: true},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(BeEmpty(), "adopting an in-progress drain must not re-issue (deadline would restart)")

		var got nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "l3", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.Status.DrainObservedGeneration).To(Equal(got.Generation))
	})
```

- [ ] **Step 2: Run it — verify RED**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A3 "L-3"`
Expected: FAIL — `fake.drainCalls` has length 1 (the drain is re-issued on adoption).

- [ ] **Step 3: Implement the fix**

In `driveDesired`, inside the `if nn.Spec.Drain != nil {` branch, after the `drainHandledThisGeneration` early-return, add the adoption guard before computing/issuing the drain:

```go
	if nn.Spec.Drain != nil {
		if drainHandledThisGeneration(nn, stub) {
			return nil // in progress (converging) or complete (converged)
		}
		// Adoption: the node is ALREADY draining (e.g. drained out-of-band, seeded
		// at first mint). Don't re-issue — it would restart the deadline. Mark this
		// generation handled and persist it (L-3, using the L-1 immediate-persist).
		if stub.Drain {
			nn.Status.DrainObservedGeneration = nn.Generation
			return r.Status().Update(ctx, nn)
		}
		deadline := defaultDrainDeadline
		if nn.Spec.Drain.Deadline != nil {
			deadline = nn.Spec.Drain.Deadline.Duration
		}
		spec := &api.DrainSpec{Deadline: deadline, IgnoreSystemJobs: nn.Spec.Drain.IgnoreSystemJobs}
		if err := ops.UpdateDrain(ctx, stub.ID, spec, false); err != nil {
			return err
		}
		nn.Status.DrainObservedGeneration = nn.Generation
		return r.Status().Update(ctx, nn)
	}
```

- [ ] **Step 4: Run it — verify GREEN + no regression**

Run: `go test ./internal/controller/ -run TestControllers`
Expected: PASS (new test + all existing drain specs, including the in-progress-drain test which already relied on `drainHandledThisGeneration`).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadnode_controller.go internal/controller/nomadnode_controller_test.go
git commit -S -m "fix(nomadnode): adopt in-progress drain without re-issuing (L-3)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: C1 (M-1) — deterministic owner across sanitize-collisions

**Files:**
- Modify: `internal/controller/nomadnode_controller.go` (`Reconcile` loop, `upsertNode`, add `resolveCollisionOwners`)
- Test: `internal/controller/nomadnode_controller_test.go`

**Interfaces:**
- Produces: `resolveCollisionOwners(bound map[string]*api.NodeListStub) map[string]string` (objName → owning Nomad Name); `upsertNode` gains an `owners map[string]string` parameter.

Current behavior: two distinct Nomad Names that `sanitizeNodeName` to the same object name both call `upsertNode` on the same CR; whichever runs first (random map order) creates it, the other clobbers the shared CR's condition with `DuplicateNodeName`. The CR's `Reconciled` condition flaps across passes.

- [ ] **Step 1: Write the failing test**

Add under `Describe("NomadNode reflector: prune + cascade", ...)` (or a new drive Describe):

```go
	It("picks a deterministic owner across sanitize-collisions and does not flap (M-1)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-m1-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		// "web.1" and "web-1" both sanitize to "web-1". Lower CreateIndex owns.
		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "a", Name: "web.1", Status: "ready", SchedulingEligibility: "eligible", CreateIndex: 10},
			{ID: "b", Name: "web-1", Status: "ready", SchedulingEligibility: "eligible", CreateIndex: 20},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}

		assertOwner := func() {
			var nn nomadv1alpha1.NomadNode
			Expect(k8s.Get(ctx, types.NamespacedName{Name: "web-1", Namespace: ns.Name}, &nn)).To(Succeed())
			Expect(nn.Spec.NodeName).To(Equal("web.1"), "lowest CreateIndex owns the object name")
			Expect(meta.IsStatusConditionTrue(nn.Status.Conditions, nomadv1alpha1.NomadNodeCondReconciled)).To(BeTrue(),
				"owner CR must stay Reconciled=True, not flap to DuplicateNodeName")
			// The colliding loser mints no CR of its own.
			var list nomadv1alpha1.NomadNodeList
			Expect(k8s.List(ctx, &list, client.InNamespace(ns.Name))).To(Succeed())
			Expect(list.Items).To(HaveLen(1))
		}

		// Run twice; ownership + condition must be stable across passes.
		for range 2 {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
			Expect(err).NotTo(HaveOccurred())
			assertOwner()
		}
	})
```

(Add `"sigs.k8s.io/controller-runtime/pkg/client"` to the test imports — `meta` (`k8s.io/apimachinery/pkg/api/meta`, providing `meta.IsStatusConditionTrue`) is already imported per the file header; `client` is not.)

- [ ] **Step 2: Run it — verify RED**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A3 "M-1"`
Expected: FAIL — ownership (`nn.Spec.NodeName`) is order-dependent and/or the `Reconciled` condition is `DuplicateNodeName`/False.

- [ ] **Step 3: Add the deterministic-owner helper**

In `nomadnode_controller.go`, add:

```go
// resolveCollisionOwners maps each sanitized object name to the single Nomad
// node Name that owns its CR when multiple distinct Names sanitize to the same
// object name. The owner is chosen deterministically (lowest CreateIndex, then
// Name), so ownership never flaps with map-iteration order (M-1).
func resolveCollisionOwners(bound map[string]*api.NodeListStub) map[string]string {
	best := map[string]*api.NodeListStub{}
	for _, stub := range bound {
		obj := sanitizeNodeName(stub.Name)
		cur, ok := best[obj]
		if !ok || stub.CreateIndex < cur.CreateIndex ||
			(stub.CreateIndex == cur.CreateIndex && stub.Name < cur.Name) {
			best[obj] = stub
		}
	}
	owners := make(map[string]string, len(best))
	for obj, stub := range best {
		owners[obj] = stub.Name
	}
	return owners
}
```

- [ ] **Step 4: Thread it through `Reconcile` and `upsertNode`**

In `Reconcile`, after `bound, dupes := bindNodes(stubs)`:

```go
	bound, dupes := bindNodes(stubs)
	owners := resolveCollisionOwners(bound)
	var errs []error
	for _, stub := range bound {
		if err := r.upsertNode(ctx, &nc, stub, owners, ops); err != nil {
			errs = append(errs, err)
		}
	}
```

In `upsertNode`, add the `owners` parameter and replace the create/mismatch logic so a colliding loser skips (logs) instead of minting or clobbering:

```go
func (r *NomadNodeReconciler) upsertNode(ctx context.Context, nc *nomadv1alpha1.NomadCluster, stub *api.NodeListStub, owners map[string]string, ops NomadNodeOps) error {
	objName := sanitizeNodeName(stub.Name)
	var nn nomadv1alpha1.NomadNode
	err := r.Get(ctx, types.NamespacedName{Name: objName, Namespace: nc.Namespace}, &nn)
	switch {
	case apierrors.IsNotFound(err):
		// Only the deterministic owner mints the CR; a colliding loser skips so
		// ownership never flaps with map-iteration order (M-1).
		if owners[objName] != stub.Name {
			log.FromContext(ctx).Info("skipping node whose sanitized name collides with the owner",
				"node", stub.Name, "object", objName, "owner", owners[objName])
			return nil
		}
		nn = nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: objName, Namespace: nc.Namespace, Labels: names(nc).Labels()},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name},
				NodeName:   stub.Name,
				Eligible:   stub.SchedulingEligibility != api.NodeSchedulingIneligible,
			},
		}
		if stub.Drain {
			nn.Spec.Drain = r.seedDrain(ctx, stub.ID, ops)
		}
		if err := controllerutil.SetControllerReference(nc, &nn, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, &nn); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return err
			}
			if err := r.Get(ctx, types.NamespacedName{Name: objName, Namespace: nc.Namespace}, &nn); err != nil {
				return err
			}
		}
	case err != nil:
		return err
	}
	// An existing CR is owned by its Spec.NodeName; a different colliding node
	// must not hijack it or clobber its status — skip deterministically (M-1).
	if nn.Spec.NodeName != stub.Name {
		log.FromContext(ctx).Info("skipping node whose sanitized name collides with an existing owner",
			"node", stub.Name, "object", objName, "owner", nn.Spec.NodeName)
		return nil
	}
	if err := r.driveDesired(ctx, &nn, stub, ops); err != nil {
		return err
	}
	return r.mirrorStatus(ctx, &nn, stub)
}
```

Ensure `nomadnode_controller.go` imports `"sigs.k8s.io/controller-runtime/pkg/log"` (add if absent).

- [ ] **Step 5: Reconcile any existing DuplicateNodeName-on-collision test**

Run: `grep -rn "DuplicateNodeName\|ReasonDuplicateNodeName" internal/controller/*_test.go`
For any test asserting the OLD behavior (a sanitize-collision setting `DuplicateNodeName` on the shared CR), update it to the new deterministic-owner + loser-skips behavior. (The separate `markDuplicates` path — two live nodes with the *same* Nomad Name — is unchanged and still uses `ReasonDuplicateNodeName`; do not touch its tests.)

- [ ] **Step 6: Run it — verify GREEN + no regression**

Run: `go test ./internal/controller/ -run TestControllers && go vet ./...`
Expected: PASS; `ReasonDuplicateNodeName` still referenced by `markDuplicates` (no unused-const/vet error).

- [ ] **Step 7: Commit**

```bash
git add internal/controller/nomadnode_controller.go internal/controller/nomadnode_controller_test.go
git commit -S -m "fix(nomadnode): deterministic owner across sanitize-collisions (M-1)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: C4 (L-2) — guard the empty-list mass-prune (updates a test)

**Files:**
- Modify: `internal/controller/nomadnode_controller.go` (`pruneAbsent`)
- Test: `internal/controller/nomadnode_controller_test.go` (UPDATE the existing empty-prune spec + add a companion)

Current behavior: an empty-but-error-free `ListNodes` yields empty `bound`/`dupes`, so `pruneAbsent` deletes every CR of the cluster. Intended today (`_test.go:229-247` asserts it), but a spurious empty result mass-deletes real CRs.

- [ ] **Step 1: Update the existing test to assert the new guard**

Replace the body of the existing spec `"deletes a CR whose node is absent from a successful list"` (`nomadnode_controller_test.go:229-247`) — keep the per-node prune it exercises but change the empty-list expectation, and rename it:

```go
	It("does NOT mass-prune when a successful list is unexpectedly empty (L-2)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-l2-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		// Pass 1: node present -> CR minted.
		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "g1", Name: "ghost", Status: "ready", SchedulingEligibility: "eligible"}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "ghost", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})).To(Succeed())

		// Pass 2: empty (but successful) list while a CR exists -> NOT pruned
		// (a suspect full-empty must not mass-delete; scale-to-zero retains CRs).
		fake.list = nil
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "ghost", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})).To(Succeed(),
			"an unexpectedly-empty list must not mass-prune existing CRs (L-2)")
	})

	It("still prunes an absent node from a NON-empty successful list", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-l2b-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "g1", Name: "ghost", Status: "ready", SchedulingEligibility: "eligible"},
			{ID: "k1", Name: "keep", Status: "ready", SchedulingEligibility: "eligible"},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		// "ghost" gone from a still-non-empty list -> pruned; "keep" remains.
		fake.list = []*api.NodeListStub{{ID: "k1", Name: "keep", Status: "ready", SchedulingEligibility: "eligible"}}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		err = k8s.Get(ctx, types.NamespacedName{Name: "ghost", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "keep", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})).To(Succeed())
	})
```

- [ ] **Step 2: Run — verify RED on the updated spec**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A3 "L-2"`
Expected: the "does NOT mass-prune" spec FAILS (the ghost CR is currently deleted on the empty pass).

- [ ] **Step 3: Implement the guard in `pruneAbsent`**

Guard only the all-or-nothing wipe: when `bound` and `dupes` are both empty (nothing present this pass) but CRs exist, skip and warn. Insert at the top of `pruneAbsent`, before the `List`:

```go
func (r *NomadNodeReconciler) pruneAbsent(ctx context.Context, nc *nomadv1alpha1.NomadCluster, bound map[string]*api.NodeListStub, dupes map[string]bool) error {
	present := map[string]bool{}
	for name := range bound {
		present[sanitizeNodeName(name)] = true
	}
	for name := range dupes {
		present[sanitizeNodeName(name)] = true
	}
	var list nomadv1alpha1.NomadNodeList
	if err := r.List(ctx, &list, client.InNamespace(nc.Namespace), client.MatchingLabels(names(nc).Labels())); err != nil {
		return err
	}
	// L-2: a successful-but-empty node list would prune EVERY CR. Treat a
	// sudden full-empty as suspect (transient API glitch) rather than a genuine
	// scale-to-zero: skip the mass-delete and warn. Accepted consequence: a
	// cluster that legitimately runs zero clients retains its (stale) CRs until
	// a node reappears (non-empty list -> normal per-node prune) or the cluster
	// is deleted (ownerRef GC). This is preferred over a spurious mass-delete.
	if len(present) == 0 && len(list.Items) > 0 {
		log.FromContext(ctx).Info("skipping prune: node list is unexpectedly empty while CRs exist (L-2)",
			"cluster", nc.Name, "existingCRs", len(list.Items))
		return nil
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

- [ ] **Step 4: Run — verify GREEN + no regression**

Run: `go test ./internal/controller/ -run TestControllers`
Expected: PASS (both L-2 specs + all others).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/nomadnode_controller.go internal/controller/nomadnode_controller_test.go
git commit -S -m "fix(nomadnode): guard empty-list mass-prune; retain CRs on scale-to-zero (L-2)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: B2 + B3 — slice-3 test-gap backfill (no code change)

*Test-only.* Guards two existing behaviors currently untested.

**Files:**
- Test: `internal/controller/nomadnode_controller_test.go`

- [ ] **Step 1: Write B2 — out-of-band drain-cancel re-issue**

```go
	It("re-issues a drain when it was cancelled out-of-band (B2)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-b2-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "b2", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "b2",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "b2", Namespace: ns.Name}, nn)).To(Succeed())
		nn.Status.DrainObservedGeneration = nn.Generation // we already issued it this generation
		Expect(k8s.Status().Update(ctx, nn)).To(Succeed())

		// Out-of-band cancel: Nomad reports NOT draining, and the last drain is not complete.
		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "b2id", Name: "b2", Status: "ready", SchedulingEligibility: "eligible", Drain: false, LastDrain: nil},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(HaveLen(1), "an out-of-band-cancelled drain must be re-issued to satisfy spec")
	})
```

- [ ] **Step 2: Write B3 — eligibility no-op**

```go
	It("does not call SetEligibility when eligibility already matches (B3)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-b3-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "b3", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNodeSpec{ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "b3", Eligible: true},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		// Nomad already reports eligible -> compare-before-write must be a no-op.
		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "b3id", Name: "b3", Status: "ready", SchedulingEligibility: "eligible"}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.eligCalls).To(BeEmpty(), "matching eligibility must not trigger a write")
	})
```

- [ ] **Step 3: Run — verify PASS**

Run: `go test ./internal/controller/ -run TestControllers`
Expected: PASS (both guard existing behavior).

- [ ] **Step 4: Commit**

```bash
git add internal/controller/nomadnode_controller_test.go
git commit -S -m "test(nomadnode): out-of-band drain-cancel re-issue + eligibility no-op (B2,B3)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 8: D-cleanup batch (D1 lint, D2 interface trim, D3 gossip mount, 6a opts nit)

Trivial, mostly no-behavior-change cleanups batched into one task. Only D3 gets a new test (an observable builder assertion).

**Files:**
- Modify: `internal/controller/resources_gateway.go` (D1 prealloc), `internal/controller/security_test.go` (D1 unparam)
- Modify: `internal/controller/nomadcluster_controller.go` (D2 interface)
- Modify: `internal/controller/resources_workload.go` (D3 mount)
- Modify: `internal/nomad/client.go`, `internal/nomad/namespace.go` (6a nit)
- Test: `internal/controller/resources_workload_test.go` (D3 assertion)

- [ ] **Step 1: D1 — preallocate the `listeners` slice**

In `resources_gateway.go` `buildManagedGateway`, replace the `listeners := []gwapiv1.Listener{{...}}` literal with a preallocated slice + append:

```go
	listeners := make([]gwapiv1.Listener, 0, 1+len(nc.Spec.ExternalAccess.Gateway.RPCPorts))
	listeners = append(listeners, gwapiv1.Listener{
		Name:     listenerNameHTTP,
		Port:     gwapiv1.PortNumber(portHTTP),
		Protocol: gwapiv1.TLSProtocolType,
		Hostname: ptrHostname(nc.Spec.ExternalAccess.Gateway.HTTPHostname),
		TLS:      &gwapiv1.GatewayTLSConfig{Mode: new(gwapiv1.TLSModePassthrough)},
	})
	for ordinal, p := range nc.Spec.ExternalAccess.Gateway.RPCPorts {
		listeners = append(listeners, gwapiv1.Listener{
			Name:     gwapiv1.SectionName(listenerNameRPC(ordinal)),
			Port:     gwapiv1.PortNumber(p),
			Protocol: gwapiv1.TCPProtocolType,
		})
	}
```

- [ ] **Step 2: D1 — drop the always-`"nomad-tls"` `makeCertSecret` param**

In `security_test.go`, remove the `name` parameter (add a package const) and update every call site:

```go
const testCertSecretName = "nomad-tls"

func makeCertSecret(ctx context.Context, ns string) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testCertSecretName, Namespace: ns},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("x"), "tls.key": []byte("y"), "ca.crt": []byte("z")},
	}
	Expect(k8s.Create(ctx, s)).To(Succeed())
}
```

Run `grep -rn 'makeCertSecret(ctx, "nomad-tls"' internal/controller` and rewrite each to `makeCertSecret(ctx, ns)` (call sites: `nomadcluster_controller_test.go` and `security_test.go`). Fixtures that reference the secret name (`TLS: TLSSpec{CertSecretRef: "nomad-tls"}`) keep the literal.

- [ ] **Step 3: D2 — trim unused `NomadOps` methods**

In `nomadcluster_controller.go`, remove `Ping` and `ServerHealthy` from the `NomadOps` interface (they have zero reconciler call sites):

```go
type NomadOps interface {
	Leader(ctx context.Context) (string, error)
	ACLBootstrap(ctx context.Context, bootstrapToken string) (string, error)
	ServerHealth(ctx context.Context) ([]nomad.NomadMember, error)
}
```

Leave the concrete `(*nomad.Client).Ping`/`ServerHealthy` methods and their `contract.go` pins (`(*api.Agent).Self`/`Health`) untouched. Leave the `fakeNomad` methods/fields as-is (harmless extra methods; `serverHealthy` is still set by fixtures).

- [ ] **Step 4: D3 — remove the redundant gossip mount, write the failing test first**

`resources_workload_test.go` is a **plain `testing.T`** file (not Ginkgo), so write a stdlib test matching that file's pattern (NOT a bare `It(...)`, which fails Ginkgo tree construction):

```go
func TestGossipMountedOnlyOnInitContainer(t *testing.T) {
	nc := minimalCluster("prod", "wl")
	sts := buildStatefulSet(nc, "hash")
	main := sts.Spec.Template.Spec.Containers[0]
	for _, m := range main.VolumeMounts {
		if m.MountPath == "/nomad/gossip" {
			t.Errorf("main container must not mount gossip at /nomad/gossip")
		}
	}
	init := sts.Spec.Template.Spec.InitContainers[0]
	found := false
	for _, m := range init.VolumeMounts {
		if m.MountPath == "/nomad/gossip" {
			found = true
		}
	}
	if !found {
		t.Errorf("init container must still mount gossip at /nomad/gossip")
	}
}
```

(`minimalCluster` and `buildStatefulSet` are package-scoped — same `package controller`.) Run it — RED (main container currently mounts `/nomad/gossip` at `resources_workload.go:212`). Then remove the redundant mount in `resources_workload.go` (main container `VolumeMounts`):

```go
						VolumeMounts: []corev1.VolumeMount{
							{Name: "rendered", MountPath: "/nomad/config"},
							{Name: "data", MountPath: "/var/lib/nomad"},
							{Name: "tls", MountPath: "/nomad/tls", ReadOnly: true},
						},
```

Keep the `gossip` Volume definition and the init-container mount (the init entrypoint reads `/nomad/gossip/key`). Re-run — GREEN.

- [ ] **Step 5: 6a nit — reuse option helpers in the nomad namespace client**

In `internal/nomad/client.go`, add a plain write-options helper next to the existing helpers:

```go
// writeOpts builds plain WriteOptions carrying the context.
func writeOpts(ctx context.Context) *api.WriteOptions {
	return (&api.WriteOptions{}).WithContext(ctx)
}
```

In `internal/nomad/namespace.go`, reuse the helpers (behavior-identical):

```go
func (c *Client) UpsertNamespace(ctx context.Context, ns *api.Namespace) error {
	if _, err := c.api.Namespaces().Register(ns, writeOpts(ctx)); err != nil {
		return fmt.Errorf("nomad: upsert namespace %q: %w", ns.Name, err)
	}
	return nil
}

func (c *Client) DeleteNamespace(ctx context.Context, name string) error {
	if _, err := c.api.Namespaces().Delete(name, writeOpts(ctx)); err != nil {
		return fmt.Errorf("nomad: delete namespace %q: %w", name, err)
	}
	return nil
}

func (c *Client) CountNamespaceJobs(ctx context.Context, name string) (int, error) {
	jobs, _, err := c.api.Jobs().List(nsQueryOpts(ctx, name))
	if err != nil {
		return 0, fmt.Errorf("nomad: list namespace %q jobs: %w", name, err)
	}
	return len(jobs), nil
}
```

- [ ] **Step 6: Run the full gate**

Run: `make manifests generate fmt vet && make test && go vet -tags integration ./... && make lint`
Expected: PASS, **zero regen drift**, `make lint` clean (the two D1 findings resolved). `go build ./...` proves the interface trim and the nomad-client refactor compile.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -S -m "chore: batch cleanups (prealloc, unparam, NomadOps trim, gossip mount, nomad opts)

known-issues #2/#3/#4 + 6a option-helper nit. No behavior change except the
redundant gossip mount removal (main container).

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 9: D4 (#5) — don't demote a provisioned cluster on a transient read

**Files:**
- Modify: `internal/controller/nomadcluster_controller.go` (cert gate ~:102-106, gateway gate ~:120-124)
- Test: `internal/controller/nomadcluster_controller_test.go`

The flap fires on the `!certReady` / `!extReady` FALSE path (which sets `Pending` + `finish` persists). An error return does not persist status, so it cannot flap — the fix targets only the false path.

- [ ] **Step 1: Promote the `driveToReady` / `driftTo` test helpers to package scope**

These are currently **local closures** inside `Describe("advertise.rpc drift guard", ...)` (`nomadcluster_controller_test.go:414` and `:439`), so new top-level `Describe`s (this task and Task 11) cannot reach them (`undefined: driveToReady`). Hoist both to **package-level functions** in `nomadcluster_controller_test.go` — signatures unchanged, they already take `ctx context.Context` as the first param:

```go
func driveToReady(ctx context.Context, name, ns, addrA string, servers int32, rpcPorts []int32) (*NomadClusterReconciler, *record.FakeRecorder) { /* moved body */ }
func driftTo(ctx context.Context, name, ns, addrB string, r *NomadClusterReconciler) nomadv1alpha1.NomadCluster { /* moved body */ }
```

Delete the two `:=` closure definitions from inside the drift-guard `Describe`; its existing specs call them unchanged. Run `go test ./internal/controller/ -run TestControllers` to confirm the existing drift specs still pass after the hoist. (`minimalCluster`, `makeCertSecret`, `reconcileOnce`, `newFakeFactory`, `fakeNomad` are already package-scoped — fine.)

**Confirmed fact (do not re-verify):** `certSecretReady` (`security.go:64-78`) returns `(false, nil)` when any of `tls.crt`/`tls.key`/`ca.crt` is empty — so `delete(s.Data, "tls.crt")` drives the `!certReady` **false path** (the flap trigger), not an error. Clearing `gw.Status.Addresses` makes `ensureManagedGateway` return `("", false, nil)`.

- [ ] **Step 2: Write the failing tests**

Add a `Describe("transient-read flap guard (#5)", ...)` block, reusing the `driveToReady` / `minimalCluster` / `makeCertSecret` helpers already in the file:

```go
var _ = Describe("transient-read flap guard (#5)", func() {
	It("keeps a Ready cluster Ready when the gateway address momentarily disappears", func() {
		ctx := context.Background()
		r, _ := driveToReady(ctx, "flapgw", "flap-gw", "10.0.0.5", 3, []int32{14647, 24647, 34647})

		// Transient blip: gateway loses its address.
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "flapgw-gateway", Namespace: "flap-gw"}, &gw)).To(Succeed())
		gw.Status.Addresses = nil
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
		reconcileOnce(r, "flapgw", "flap-gw")

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "flapgw", Namespace: "flap-gw"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady), "must not demote to Pending on a transient gateway blip")
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondReady)).To(BeTrue())
	})

	It("keeps a Ready cluster Ready when the cert Secret momentarily becomes incomplete", func() {
		ctx := context.Background()
		r, _ := driveToReady(ctx, "flapcert", "flap-cert", "10.0.0.5", 3, []int32{14647, 24647, 34647})

		// Transient blip: cert Secret loses tls.crt -> certSecretReady == false.
		var s corev1.Secret
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "nomad-tls", Namespace: "flap-cert"}, &s)).To(Succeed())
		delete(s.Data, "tls.crt")
		Expect(k8s.Update(ctx, &s)).To(Succeed())
		reconcileOnce(r, "flapcert", "flap-cert")

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "flapcert", Namespace: "flap-cert"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady), "must not demote to Pending on a transient cert blip")
	})

	It("still gates an unprovisioned cluster to Pending on a missing address", func() {
		ctx := context.Background()
		ns := "flap-new"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		nc := minimalCluster("fresh", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "fresh", ns) // gateway has no address yet
		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "fresh", Namespace: ns}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhasePending))
	})
})
```

(Note: `makeCertSecret` is now the Task-8 single-arg form. If Task 8 has not merged when this task runs, use the current two-arg form and reconcile in Task 8.)

- [ ] **Step 3: Run — verify RED**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A3 "flap"`
Expected: the two "keeps ... Ready" specs FAIL (cluster is demoted to `Pending`).

- [ ] **Step 4: Implement the guard on both gates**

Cert gate:

```go
	if !certReady {
		if nc.Status.Phase == nomadv1alpha1.PhaseReady || nc.Status.Phase == nomadv1alpha1.PhaseDegraded {
			// Provisioned cluster, transient cert-read blip: don't demote or flip
			// conditions; keep last-known state and requeue to retry (#5).
			return r.finish(ctx, &nc, ctrl.Result{RequeueAfter: requeueShort})
		}
		nc.Status.Phase = nomadv1alpha1.PhasePending
		setCondition(&nc, nomadv1alpha1.CondReady, metav1ConditionFalse, "WaitingForCert", "cert Secret not ready")
		return r.finish(ctx, &nc, ctrl.Result{RequeueAfter: requeueShort})
	}
```

Gateway gate:

```go
	if !extReady {
		if nc.Status.Phase == nomadv1alpha1.PhaseReady || nc.Status.Phase == nomadv1alpha1.PhaseDegraded {
			// Provisioned cluster, transient address-read blip: keep last-known
			// state and requeue; never demote a running cluster to Pending (#5).
			return r.finish(ctx, &nc, ctrl.Result{RequeueAfter: requeueShort})
		}
		nc.Status.Phase = nomadv1alpha1.PhasePending
		setCondition(&nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionFalse, "WaitingForAddress", "external address not assigned")
		return r.finish(ctx, &nc, ctrl.Result{RequeueAfter: requeueShort})
	}
```

- [ ] **Step 5: Run — verify GREEN + no regression**

Run: `go test ./internal/controller/ -run TestControllers`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/nomadcluster_controller.go internal/controller/nomadcluster_controller_test.go
git commit -S -m "fix(nomadcluster): don't demote a provisioned cluster on a transient read (#5)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 10: G #6 — localized `ExternalAccessReady` reason (Existing mode)

**Files:**
- Modify: `internal/controller/resources_gateway.go` (`ensureExistingGateway`), `internal/controller/nomadcluster_controller.go` (gateway gate)
- Test: `internal/controller/nomadcluster_controller_test.go` (or the existing gateway test file)

Do NOT reshape the shared `(string, bool, error)` tuple (all four `ensure*` funcs share it via the `Reconcile` switch — reshaping ripples into LB/Managed). Instead `ensureExistingGateway` sets `CondExternalAccessReady` directly with a per-failure reason; the Reconcile-level generic reason is stamped only for the non-Existing modes.

- [ ] **Step 1: Write the failing tests (per Existing-mode failure)**

Model on any existing `GatewayModeExisting` spec (`grep -n GatewayModeExisting internal/controller/*_test.go`). Add specs that create a NomadCluster referencing a user Gateway in various broken states and assert the specific `ExternalAccessReady` reason. Two representative cases (write both; then add the remaining three following the same shape):

```go
var _ = Describe("Existing-mode gateway reason (#6)", func() {
	// helper: an Existing-mode cluster referencing a Gateway named "shared" in ns.
	existingCluster := func(name, ns string) *nomadv1alpha1.NomadCluster {
		nc := minimalCluster(name, ns)
		nc.Spec.ExternalAccess.Gateway.Mode = nomadv1alpha1.GatewayModeExisting
		nc.Spec.ExternalAccess.Gateway.Ref = &nomadv1alpha1.GatewayRef{Name: "shared", Namespace: ns}
		return nc
	}
	reasonFor := func(name, ns string) string {
		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, &got)).To(Succeed())
		for _, c := range got.Status.Conditions {
			if c.Type == nomadv1alpha1.CondExternalAccessReady {
				return c.Reason
			}
		}
		return ""
	}

	It("reports GatewayNotFound when the referenced Gateway is absent", func() {
		ctx := context.Background()
		ns := "ex-notfound"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		Expect(k8s.Create(ctx, existingCluster("c", ns))).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "c", ns)
		Expect(reasonFor("c", ns)).To(Equal("GatewayNotFound"))
	})

	It("reports GatewayNoAddress when the referenced Gateway is valid but has no address", func() {
		ctx := context.Background()
		ns := "ex-noaddr"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		// A fully-valid referenced Gateway (HTTP TLS listener + one RPC TCP listener), no address.
		gw := &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns},
			Spec: gwapiv1.GatewaySpec{
				GatewayClassName: "cilium",
				Listeners: []gwapiv1.Listener{
					{Name: listenerNameHTTP, Port: gwapiv1.PortNumber(portHTTP), Protocol: gwapiv1.TLSProtocolType,
						Hostname: ptrHostname("nomad.example.com"), TLS: &gwapiv1.GatewayTLSConfig{Mode: new(gwapiv1.TLSModePassthrough)}},
					{Name: gwapiv1.SectionName(listenerNameRPC(0)), Port: 14647, Protocol: gwapiv1.TCPProtocolType},
				},
			},
		}
		Expect(k8s.Create(ctx, gw)).To(Succeed())
		Expect(k8s.Create(ctx, existingCluster("c", ns))).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "c", ns)
		Expect(reasonFor("c", ns)).To(Equal("GatewayNoAddress"))
	})
})
```

Add three more specs following the same shape, asserting these reasons (build the referenced Gateway broken in exactly the named way):
- **`HTTPListenerInvalid`** — Gateway present but missing/misnamed/wrong-protocol/wrong-hostname HTTP listener.
- **`RPCListenerInvalid`** — HTTP listener valid, but the RPC listener is missing or the wrong port/protocol.
- **`NamespaceNotAdmitted`** — listeners present and correct, but `allowedRoutes` does not admit the CR's namespace (set the HTTP listener's `AllowedRoutes` to a namespace selector that excludes `ns`).

(**Confirmed types:** the type is `GatewayRef` — NOT `GatewayReference` — and `GatewaySpec.Ref` is a **pointer** `*GatewayRef` (`api/v1alpha1/nomadcluster_types.go:73,91`); the `&nomadv1alpha1.GatewayRef{...}` idiom matches `resources_gateway_test.go:139`.)

- [ ] **Step 2: Run — verify RED**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A2 "#6"`
Expected: FAIL — the reason is the generic `"WaitingForAddress"` for every case.

- [ ] **Step 3: Implement — set specific reasons inside `ensureExistingGateway`**

In `resources_gateway.go`, at each verification-failure `return "", false, nil`, first set the condition with the specific reason (the function already receives `nc`):

```go
func (r *NomadClusterReconciler) ensureExistingGateway(ctx context.Context, nc *nomadv1alpha1.NomadCluster) (string, bool, error) {
	ref := nc.Spec.ExternalAccess.Gateway.Ref
	var gw gwapiv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			setCondition(nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionFalse, "GatewayNotFound",
				fmt.Sprintf("referenced Gateway %s/%s not found", ref.Namespace, ref.Name))
			return "", false, nil
		}
		return "", false, err
	}
	byName := make(map[gwapiv1.SectionName]gwapiv1.Listener, len(gw.Spec.Listeners))
	for _, l := range gw.Spec.Listeners {
		byName[l.Name] = l
	}
	httpListener, ok := byName[listenerNameHTTP]
	if !ok || httpListener.Protocol != gwapiv1.TLSProtocolType ||
		httpListener.Hostname == nil || string(*httpListener.Hostname) != nc.Spec.ExternalAccess.Gateway.HTTPHostname {
		setCondition(nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionFalse, "HTTPListenerInvalid",
			"referenced Gateway lacks a valid HTTPS/TLS listener for the configured hostname")
		return "", false, nil
	}
	if !listenerAdmitsNamespace(httpListener, gw.Namespace, nc.Namespace) {
		setCondition(nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionFalse, "NamespaceNotAdmitted",
			"referenced Gateway HTTP listener does not admit namespace "+nc.Namespace)
		return "", false, nil
	}
	for ordinal, p := range nc.Spec.ExternalAccess.Gateway.RPCPorts {
		l, ok := byName[gwapiv1.SectionName(listenerNameRPC(ordinal))]
		if !ok || l.Protocol != gwapiv1.TCPProtocolType || int32(l.Port) != p {
			setCondition(nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionFalse, "RPCListenerInvalid",
				fmt.Sprintf("referenced Gateway lacks a valid TCP listener for RPC port %d", p))
			return "", false, nil
		}
		if !listenerAdmitsNamespace(l, gw.Namespace, nc.Namespace) {
			setCondition(nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionFalse, "NamespaceNotAdmitted",
				"referenced Gateway RPC listener does not admit namespace "+nc.Namespace)
			return "", false, nil
		}
	}
	for _, a := range gw.Status.Addresses {
		if a.Value != "" {
			return a.Value, true, nil
		}
	}
	setCondition(nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionFalse, "GatewayNoAddress",
		"referenced Gateway has no assigned address yet")
	return "", false, nil
}
```

Ensure `resources_gateway.go` imports `fmt` (add if absent).

- [ ] **Step 4: Don't clobber the specific reason in `Reconcile`**

In the gateway gate, stamp the generic reason only for non-Existing modes:

```go
	if !extReady {
		if nc.Status.Phase == nomadv1alpha1.PhaseReady || nc.Status.Phase == nomadv1alpha1.PhaseDegraded {
			return r.finish(ctx, &nc, ctrl.Result{RequeueAfter: requeueShort})
		}
		nc.Status.Phase = nomadv1alpha1.PhasePending
		// Existing mode sets a specific ExternalAccessReady reason inside
		// ensureExistingGateway; only stamp the generic reason for the others (#6).
		existing := nc.Spec.ExternalAccess.Mode == nomadv1alpha1.ExternalAccessGateway &&
			nc.Spec.ExternalAccess.Gateway.Mode == nomadv1alpha1.GatewayModeExisting
		if !existing {
			setCondition(&nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionFalse, "WaitingForAddress", "external address not assigned")
		}
		return r.finish(ctx, &nc, ctrl.Result{RequeueAfter: requeueShort})
	}
```

(This composes with the Task-9 flap guard — keep both edits; the provisioned-cluster early-return sits above this block.)

- [ ] **Step 5: Run — verify GREEN + no regression**

Run: `go test ./internal/controller/ -run TestControllers`
Expected: PASS (the five reason specs; LB/Managed specs still see `"WaitingForAddress"`).

- [ ] **Step 6: Commit**

```bash
git add internal/controller/resources_gateway.go internal/controller/nomadcluster_controller.go internal/controller/nomadcluster_controller_test.go
git commit -S -m "feat(nomadcluster): per-failure ExternalAccessReady reason in Existing mode (#6)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 11: E (6b Minor 2) — persist `ExternalAddress` at drift-detection

**Files:**
- Modify: `internal/controller/nomadcluster_controller.go` (`Reconcile`, after `checkAddressDrift`)
- Test: `internal/controller/nomadcluster_controller_test.go`

The `RaftAddressDrift` Warning re-fires every reconcile if a later apply/client error returns before `finish` persists `Status.ExternalAddress` (prevAddr never advances). Fix: persist status right after `checkAddressDrift`, before the error-prone applies. (SGE audit: `Status.ExternalAddress`'s only functional consumer is the drift guard's own `prevAddr` — early-persist is safe.)

- [ ] **Step 1: (Confirmed) `r.apply` uses SSA `Patch`; first applied object is the ConfigMap**

`r.apply` is `r.Patch(ctx, obj, client.Apply, client.FieldOwner("nomad-operator"), client.ForceOwnership)` (`security.go:183`); the first apply in `Reconcile` is `buildConfigMap` (`nomadcluster_controller.go:132`). So the `configMapApplyFails.Patch` override below is correct — **keep `Patch`** (do not override `Create`). This task also depends on the Task-9 hoist of `driveToReady`/`driftTo` to package scope; run Task 9 first, or perform that hoist here if executing out of order.

- [ ] **Step 2: Write the failing test (a client that fails the first ConfigMap apply)**

```go
var _ = Describe("drift Warning does not re-fire during an apply-error window (6b Minor 2)", func() {
	It("emits the servers:1 drift Warning only once across a persistent apply error", func() {
		ctx := context.Background()
		r, rec := driveToReady(ctx, "efire", "e-fire", "10.0.0.5", 1, []int32{14647})

		// Wrap the reconciler's client so every ConfigMap apply fails — simulating
		// a persistent apply-error window concurrent with a drift.
		r.Client = &configMapApplyFails{Client: r.Client}

		drift := func() {
			var gw gwapiv1.Gateway
			Expect(k8s.Get(ctx, types.NamespacedName{Name: "efire-gateway", Namespace: "e-fire"}, &gw)).To(Succeed())
			gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.9"}}
			Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
			_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "efire", Namespace: "e-fire"}})
		}
		drift() // reconcile 1: address 10.0.0.5 -> 10.0.0.9, apply fails after persist
		drift() // reconcile 2: address already 10.0.0.9 persisted -> no new drift

		// Exactly one Warning across both reconciles.
		warnings := 0
		for {
			select {
			case ev := <-rec.Events:
				if strings.Contains(ev, "Warning") {
					warnings++
				}
				continue
			default:
			}
			break
		}
		Expect(warnings).To(Equal(1), "drift Warning must fire once, not per-reconcile, during an apply-error window")
	})
})

// configMapApplyFails fails every write to a ConfigMap; all other calls pass through.
type configMapApplyFails struct {
	client.Client
}

func (c *configMapApplyFails) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok {
		return errors.New("simulated ConfigMap apply failure")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}
```

(Adjust the overridden method to `Create` if Step 1 shows `apply` uses `Create`. Add `"strings"` to the test imports. `record.FakeRecorder` from `driveToReady` buffers 10 events.)

- [ ] **Step 3: Run — verify RED**

Run: `go test ./internal/controller/ -run TestControllers -v 2>&1 | grep -A3 "re-fire"`
Expected: FAIL — `warnings == 2` (the drift re-detects and re-warns on reconcile 2 because `ExternalAddress` was never persisted).

- [ ] **Step 4: Implement — persist after `checkAddressDrift`**

In `Reconcile`, right after the `r.checkAddressDrift(&nc, prevAddr, extAddr)` line, persist status before the provisioning applies:

```go
	prevAddr := nc.Status.ExternalAddress
	nc.Status.ExternalAddress = extAddr
	setCondition(&nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionTrue, "Assigned", "external address assigned")
	r.checkAddressDrift(&nc, prevAddr, extAddr)
	// Persist the resolved address + drift condition NOW, before the error-prone
	// provisioning applies below. Otherwise an apply/client error returns before
	// finish() persists, prevAddr never advances, and the next reconcile re-detects
	// the same drift and re-emits the Warning every pass (6b Minor 2). ExternalAddress
	// has no "last-successfully-applied" consumer (reviewer-audited), so this is safe.
	if err := r.Status().Update(ctx, &nc); err != nil {
		return ctrl.Result{}, err
	}
```

- [ ] **Step 5: Run — verify GREEN + no regression**

Run: `go test ./internal/controller/ -run TestControllers`
Expected: PASS (the re-fire test + the existing drift-guard specs, which drift once and still see their single event).

- [ ] **Step 6: Commit**

```bash
git add internal/controller/nomadcluster_controller.go internal/controller/nomadcluster_controller_test.go
git commit -S -m "fix(nomadcluster): persist ExternalAddress at drift-detection to stop Warning re-fire (6b Minor 2)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 12: F — documented won't-fix + known-issues reconciliation

*Docs only.* Records terminal dispositions so nothing is silently dropped.

**Files:**
- Modify: `docs/development/issues/known-issues.md`
- Modify: `docs/runbooks/nomadcluster.md` (only if an entry references it)

- [ ] **Step 1: Mark the fixed slice-2 items Resolved**

In `docs/development/issues/known-issues.md`, add a `**Status: Resolved (2026-07-17, slice 6c).**` line (mirroring #1's format) to:
- **#2** (lint prealloc/unparam) — Task 8.
- **#3** (unused `NomadOps` methods) — Task 8 (interface trimmed; concrete methods + pins retained).
- **#4** (redundant gossip mount) — Task 8.
- **#5** (Ready→Pending flap) — Task 9.
- **#6** (imprecise `ExternalAccessReady` reason) — Task 10.
- **#7** (operator does not watch the referenced Gateway) — **already resolved on `main` by `fbbf66e`**; mark Resolved and cite that commit.

- [ ] **Step 2: Add the won't-fix (documented) entries**

Append a "Won't-fix (by design)" section to `docs/development/issues/known-issues.md` with three entries + rationale:
- **6b Minor 3** — empty `ServerHealth` read keeps prior `Members`/`Quorum`. Intended keep-prior-status per restart-resilience design §6.3; near-impossible while a leader exists. Won't-fix.
- **6a finalize reserved-name guard** — phantom: no reserved-name guard exists in `finalizeNamespace`; the `reconcileNamespace` guard is intentional defense-in-depth and CEL-redundant (`namespaceName != 'default'`, immutable). Nothing to fix.
- **6a conflict-then-delete shared-namespace** — self-heals (loser re-registers once the winner's finalizer deletes the shared namespace); deliberately mirrors merged `NomadPool`. Won't-fix for parity; if the transient window is ever unacceptable, change **both** CRDs together.

- [ ] **Step 3: Record the A1 live-run evidence**

Add a short "Integration coverage" note (in `known-issues.md` or the runbook): `make test-integration` runs all six live specs green against real Nomad v2.0.4 via the Docker harness (Task 1 baseline / Task 13 final), closing the Foundation open item.

- [ ] **Step 4: Commit**

```bash
git add docs/development/issues/known-issues.md docs/runbooks/nomadcluster.md
git commit -S -m "docs(6c): reconcile known-issues (resolve #2-#7, record won't-fix rationale + live evidence)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 13: A1 re-run + full gate

*Verification.* Confirms the whole slice is green.

- [ ] **Step 1: Full build/regen/test gate**

Run: `make manifests generate fmt vet && make test && go vet -tags integration ./... && make lint`
Expected: PASS, **zero regen drift** (`git status` shows no generated-file changes), lint clean.

- [ ] **Step 2: Re-run the live integration suite**

```bash
docker run --rm --privileged --cgroupns=host -v "$PWD":/src -w /src nomad-itest:local make test-integration
```
Expected: all six live specs PASS.

- [ ] **Step 3: Confirm no unintended diff**

Run: `git status --short` — only intended files changed; the pre-existing uncommitted `config/manager/kustomization.yaml` (unrelated) may remain.

- [ ] **Step 4: Hand off to the whole-branch review + finish**

Do not commit anything here unless a doc note was added. Proceed to the independent whole-branch review, then `superpowers:finishing-a-development-branch`.

---

## Self-Review (author checklist — completed)

- **Spec coverage:** A1 (T1/T13) · B1 (T3) · B2/B3 (T7) · C1 (T5) · C2 (T2) · C3 (T4) · C4 (T6) · D1/D2/D3/6a-nit (T8) · D4 (T9) · #6 (T10) · #7 (T12, resolved on `main`) · E (T11) · F/6b-Minor-3/6a-phantom/6a-conflict (T12). Every design item maps to a task.
- **Placeholder scan:** two tasks (E, D4) carry a "confirm this internal fact" first step (apply verb; certSecretReady-on-incomplete) rather than a placeholder — each is a concrete read of a named function, with the test written against the confirmed behavior. #6 enumerates its five reasons with exact strings and the exact broken-Gateway setup per case.
- **Type consistency:** `resolveCollisionOwners(map[string]*api.NodeListStub) map[string]string` (T5) is consumed only by `upsertNode`'s new `owners` param (T5). `NomadOps` trimmed to `Leader`/`ACLBootstrap`/`ServerHealth` (T8) — matches the actual call sites. `writeOpts(ctx)`/`nsQueryOpts(ctx, name)` (T8) match the existing helper signatures. Reason strings are free-form literals per the NomadCluster convention.
- **Constraints:** no CRD schema change (verified per task); no `contract.go` edit; no `api.*` leak; build+integration-vet gate on every code task.
