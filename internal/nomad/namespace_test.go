package nomad

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hashicorp/nomad/api"
)

func TestGetNamespace_NotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "namespace not found", http.StatusNotFound)
	})
	ns, err := c.GetNamespace(t.Context(), "missing")
	if err != nil || ns != nil {
		t.Fatalf("GetNamespace 404 = (%v, %v), want (nil, nil)", ns, err)
	}
}

func TestGetNamespace_Found(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Name":"team-a","Description":"d"}`))
	})
	ns, err := c.GetNamespace(t.Context(), "team-a")
	if err != nil || ns == nil || ns.Name != "team-a" {
		t.Fatalf("GetNamespace = (%v, %v), want name team-a", ns, err)
	}
}

func TestUpsertNamespace_PostsName(t *testing.T) {
	var gotPath, gotName string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var ns api.Namespace
		_ = json.NewDecoder(r.Body).Decode(&ns)
		gotName = ns.Name
		w.WriteHeader(http.StatusOK)
	})
	if err := c.UpsertNamespace(t.Context(), &api.Namespace{Name: "team-a"}); err != nil {
		t.Fatalf("UpsertNamespace: %v", err)
	}
	// Register PUTs to the FIXED /v1/namespace path with the name in the JSON
	// body (not /v1/namespace/<name>).
	if gotPath != "/v1/namespace" || gotName != "team-a" {
		t.Fatalf("Register = path %q name %q, want /v1/namespace + team-a", gotPath, gotName)
	}
}

func TestDeleteNamespace_DeletesByName(t *testing.T) {
	var gotMethod, gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	if err := c.DeleteNamespace(t.Context(), "team-a"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/namespace/team-a" {
		t.Fatalf("delete = %s %s, want DELETE /v1/namespace/team-a", gotMethod, gotPath)
	}
}

func TestCountNamespaceJobs_ScopesByNamespace(t *testing.T) {
	var gotNS string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotNS = r.URL.Query().Get("namespace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"ID":"a"},{"ID":"b"}]`))
	})
	n, err := c.CountNamespaceJobs(t.Context(), "team-a")
	if err != nil || n != 2 {
		t.Fatalf("CountNamespaceJobs = (%d, %v), want (2, nil)", n, err)
	}
	if gotNS != "team-a" {
		t.Fatalf("namespace query = %q, want team-a", gotNS)
	}
}
