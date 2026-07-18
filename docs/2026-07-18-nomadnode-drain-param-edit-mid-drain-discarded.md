# NomadNode: a drain-parameter edit mid-drain is silently discarded

- **Severity:** Minor · **Area:** reconciler / NomadNode drain convergence
- **Source:** slice-6c whole-branch review (2026-07-18), finding M-1; originates from the slice-6c L-3 adopt-guard.
- **Status:** Open (follow-up)

## Problem

The NomadNode reflector's adopt-guard in `driveDesired`
(`internal/controller/nomadnode_controller.go:233`) short-circuits on
`stub.Drain == true` alone:

```go
// Adoption: the node is ALREADY draining ... don't re-issue (it would
// restart the deadline). Mark this generation handled and persist.
if stub.Drain {
    nn.Status.DrainObservedGeneration = nn.Generation
    return r.Status().Update(ctx, nn)
}
```

This correctly avoids re-issuing (and thereby restarting the deadline of) an
already-in-progress drain — the intended L-3 behavior. But it does not
distinguish "node is draining out-of-band / freshly adopted" from "the user
**edited** `spec.Drain.Deadline` or `spec.Drain.IgnoreSystemJobs` while the node
happens to still be mid-drain." In the second case the generation bumps,
`drainHandledThisGeneration` returns false, execution falls into this guard, and
the **new drain parameters are dropped** — with no error, condition, or event to
tell the operator their edit was ignored.

## Why it matters

A user who lowers a drain deadline (or toggles `ignoreSystemJobs`) on an
already-draining node gets silent no-op behavior. It is low-frequency and a
genuine wash against the alternative (re-issuing restarts the deadline, which is
also imperfect), but the *silence* is the real defect — an ignored spec change
should be observable.

## Proposed fix

Surface the ignored edit rather than changing the drain semantics:

- When the adopt-guard fires but the desired `DrainSpec` (deadline /
  ignoreSystemJobs) differs from what Nomad currently reports for the in-flight
  drain, emit a `Warning` Event and/or set a `DrainSpecPendingRestart`-style
  condition on the NomadNode, noting that the changed drain parameters will take
  effect on the next (re-issued) drain, not the in-flight one.
- Do **not** re-issue the drain mid-flight (that restarts the deadline — the L-3
  behavior we deliberately kept).

## Test to add

- envtest: a mid-drain node (`stub.Drain == true`), a NomadNode whose
  `spec.Drain.Deadline` was edited (generation bumped), asserts (a) `UpdateDrain`
  is NOT called and (b) the new observability signal (Event/condition) fires.

## Acceptance

- The changed-spec-mid-drain path produces a visible signal; no `UpdateDrain`
  re-issue; existing L-3 adopt/convergence tests stay green.
