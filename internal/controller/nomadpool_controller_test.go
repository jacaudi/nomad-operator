package controller

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hashicorp/nomad/api"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation), "observedGeneration must advance on the ClusterNotFound status write")
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
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation), "observedGeneration must advance on the ClusterNotReady status write")
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

var _ = Describe("NomadPool reconciler: apply", func() {
	It("registers a pool, preserves unmanaged fields, and skips redundant writes on the next reconcile", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-apply-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakePoolOps()
		// Seed an out-of-band pool carrying an unmanaged SchedulerConfiguration.
		sched := &api.NodePoolSchedulerConfiguration{SchedulerAlgorithm: api.SchedulerAlgorithmSpread}
		f.pools["gpu"] = &api.NodePool{Name: "gpu", Description: "old", SchedulerConfiguration: sched}

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadPoolSpec{
				ClusterRef:  nomadv1alpha1.PoolClusterRef{Name: nc.Name},
				PoolName:    "gpu",
				Description: "GPU workers",
				Meta:        map[string]string{"team": "ml"},
			},
		}
		Expect(k8s.Create(ctx, np)).To(Succeed())

		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: ns.Name}}
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		Expect(f.registered).To(HaveLen(1), "want exactly 1 Register")
		got := f.registered[0]
		Expect(got.Description).To(Equal("GPU workers"), "managed field Description not applied")
		Expect(got.Meta["team"]).To(Equal("ml"), "managed field Meta not applied")
		Expect(got.SchedulerConfiguration).To(BeIdenticalTo(sched), "unmanaged SchedulerConfiguration not preserved")

		var gotPool nomadv1alpha1.NomadPool
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "gpu", Namespace: ns.Name}, &gotPool)).To(Succeed())
		cond := meta.FindStatusCondition(gotPool.Status.Conditions, nomadv1alpha1.NomadPoolCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonRegistered))

		// Second reconcile: nothing changed → no new Register (compare-before-write).
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.registered).To(HaveLen(1), "compare-before-write failed: a redundant Register was issued")
	})
})

var _ = Describe("NomadPool reconciler: status nodeCount", func() {
	It("mirrors CountNodePoolNodes into status.nodeCount on a successful reconcile", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-nodecount-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakePoolOps()
		f.nodeCount = 3

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadPoolSpec{
				ClusterRef: nomadv1alpha1.PoolClusterRef{Name: nc.Name},
				PoolName:   "gpu",
			},
		}
		Expect(k8s.Create(ctx, np)).To(Succeed())

		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadPool
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "gpu", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.Status.NodeCount).To(Equal(3))
	})
})

var _ = Describe("NomadPool reconciler: poolName collision", func() {
	It("skips Register and sets PoolNameConflict on both CRs when two pools share a poolName+cluster", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-conflict-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakePoolOps()
		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		// Both CRs must exist before either is reconciled, so the List sees both.
		for _, objName := range []string{"gpu-a", "gpu-b"} {
			np := &nomadv1alpha1.NomadPool{
				ObjectMeta: metav1.ObjectMeta{Name: objName, Namespace: ns.Name},
				Spec: nomadv1alpha1.NomadPoolSpec{
					ClusterRef: nomadv1alpha1.PoolClusterRef{Name: nc.Name},
					PoolName:   "gpu",
				},
			}
			Expect(k8s.Create(ctx, np)).To(Succeed())
		}

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu-b", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(f.registered).To(BeEmpty(), "colliding CRs must skip Register")

		for _, objName := range []string{"gpu-a", "gpu-b"} {
			var got nomadv1alpha1.NomadPool
			Expect(k8s.Get(ctx, types.NamespacedName{Name: objName, Namespace: ns.Name}, &got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadPoolCondReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonPoolNameConflict))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation), "observedGeneration must advance on the PoolNameConflict status write")
		}
	})

	It("does not conflict with a same-poolName sibling that is Terminating, and Registers", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-conflict-terminating-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakePoolOps()
		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		mustCreateTerminatingPool(ctx, ns.Name, "gpu-old", nc.Name, "gpu")

		live := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu-live", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadPoolSpec{
				ClusterRef: nomadv1alpha1.PoolClusterRef{Name: nc.Name},
				PoolName:   "gpu",
			},
		}
		Expect(k8s.Create(ctx, live)).To(Succeed())

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu-live", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadPool
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "gpu-live", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadPoolCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).NotTo(Equal(nomadv1alpha1.ReasonPoolNameConflict), "a Terminating sibling must not count as a live conflict")
		Expect(len(f.registered)).To(BeNumerically(">=", 1), "the replacement pool must be allowed to Register")
	})
})

