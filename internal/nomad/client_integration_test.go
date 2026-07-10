//go:build integration

package nomad

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
)

const devAddr = "http://127.0.0.1:4646"

// startDevAgent boots `nomad agent -dev` and returns its address plus a stop
// func. It skips (not fails) when no nomad binary exists or 4646 is occupied.
func startDevAgent(t *testing.T) (addr string, stop func()) {
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

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "nomad", "agent", "-dev")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start nomad dev agent: %v", err)
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(devAddr + "/v1/agent/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return devAddr, func() { cancel(); _ = cmd.Wait() }
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	cancel()
	_ = cmd.Wait()
	t.Fatalf("nomad dev agent did not become ready within 60s")
	return "", func() {}
}

func TestDevAgentReadPath(t *testing.T) {
	addr, stop := startDevAgent(t)
	defer stop()

	c, err := New(Config{Address: addr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// The dev agent registers its client node asynchronously and transitions
	// initializing -> ready. Poll until exactly one node reports ready.
	var nodes []*api.NodeListStub
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		nodes, err = c.ListNodes(ctx)
		if err == nil && len(nodes) == 1 && nodes[0].Status == api.NodeStatusReady {
			break
		}
		time.Sleep(time.Second)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected exactly one dev node, got %d (err=%v)", len(nodes), err)
	}
	if nodes[0].Status != api.NodeStatusReady {
		t.Fatalf("node status = %q, want %q", nodes[0].Status, api.NodeStatusReady)
	}

	// Exercise the full read surface against a real Nomad.
	node, err := c.NodeInfo(ctx, nodes[0].ID)
	if err != nil {
		t.Fatalf("NodeInfo: %v", err)
	}
	if node.ID != nodes[0].ID {
		t.Fatalf("NodeInfo returned %q, want %q", node.ID, nodes[0].ID)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// Closes design open-item #1: confirm the observed status is in the
	// documented set. Fail loudly on an undocumented value.
	switch node.Status {
	case api.NodeStatusInit, api.NodeStatusReady, api.NodeStatusDown, api.NodeStatusDisconnected:
	default:
		t.Fatalf("undocumented Node.Status observed on 2.0.4: %q", node.Status)
	}
}
