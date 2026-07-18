# Slice 6c — Hardening & Backlog Close-Out — Design

**Type:** design · **Date:** 2026-07-17 · **Status:** proposed
**Feature:** the final roadmap slice. Not a feature — a **backlog close-out**: drive every deferred item accumulated across slices 2–6b to a *terminal disposition* (fixed / covered by a new test / documented won't-fix with rationale), and run the still-open full live integration suite. This is the slice that clears the deferred backlog so the repository can be published (the pinned "local-only until the backlog is cleared" gate — see §11).

This is slice **6c** of the hardening slice (6a = `NomadNamespace`, done+merged; 6b = restart resilience, done+merged). Unlike a feature slice, its deliverable is *coverage of a checklist*, so its organizing principle is: **no silent drops.** Every item below ends in one of three states — **Fix**, **Test**, or **Won't-fix (documented)** — and the design is the audit ledger for that.

---

## Grounding

Every item below was re-verified against current `main` (HEAD `e429a01`), not taken from memory:

- The **slice-2 backlog** is the durable, authoritative record in `docs/known-issues.md` (#1 already Resolved by 6b; FR-1 servers:1 already shipped; #2–#7 remain).
- The **slice-3 Minors + test gaps** and the **6a deferrals** lived only in worktree ledgers (now gone); each was re-grounded against current code via a read-only inventory pass, which produced the `file:line` anchors and current-behavior notes cited throughout. Two findings changed the triage: **L-2** is currently *intended, test-asserted* behavior (not a plain bug), and the **6a finalize reserved-guard is a phantom** (no such guard exists in the finalize path).
- The **6b Minors** are recorded in `docs/designs/2026-07-17-nomadcluster-restart-resilience-design.md` and the slice-6b whole-branch review.

User scope decisions folded in (2026-07-17): (a) **comprehensive close-out** — every item to terminal state; (b) 6b Minor 2 → **root-cause fix** (persist `ExternalAddress`), not a dedup marker; (c) the three borderline items **L-2, #6, #7 are all fixed in 6c**, not deferred as documented enhancements.

---

## 1. Group A — full live integration run

*Verification, no code.* Closes the last open Foundation item: a single `make test-integration` run of **all** `//go:build integration` live tests against **one** real Nomad **v2.0.4** binary, rather than the per-package spikes each prior slice ran in isolation.

- **Target:** `make test-integration` (`Makefile:278`) → `TestDevAgent | TestACLBootstrapAndLeaderLive | TestNodeEligibilityAndDrainLive | TestNodePoolLifecycleLive | TestJobLifecycleLive | TestNamespaceLifecycleLive` across the 5 `internal/nomad/*_integration_test.go` files.
- **Harness (proven):** Docker image `nomad-itest:local` = `golang:1.26` + `COPY --from=hashicorp/nomad:2.0.4 /bin/nomad`, run `--privileged --cgroupns=host` with the repo mounted from `/Users` (each live test execs its own `nomad agent -dev` subprocess; `--privileged`/`cgroupns` are required for the client cgroup-v2 subtree, `docker.sock` absence is benign). Environmental dependency: Docker Desktop up + image built.
- **Sequencing (important):** run **early** in 6c as a *baseline*, before the code fixes — a failure here surfaces a real regression that feeds the fix list. Re-run once at the end as the final green gate.
- **Disposition:** if the suite passes, the Foundation item is **closed with recorded evidence**. If any test fails, the failure becomes a new 6c fix item (tracked, not swept).

---

## 2. Group B — slice-3 test gaps (test-only)

Three genuinely-absent tests confirmed missing in `internal/controller/nomadnode_controller_test.go`. Pure additions, no production change. TDD is trivially satisfied (write the test, watch it exercise existing behavior; if it fails, that's a latent bug promoted into Group C).

| Item | Gap | Where the near-miss test is today |
| --- | --- | --- |
| **B1** two-pass persisted-generation *(prioritized)* | No test issues a drain in pass 1 and verifies pass 2 does not re-issue via the *persisted* `DrainObservedGeneration`. Existing drain tests manually pre-set the generation and run a single Reconcile. | `_test.go:135` (completed), `:183` (in-progress) |
| **B2** out-of-band drain-cancel re-issue | No test sets `DrainObservedGeneration == Generation` with `stub.Drain == false` and `LastDrain != complete` to prove the spec path re-issues. | `_test.go:161` (drives via generation mismatch, not the out-of-band branch) |
| **B3** eligibility no-op | Existing test asserts the call happens *on mismatch*, but never asserts `eligCalls` is empty when eligibility already matches (the compare-before-write no-op). | `_test.go:116`; no-op is `nomadnode_controller.go:222` |

B1 must land against C2's persisted-generation fix (below), so it exercises the *persisted* path, not the in-memory one.

---

## 3. Group C — slice-3 correctness Minors (code + TDD)

All confirmed **still present** on current `main`. Each is a localized reconciler fix in `internal/controller/nomadnode_controller.go`.

### C1 (M-1) — deterministic owner-pick across sanitize-collisions
- **Where:** bound loop `:73-77`, `upsertNode` `:132-170`, `bindNodes` `:95-126`; `sanitizeNodeName` at `nomadnode_names.go:12`.
- **Current behavior:** `bound` is keyed by Nomad node **Name**, so two distinct Names that sanitize to the same RFC1123 `objName` both call `upsertNode` on the same object. First creates the CR; the second `Get`s it, sees `nn.Spec.NodeName != stub.Name` and writes `DuplicateNodeName`. Randomized map iteration flaps the object between `Reconciled=True` and `DuplicateNodeName` across passes. `bindNodes` only tie-breaks *within* one Name (by `CreateIndex`), not across sanitize-collisions.
- **Fix:** make owner selection deterministic across a sanitize-collision — pick a stable owner (e.g. lowest `CreateIndex`, then Name) for the shared `objName`; the loser gets a stable `DuplicateNodeName` (not a flap). No behavior change for the non-colliding common case.

### C2 (L-1) — persist `DrainObservedGeneration` immediately
- **Where:** set in-memory at `:208`, persisted only later at `mirrorStatus:263` (`r.Status().Update`).
- **Current behavior:** `driveDesired` issues `UpdateDrain` (`:205`) then sets `DrainObservedGeneration` on the in-memory object but defers persistence to `mirrorStatus`. If that status update fails, the generation is lost and the next pass **re-issues the drain** (double-issue, sliding deadline).
- **Fix:** persist `DrainObservedGeneration` immediately after a successful `UpdateDrain`, decoupled from `mirrorStatus` success.

### C3 (L-3) — no drain re-issue on adoption
- **Where:** `seedDrain` at mint `:145-147`, `driveDesired:196-209`, `drainHandledThisGeneration:234-243`.
- **Current behavior:** at first mint of an already-draining node, `spec.Drain` is seeded but `DrainObservedGeneration`(0) != `Generation`(1), so `drainHandledThisGeneration` is false and `UpdateDrain` is issued **once** even though `stub.Drain == true` — re-issuing an in-progress drain and restarting its deadline.
- **Fix:** skip the issue when `stub.Drain` is already true (or seed `DrainObservedGeneration` at mint so adoption is a no-op).

### C4 (L-2) — guard the empty-list mass-prune *(behavior change — updates a test)*
- **Where:** `pruneAbsent:327-353`, reached from `:81`; empty-list path returns-only-on-error at `:58-65`.
- **Current behavior:** an empty-but-error-free `ListNodes` yields empty `bound`/`dupes`, so `pruneAbsent` **deletes every one of the cluster's NomadNode CRs**. There is no guard. This is presently *intended*: `_test.go:229-247` asserts an empty list prunes.
- **Why fix it anyway:** a spurious empty result (transient API glitch returning `[], nil`) would mass-delete real CRs — an all-or-nothing data-loss edge disproportionate to the benefit of promptly pruning a genuinely-empty cluster.
- **Fix (KISS, stateless):** guard only the all-or-nothing wipe — when `ListNodes` returns empty/`nil`-error **and** there are currently `> 0` owned NomadNode CRs, **skip** the prune for this pass and log a warning (treat a sudden full-empty as suspect). A genuinely-zero-client cluster keeps a few stale-but-harmless CRs until a node reappears — strictly preferable to a spurious mass-delete. Per-node pruning of *some* absent nodes (a non-empty list missing entries) is unchanged.
- **Test impact:** `_test.go:229-247` is **updated** to assert the new guard (empty list + existing CRs → no prune + warning), plus a companion asserting a non-empty list still prunes its absent entries. This is a deliberate, documented semantics change, not an accidental one.

---

## 4. Group D — slice-2 cleanup (`known-issues.md` #2–#5) + 6a nit

Small, mostly mechanical. #1 is already Resolved (6b); FR-1 shipped. **D1–D3 + the 6a nit batch into one "cleanup" task** (each is ≤ a few lines, no behavior change); **D4 is its own task** (a real robustness fix with its own tests).

- **D1 (#2) lint** — preallocate `listeners` with capacity `1 + len(nc.Spec.Gateway.RPCPorts)` (`resources_gateway.go:30`, `prealloc`); drop/use the always-`"nomad-tls"` `name` param of `makeCertSecret` (`security_test.go:58`, `unparam`). No behavior change.
- **D2 (#3) trim unused `NomadOps` methods** — remove `Ping`, `ServerHealthy` from the `NomadOps` interface (`nomadcluster_controller.go:~42-44`); they are never called by the reconciler. **Keep the concrete `(*nomad.Client).ServerHealthy`** — it backs the `(*api.Agent).Health` `contract.go` pin via a real call. Only the interface surface is trimmed.
- **D3 (#4) remove redundant gossip mount** — the `gossip` Secret is mounted read-only at `/nomad/gossip` on the **main** container (`resources_workload.go:~212`), but the encrypt key is baked into `overlay.hcl` by the **init** container; the main container never reads it. Remove it from the main container (keep it on the init container); add/confirm a builder test that the main container has no `/nomad/gossip` mount.
- **6a nit** — `CountNamespaceJobs` (`internal/nomad/namespace.go:44`) inlines `(&api.QueryOptions{Namespace: name}).WithContext(ctx)` instead of the existing `nsQueryOpts` helper (`client.go:64`); `UpsertNamespace`/`DeleteNamespace` (`namespace.go:24,33`) inline `(&api.WriteOptions{}).WithContext(ctx)`. Call `nsQueryOpts`; optionally add a plain `writeOpts` helper and reuse it. DRY tidy, no behavior change.

- **D4 (#5) Ready/Degraded → Pending flap guard** *(real robustness fix, own task + tests)* — the cert gate (`nomadcluster_controller.go:~92-96`) and gateway gate (`~103-107`) set `Phase = Pending` and return early if the cert Secret or gateway address read momentarily fails, **even for an already-`Ready`/`Degraded` cluster** (in Existing mode a shared-Gateway blip could flap a healthy cluster). **Fix:** don't demote a provisioned cluster on a transient dependency blip — only gate to `Pending` when phase is empty/`Pending` (mirroring the Bootstrapping-seed guard added for the Ready→Degraded fix), distinguishing "never provisioned" from "provisioned, transient blip." Envtest: drive to `Ready`, then a transient cert/gateway read error asserts the phase stays `Ready`/`Degraded`, not `Pending`.

---

## 5. Group G — Existing-mode Gateway hardening (`known-issues.md` #6, #7)

Both touch Existing-mode gateway handling (`ensureExistingGateway` in `resources_gateway.go` + the gateway gate in `nomadcluster_controller.go`). They are cohesive and share a signature, so they're grouped. **#6 lands first** — it changes the `ensureGateway`/`ensureExistingGateway` signature that #7's reconcile-trigger builds on, so doing #7 first would churn.

### #6 — typed `ExternalAccessReady` reason
- **Where:** gateway gate condition `nomadcluster_controller.go:~105` + `ensureExistingGateway` (`resources_gateway.go`).
- **Current behavior:** all Existing-mode verification failures (Gateway not found, missing/misnamed listener, namespace not admitted, no address yet) collapse into one generic `ExternalAccessReady=False` / `"WaitingForAddress"` reason. Operators can't tell which prerequisite failed from status alone.
- **Fix:** return a typed verification result (reason enum + message) from `ensureExistingGateway` — thread it through the current `(string, bool, error)` signature — and surface the specific reason in the `ExternalAccessReady` condition. Managed mode unaffected.
- **Tests:** envtest per failure mode (missing Gateway / bad listener / not-admitted / no-address) asserting the distinct reason.

### #7 — watch the referenced Gateway (Existing mode)
- **Where:** `SetupWithManager` watch set (`nomadcluster_controller.go`) + `ensureExistingGateway` (`resources_gateway.go`).
- **Current behavior:** in `mode: Existing`, the operator reads the referenced Gateway's `status.addresses` but sets up **no Watch** on it. If the Gateway's address is assigned/changed *after* the operator's reconcile, the operator won't react until periodic resync or the next NomadCluster change — the cluster sits at `Pending`/stale `externalAddress` longer than necessary (observed directly: patching `status.addresses` did not trigger a reconcile). Managed mode is fine (owned + watched via ownerRef).
- **Fix:** add a `Watches` on `gatewayapi.Gateway` in `SetupWithManager` with a mapping func that resolves a Gateway back to the owning NomadCluster(s) that reference it in Existing mode (enqueue those). Managed mode unaffected.
- **Tests:** envtest asserting a Gateway `status.addresses` change enqueues a reconcile for the referencing NomadCluster; a Gateway referenced by nobody enqueues nothing.

---

## 6. Group E — 6b Minor 2: persist `ExternalAddress` at drift-detection (code + TDD)

- **Where:** `Reconcile` error-return paths (`nomadcluster_controller.go:~132-163, ~202`); the drift guard `checkAddressDrift` (`~298-326`); `Status.ExternalAddress` overwrite.
- **Current behavior:** the drift guard captures `prevAddr := nc.Status.ExternalAddress` before overwrite and compares to the resolved `extAddr`. If a later apply/client error makes `Reconcile` return early **without** persisting `Status.ExternalAddress = extAddr`, then across a persistent-error window every reconcile re-detects the *same* drift (prevAddr never advances) and **re-emits the `RaftAddressDrift` Warning** each pass.
- **Fix (user-chosen, root-cause):** persist `Status.ExternalAddress = extAddr` as soon as drift is detected — before the error-prone apply — so the next reconcile sees `prevAddr == extAddr` and stops re-detecting. Smallest change, no new field/annotation.
- **Must-verify (implementer gate):** confirm **no consumer treats `Status.ExternalAddress` as "last *successfully-applied*/confirmed-live" address**. It is grep-audited before the change lands. If a consumer does rely on that meaning, **fall back to the dedup-marker approach** (record the last drifted-to address the Warning fired for; re-emit only when it changes) rather than change `ExternalAddress` semantics. The design permits either; the root-cause fix is preferred if the audit is clean.
- **Tests:** envtest driving a drift with a subsequent apply error across two reconciles; assert the Warning fires **once**, not per-pass. The durable `RaftAddressDrift` Condition still records the drift (unchanged).

---

## 7. Group F — Won't-fix (documented, no code)

These three reach terminal state as **documented won't-fix** — not out of expedience, but because fixing each would be *wrong*. Each gets a `docs/known-issues.md` entry recording the rationale (portable to a GitHub issue-close comment on publish).

- **6b Minor 3 — empty `ServerHealth` read keeps prior `Members`/`Quorum`.** This is the *intended* keep-prior-status behavior from restart-resilience design §6.3, and is near-impossible to hit while a leader exists (the read is placed after the leader gate). "Fixing" it would contradict a shipped design decision. **Disposition: won't-fix, by design.**
- **6a finalize reserved-name guard — phantom.** The inventory confirmed there is **no** reserved-name guard in the finalize path (`finalizeNamespace:192-242`) to be "unreachable." The only reserved guard is in `reconcileNamespace:130-134`, it is intentional defense-in-depth, and CEL (`namespaceName != 'default'` at `nomadnamespace_types.go:59`, immutable) makes it redundant-by-design. There is nothing to fix; the existing guard is correct as belt-and-suspenders. **Disposition: won't-fix, no code exists to change.**
- **6a conflict-then-delete shared-namespace.** Two CRs targeting the same `cluster/namespaceName`: the loser is flagged `NamespaceNameConflict` and skips Register; if the winner is deleted, its finalizer `DeleteNamespace`s the shared Nomad namespace and the loser (which ignores Terminating siblings at `:299`) re-registers — it **self-heals**, with a transient delete/recreate window, and this behavior deliberately **mirrors the merged `NomadPool`**. **Disposition: won't-fix, for parity** — tightening the transient window would make `NomadNamespace` diverge from `NomadPool`; if that window is ever deemed unacceptable it should be changed in **both** CRDs together, as a separate parity-preserving change (noted in the known-issues entry).

---

## 8. Task decomposition & sequencing

One implementation plan, executed via the subagent-driven-development bundle (worktree → fresh `sr-go-engineer` implementer + reviewer per task w/ TDD → independent whole-branch review → finish). Proposed task order (dependency-aware):

1. **A1** live `make test-integration` baseline run — *first*, to surface any live regression before code work (and to establish the harness is up). Its result is recorded; failures append fix items.
2. **C2 (L-1)** persist generation → **B1** two-pass persisted-generation test (B1 depends on C2 so it exercises the persisted path).
3. **C1 (M-1)**, **C3 (L-3)**, **C4 (L-2)** — the remaining slice-3 fixes; **B2**, **B3** test gaps alongside their subjects.
4. **D-cleanup** (D1 + D2 + D3 + 6a nit) — one batched cleanup task.
5. **D4 (#5)** flap guard.
6. **G: #6** typed reason → **#7** Gateway watch (in this order — shared signature).
7. **E (6b Minor 2)** persist `ExternalAddress` (with the semantics audit).
8. **F + docs** — `known-issues.md` won't-fix rationales; close #2–#7 entries that were fixed; record the A1 evidence; correct/annotate resolved entries.
9. **A1 re-run** — final green gate.

Batching intent (from KISS): the trivial cleanups are **one** task, not one-per-nit; the won't-fix items are **one** docs task. Everything else is a discrete TDD task so per-task review stays meaningful.

The build/regen gate for every code task: `make manifests generate fmt vet && make test` with zero regen drift, plus `go vet -tags integration ./...`. **No 6c task changes the CRD schema** (6b already added `MemberStatus.Voter`; C4 touches only reconciler logic and a test), so `make manifests generate` must produce **no** diff — any diff is a bug.

---

## 9. Testing strategy (summary)

- **Group A:** the live suite itself is the test; capture pass/fail evidence in the known-issues/runbook update.
- **Group B:** three new unit/envtest cases in `nomadnode_controller_test.go` (B1 against the C2 persisted path).
- **Group C:** TDD per fix; C4 **updates** `_test.go:229-247` (documented semantics change) and adds a companion.
- **Group D:** D1–D3 covered by existing build/lint + a new builder assertion for D3; D4 gets dedicated flap envtests.
- **Group G:** #6 per-failure-reason envtests; #7 a watch-enqueue envtest.
- **Group E:** a two-reconcile drift+error envtest asserting single-fire.
- **No regression** to the existing slice-2/3/4/5/6a/6b reconcile, gateway, loadbalancer, teardown, pool, job, namespace suites; coverage should not drop materially from current (`controller ~78%`, `nomad ~74%`).

---

## 10. What 6c deliberately does **not** do

- **No new CRDs, no new spec surface, no new features.** Everything is a fix, a test, a cleanup, or a doc. (#6/#7 harden an existing mode; they add a reason enum and a watch, not a new API.)
- **No networking/advertise/topology change** (that was 6b's domain and is explicitly frozen).
- **No auto-recovery, no `servers:1` behavior change** — 6b's rejections stand.
- **No parity-breaking change to `NomadNamespace`** (the 6a conflict-then-delete window is left mirroring `NomadPool`).
- **No publish/push.** 6c *clears the gate* for going public; the actual repository creation/push remains a separate, explicit user action (§11).

---

## 11. The go-public gate

The pinned project decision is that the repository stays **local-only until the deferred backlog is cleared** — no push, no remote, no repo creation until then. 6c is the slice that clears it: on completion, every backlog item is fixed, tested, or documented-won't-fix, and `docs/known-issues.md` reflects reality (resolved entries closed, won't-fix entries carrying rationale, #6/#7 fixed).

At that point the gate is satisfied — but publishing is still an **explicit, separate decision the user makes**, not an automatic consequence of merging 6c. This design does not authorize any outward-facing action. When the user chooses to publish, the remaining known-issues entries (if any are consciously kept as enhancements) become filed GitHub issues verbatim, as their preamble intends.

---

## 12. Slice decomposition recap

- **6a — `NomadNamespace`** ✅ done + merged (local `main`).
- **6b — restart resilience** ✅ done + merged (local `main`).
- **6c — hardening & backlog close-out** ← *this design*: A live-integration run · B slice-3 test gaps · C slice-3 Minors (incl. L-2 guard) · D slice-2 cleanup + flap guard + 6a nit · G Existing-mode Gateway (#6 typed reason, #7 watch) · E 6b Minor 2 persist-`ExternalAddress` · F documented won't-fix (6b Minor 3, 6a finalize-phantom, 6a conflict-then-delete parity). Terminal state = the deferred backlog is cleared and the go-public gate (§11) is satisfied.
