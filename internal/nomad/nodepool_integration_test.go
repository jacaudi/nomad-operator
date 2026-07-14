//go:build integration

package nomad

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
)

// TestNodePoolLifecycleLive exercises the full node-pool surface against a
// real Nomad (skips when no binary/endpoint is available, matching the other
// integration tests in this package). It confirms Upsert/Get/Count/Delete
// round-trip against the real v2.0.4 API, then targets a re-created pool with
// a non-terminal job to trip Nomad's node-pool deletion guard and confirms
// the exact error is matched by IsNodePoolNotEmpty — closing design I-2.
func TestNodePoolLifecycleLive(t *testing.T) {
	addr, stop := startDevAgent(t)
	defer stop()

	c, err := New(Config{Address: addr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := t.Context()
	const pool = "operator-it-pool"

	// Upsert (create).
	if err := c.UpsertNodePool(ctx, &api.NodePool{Name: pool, Description: "it"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteNodePool(ctx, pool) })

	// Get round-trips.
	got, err := c.GetNodePool(ctx, pool)
	if err != nil || got == nil || got.Description != "it" {
		t.Fatalf("get: pool=%v err=%v", got, err)
	}

	// Get on a missing pool -> (nil, nil).
	if p, err := c.GetNodePool(ctx, "does-not-exist-xyz"); err != nil || p != nil {
		t.Fatalf("missing get: pool=%v err=%v (want nil,nil)", p, err)
	}

	// Counts on an empty pool.
	if n, err := c.CountNodePoolNodes(ctx, pool); err != nil || n != 0 {
		t.Fatalf("node count: %d err=%v", n, err)
	}

	// Delete an empty pool succeeds.
	if err := c.DeleteNodePool(ctx, pool); err != nil {
		t.Fatalf("delete empty: %v", err)
	}

	// Re-create the pool and target it with a job that can never place (the
	// dev agent's sole client stays in the "default" pool, so this job stays
	// queued/non-terminal for the rest of the test). This exercises the
	// "has non-terminal jobs in regions" half of Nomad's node-pool deletion
	// guard without needing a second dev-agent restart to register a node
	// directly into the pool.
	if err := c.UpsertNodePool(ctx, &api.NodePool{Name: pool}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	job := api.NewServiceJob("operator-it-job", "operator-it-job", "global", 50)
	job.Datacenters = []string{"dc1"}
	job.NodePool = new(pool)
	task := api.NewTask("task", "raw_exec")
	task.Config = map[string]any{"command": "/bin/sleep", "args": []string{"3600"}}
	task.Require(&api.Resources{CPU: new(100), MemoryMB: new(128)})
	job.AddTaskGroup(api.NewTaskGroup("group", 1).AddTask(task))

	if _, _, err := c.api.Jobs().Register(job, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		t.Fatalf("register job: %v", err)
	}
	t.Cleanup(func() {
		_, _, _ = c.api.Jobs().Deregister(*job.ID, true, (&api.WriteOptions{}).WithContext(ctx))
	})

	err = c.DeleteNodePool(ctx, pool)
	if err == nil {
		t.Fatalf("delete non-empty pool: want error, got nil")
	}
	t.Logf("v2.0.4 non-empty Delete error body: %v", err)
	if !IsNodePoolNotEmpty(err) {
		t.Errorf("IsNodePoolNotEmpty(%v) = false, want true — update nodePoolNotEmptyTexts in errors.go with the logged wording above", err)
	}

	// Deregister the job (purge) so the pool is empty again, then confirm
	// Delete succeeds once the guard clears. Deregistration is asynchronous
	// (it triggers an evaluation), so poll rather than delete immediately.
	if _, _, err := c.api.Jobs().Deregister(*job.ID, true, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		t.Fatalf("deregister job: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := c.DeleteNodePool(ctx, pool); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("delete after job removed: %v (timed out waiting for guard to clear)", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