var _ = Describe("NomadPool reconciler: finalize", func() {
	It("deletes the pool from Nomad and removes the finalizer on successful delete", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-delete-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakePoolOps()
		f.pools["gpu"] = &api.NodePool{Name: "gpu"}
		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns.Name, Finalizers: []string{nomadPoolFinalizer}},
			Spec:       nomadv1alpha1.NomadPoolSpec{ClusterRef: nomadv1alpha1.PoolClusterRef{Name: nc.Name}, PoolName: "gpu"},
		}
		Expect(k8s.Create(ctx, np)).To(Succeed())
		mustDelete(ctx, np)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deleted).To(Equal([]string{"gpu"}), "pool not deleted from Nomad")
		assertGonePool(ctx, ns.Name, "gpu")
	})

	It("holds the finalizer, sets DeleteBlocked, and surfaces node/job counts when Delete fails", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-delblocked-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakePoolOps()
		f.pools["gpu"] = &api.NodePool{Name: "gpu"}
		// A plain error: the fake cannot construct a real api.UnexpectedResponseError
		// (unexported fields), so IsNodePoolNotEmpty is false -> the generic
		// ReasonDeleteFailed applies. The real ReasonPoolNotEmpty/URE-body branch is
		// covered by Task 1's httptest test and Task 10's live spike.
		f.deleteErr = errors.New("delete failed: node pool has nodes")
		f.nodeCount, f.jobCount = 2, 1
		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns.Name, Finalizers: []string{nomadPoolFinalizer}},
			Spec:       nomadv1alpha1.NomadPoolSpec{ClusterRef: nomadv1alpha1.PoolClusterRef{Name: nc.Name}, PoolName: "gpu"},
		}
		Expect(k8s.Create(ctx, np)).To(Succeed())
		mustDelete(ctx, np)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadPool
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "gpu", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(&got, nomadPoolFinalizer)).To(BeTrue(), "finalizer must be held when Delete fails")
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadPoolCondDeleteBlocked)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonDeleteFailed))
		Expect(got.Status.NodeCount).To(Equal(2))
		Expect(got.Status.JobCount).To(Equal(1))
	})

	It("holds the finalizer, sets DeleteBlocked/ClusterNotReady, and does not call Delete when the cluster is present but not Ready", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-delnotready-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nc.Status.Phase = nomadv1alpha1.PhaseDegraded // present, not deleting, but not Ready
		Expect(k8s.Status().Update(ctx, nc)).To(Succeed())

		f := newFakePoolOps()
		f.pools["gpu"] = &api.NodePool{Name: "gpu"}
		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns.Name, Finalizers: []string{nomadPoolFinalizer}},
			Spec:       nomadv1alpha1.NomadPoolSpec{ClusterRef: nomadv1alpha1.PoolClusterRef{Name: nc.Name}, PoolName: "gpu"},
		}
		Expect(k8s.Create(ctx, np)).To(Succeed())
		mustDelete(ctx, np)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadPool
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "gpu", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(&got, nomadPoolFinalizer)).To(BeTrue(), "finalizer must be held while the cluster is not Ready")
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadPoolCondDeleteBlocked)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotReady))
		Expect(f.deleted).To(BeEmpty(), "must not call Delete while the cluster is not Ready (don't orphan on a blip)")
	})

	It("drops the finalizer without calling Delete when the cluster is gone or going (foreground cascade)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-clustergoing-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := mustCreateTerminatingCluster(ctx, ns.Name)

		f := newFakePoolOps()
		r := &NomadPoolReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns.Name, Finalizers: []string{nomadPoolFinalizer}},
			Spec:       nomadv1alpha1.NomadPoolSpec{ClusterRef: nomadv1alpha1.PoolClusterRef{Name: nc.Name}, PoolName: "gpu"},
		}
		Expect(k8s.Create(ctx, np)).To(Succeed())
		mustDelete(ctx, np)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gpu", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deleted).To(BeEmpty(), "must NOT call Delete when the cluster is going away")
		assertGonePool(ctx, ns.Name, "gpu")
	})
})

// mustDelete deletes obj. Because the object carries a finalizer, this sets a
// DeletionTimestamp rather than removing it from etcd immediately.
func mustDelete(ctx SpecContext, obj client.Object) {
	Expect(k8s.Delete(ctx, obj)).To(Succeed())
}

// assertGonePool asserts the pool CR no longer exists: the finalizer was
// removed and Kubernetes garbage-collected the object.
func assertGonePool(ctx SpecContext, ns, name string) {
	var got nomadv1alpha1.NomadPool
	err := k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got)
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected pool %s/%s to be gone, got: %v", ns, name, err)
}

// mustCreateTerminatingCluster creates a NomadCluster carrying a dummy
// finalizer, then deletes it, so it stays present with a non-zero
// DeletionTimestamp instead of vanishing — simulating a foreground-cascade
// delete (`--cascade=foreground`) in progress.
func mustCreateTerminatingCluster(ctx SpecContext, ns string) *nomadv1alpha1.NomadCluster {
	nc := &nomadv1alpha1.NomadCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: ns, Finalizers: []string{"test.nomad.operator.io/hold"}},
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
	Expect(k8s.Delete(ctx, nc)).To(Succeed())

	var got nomadv1alpha1.NomadCluster
	Expect(k8s.Get(ctx, types.NamespacedName{Name: nc.Name, Namespace: ns}, &got)).To(Succeed())
	Expect(got.DeletionTimestamp).NotTo(BeNil(), "expected cluster to remain Terminating (finalizer held) with a DeletionTimestamp set")
	return &got
}

// mustCreateTerminatingPool creates a NomadPool carrying the cleanup finalizer,
// then deletes it, so it stays present with a non-zero DeletionTimestamp
// instead of vanishing — simulating a same-name pool being replaced while GC
// is still in flight (mirrors mustCreateTerminatingCluster).
func mustCreateTerminatingPool(ctx SpecContext, ns, name, clusterName, poolName string) *nomadv1alpha1.NomadPool {
	np := &nomadv1alpha1.NomadPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{nomadPoolFinalizer}},
		Spec: nomadv1alpha1.NomadPoolSpec{
			ClusterRef: nomadv1alpha1.PoolClusterRef{Name: clusterName},
			PoolName:   poolName,
		},
	}
	Expect(k8s.Create(ctx, np)).To(Succeed())
	Expect(k8s.Delete(ctx, np)).To(Succeed())

	var got nomadv1alpha1.NomadPool
	Expect(k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got)).To(Succeed())
	Expect(got.DeletionTimestamp).NotTo(BeNil(), "expected pool to remain Terminating (finalizer held) with a DeletionTimestamp set")
	return &got
}

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
