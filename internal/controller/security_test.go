package controller

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewBootstrapToken asserts the crypto/rand-minted token is a well-formed
// version-4 UUID string: 36 chars, 8-4-4-4-12 hex groups, version nibble "4".
func TestNewBootstrapToken(t *testing.T) {
	tok, err := newBootstrapToken()
	if err != nil {
		t.Fatalf("newBootstrapToken() error = %v, want nil", err)
	}
	if len(tok) != 36 {
		t.Fatalf("newBootstrapToken() len = %d, want 36 (got %q)", len(tok), tok)
	}
	if !uuidV4Pattern.MatchString(tok) {
		t.Fatalf("newBootstrapToken() = %q, want 8-4-4-4-12 hex UUID with version nibble 4", tok)
	}
}

var _ = Describe("gossip key", func() {
	It("generates a 32-byte key once and is idempotent", func() {
		ctx := context.Background()
		nc := minimalCluster("gk", "default")
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(&fakeNomad{})}

		name1, err := r.ensureGossipKey(ctx, nc)
		Expect(err).NotTo(HaveOccurred())

		var s corev1.Secret
		Expect(k8s.Get(ctx, types.NamespacedName{Name: name1, Namespace: "default"}, &s)).To(Succeed())
		raw, err := base64.StdEncoding.DecodeString(string(s.Data["key"]))
		Expect(err).NotTo(HaveOccurred())
		Expect(raw).To(HaveLen(32))

		name2, err := r.ensureGossipKey(ctx, nc)
		Expect(err).NotTo(HaveOccurred())
		Expect(name2).To(Equal(name1))
		var s2 corev1.Secret
		Expect(k8s.Get(ctx, types.NamespacedName{Name: name2, Namespace: "default"}, &s2)).To(Succeed())
		Expect(s2.Data["key"]).To(Equal(s.Data["key"])) // not regenerated
	})
})

const testCertSecretName = "nomad-tls"

func makeCertSecret(ctx context.Context, ns string) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testCertSecretName, Namespace: ns},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("x"), "tls.key": []byte("y"), "ca.crt": []byte("z")},
	}
	Expect(k8s.Create(ctx, s)).To(Succeed())
}

var _ = Describe("ACL bootstrap idempotency", func() {
	It("writes the token Secret before bootstrap and re-attempts idempotent bootstrap every reconcile", func() {
		ctx := context.Background()
		ns := "acl"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		nc := minimalCluster("prod", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		Expect(r.ensureBootstrapToken(ctx, nc, fake)).To(Succeed())
		Expect(fake.bootstrapCalls).To(Equal(1))

		var s corev1.Secret
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).TokenSecret, Namespace: ns}, &s)).To(Succeed())
		Expect(s.Data["token"]).To(Equal([]byte(fake.lastToken)))          // Secret holds the supplied token
		Expect(s.Annotations[annotationACLBootstrapped]).To(Equal("true")) // durably confirmed
		rvAfterFirst := s.ResourceVersion

		// Second call: the idempotent bootstrap is attempted again (a fresh
		// recreated cluster can only be detected by asking Nomad), but the
		// already-annotated Secret is NOT rewritten (no resourceVersion churn).
		Expect(r.ensureBootstrapToken(ctx, nc, fake)).To(Succeed())
		Expect(fake.bootstrapCalls).To(Equal(2))
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).TokenSecret, Namespace: ns}, &s)).To(Succeed())
		Expect(s.ResourceVersion).To(Equal(rvAfterFirst)) // no spurious Update
	})
})

var _ = Describe("ACL re-bootstrap on recreate", func() {
	It("re-attempts bootstrap with the stored token when a retained annotated Secret meets a fresh (un-bootstrapped) cluster", func() {
		ctx := context.Background()
		ns := "acl-recreate"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		nc := minimalCluster("prod", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		// Simulate the retained-across-delete token Secret: it already carries the
		// durable acl-bootstrapped=true annotation from the PRIOR cluster's
		// lifetime, but the recreated Nomad is brand new and un-bootstrapped.
		const storedToken = "3b5e0f0a-8c1e-4c2a-9f3a-1d2e3f4a5b6c"
		retained := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        names(nc).TokenSecret,
				Namespace:   ns,
				Annotations: map[string]string{annotationACLBootstrapped: "true"},
			},
			Data: map[string][]byte{"token": []byte(storedToken)},
		}
		Expect(k8s.Create(ctx, retained)).To(Succeed())

		// Fresh cluster: ACLBootstrap has NOT been done, so the fake accepts the
		// operator-supplied token (bootstrapped=false → ACLBootstrap succeeds).
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		Expect(r.ensureBootstrapToken(ctx, nc, fake)).To(Succeed())

		// The core regression: the stale annotation must NOT short-circuit the
		// call. ACLBootstrap must run with the retained token so the fresh
		// cluster registers it (self-heal) rather than being left with a dead one.
		Expect(fake.bootstrapCalls).To(Equal(1))
		Expect(fake.lastToken).To(Equal(storedToken))
		Expect(fake.bootstrapped).To(BeTrue())
	})
})

var _ = Describe("ACL bootstrap already-bootstrapped self-heal", func() {
	It("tolerates the already-bootstrapped response without error or Secret churn across reconciles", func() {
		ctx := context.Background()
		ns := "acl-already"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		nc := minimalCluster("prod", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		// A real *nomad.Client against a fake server returning Nomad's exact
		// "already bootstrapped" response (see internal/nomad/errors_test.go),
		// so this exercises the real nomad.IsACLAlreadyBootstrapped detection
		// end to end rather than a hand-rolled error the fake invents.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("ACL bootstrap already done (reset index: 7)"))
		}))
		defer srv.Close()
		ops, err := nomad.New(nomad.Config{Address: srv.URL})
		Expect(err).NotTo(HaveOccurred())

		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(&fakeNomad{})}

		Expect(r.ensureBootstrapToken(ctx, nc, ops)).To(Succeed())

		var s corev1.Secret
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).TokenSecret, Namespace: ns}, &s)).To(Succeed())
		Expect(s.Annotations[annotationACLBootstrapped]).To(Equal("true"))
		rvAfterFirst := s.ResourceVersion

		// Steady state: the already-bootstrapped response keeps coming back but
		// must not error and must not rewrite the already-annotated Secret.
		Expect(r.ensureBootstrapToken(ctx, nc, ops)).To(Succeed())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).TokenSecret, Namespace: ns}, &s)).To(Succeed())
		Expect(s.ResourceVersion).To(Equal(rvAfterFirst)) // no spurious Update
	})
})

var _ = Describe("teardown retention", func() {
	It("does not own the token/gossip secrets (retained on delete)", func() {
		ctx := context.Background()
		ns := "teardown"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		nc := minimalCluster("prod", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		_, _ = r.ensureGossipKey(ctx, nc)
		_ = r.ensureBootstrapToken(ctx, nc, fake)
		var g, tk corev1.Secret
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).GossipSecret, Namespace: ns}, &g)).To(Succeed())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).TokenSecret, Namespace: ns}, &tk)).To(Succeed())
		Expect(g.OwnerReferences).To(BeEmpty())
		Expect(tk.OwnerReferences).To(BeEmpty())
	})
})
