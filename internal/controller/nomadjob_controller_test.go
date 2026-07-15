package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

// minimalJobRaw is a valid schemaless spec.job payload for skeleton tests. The
// skeleton never decodes it (reconcileJob is a stub), but the CRD marks job
// Required, so every NomadJob needs one.
func minimalJobRaw() runtime.RawExtension {
	return runtime.RawExtension{Raw: []byte(`{"type":"service","taskGroups":[{"name":"app","count":1}]}`)}
}

var _ = Describe("NomadJob reconciler: cluster resolution", func() {
	It("sets ClusterNotFound and adds the finalizer when the referenced cluster is missing", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-notfound-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: "missing"},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		f := newFakeJobOps()
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadJobCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotFound))
		Expect(controllerutil.ContainsFinalizer(&got, nomadJobFinalizer)).To(BeTrue(), "finalizer not added")
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation), "observedGeneration must advance on the ClusterNotFound status write")
	})

	It("sets ClusterNotReady and registers nothing when the referenced cluster is not Ready", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-notready-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nc := readyCluster(ctx, ns.Name)
		nc.Status.Phase = nomadv1alpha1.PhaseDegraded // cluster drops out of Ready
		Expect(k8s.Status().Update(ctx, nc)).To(Succeed())

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		f := newFakeJobOps()
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadJobCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotReady))
		Expect(f.registered).To(BeEmpty(), "must not register a job while the cluster is not Ready")
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation), "observedGeneration must advance on the ClusterNotReady status write")
	})
})

var _ = Describe("NomadJob reconciler: ownerRef + requeue", func() {
	It("sets a controller ownerReference to the NomadCluster and requeues after jobResync", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-owner-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		f := newFakeJobOps()
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(ctrl.Result{RequeueAfter: jobResync}))

		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.OwnerReferences).To(HaveLen(1))
		Expect(got.OwnerReferences[0].Name).To(Equal(nc.Name))
		Expect(got.OwnerReferences[0].Controller).NotTo(BeNil())
		Expect(*got.OwnerReferences[0].Controller).To(BeTrue())
	})
})

var _ = Describe("NomadJob reconciler: jobsForCluster mapper", func() {
	It("returns exactly the jobs referencing the given cluster", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-mapper-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		prodCluster := readyCluster(ctx, ns.Name)

		job1 := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "job1", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadJobSpec{ClusterRef: nomadv1alpha1.JobClusterRef{Name: "prod"}, JobID: "job1", Job: minimalJobRaw()},
		}
		Expect(k8s.Create(ctx, job1)).To(Succeed())
		job2 := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "job2", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadJobSpec{ClusterRef: nomadv1alpha1.JobClusterRef{Name: "prod"}, JobID: "job2", Job: minimalJobRaw()},
		}
		Expect(k8s.Create(ctx, job2)).To(Succeed())
		otherJob := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "job3", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadJobSpec{ClusterRef: nomadv1alpha1.JobClusterRef{Name: "other"}, JobID: "job3", Job: minimalJobRaw()},
		}
		Expect(k8s.Create(ctx, otherJob)).To(Succeed())

		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme()}
		reqs := r.jobsForCluster(ctx, prodCluster)
		var names []string
		for _, req := range reqs {
			names = append(names, req.Name)
		}
		Expect(names).To(ConsistOf("job1", "job2"))
	})
})
