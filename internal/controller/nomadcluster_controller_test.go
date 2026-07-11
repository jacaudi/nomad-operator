package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

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

func meta_IsStatusConditionFalse(conds []metav1.Condition, t string) bool {
	for _, c := range conds {
		if c.Type == t {
			return c.Status == metav1.ConditionFalse
		}
	}
	return false
}

var _ = Describe("Managed provisioning", func() {
	It("creates workloads and routes and reaches Ready when gateway+cert are ready and the fake reports a leader", func() {
		ctx := context.Background()
		ns := "mgd"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, "nomad-tls", ns)
		nc := minimalCluster("prod", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		// Pre-create the Gateway with an assigned address (envtest runs no Gateway controller).
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		// First reconcile creates the Gateway (no address yet) → Pending.
		reconcileOnce(r, "prod", ns)
		gwName := names(nc).Gateway
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: gwName, Namespace: ns}, &gw)).To(Succeed())

		var afterFirst nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "prod", Namespace: ns}, &afterFirst)).To(Succeed())
		Expect(afterFirst.Status.Phase).To(Equal(nomadv1alpha1.PhasePending))

		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())

		// Second reconcile provisions workloads → Bootstrapping.
		reconcileOnce(r, "prod", ns)

		var ss appsv1.StatefulSet
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).StatefulSet, Namespace: ns}, &ss)).To(Succeed())
		Expect(ss.Spec.PodManagementPolicy).To(Equal(appsv1.ParallelPodManagement))
		var tcp gwapiv1a2.TCPRoute
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "prod-rpc-0", Namespace: ns}, &tcp)).To(Succeed())

		var afterSecond nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "prod", Namespace: ns}, &afterSecond)).To(Succeed())
		// Task 8: the fake reports a leader and bootstraps ACLs successfully, so
		// the same reconcile that provisions workloads also completes bootstrap
		// and reaches Ready (not just Bootstrapping).
		Expect(afterSecond.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))
	})

	It("persists Bootstrapping (not Ready) and CondQuorumHealthy=False when the fake reports no leader", func() {
		ctx := context.Background()
		ns := "mgd-noleader"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, "nomad-tls", ns)
		nc := minimalCluster("noleader", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		// No leader reported: fakeNomad.Leader() returns an error when leader=="".
		fake := &fakeNomad{}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		// First reconcile creates the Gateway (no address yet) → Pending.
		reconcileOnce(r, "noleader", ns)
		gwName := names(nc).Gateway
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: gwName, Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())

		// Second reconcile provisions workloads and reaches bootstrapAndReady,
		// where the no-leader branch must leave Phase at Bootstrapping and
		// mark CondQuorumHealthy False (nomadcluster_controller.go ~197-211).
		reconcileOnce(r, "noleader", ns)

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "noleader", Namespace: ns}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseBootstrapping))
		Expect(meta_IsStatusConditionFalse(got.Status.Conditions, nomadv1alpha1.CondQuorumHealthy)).To(BeTrue())
	})

	It("transitions Ready to Degraded (QuorumLost) when the fake later reports no leader", func() {
		ctx := context.Background()
		ns := "mgd-quorumlost"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, "nomad-tls", ns)
		nc := minimalCluster("quorumlost", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		// Same mutable fake+reconciler reused across all three reconciles below,
		// matching the pattern in the two specs above.
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		// First reconcile creates the Gateway (no address yet) → Pending.
		reconcileOnce(r, "quorumlost", ns)
		gwName := names(nc).Gateway
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: gwName, Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())

		// Second reconcile provisions workloads and, with a leader reported,
		// reaches Ready.
		reconcileOnce(r, "quorumlost", ns)
		var afterReady nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "quorumlost", Namespace: ns}, &afterReady)).To(Succeed())
		Expect(afterReady.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))

		// Mutate the same fake to report no leader, then reconcile again. This
		// exercises bootstrapAndReady's Ready->Degraded guard
		// (nomadcluster_controller.go ~202-206), which requires Reconcile to
		// preserve the Ready phase instead of clobbering it to Bootstrapping
		// before calling bootstrapAndReady (nomadcluster_controller.go ~143).
		fake.leader = ""
		fake.serverHealthy = false
		reconcileOnce(r, "quorumlost", ns)

		var afterLoss nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "quorumlost", Namespace: ns}, &afterLoss)).To(Succeed())
		Expect(afterLoss.Status.Phase).To(Equal(nomadv1alpha1.PhaseDegraded))
		Expect(meta_IsStatusConditionFalse(afterLoss.Status.Conditions, nomadv1alpha1.CondReady)).To(BeTrue())
		reason := ""
		for _, c := range afterLoss.Status.Conditions {
			if c.Type == nomadv1alpha1.CondReady {
				reason = c.Reason
			}
		}
		Expect(reason).To(Equal("QuorumLost"))
	})
})

func reconcileOnce(r *NomadClusterReconciler, name, ns string) {
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	Expect(err).NotTo(HaveOccurred())
}
