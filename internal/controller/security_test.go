package controller

import (
	"context"
	"encoding/base64"
	"regexp"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

func makeCertSecret(ctx context.Context, name, ns string) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("x"), "tls.key": []byte("y"), "ca.crt": []byte("z")},
	}
	Expect(k8s.Create(ctx, s)).To(Succeed())
}

var _ = Describe("ACL bootstrap idempotency", func() {
	It("writes the token Secret before bootstrap and does not re-bootstrap when the Secret exists", func() {
		ctx := context.Background()
		ns := "acl"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		nc := minimalCluster("prod", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		Expect(r.ensureBootstrapToken(ctx, nc, fake)).To(Succeed())
		Expect(fake.bootstrapped).To(BeTrue())

		var s corev1.Secret
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).TokenSecret, Namespace: ns}, &s)).To(Succeed())
		Expect(s.Data["token"]).To(Equal([]byte(fake.lastToken))) // Secret holds the supplied token

		// Second call: Secret exists → no re-bootstrap.
		fake.bootstrapped = false
		Expect(r.ensureBootstrapToken(ctx, nc, fake)).To(Succeed())
		Expect(fake.bootstrapped).To(BeFalse())
	})

	It("does not re-bootstrap when the Secret exists even if the condition was wiped", func() {
		ctx := context.Background()
		ns := "acl2"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		nc := minimalCluster("prod", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		Expect(r.ensureBootstrapToken(ctx, nc, fake)).To(Succeed())
		fake.bootstrapped = false
		nc.Status.Conditions = nil // simulate wiped status
		Expect(r.ensureBootstrapToken(ctx, nc, fake)).To(Succeed())
		Expect(fake.bootstrapped).To(BeFalse()) // gated on Secret, not condition
	})
})
