//go:build integration

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/api"
)

// TestNamespaceLifecycleLive exercises the namespace client against a real
// Nomad v2.0.4 dev agent: create a namespace, register a job into it, confirm
// DeleteNamespace is refused while the job is non-terminal (and that
// IsNamespaceNotEmpty matches the exact v2.0.4 wording — closes design §6.1),
// then deregister the job and delete the namespace. Skips if no nomad binary.
func TestNamespaceLifecycleLive(t *testing.T) {
	// devAgentWithNode(t) is the existing harness in client_write_integration_test.go
	// (starts a real `nomad agent -dev`, registers a node, returns (*Client, nodeID),
	// and skips when no nomad binary is present). Reuse it — do NOT invent a helper.
	c, _ := devAgentWithNode(t)
	ctx := t.Context()
	nsName := "it-team-a"

	if err := c.UpsertNamespace(ctx, &api.Namespace{Name: nsName, Description: "it"}); err != nil {
		t.Fatalf("UpsertNamespace: %v", err)
	}

	// A structurally valid job (Nomad rejects a job with no task groups at
	// Register with "Missing job task groups"). Mirrors TestJobLifecycleLive's
	// job body — raw_exec is healthy under `nomad agent -dev` — with
	// job.Namespace set so it lands in the managed namespace.
	id := "it-web"
	region := "global"
	typ := "service"
	grp := "app"
	count := 1
	job := &api.Job{
		ID: &id, Name: &id, Region: &region, Type: &typ, Namespace: &nsName,
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
	if _, err := c.RegisterJob(ctx, job); err != nil {
		t.Fatalf("RegisterJob: %v", err)
	}
	t.Cleanup(func() { _ = c.DeregisterJob(ctx, nsName, id, true) })

	// Delete must be refused while the job is non-terminal.
	err := c.DeleteNamespace(ctx, nsName)
	if err == nil {
		t.Fatal("DeleteNamespace succeeded with a live job; want not-empty refusal")
	}
	t.Logf("v2.0.4 non-empty Delete error body: %v", err)
	if !IsNamespaceNotEmpty(err) {
		t.Fatalf("IsNamespaceNotEmpty(%q) = false; update namespaceNotEmptyTexts to the real v2.0.4 wording", err.Error())
	}

	n, err := c.CountNamespaceJobs(ctx, nsName)
	if err != nil || n < 1 {
		t.Fatalf("CountNamespaceJobs = (%d, %v), want ≥1", n, err)
	}

	if err := c.DeregisterJob(ctx, nsName, id, true); err != nil {
		t.Fatalf("DeregisterJob: %v", err)
	}
	if err := c.DeleteNamespace(ctx, nsName); err != nil {
		t.Fatalf("DeleteNamespace after empty: %v", err)
	}
}
