package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

func minimalCluster(name, ns string) *nomadv1alpha1.NomadCluster {
	return &nomadv1alpha1.NomadCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: nomadv1alpha1.NomadClusterSpec{
			Image:   "hashicorp/nomad:2.0.4",
			Servers: 3,
			Storage: nomadv1alpha1.StorageSpec{Size: "1Gi"},
			TLS:     nomadv1alpha1.TLSSpec{CertSecretRef: "nomad-tls"},
			Gateway: nomadv1alpha1.GatewaySpec{
				Mode: nomadv1alpha1.GatewayModeManaged, ClassName: "cilium",
				RPCPorts: []int32{14647, 24647, 34647}, HTTPHostname: "nomad.example.com",
			},
		},
	}
}

var _ = Describe("NomadCluster reconcile skeleton", func() {
	It("sets Pending phase and Reconciled condition", func() {
		ctx := context.Background()
		nc := minimalCluster("skel", "default")
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(&fakeNomad{})}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "skel", Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "skel", Namespace: "default"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhasePending))
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondReconciled)).To(BeTrue())
	})

	It("rejects mutation of the immutable servers field", func() {
		ctx := context.Background()
		nc := minimalCluster("immut", "default")
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		nc.Spec.Servers = 5
		Expect(k8s.Update(ctx, nc)).NotTo(Succeed()) // CEL immutability
	})
})

func meta_IsStatusConditionTrue(conds []metav1.Condition, t string) bool {
	for _, c := range conds {
		if c.Type == t {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}
