# NomadNode: sanitize-collision stale-owner lockout after owner disappears

- **Severity:** Minor · **Area:** reconciler / NomadNode sanitize-collision ownership
- **Source:** slice-6c whole-branch review (2026-07-18), finding M-2; related to the slice-6c M-1 deterministic-owner fix.
- **Status:** Open (follow-up). Pre-existing *shape* — not introduced or worsened by slice 6c.

## Problem

Slice 6c (M-1) made sanitize-collision ownership deterministic: when two distinct
Nomad node Names sanitize to the same Kubernetes object name,
`resolveCollisionOwners` (`internal/controller/nomadnode_controller.go:134`)
picks a single owner (lowest `CreateIndex`, then Name), and the loser skips +
logs instead of clobbering the shared CR.

The remaining gap is in the **existing-CR branch** of `upsertNode`
(`nomadnode_controller.go:196`):

```go
// An existing CR is owned by its Spec.NodeName; a different colliding node
// must not hijack it or clobber its status - skip deterministically (M-1).
if nn.Spec.NodeName != stub.Name {
    log.FromContext(ctx).Info("skipping node whose sanitized name collides ...")
    return nil
}
```

If the **owning** node later disappears while a *different* colliding node stays
live, the CR persists (its object name is still "present" via the live
collider's mapping, so `pruneAbsent` won't delete it) but keeps
`Spec.NodeName` = the now-dead owner. The live collider hits this branch, sees
`nn.Spec.NodeName != stub.Name`, and **skips forever** — never adopting the CR,
even though `resolveCollisionOwners` now names *it* the owner. The `owners` map
is consulted only in the `IsNotFound` (mint) branch, not here.

## Why it matters

A NomadNode object can be permanently stuck reflecting a dead node while a live
node with a colliding sanitized name goes unrepresented. Pre-6c behavior was no
better (the CR was equally stuck, just with a `DuplicateNodeName` condition
instead of a silent skip), so this is not a regression — but the new code has
the data to fix it cheaply.

## Proposed fix

Allow re-adoption in the existing-CR branch: when `nn.Spec.NodeName != stub.Name`
but `owners[objName] == stub.Name` (this stub is the current deterministic
owner) **and** the recorded owner is no longer present this pass, rewrite
`nn.Spec.NodeName` to the live owner and drive/mirror it. Guard carefully so a
genuine live collision (both nodes present) still deterministically skips the
loser — only re-own when the recorded owner is absent.

## Test to add

- envtest: two colliding Names present → CR owned by the lowest-CreateIndex node;
  then the owner disappears from the list while the collider stays → next
  reconcile re-owns the CR to the live collider (Spec.NodeName updated,
  Reconciled=True), rather than skipping forever.

## Acceptance

- A live collider re-adopts the CR once the recorded owner is gone; a genuine
  both-present collision still deterministically keeps one owner; existing M-1
  no-flap tests stay green.
