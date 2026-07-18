# Lint debt: `GetEventRecorderFor` flagged SA1019 (deprecated) by staticcheck

- **Severity:** Minor · **Area:** lint / cleanup
- **Source:** slice-6c whole-branch review + Task 8 (2026-07-18). Pre-existing; not introduced by slice 6c.
- **Status:** Open (follow-up / lint-debt)

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
