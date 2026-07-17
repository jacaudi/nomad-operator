package controller

import (
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
