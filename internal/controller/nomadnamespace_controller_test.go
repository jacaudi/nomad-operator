package controller

import (
	"github.com/hashicorp/nomad/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

var _ = Describe("NomadNamespace reconciler: cluster resolution", func() {
	It("sets ClusterNotFound and adds the finalizer when the cluster is missing", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-notfound-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef:    nomadv1alpha1.NamespaceClusterRef{Name: "missing"},
				NamespaceName: "team-a",
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotFound))
		Expect(controllerutil.ContainsFinalizer(&got, nomadNamespaceFinalizer)).To(BeTrue(), "finalizer not added")
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
	})

	It("sets ClusterNotReady and registers nothing when the cluster is not Ready", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-notready-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nc := readyCluster(ctx, ns.Name)
		nc.Status.Phase = nomadv1alpha1.PhaseDegraded
		Expect(k8s.Status().Update(ctx, nc)).To(Succeed())

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef:    nomadv1alpha1.NamespaceClusterRef{Name: nc.Name},
				NamespaceName: "team-a",
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotReady))
		Expect(f.registered).To(BeEmpty(), "must not Register when cluster not Ready")
	})
})

var _ = Describe("NomadNamespace reconciler: apply", func() {
	It("registers on create and preserves unmanaged fields on update", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-apply-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef:    nomadv1alpha1.NamespaceClusterRef{Name: nc.Name},
				NamespaceName: "team-a",
				Description:   "Team A",
				Meta:          map[string]string{"owner": "team-a"},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		f := newFakeNamespaceOps()
		// Seed an existing namespace carrying an unmanaged Quota to prove preservation.
		f.namespaces["team-a"] = &api.Namespace{Name: "team-a", Quota: "q1", Description: "old"}
		f.jobCount = 3
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(f.registered).To(HaveLen(1))
		Expect(f.registered[0].Description).To(Equal("Team A"))
		Expect(f.registered[0].Meta).To(Equal(map[string]string{"owner": "team-a"}))
		Expect(f.registered[0].Quota).To(Equal("q1"), "unmanaged Quota must be preserved")

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.Status.JobCount).To(Equal(3))
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("does not re-register when description and meta are unchanged", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-nodrift-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: nc.Name}, NamespaceName: "team-a",
				Description: "same", Meta: map[string]string{"k": "v"},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		f := newFakeNamespaceOps()
		f.namespaces["team-a"] = &api.Namespace{Name: "team-a", Description: "same", Meta: map[string]string{"k": "v"}}
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.registered).To(BeEmpty(), "no Register when nothing drifted")
	})
})

var _ = Describe("NomadNamespace reconciler: conflict", func() {
	It("sets NamespaceNameConflict and skips Register when a live sibling shares the name", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-conflict-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		first := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a-1", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: nc.Name}, NamespaceName: "team-a"},
		}
		Expect(k8s.Create(ctx, first)).To(Succeed())
		second := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a-2", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: nc.Name}, NamespaceName: "team-a"},
		}
		Expect(k8s.Create(ctx, second)).To(Succeed())

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a-2", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a-2", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonNamespaceNameConflict))
		Expect(f.registered).To(BeEmpty())
	})
})
