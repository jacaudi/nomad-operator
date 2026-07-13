package controller

import (
	"time"

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

var _ = Describe("NomadNode reflector: drive", func() {
	It("toggles eligibility only on mismatch", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-elig-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		// Pre-create a NomadNode whose spec wants ineligible; Nomad reports eligible.
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNodeSpec{ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "e1", Eligible: false},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "e1id", Name: "e1", Status: "ready", SchedulingEligibility: "eligible"}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.eligCalls).To(HaveLen(1))
		Expect(fake.eligCalls[0]).To(Equal(eligCall{"e1id", false}))
	})

	It("does not re-issue a completed drain (converges)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-drain-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "d1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "d1",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		// record drainObservedGeneration == current generation, node already drained
		nn.Status.DrainObservedGeneration = nn.Generation
		Expect(k8s.Status().Update(ctx, nn)).To(Succeed())

		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "d1id", Name: "d1", Status: "ready", SchedulingEligibility: "ineligible", Drain: false,
				LastDrain: &api.DrainMetadata{Status: api.DrainStatusComplete}},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(BeEmpty(), "completed drain must not re-issue")
	})

	It("issues a drain when unsatisfied", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-drain2-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "d2", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "d2",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "d2id", Name: "d2", Status: "ready", SchedulingEligibility: "eligible", Drain: false}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(HaveLen(1))
		Expect(fake.drainCalls[0].spec).NotTo(BeNil())
		Expect(fake.drainCalls[0].markEligible).To(BeFalse())
	})

	It("does not re-issue while a drain is in progress", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-drain3-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "d3", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "d3",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		nn.Status.DrainObservedGeneration = nn.Generation // already issued this generation
		Expect(k8s.Status().Update(ctx, nn)).To(Succeed())

		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "d3id", Name: "d3", Status: "ready", SchedulingEligibility: "ineligible", Drain: true},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(BeEmpty(), "in-progress drain must not re-issue (deadline would slide)")
	})

	It("cancels a drain when spec.drain is removed", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-cancel-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		// CR has no spec.drain, but Nomad reports the node still draining.
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "cx1", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNodeSpec{ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "cx1", Eligible: true},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "cx1id", Name: "cx1", Status: "ready", SchedulingEligibility: "ineligible", Drain: true}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(HaveLen(1))
		Expect(fake.drainCalls[0].spec).To(BeNil(), "cancel passes a nil spec")
		Expect(fake.drainCalls[0].markEligible).To(BeTrue(), "markEligible = spec.eligible (true)")
	})
})
