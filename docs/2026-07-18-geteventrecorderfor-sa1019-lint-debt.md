# Lint debt: `GetEventRecorderFor` flagged SA1019 (deprecated) by staticcheck

- **Severity:** Minor · **Area:** lint / cleanup
- **Source:** slice-6c whole-branch review + Task 8 (2026-07-18). Pre-existing; not introduced by slice 6c.
- **Status:** RESOLVED — 2026-07-18, commit `97095bc`, merged fast-forward to local `main` (HEAD `6ed9384`).

## Resolution

**Confirmed genuine:** controller-runtime v0.23.3 `pkg/recorder/recorder.go:32`
carries a real `// Deprecated:` marker on `GetEventRecorderFor`, naming
`GetEventRecorder` as the replacement. **But** `GetEventRecorder` returns
`events.EventRecorder` (the new events API), which is type-incompatible with the
`Recorder record.EventRecorder` field on all four reconcilers — migrating would
change the field type and rewrite every `.Event()` call, violating "field type
unaffected / events fire identically." Per this issue's Acceptance suppression
branch, applied a narrowly-scoped `//nolint:staticcheck` at all four
`SetupWithManager` sites (matching controller-runtime's own internal handling at
`pkg/manager/internal.go:264`; the fourth site, nomadpool, was hidden by
golangci-lint's default `max-same-issues:3`). `make lint` now reports **zero**
SA1019; events fire identically. The real migration to the `events.EventRecorder`
API is deferred behavioral work, tracked in
`docs/2026-07-18-events-eventrecorder-api-migration.md`.

## Problem

`make lint` (staticcheck) reports **SA1019 "using a deprecated symbol"** for
`mgr.GetEventRecorderFor(...)`, used in `SetupWithManager` of four reconcilers:

- `internal/controller/nomadcluster_controller.go:296`
- `internal/controller/nomadpool_controller.go:295`
- `internal/controller/nomadjob_controller.go:308`
- `internal/controller/nomadnamespace_controller.go:254`

(controller-runtime pinned at `sigs.k8s.io/controller-runtime v0.23.3`.)

Lint is not part of the acceptance build gate
(`make manifests generate fmt vet` + `make test`), so this did not block any
slice. It is recorded here so it isn't silently carried.

## First: confirm it's real

Before migrating, **verify** the SA1019 is genuine for v0.23.3 and not a
staticcheck-version/config artifact:

- Check whether `Manager.GetEventRecorderFor` (or the underlying
  `cluster.Cluster` / `EventRecorderProvider` method) actually carries a
  `// Deprecated:` marker in the pinned controller-runtime, and what the
  documented replacement is.
- Confirm which files trip it (the review noted findings on files not touched by
  6c; the four call sites above are the current users).

## Proposed fix (once confirmed)

- If genuinely deprecated: migrate all four call sites to the non-deprecated
  recorder API named by the `// Deprecated:` note, regenerate/verify, and keep
  the same recorder-name strings ("nomadcluster", "nomadpool", "nomadjob",
  "nomadnamespace"). The `Recorder record.EventRecorder` field type on each
  reconciler should be unaffected.
- If it's a false positive / staticcheck-version quirk: suppress with a narrowly
  scoped `//lint:ignore SA1019 <reason>` (or `//nolint`) carrying the
  justification, rather than a blanket lint-config exclusion.

## Acceptance

- `make lint` no longer reports SA1019 for these call sites (via migration or a
  justified, narrowly-scoped suppression); events still fire identically;
  build + full test gate stays green with zero regen drift.

## Note (lint tooling)

Use `make lint` (which builds the custom golangci-lint binary bundling the
`logcheck` plugin) — a bare `golangci-lint run` fails with
`plugin(logcheck): plugin "logcheck" not found` because the stock binary lacks
the compiled-in plugin.
