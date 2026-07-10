package nomad

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeNomad serves the minimal endpoints the Client reads.
func fakeNomad(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"ID":"abc123","Name":"n1","Status":"ready","SchedulingEligibility":"eligible"}]`))
	})
	mux.HandleFunc("/v1/node/abc123", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ID":"abc123","Name":"n1","Status":"ready"}`))
	})
	mux.HandleFunc("/v1/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"Region":"global"},"member":{},"stats":{}}`))
	})
	return httptest.NewServer(mux)
}

func TestClientReads(t *testing.T) {
	srv := fakeNomad(t)
	defer srv.Close()

	c, err := New(Config{Address: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	nodes, err := c.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "n1" || nodes[0].Status != "ready" {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}

	node, err := c.NodeInfo(ctx, "abc123")
	if err != nil {
		t.Fatalf("NodeInfo: %v", err)
	}
	if node.ID != "abc123" {
		t.Fatalf("unexpected node: %+v", node)
	}

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error for empty Address")
	}
}
