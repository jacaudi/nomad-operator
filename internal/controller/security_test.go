package controller

import (
	"context"
	"encoding/base64"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

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
