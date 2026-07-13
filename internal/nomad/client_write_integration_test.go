//go:build integration

package nomad

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
)

// devToken is a fixed, valid RFC 4122 v4 UUID literal used as the ACL
// bootstrap token. A literal is used instead of a generated UUID (e.g.
// github.com/google/uuid) to avoid promoting an indirect dependency to a
// direct one for a test fixture; api.ACLTokens.BootstrapOpts accepts any
// caller-supplied token, so determinism does not weaken the assertion below
// that it echoes back exactly the token supplied.
const devToken = "3b5e0f0a-8c1e-4c2a-9f3a-1d2e3f4a5b6c"

// startDevAgentWithACL boots an ACL-enabled `nomad agent -dev` from a
// generated config file and returns its HTTP address. It mirrors
// startDevAgent's pre-flight/readiness pattern (client_integration_test.go)
// but layers in an `acl { enabled = true }` stanza via -config, per Nomad's
// documented ACL-bootstrap testing procedure. It skips (not fails) when no
// nomad binary exists, and stops the agent via t.Cleanup.
func startDevAgentWithACL(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("nomad"); err != nil {
		t.Skip("nomad binary not found in PATH; skipping hermetic integration test")
	}
	// Pre-flight: the dev agent binds fixed ports. If 4646 is already taken,
	// skip rather than clobber a running Nomad or a leaked prior run.
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:4646", 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Skip("127.0.0.1:4646 already in use; skipping to avoid clobbering a running Nomad")
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.hcl")
	cfg := fmt.Sprintf("data_dir = %q\n\nacl {\n  enabled = true\n}\n", filepath.Join(dir, "data"))
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write agent config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "nomad", "agent", "-dev", "-config="+cfgPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start nomad dev agent: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })

	// /v1/agent/health is unauthenticated by design (used by orchestrators
	// with no token), so this readiness poll works even before ACL
	// bootstrap. Mirrors startDevAgent's poll exactly.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(devAddr + "/v1/agent/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return devAddr
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("nomad dev agent did not become ready within 60s")
	return ""
}

// devAgentWithNode boots `nomad agent -dev`, waits for its single client node to
// report ready, and returns a connected client plus that node's ID. It reuses
// startDevAgent (client_integration_test.go) for the agent bootstrap and mirrors
// the ready-node poll from TestDevAgentReadPath. Agent teardown is registered via
// t.Cleanup. Skips (not fails) when no nomad binary exists.
func devAgentWithNode(t *testing.T) (*Client, string) {
	t.Helper()
	addr, stop := startDevAgent(t)
	t.Cleanup(stop)

	c, err := New(Config{Address: addr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	// The dev agent registers its client node asynchronously and transitions
	// initializing -> ready. Poll until exactly one node reports ready.
	var nodes []*api.NodeListStub
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		nodes, err = c.ListNodes(ctx)
		if err == nil && len(nodes) == 1 && nodes[0].Status == api.NodeStatusReady {
			return c, nodes[0].ID
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("dev node did not become ready within 45s (got %d nodes, err=%v)", len(nodes), err)
	return nil, ""
}

// Requires a `nomad` v2.0.4 binary on PATH. Boots a dev agent, waits for a ready
// node, then flips scheduling eligibility to ineligible and exercises drain
// set + cancel against a real Nomad.
func TestNodeEligibilityAndDrainLive(t *testing.T) {
	c, nodeID := devAgentWithNode(t)
	ctx := t.Context()

	if err := c.SetEligibility(ctx, nodeID, false); err != nil {
		t.Fatalf("SetEligibility: %v", err)
	}
	node, err := c.NodeInfo(ctx, nodeID)
	if err != nil {
		t.Fatalf("NodeInfo: %v", err)
	}
	if node.SchedulingEligibility != "ineligible" {
		t.Errorf("eligibility = %q, want ineligible", node.SchedulingEligibility)
	}

	if err := c.UpdateDrain(ctx, nodeID, &api.DrainSpec{Deadline: time.Second}, false); err != nil {
		t.Fatalf("UpdateDrain: %v", err)
	}
	if err := c.UpdateDrain(ctx, nodeID, nil, true); err != nil { // cancel, mark eligible
		t.Fatalf("UpdateDrain cancel: %v", err)
	}
}

// Requires a `nomad` v2.0.4 binary on PATH. Boots a dev agent with ACLs enabled,
// bootstraps with an operator-supplied token, and reads the leader.
func TestACLBootstrapAndLeaderLive(t *testing.T) {
	addr := startDevAgentWithACL(t)
	c, err := New(Config{Address: addr})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Wait for leader.
	var leader string
	for range 60 {
		if leader, err = c.Leader(ctx); err == nil && leader != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if leader == "" {
		t.Fatalf("no leader elected: %v", err)
	}

	secretID, err := c.ACLBootstrap(ctx, devToken)
	if err != nil {
		t.Fatalf("ACLBootstrap: %v", err)
	}
	if secretID != devToken {
		t.Fatalf("BootstrapOpts returned %q, want supplied token %q", secretID, devToken)
	}

	// Authenticated read with the token.
	authed, err := New(Config{Address: addr, Token: secretID})
	if err != nil {
		t.Fatal(err)
	}
	if err := authed.Ping(ctx); err != nil {
		t.Fatalf("authed Ping: %v", err)
	}
	// Closes Foundation open-item #1: record the node status set.
	nodes, err := authed.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	for _, n := range nodes {
		t.Logf("node %s status=%s", n.ID, n.Status)
	}
}
