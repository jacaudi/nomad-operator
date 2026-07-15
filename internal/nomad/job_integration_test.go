//go:build integration

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/api"
)

// TestJobLifecycleLive exercises the full job surface against a real Nomad
// (skips when no binary/endpoint is available, matching the other integration
// tests). It resolves the design's two remaining plan-time opens:
//   - §6.3: whether a second identical Register creates a new Version (if it
//     no-ops, Plan-gating is a KISS-optional optimization; if it churns, keep it).
//   - §6.4: Deregister on a missing job returns success/404, not an opaque error.
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
	// A trivial always-runnable job is driver-dependent; if the dev agent lacks a
	// usable driver, keep the assertions to Register/Plan/Deregister acceptance
	// (scheduling health is out of scope for this client-level spike).
	job := &api.Job{ID: &id, Name: &id, Region: &region, Type: &typ}

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

	// Second identical register → does Version advance? (§6.3)
	if _, err := c.RegisterJob(ctx, job); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got2, err := c.GetJob(ctx, id)
	if err != nil || got2 == nil {
		t.Fatalf("get2: %v", err)
	}
	t.Logf("§6.3 spike: Version before=%v after identical re-register=%v (equal ⇒ Register self-dedups ⇒ Plan-gating is optional)", deref(v1), deref(got2.Version))

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
