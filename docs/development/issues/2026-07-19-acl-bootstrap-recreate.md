# Delete+recreate leaves a dead ACL token — stale `acl-bootstrapped` annotation skips re-bootstrap

- **Severity:** Critical (every authenticated operator→Nomad call 403s after recreate) · **Area:** controller / ACL bootstrap (`internal/controller/security.go`)
- **Source:** live kind end-to-end run, 2026-07-19.
- **Status:** RESOLVED — 2026-07-19, commit `__COMMIT__` (this branch), TDD regression test + idempotent-self-heal fix.

## Resolution

Made the ACL-bootstrap fast-path sound in `ensureBootstrapToken`
(`internal/controller/security.go`): the durable `nomad.operator.io/acl-bootstrapped`
annotation on the retained token Secret now gates only the Secret *write*, never
the `ops.ACLBootstrap` *call*. Every reconcile attempts the already-idempotent
`ACLBootstrap(storedToken)` — on a fresh/recreated cluster Nomad's `BootstrapOpts`
accepts the operator-supplied token and registers it (self-heal); on an
already-bootstrapped cluster it returns the already-bootstrapped error
(`nomad.IsACLAlreadyBootstrapped`), which is treated as success. The annotation
`Update` is skipped when the Secret already carries it, so steady-state reconciles
do not churn the Secret's `resourceVersion`. No new `NomadOps` interface surface
was added — the fix reuses the existing idempotent bootstrap call.

## Problem

A `NomadCluster` deleted and recreated with fresh state (fresh Raft/data) comes
back `phase: Ready`, `ACLBootstrapped=True`, but every authenticated
operator→Nomad call 403s — autopilot server-health for `status.members`, and
critically `NomadJob` / `NomadPool` / `NomadNamespace` registration. The stored
management token is not valid against the recreated Nomad.

Root cause: the token Secret (`<cluster>-nomad-bootstrap-token`) is
**retained-by-design** across `NomadCluster` deletion (no owner ref, so it and
the gossip key and Raft PVCs survive the ownerRef cascade), and it carries the
durable `nomad.operator.io/acl-bootstrapped=true` annotation once bootstrap has
been confirmed. On recreate the operator found the retained, still-annotated
Secret and hit the fast-path:

```go
if sec.Annotations[annotationACLBootstrapped] == "true" {
    return nil // skips bootstrapping the brand-new, un-bootstrapped Nomad
}
```

so it **never bootstrapped the fresh Nomad**. The retained token was never
registered with the recreated cluster → 403 on every authenticated call, forever.

## Confirmation

On a live kind cluster showing `Ready` / `ACLBootstrapped=True` after a
delete+recreate, running `nomad acl bootstrap` against it **succeeded** and
returned a fresh management token — proving the cluster had never actually been
bootstrapped by the operator, despite the reported status. First-ever deploys are
unaffected (the annotation is absent, so bootstrap runs normally); only
delete+recreate triggers the skip.

## Why it went unnoticed

The controller tests fake the Nomad API (envtest) and the existing ACL specs
asserted the *buggy* invariant directly — "Secret exists → do NOT call
`ACLBootstrap` again" — so they codified the skip rather than catching it. No test
simulated a *retained annotated Secret meeting a fresh (un-bootstrapped) cluster*,
which is the only state that exposes the dead token.

## Fix / test

- `internal/controller/security.go` — `ensureBootstrapToken` no longer returns
  early on the annotation; it always attempts the idempotent `ACLBootstrap` and
  guards the annotation `Update` behind an "already annotated" check. Doc comments
  on the function and `annotationACLBootstrapped` updated to describe the sound
  behavior (annotation gates the write, not the call).
- `internal/controller/security_test.go` — new **regression** spec: a token Secret
  pre-annotated `acl-bootstrapped=true` + a fake reporting NOT bootstrapped asserts
  `ACLBootstrap` IS called with the stored token (fails on the old skip). Plus
  steady-state no-churn assertions (stable `resourceVersion`) and an
  already-bootstrapped-response tolerance spec driven by a real `*nomad.Client`
  against Nomad's exact 400 body.
- `internal/controller/nomadcluster_controller_test.go` — the managed-provisioning
  crash-retry spec's steady-state tail updated from "must NOT re-bootstrap" to the
  sound invariant (re-attempts idempotently, stays `Ready`, no Secret churn).

## Known limitation (pre-existing, non-blocking)

Detecting a recreated/fresh cluster requires asking Nomad, so there is one
idempotent `ACLBootstrap` round-trip per reconcile in steady state. The reconciler
already makes several Nomad calls per reconcile (Leader, ServerHealth), and the
already-bootstrapped response is a cheap 400 rejection (not a mutation), so this is
acceptable.
