package nomad

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient points a real *Client at an httptest server. Config.Validate
// (config.go) only requires Address for a plaintext endpoint; if it also
// requires Region, add Region: "global" here (check config_test.go).
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{Address: srv.URL})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

type errString string

func (e errString) Error() string { return string(e) }

func TestGetNodePool_NotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "node pool not found", http.StatusNotFound)
	})
	pool, err := c.GetNodePool(t.Context(), "missing")
	if err != nil || pool != nil {
		t.Fatalf("GetNodePool 404 = (%v, %v), want (nil, nil)", pool, err)
	}
}

func TestIsNotFound_NonURE(t *testing.T) {
	if IsNotFound(nil) || IsNotFound(errString("boom")) {
		t.Fatal("nil / non-URE errors must not be NotFound")
	}
}

func TestIsNodePoolNotEmpty_Body(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `node pool "gpu" has nodes in regions: [global]`, http.StatusBadRequest)
	})
	err := c.DeleteNodePool(t.Context(), "gpu")
	if err == nil || !IsNodePoolNotEmpty(err) {
		t.Fatalf("IsNodePoolNotEmpty(%v) = false, want true", err)
	}
	if IsNodePoolNotEmpty(nil) || IsNodePoolNotEmpty(errString("unrelated")) {
		t.Fatal("nil / unrelated errors must not be non-empty")
	}
}
