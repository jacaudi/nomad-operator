package nomad

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// aclBootstrapAlreadyDoneBody mirrors the exact body hashicorp/nomad's server
// returns from /v1/acl/bootstrap once the cluster is already bootstrapped
// (nomad/acl_endpoint.go: structs.NewErrRPCCodedf(400, "ACL bootstrap already
// done (reset index: %d)", resetIdx)).
const aclBootstrapAlreadyDoneBody = "ACL bootstrap already done (reset index: 7)"

// TestIsACLAlreadyBootstrapped_RealAlreadyBootstrappedResponse drives a real
// api.Client (via ACLBootstrap) against a fake server returning Nomad's exact
// "already bootstrapped" response, so the detection is pinned against the
// real github.com/hashicorp/nomad/api error shape rather than a guessed
// string match against our own error text.
func TestIsACLAlreadyBootstrapped_RealAlreadyBootstrappedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(aclBootstrapAlreadyDoneBody))
	}))
	defer srv.Close()

	c, err := New(Config{Address: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.ACLBootstrap(t.Context(), "3b5e0f0a-8c1e-4c2a-9f3a-1d2e3f4a5b6c")
	if err == nil {
		t.Fatal("ACLBootstrap() = nil error, want the already-bootstrapped 400")
	}
	if !IsACLAlreadyBootstrapped(err) {
		t.Fatalf("IsACLAlreadyBootstrapped(%v) = false, want true", err)
	}
}

// TestIsACLAlreadyBootstrapped_OtherErrorsAreNotMatched guards against a
// detector that's too loose: an unrelated 400 (or any non-400) must not be
// misclassified as "already bootstrapped".
func TestIsACLAlreadyBootstrapped_OtherErrorsAreNotMatched(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
	}{
		"unrelated 400":    {http.StatusBadRequest, "invalid request"},
		"500":              {http.StatusInternalServerError, "internal error"},
		"401 unauthorized": {http.StatusUnauthorized, "Permission denied"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c, err := New(Config{Address: srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.ACLBootstrap(t.Context(), "3b5e0f0a-8c1e-4c2a-9f3a-1d2e3f4a5b6c")
			if err == nil {
				t.Fatal("ACLBootstrap() = nil error, want an error")
			}
			if IsACLAlreadyBootstrapped(err) {
				t.Fatalf("IsACLAlreadyBootstrapped(%v) = true, want false", err)
			}
		})
	}
}

// TestIsACLAlreadyBootstrapped_NilAndUnrelatedErrors documents the
// non-UnexpectedResponseError edge cases.
func TestIsACLAlreadyBootstrapped_NilAndUnrelatedErrors(t *testing.T) {
	if IsACLAlreadyBootstrapped(nil) {
		t.Fatal("IsACLAlreadyBootstrapped(nil) = true, want false")
	}
	if IsACLAlreadyBootstrapped(errors.New("boom")) {
		t.Fatal("IsACLAlreadyBootstrapped(unrelated error) = true, want false")
	}
}

func TestIsNamespaceNotEmpty(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "namespace \"team-a\" has non-terminal jobs", http.StatusBadRequest)
	})
	err := c.DeleteNamespace(t.Context(), "team-a")
	if err == nil || !IsNamespaceNotEmpty(err) {
		t.Fatalf("IsNamespaceNotEmpty(%v) = false, want true", err)
	}
	if IsNamespaceNotEmpty(nil) {
		t.Fatal("IsNamespaceNotEmpty(nil) = true, want false")
	}
}
