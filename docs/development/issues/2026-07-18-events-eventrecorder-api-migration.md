# Migrate reconcilers from the deprecated `record.EventRecorder` to the new `events.EventRecorder` API

- **Severity:** Minor · **Area:** controller-runtime / event emission
- **Source:** slice-6c follow-ups (2026-07-18); spun out of the SA1019 `GetEventRecorderFor` lint-debt fix (see `docs/development/issues/2026-07-18-geteventrecorderfor-sa1019-lint-debt.md`).
- **Status:** RESOLVED — 2026-07-19, commits `4ad8764` + `dfc9eba`, merged fast-forward to local `main` (HEAD `dfc9eba`).

## Resolution

Done as the real behavioral migration this issue tracked; the slice-6c
`//nolint:staticcheck` suppressions are retired. All four reconcilers
(`nomadcluster`, `nomadjob`, `nomadnamespace`, `nomadpool`) now hold a
`Recorder events.EventRecorder` field wired via `mgr.GetEventRecorder("<name>")`
(same recorder-name strings), and every emission moved from
`.Event(obj, type, reason, msg)` to
`.Eventf(regarding, related=nil, type, reason, action, note, args...)`. The four
`//nolint:staticcheck` suppressions were removed (the unrelated `client.Apply`
one at `security.go:183` and the recorder-less `NomadNode` reconciler were left
untouched).

Correctness note: the new API exposes only the format-based `Eventf`, so the
runtime Nomad `warnings` string in `nomadjob` is passed as a `"%s"` argument
rather than inlined into the `note` format string — a stray `%` in a warning
would otherwise corrupt the event. A regression test (`"deprecated 50% soon"`)
guards this and fails on the naive inlined form. Chosen `action` verbs:
`RaftAddressDrift` (drift ×2), `RegisterJob` (register warnings), and
`RegisterSkipped` for the two name-conflict events (they fire when registration
is *skipped*, so `"Register"` would have been misleading in the operator-visible
`Action` field).

Verification: `make lint` reports **zero** SA1019 (the deprecated symbol is gone,
not suppressed); build + full test gate green with zero regen drift
(`internal/controller` 80.2%, `internal/nomad` 73.9%); `go vet -tags integration`
clean; independent whole-branch review Ready-to-merge (0 Critical / 0 Important).
A full kind end-to-end smoke test passed — the manager boots healthy and the new
`GetEventRecorder` wiring was confirmed live by an emitted `LeaderElection` event.

## Problem

controller-runtime v0.23.3 deprecates `Manager.GetEventRecorderFor`
(`pkg/recorder/recorder.go:32`: `// Deprecated: this uses the old events API and
will be removed in a future release. Please use GetEventRecorder instead.`). The
four managed reconcilers — `nomadcluster`, `nomadpool`, `nomadjob`,
`nomadnamespace` — obtain their recorders via `mgr.GetEventRecorderFor(...)` in
`SetupWithManager`, store them in a `Recorder record.EventRecorder` field, and
emit with `.Event(obj, eventtype, reason, message)`.

The SA1019 lint finding was resolved by a narrowly-scoped `//nolint:staticcheck`
suppression at each site, **not** a migration, because the replacement is not a
mechanical swap (see next section). This issue tracks the real migration.

## Why it matters

The deprecated method "will be removed in a future release" of controller-runtime.
When the operator eventually bumps controller-runtime past that removal,
`GetEventRecorderFor` stops compiling and the suppression hides nothing. Migrating
ahead of the removal is the durable fix; until then the suppression is safe and
correct.

## Why this is a behavioral migration, not a lint fix

`GetEventRecorder(name)` returns `events.EventRecorder`
(`k8s.io/client-go/tools/events`), **not** the `record.EventRecorder`
(`k8s.io/client-go/tools/record`) the reconcilers use today. The new API differs:

- The reconciler field type changes from `record.EventRecorder` to
  `events.EventRecorder` on all four reconcilers.
- The emission call changes from
  `Event(regarding runtime.Object, eventtype, reason, message string)` to
  `Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...any)`
  — a different signature with an added `related` object and `action` field.
- Event aggregation / dedup semantics differ between the two APIs, so the
  resulting Events have a slightly different shape.

So the migration touches every event-emitting call site plus the tests that assert
on emitted events (`record.FakeRecorder` moves to the events-API fake).

## Proposed change

1. Switch the four reconcilers' `Recorder` field type to `events.EventRecorder`
   and wire them via `mgr.GetEventRecorder("<name>")` in `SetupWithManager`,
   keeping the same recorder-name strings ("nomadcluster", "nomadpool",
   "nomadjob", "nomadnamespace").
2. Rewrite every `.Event(...)` call to `.Eventf(...)`, choosing an appropriate
   `action` value per event and threading the `related` object where meaningful
   (usually `nil`).
3. Migrate the event-assertion tests from `record.FakeRecorder` to the events-API
   fake recorder, updating assertions for the new Event shape.
4. Remove the four `//nolint:staticcheck` suppressions added by the SA1019 fix.
5. Confirm events still fire for every prior reason. (The `NomadNode` reconciler
   is unaffected — it surfaces state via a Condition, not a recorder.)

## Test to add / update

- Update the existing event-emission assertions across the four reconcilers to the
  events-API fake recorder; confirm each prior event reason still fires with the
  expected `action`.

## Acceptance

- All four reconcilers use `GetEventRecorder` / `events.EventRecorder`; the four
  `//nolint:staticcheck` suppressions are gone; `make lint` reports zero SA1019
  (now because the deprecated symbol is no longer used, not via suppression);
  events fire for every prior reason; build + full test gate green with zero regen
  drift.
