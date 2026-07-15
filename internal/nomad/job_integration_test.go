//go:build integration

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/api"
)

// TestJobLifecycleLive exercises the full job surface against a real Nomad
// (skips when no binary/endpoint is available, matching the other integration
// tests). It resolves the design's two remaining plan-time opens.
//
// Live outcome against Nomad v2.0.4 (rev 5b83b133998a, run 2026-07-15):
//   - §6.3 RESOLVED: an identical re-Register does NOT advance Version
//     (0 -> 0) and Plan(identical).changed == false. Register self-dedups, so
//     Plan-gating is a KISS-optional optimization, not a correctness requirement.
//   - §6.4 RESOLVED: Deregister(purge) on a now-missing job returns no error,
//     and GetJob on a missing id returns (nil, nil) via the 404 mapping.
func TestJobLifecycleLive(t *testing.T) {
	// devAgentWithNode(t) is the existing harness in client_write_integration_test.go
	// (starts a real `nomad agent -dev`, registers a node, returns (*Client, nodeID),
	// and skips when no nomad binary is present). Reuse it — do NOT invent a helper
	// (SGE plan-review M-1).
	c, _ := devAgentWithNode(t)

	ctx := t.Context()
	id := "operator-it-job"
	region := "global"
	typ := "service"
	grp := "app"
	count := 1
	// A structurally valid job (Nomad rejects a job with no task groups at
	// Register with "Missing job task groups"). The task uses raw_exec (healthy
	// in `nomad agent -dev`) with a trivial command — Register only validates
	// structure, so placement/scheduling health is out of scope for this
	// client-level spike.
	job := &api.Job{
		ID: &id, Name: &id, Region: &region, Type: &typ,
		TaskGroups: []*api.TaskGroup{{
			Name:  &grp,
			Count: &count,
			Tasks: []*api.Task{{
				Name:   "noop",
				Driver: "raw_exec",
				Config: map[string]any{"command": "/bin/sleep", "args": []string{"3600"}},
			}},
		}},
	}

	// First register.
	if _, err := c.RegisterJob(ctx, job); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = c.DeregisterJob(ctx, id, true) })

	// Info round-trips; capture the version.
	got, err := c.GetJob(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: job=%v err=%v", got, err)
	}
	v1 := got.Version

	// Plan an identical job → expect Diff.Type "None" (no change) OR document
	// what it reports.
	changed, err := c.PlanJob(ctx, job)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	t.Logf("§6.3 spike: Plan(identical).changed = %v", changed)
	if changed {
		t.Fatalf("§6.3: Plan(identical).changed = true, want false — an identical re-plan must report no change; the decision to keep the Plan gate rests on Nomad self-dedup'ing an identical job, and this premise has regressed")
	}

	// Second identical register → does Version advance? (§6.3)
	if _, err := c.RegisterJob(ctx, job); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got2, err := c.GetJob(ctx, id)
	if err != nil || got2 == nil {
		t.Fatalf("get2: %v", err)
	}
	t.Logf("§6.3 spike: Version before=%v after identical re-register=%v (equal ⇒ Register self-dedups ⇒ Plan-gating is optional)", deref(v1), deref(got2.Version))
	if deref(v1) != deref(got2.Version) {
		t.Fatalf("§6.3: Version advanced on an identical re-register (before=%v after=%v) — Register no longer self-dedups; the Plan-gate keep-decision premise has regressed", deref(v1), deref(got2.Version))
	}

	// Deregister with purge, then Deregister the now-missing job (§6.4).
	if err := c.DeregisterJob(ctx, id, true); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	if err := c.DeregisterJob(ctx, id, true); err != nil && !IsNotFound(err) {
		t.Fatalf("§6.4: deregister-missing must be nil or 404, got %v", err)
	}

	// GetJob on a missing job → (nil, nil).
	if p, err := c.GetJob(ctx, "does-not-exist-xyz"); err != nil || p != nil {
		t.Fatalf("missing get: job=%v err=%v (want nil,nil)", p, err)
	}
}

func deref(p *uint64) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}
