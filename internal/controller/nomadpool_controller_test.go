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

var _ = Describe("NomadPool reconciler: cluster resolution", func() {
	It("sets ClusterNotFound and adds the finalizer when the referenced cluster is missing", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-notfound-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadPoolSpec{
				ClusterRef: nomadv1alpha1.PoolClusterRef{Name: "missing"},
				PoolName:   "gpu",
			},
		}
		Expect(k8s.Create(ctx, np)).To(Succeed())

		f := newFakePoolOps()
		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadPool
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "gpu", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadPoolCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotFound))
		Expect(controllerutil.ContainsFinalizer(&got, nomadPoolFinalizer)).To(BeTrue(), "finalizer not added")
	})

	It("sets ClusterNotReady and registers nothing when the referenced cluster is not Ready", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-notready-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nc := readyCluster(ctx, ns.Name)
		nc.Status.Phase = nomadv1alpha1.PhaseDegraded // cluster drops out of Ready
		Expect(k8s.Status().Update(ctx, nc)).To(Succeed())

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadPoolSpec{
				ClusterRef: nomadv1alpha1.PoolClusterRef{Name: nc.Name},
				PoolName:   "gpu",
			},
		}
		Expect(k8s.Create(ctx, np)).To(Succeed())

		f := newFakePoolOps()
		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadPool
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "gpu", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadPoolCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotReady))
		Expect(f.registered).To(BeEmpty(), "must not register a pool while the cluster is not Ready")
	})
})

var _ = Describe("NomadPool reconciler: ownerRef + requeue", func() {
	It("sets a controller ownerReference to the NomadCluster and requeues after poolResync", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-owner-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadPoolSpec{
				ClusterRef: nomadv1alpha1.PoolClusterRef{Name: nc.Name},
				PoolName:   "gpu",
			},
		}
		Expect(k8s.Create(ctx, np)).To(Succeed())

		f := newFakePoolOps()
		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(ctrl.Result{RequeueAfter: poolResync}))

		var got nomadv1alpha1.NomadPool
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "gpu", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.OwnerReferences).To(HaveLen(1))
		Expect(got.OwnerReferences[0].Name).To(Equal(nc.Name))
		Expect(got.OwnerReferences[0].Controller).NotTo(BeNil())
		Expect(*got.OwnerReferences[0].Controller).To(BeTrue())
	})
})

var _ = Describe("NomadPool reconciler: poolsForCluster mapper", func() {
	It("returns exactly the pools referencing the given cluster", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-mapper-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		prodCluster := readyCluster(ctx, ns.Name)

		pool1 := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool1", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadPoolSpec{ClusterRef: nomadv1alpha1.PoolClusterRef{Name: "prod"}, PoolName: "pool1"},
		}
		Expect(k8s.Create(ctx, pool1)).To(Succeed())
		pool2 := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool2", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadPoolSpec{ClusterRef: nomadv1alpha1.PoolClusterRef{Name: "prod"}, PoolName: "pool2"},
		}
		Expect(k8s.Create(ctx, pool2)).To(Succeed())
		otherPool := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool3", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadPoolSpec{ClusterRef: nomadv1alpha1.PoolClusterRef{Name: "other"}, PoolName: "pool3"},
		}
		Expect(k8s.Create(ctx, otherPool)).To(Succeed())

		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme()}
		reqs := r.poolsForCluster(ctx, prodCluster)
		var names []string
		for _, req := range reqs {
			names = append(names, req.Name)
		}
		Expect(names).To(ConsistOf("pool1", "pool2"))
	})
})
