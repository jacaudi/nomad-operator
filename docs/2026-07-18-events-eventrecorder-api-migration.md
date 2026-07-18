# Migrate reconcilers from the deprecated `record.EventRecorder` to the new `events.EventRecorder` API

- **Severity:** Minor · **Area:** controller-runtime / event emission
- **Source:** slice-6c follow-ups (2026-07-18); spun out of the SA1019 `GetEventRecorderFor` lint-debt fix (see `docs/2026-07-18-geteventrecorderfor-sa1019-lint-debt.md`).
- **Status:** Open (follow-up / deferred behavioral migration)

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
