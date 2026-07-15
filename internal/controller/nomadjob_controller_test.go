package controller

import (
	"github.com/hashicorp/nomad/api"
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

var _ = Describe("NomadJob reconciler: apply", func() {
	It("registers the job when Plan reports a change and injects ID/Region", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-apply-change-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakeJobOps()
		f.planChanged = true

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        runtime.RawExtension{Raw: []byte(`{"type":"service","taskGroups":[{"name":"app","count":1}]}`)},
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(f.registered).To(HaveLen(1))
		Expect(f.registered[0].ID).NotTo(BeNil())
		Expect(*f.registered[0].ID).To(Equal("web"))
		Expect(f.registered[0].Region).NotTo(BeNil())
		Expect(*f.registered[0].Region).To(Equal("global"))

		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadJobCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonRegistered))
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
	})

	It("skips Register when Plan reports no change but still reports Ready", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-apply-nochange-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakeJobOps()
		f.planChanged = false

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(f.registered).To(BeEmpty(), "must not register when Plan shows no drift")

		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadJobCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonRegistered))
	})

	It("sets InvalidJobSpec and registers nothing when spec.job has an unknown key", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-apply-invalid-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakeJobOps()
		f.planChanged = true // proves the decode gate short-circuits before Plan/Register

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        runtime.RawExtension{Raw: []byte(`{"taskGropus":[]}`)},
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(f.registered).To(BeEmpty())

		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadJobCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonInvalidJobSpec))
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
	})

	It("sets JobIDMismatch and registers nothing when spec.job.id disagrees with spec.jobID", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-apply-mismatch-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakeJobOps()
		f.planChanged = true

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        runtime.RawExtension{Raw: []byte(`{"id":"other"}`)},
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(f.registered).To(BeEmpty())

		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadJobCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonJobIDMismatch))
	})

	It("emits a RegisterWarnings event when Register returns warnings", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-apply-warn-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakeJobOps()
		f.planChanged = true
		f.warnings = "deprecated x"

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		rec := record.NewFakeRecorder(10)
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: rec}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got string
		Eventually(rec.Events).Should(Receive(&got))
		Expect(got).To(ContainSubstring("deprecated x"))
	})
})

var _ = Describe("NomadJob reconciler: status", func() {
	It("mirrors jobStatus, jobVersion, and per-group running/desired into status", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-status-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakeJobOps()
		// planChanged=false so NO Register runs: the fake's RegisterJob would
		// overwrite f.jobs["web"] with the decoded desired job (which has no
		// server-set Status/Version), clobbering the seed below and making GetJob
		// return a statusless job (SGE plan-review I-1). With no Register, the seeded
		// job survives and GetJob returns running/4.
		f.planChanged = false
		status, ver := "running", uint64(4)
		f.jobs["web"] = &api.Job{Status: &status, Version: &ver}
		f.summary["app"] = api.TaskGroupSummary{Running: 2}

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        runtime.RawExtension{Raw: []byte(`{"type":"service","taskGroups":[{"name":"app","count":3}]}`)},
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.Status.JobStatus).To(Equal("running"))
		Expect(got.Status.JobVersion).To(Equal(int64(4)))
		Expect(got.Status.Groups["app"]).To(Equal(nomadv1alpha1.NomadJobGroupStatus{Running: 2, Desired: 3}))
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
