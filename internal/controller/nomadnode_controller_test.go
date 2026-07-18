package controller

import (
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hashicorp/nomad/api"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

	It("signals DrainSpecPendingRestart when the drain spec is edited mid-drain, without re-issuing", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-drain-edit-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		// spec wants a 30m deadline; Nomad reports the in-flight drain at 1h.
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "de1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "de1",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: 30 * time.Minute}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		// Generation bumped by the edit; drain never observed at this generation.
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "de1", Namespace: ns.Name}, nn)).To(Succeed())

		fake := &fakeNodeOps{
			list: []*api.NodeListStub{{ID: "de1id", Name: "de1", Status: "ready", SchedulingEligibility: "ineligible", Drain: true}},
			info: map[string]*api.Node{
				"de1id": {DrainStrategy: &api.DrainStrategy{DrainSpec: api.DrainSpec{Deadline: time.Hour}}},
			},
		}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(BeEmpty(), "an edited mid-drain spec must not re-issue (deadline would restart)")

		got := &nomadv1alpha1.NomadNode{}
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "de1", Namespace: ns.Name}, got)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.NomadNodeCondDrainSpecPendingRestart)).
			To(BeTrue(), "the ignored edit must be surfaced as a condition")
	})

	It("does not signal DrainSpecPendingRestart when a freshly-adopted drain matches Nomad", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-drain-adopt-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		// spec deadline equals the in-flight drain deadline (as seedDrain would set it).
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "da1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "da1",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		fake := &fakeNodeOps{
			list: []*api.NodeListStub{{ID: "da1id", Name: "da1", Status: "ready", SchedulingEligibility: "ineligible", Drain: true}},
			info: map[string]*api.Node{
				"da1id": {DrainStrategy: &api.DrainStrategy{DrainSpec: api.DrainSpec{Deadline: time.Hour}}},
			},
		}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(BeEmpty(), "an in-sync adopted drain must not re-issue")

		got := &nomadv1alpha1.NomadNode{}
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "da1", Namespace: ns.Name}, got)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.NomadNodeCondDrainSpecPendingRestart)).
			To(BeFalse(), "a matching adopted drain must not false-fire the signal")
	})

	It("clears DrainSpecPendingRestart (True->False) when the edit is reverted mid-drain", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-drain-revert-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		// spec wants 30m; Nomad reports the in-flight drain at 1h -> diverged.
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "dr1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "dr1",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: 30 * time.Minute}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		fake := &fakeNodeOps{
			list: []*api.NodeListStub{{ID: "dr1id", Name: "dr1", Status: "ready", SchedulingEligibility: "ineligible", Drain: true}},
			info: map[string]*api.Node{
				"dr1id": {DrainStrategy: &api.DrainStrategy{DrainSpec: api.DrainSpec{Deadline: time.Hour}}},
			},
		}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		key := types.NamespacedName{Name: nc.Name, Namespace: ns.Name}

		// First pass: diverged edit -> condition True.
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		got := &nomadv1alpha1.NomadNode{}
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "dr1", Namespace: ns.Name}, got)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.NomadNodeCondDrainSpecPendingRestart)).
			To(BeTrue(), "diverged edit must first set the condition True")

		// Revert the spec to match the in-flight drain (1h); this bumps the generation.
		got.Spec.Drain.Deadline = &metav1.Duration{Duration: time.Hour}
		Expect(k8s.Update(ctx, got)).To(Succeed())

		// Second pass: spec now matches live -> condition transitions to False/InSync.
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(BeEmpty(), "revert-while-draining must not re-issue")
		got = &nomadv1alpha1.NomadNode{}
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "dr1", Namespace: ns.Name}, got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNodeCondDrainSpecPendingRestart)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse), "reverted spec must clear the pending-restart signal")
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonDrainSpecInSync))
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

	It("persists DrainObservedGeneration when it issues a drain, independent of mirrorStatus (L-1)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-l1-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "l1",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "l1", Namespace: ns.Name}, nn)).To(Succeed())

		fake := &fakeNodeOps{}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		stub := &api.NodeListStub{ID: "l1id", Name: "l1", Status: "ready", SchedulingEligibility: "eligible", Drain: false}

		// Call driveDesired directly — NOT the full Reconcile — so mirrorStatus never runs.
		Expect(r.driveDesired(ctx, nn, stub, fake)).To(Succeed())
		Expect(fake.drainCalls).To(HaveLen(1))

		var got nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "l1", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.Status.DrainObservedGeneration).To(Equal(nn.Generation),
			"driveDesired must persist the generation itself, not rely on mirrorStatus")
	})

	It("does not re-issue a drain across passes via the persisted generation (B1)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-b1-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "b1",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		// Pass 1: node not yet draining -> drain issued, generation persisted.
		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "b1id", Name: "b1", Status: "ready", SchedulingEligibility: "eligible", Drain: false},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(HaveLen(1))

		var mid nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "b1", Namespace: ns.Name}, &mid)).To(Succeed())
		Expect(mid.Status.DrainObservedGeneration).To(Equal(mid.Generation))

		// Pass 2: node now draining (in progress) -> must NOT re-issue.
		fake.list[0].Drain = true
		fake.list[0].SchedulingEligibility = "ineligible"
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(HaveLen(1), "pass 2 must not re-issue via the persisted generation")
	})

	It("adopts an already-draining node without re-issuing the drain (L-3)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-l3-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		// No pre-created CR: this pass MINTS it from an already-draining node.
		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "l3id", Name: "l3", Status: "ready", SchedulingEligibility: "ineligible", Drain: true},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(BeEmpty(), "adopting an in-progress drain must not re-issue (deadline would restart)")

		var got nomadv1alpha1.NomadNode
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "l3", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.Status.DrainObservedGeneration).To(Equal(got.Generation))
	})

	It("re-issues a drain when it was cancelled out-of-band (B2)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-b2-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "b2", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "b2",
				Drain: &nomadv1alpha1.NodeDrainSpec{Deadline: &metav1.Duration{Duration: time.Hour}},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "b2", Namespace: ns.Name}, nn)).To(Succeed())
		nn.Status.DrainObservedGeneration = nn.Generation // we already issued it this generation
		Expect(k8s.Status().Update(ctx, nn)).To(Succeed())

		// Out-of-band cancel: Nomad reports NOT draining, and the last drain is not complete.
		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "b2id", Name: "b2", Status: "ready", SchedulingEligibility: "eligible", Drain: false, LastDrain: nil},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.drainCalls).To(HaveLen(1), "an out-of-band-cancelled drain must be re-issued to satisfy spec")
	})

	It("does not call SetEligibility when eligibility already matches (B3)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-b3-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "b3", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNodeSpec{ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "b3", Eligible: true},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		// Nomad already reports eligible -> compare-before-write must be a no-op.
		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "b3id", Name: "b3", Status: "ready", SchedulingEligibility: "eligible"}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.eligCalls).To(BeEmpty(), "matching eligibility must not trigger a write")
	})
})

