package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hashicorp/nomad/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

// readyCluster creates a NomadCluster already in Ready phase + a cert Secret,
// so the node reflector will build a client and list nodes.
func readyCluster(ctx SpecContext, ns string) *nomadv1alpha1.NomadCluster {
	nc := &nomadv1alpha1.NomadCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: ns},
		Spec: nomadv1alpha1.NomadClusterSpec{
			Image: "hashicorp/nomad:2.0.4", Servers: 1, Region: "global",
			Storage: nomadv1alpha1.StorageSpec{Size: "1Gi"},
			TLS:     nomadv1alpha1.TLSSpec{CertSecretRef: "cert"},
			ExternalAccess: nomadv1alpha1.ExternalAccessSpec{
				Mode:         nomadv1alpha1.ExternalAccessLoadBalancer,
				LoadBalancer: &nomadv1alpha1.LoadBalancerSpec{},
			},
		},
	}
	Expect(k8s.Create(ctx, nc)).To(Succeed())
	nc.Status.Phase = nomadv1alpha1.PhaseReady
	Expect(k8s.Status().Update(ctx, nc)).To(Succeed())
	cert := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cert", Namespace: ns},
		Data:       map[string][]byte{"ca.crt": []byte("CA"), "tls.crt": []byte("CRT"), "tls.key": []byte("KEY")},
	}
	Expect(k8s.Create(ctx, cert)).To(Succeed())
	return nc
}

var _ = Describe("NomadNode reflector: mint", func() {
	It("mints a NomadNode with seeded spec, mirrored status, and an ownerRef", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-mint-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "id-abc", Name: "TrueNAS-01", Status: "ready", SchedulingEligibility: "ineligible", NodePool: "default", NodeClass: "truenas", Datacenter: "dc1"},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var nn nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "truenas-01", Namespace: ns.Name}, &nn)).To(Succeed())
		Expect(nn.Spec.NodeName).To(Equal("TrueNAS-01"))
		Expect(nn.Spec.Eligible).To(BeFalse(), "seeded from observed ineligible")
		Expect(nn.Status.NodeID).To(Equal("id-abc"))
		Expect(nn.Status.Status).To(Equal("ready"))
		Expect(nn.Status.NodePool).To(Equal("default"))
		Expect(nn.OwnerReferences).To(HaveLen(1))
		Expect(nn.OwnerReferences[0].Name).To(Equal(nc.Name))
	})
})

var _ = Describe("NomadNode reflector: disambiguation", func() {
	It("binds the live stub when a down straggler shares its Name", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-dis-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		// Live stub FIRST, down straggler LAST: a naive last-wins bindNodes would
		// pick the down straggler, so this test genuinely fails before the fix.
		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "new", Name: "box", Status: "ready", SchedulingEligibility: "eligible", CreateIndex: 9},
			{ID: "old", Name: "box", Status: "down", CreateIndex: 1},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var nn nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "box", Namespace: ns.Name}, &nn)).To(Succeed())
		Expect(nn.Status.NodeID).To(Equal("new"), "bound to the live, not the down straggler")
		Expect(nn.Status.Status).To(Equal("ready"))
	})

	It("flags DuplicateNodeName when two non-down stubs share a Name", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-dup-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "a", Name: "twin", Status: "ready", CreateIndex: 1},
			{ID: "b", Name: "twin", Status: "ready", CreateIndex: 2},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var nn nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "twin", Namespace: ns.Name}, &nn)).To(Succeed())
		cond := meta.FindStatusCondition(nn.Status.Conditions, nomadv1alpha1.NomadNodeCondReconciled)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonDuplicateNodeName))
	})
})
