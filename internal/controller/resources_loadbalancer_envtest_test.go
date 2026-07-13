package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

var _ = Describe("LoadBalancer external-access mode", func() {
	It("provisions an LB Service, gates on ingress, and reaches Ready with no Gateway objects", func() {
		ctx := context.Background()
		ns := "lbmode"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, "nomad-tls", ns)
		nc := lbCluster("edge", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		fake := &fakeNomad{leader: "203.0.113.7:4647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		// First reconcile creates the LB Service (no ingress yet) → Pending.
		reconcileOnce(r, "edge", ns)
		var svc corev1.Service
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).LBService, Namespace: ns}, &svc)).To(Succeed())
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))

		var afterFirst nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "edge", Namespace: ns}, &afterFirst)).To(Succeed())
		Expect(afterFirst.Status.Phase).To(Equal(nomadv1alpha1.PhasePending))

		// Stub the ingress address (envtest runs no LB provider).
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}
		Expect(k8s.Status().Update(ctx, &svc)).To(Succeed())

		// Second reconcile provisions the shared workloads and reaches Ready.
		reconcileOnce(r, "edge", ns)

		var afterSecond nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "edge", Namespace: ns}, &afterSecond)).To(Succeed())
		Expect(afterSecond.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))
		Expect(afterSecond.Status.ExternalAddress).To(Equal("203.0.113.7"))

		// Shared workloads exist; the ConfigMap advertises rpc_ports "4647".
		var ss appsv1.StatefulSet
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).StatefulSet, Namespace: ns}, &ss)).To(Succeed())
		var cm corev1.ConfigMap
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).ConfigMap, Namespace: ns}, &cm)).To(Succeed())
		Expect(cm.Data["rpc_ports"]).To(Equal("4647"))
		Expect(cm.Data["gateway_address"]).To(Equal("203.0.113.7"))

		// No Gateway-mode objects were created.
		var gw gwapiv1.Gateway
		errGw := k8s.Get(ctx, types.NamespacedName{Name: names(nc).Gateway, Namespace: ns}, &gw)
		Expect(errGw).To(HaveOccurred())
		var podSvc corev1.Service
		errPod := k8s.Get(ctx, types.NamespacedName{Name: names(nc).PodSvc(0), Namespace: ns}, &podSvc)
		Expect(errPod).To(HaveOccurred())
	})
})