var _ = Describe("NomadNode reflector: prune + cascade", func() {
	It("does NOT mass-prune when a successful list is unexpectedly empty (L-2)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-l2-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		// Pass 1: node present -> CR minted.
		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "g1", Name: "ghost", Status: "ready", SchedulingEligibility: "eligible"}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "ghost", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})).To(Succeed())

		// Pass 2: empty (but successful) list while a CR exists -> NOT pruned
		// (a suspect full-empty must not mass-delete; scale-to-zero retains CRs).
		fake.list = nil
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "ghost", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})).To(Succeed(),
			"an unexpectedly-empty list must not mass-prune existing CRs (L-2)")
	})

	It("still prunes an absent node from a NON-empty successful list", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-l2b-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "g1", Name: "ghost", Status: "ready", SchedulingEligibility: "eligible"},
			{ID: "k1", Name: "keep", Status: "ready", SchedulingEligibility: "eligible"},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		// "ghost" gone from a still-non-empty list -> pruned; "keep" remains.
		fake.list = []*api.NodeListStub{{ID: "k1", Name: "keep", Status: "ready", SchedulingEligibility: "eligible"}}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		err = k8s.Get(ctx, types.NamespacedName{Name: "ghost", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "keep", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})).To(Succeed())
	})

	It("does not prune when the list fails", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-noprune-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		fake := &fakeNodeOps{list: []*api.NodeListStub{{ID: "k1", Name: "keep", Status: "ready", SchedulingEligibility: "eligible"}}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		fake.listErr = errors.New("unreachable")
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "keep", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})).To(Succeed(), "must survive a list error")
	})

	It("keeps reflecting the cluster when one node's upsert fails", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-nonfatal-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		// A pre-existing CR whose desired eligibility mismatches Nomad, so
		// driveDesired calls SetEligibility — which we make fail for this node.
		bad := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns.Name, Labels: names(nc).Labels()},
			Spec:       nomadv1alpha1.NomadNodeSpec{ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "bad", Eligible: false},
		}
		Expect(k8s.Create(ctx, bad)).To(Succeed())
		// A stale CR whose node is absent from the list: pruneAbsent must delete
		// it. Under a fatal loop, bad's error returns before pruneAbsent ever
		// runs, so ghost would survive — that is the RED this asserts against.
		ghost := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "ghost", Namespace: ns.Name, Labels: names(nc).Labels()},
			Spec:       nomadv1alpha1.NomadNodeSpec{ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "ghost"},
		}
		Expect(k8s.Create(ctx, ghost)).To(Succeed())

		fake := &fakeNodeOps{
			list:    []*api.NodeListStub{{ID: "badid", Name: "bad", Status: "ready", SchedulingEligibility: "eligible"}},
			eligErr: map[string]error{"badid": errors.New("nomad rejected eligibility toggle")},
		}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).To(HaveOccurred(), "the node's transient failure is surfaced so the reconcile retries")

		// pruneAbsent still ran despite bad's failure: the absent node's CR is gone.
		gerr := k8s.Get(ctx, types.NamespacedName{Name: "ghost", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})
		Expect(apierrors.IsNotFound(gerr)).To(BeTrue(), "absent node pruned even though another node failed")
		// The failed node's own CR is NOT pruned (it stays in bound).
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "bad", Namespace: ns.Name}, &nomadv1alpha1.NomadNode{})).
			To(Succeed(), "a node that failed to reconcile must not have its CR deleted")
	})

	It("flags ClusterNotReady on existing nodes when the cluster is not Ready", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-notready-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: ns.Name, Labels: names(nc).Labels()},
			Spec:       nomadv1alpha1.NomadNodeSpec{ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: "stale"},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		nc.Status.Phase = nomadv1alpha1.PhaseDegraded // cluster drops out of Ready
		Expect(k8s.Status().Update(ctx, nc)).To(Succeed())

		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(&fakeNodeOps{})}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8s.Get(ctx, types.NamespacedName{Name: "stale", Namespace: ns.Name}, nn)).To(Succeed())
		cond := meta.FindStatusCondition(nn.Status.Conditions, nomadv1alpha1.NomadNodeCondReconciled)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotReady))
	})

	It("picks a deterministic owner across sanitize-collisions and does not flap (M-1)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-m1-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		// "web_1" and "web-1" both sanitize to "web-1" (sanitizeNodeName maps the
		// underscore to a dash but preserves a literal dash unchanged). Lower
		// CreateIndex owns.
		fake := &fakeNodeOps{list: []*api.NodeListStub{
			{ID: "a", Name: "web_1", Status: "ready", SchedulingEligibility: "eligible", CreateIndex: 10},
			{ID: "b", Name: "web-1", Status: "ready", SchedulingEligibility: "eligible", CreateIndex: 20},
		}}
		r := &NomadNodeReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeNodeFactory(fake)}

		assertOwner := func() {
			var nn nomadv1alpha1.NomadNode
			Expect(k8s.Get(ctx, types.NamespacedName{Name: "web-1", Namespace: ns.Name}, &nn)).To(Succeed())
			Expect(nn.Spec.NodeName).To(Equal("web_1"), "lowest CreateIndex owns the object name")
			Expect(meta.IsStatusConditionTrue(nn.Status.Conditions, nomadv1alpha1.NomadNodeCondReconciled)).To(BeTrue(),
				"owner CR must stay Reconciled=True, not flap to DuplicateNodeName")
			// The colliding loser mints no CR of its own.
			var list nomadv1alpha1.NomadNodeList
			Expect(k8s.List(ctx, &list, client.InNamespace(ns.Name))).To(Succeed())
			Expect(list.Items).To(HaveLen(1))
		}

		// Run twice; ownership + condition must be stable across passes.
		for range 2 {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nc.Name, Namespace: ns.Name}})
			Expect(err).NotTo(HaveOccurred())
			assertOwner()
		}
	})
})
