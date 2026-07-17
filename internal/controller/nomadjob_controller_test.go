package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"

	"github.com/hashicorp/nomad/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
	"github.com/jacaudi/nomad-operator/internal/nomad"
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
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation), "observedGeneration must advance on the JobIDMismatch decode-error write (parity with InvalidJobSpec)")
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
		Expect(got).To(ContainSubstring("RegisterWarnings"), "the event Reason must be RegisterWarnings; a changed Reason or event type is a regression")
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

var _ = Describe("NomadJob reconciler: finalize", func() {
	It("deregisters with purge and removes the finalizer on successful delete", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-del-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakeJobOps()
		f.jobs["web"] = &api.Job{}
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name, Finalizers: []string{nomadJobFinalizer}},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())
		mustDelete(ctx, nj)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deregistered).To(Equal([]string{"web"}), "job not deregistered from Nomad")
		Expect(f.purged).To(Equal([]bool{true}), "Deregister must purge so a re-created jobID does not collide with a dead record")
		assertGoneJob(ctx, ns.Name, "web")
	})

	It("drops the finalizer without calling Deregister when the cluster is gone or going (foreground cascade)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-clustergoing-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := mustCreateTerminatingCluster(ctx, ns.Name)

		f := newFakeJobOps()
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name, Finalizers: []string{nomadJobFinalizer}},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())
		mustDelete(ctx, nj)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deregistered).To(BeEmpty(), "must NOT call Deregister when the cluster is going away")
		assertGoneJob(ctx, ns.Name, "web")
	})

	It("holds the finalizer, sets DeleteBlocked/ClusterNotReady, and does not call Deregister when the cluster is present but not Ready", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-delnotready-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nc.Status.Phase = nomadv1alpha1.PhaseDegraded // present, not deleting, but not Ready
		Expect(k8s.Status().Update(ctx, nc)).To(Succeed())

		f := newFakeJobOps()
		f.jobs["web"] = &api.Job{}
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name, Finalizers: []string{nomadJobFinalizer}},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())
		mustDelete(ctx, nj)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(&got, nomadJobFinalizer)).To(BeTrue(), "finalizer must be held while the cluster is not Ready")
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadJobCondDeleteBlocked)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotReady))
		Expect(f.deregistered).To(BeEmpty(), "must not call Deregister while the cluster is not Ready (don't orphan on a blip)")
	})

	It("holds the finalizer and sets DeleteBlocked/DeregisterFailed when Deregister fails", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-delblocked-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		f := newFakeJobOps()
		f.jobs["web"] = &api.Job{}
		f.deregisterErr = errors.New("boom")
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name, Finalizers: []string{nomadJobFinalizer}},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())
		mustDelete(ctx, nj)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadJob
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "web", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(&got, nomadJobFinalizer)).To(BeTrue(), "finalizer must be held when Deregister fails")
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadJobCondDeleteBlocked)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonDeregisterFailed))
	})

	It("drops the finalizer without calling Deregister when the referenced cluster no longer exists (background cascade)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-clustergone-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		// No cluster is created: clusterRef points at a name that does not exist,
		// so finalizeJob's apierrors.IsNotFound disjunct fires. This is the
		// background-cascade / cluster-fully-gone path — distinct from the
		// terminating-cluster spec above, which exercises the DeletionTimestamp
		// disjunct.
		f := newFakeJobOps()
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name, Finalizers: []string{nomadJobFinalizer}},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: "missing"},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())
		mustDelete(ctx, nj)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deregistered).To(BeEmpty(), "cluster is gone; there is nothing to deregister")
		assertGoneJob(ctx, ns.Name, "web")
	})

	It("tolerates a 404 from Deregister (job already gone) and completes deletion without DeleteBlocked", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-del404-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		// api.UnexpectedResponseError has unexported fields and no exported
		// constructor, so a genuine 404 cannot be built directly. Obtain one the
		// way internal/nomad's own tests do — drive a real client against a 404 —
		// so this exercises IsNotFound's real error type, not a stand-in.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "job not found", http.StatusNotFound)
		}))
		DeferCleanup(srv.Close)
		nomadClient, err := nomad.New(nomad.Config{Address: srv.URL})
		Expect(err).NotTo(HaveOccurred())
		notFound := nomadClient.DeregisterJob(ctx, "web", true)
		Expect(nomad.IsNotFound(notFound)).To(BeTrue(), "sanity: the seeded error must be a real, non-nil 404 that satisfies nomad.IsNotFound")

		f := newFakeJobOps()
		f.jobs["web"] = &api.Job{}
		f.deregisterErr = notFound
		r := &NomadJobReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: record.NewFakeRecorder(10)}

		nj := &nomadv1alpha1.NomadJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name, Finalizers: []string{nomadJobFinalizer}},
			Spec: nomadv1alpha1.NomadJobSpec{
				ClusterRef: nomadv1alpha1.JobClusterRef{Name: nc.Name},
				JobID:      "web",
				Job:        minimalJobRaw(),
			},
		}
		Expect(k8s.Create(ctx, nj)).To(Succeed())
		mustDelete(ctx, nj)

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "web", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		// An already-gone job (404) is treated as success: the finalizer is dropped
		// and the CR is GC'd. Had the 404 been treated as a DeregisterFailed, the
		// finalizer would be held and the CR would still exist — so GC is the proof
		// that no DeleteBlocked/DeregisterFailed path was taken.
		assertGoneJob(ctx, ns.Name, "web")
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

var _ = Describe("decodeJob namespace injection", func() {
	It("injects spec.nomadNamespace into job.Namespace", func() {
		spec := nomadv1alpha1.NomadJobSpec{
			JobID:          "web",
			NomadNamespace: "team-a",
			Job:            runtime.RawExtension{Raw: []byte(`{"type":"service"}`)},
		}
		job, err := decodeJob(spec, "global")
		Expect(err).NotTo(HaveOccurred())
		Expect(job.Namespace).NotTo(BeNil())
		Expect(*job.Namespace).To(Equal("team-a"))
	})

	It("rejects a blob namespace that disagrees with spec.nomadNamespace", func() {
		spec := nomadv1alpha1.NomadJobSpec{
			JobID:          "web",
			NomadNamespace: "team-a",
			Job:            runtime.RawExtension{Raw: []byte(`{"namespace":"team-b"}`)},
		}
		_, err := decodeJob(spec, "global")
		Expect(err).To(MatchError(errNamespaceMismatch))
	})

	It("accepts a blob namespace equal to spec.nomadNamespace", func() {
		spec := nomadv1alpha1.NomadJobSpec{
			JobID:          "web",
			NomadNamespace: "team-a",
			Job:            runtime.RawExtension{Raw: []byte(`{"namespace":"team-a"}`)},
		}
		job, err := decodeJob(spec, "global")
		Expect(err).NotTo(HaveOccurred())
		Expect(*job.Namespace).To(Equal("team-a"))
	})
})

// assertGoneJob asserts the job CR no longer exists: the finalizer was removed
// and Kubernetes garbage-collected the object (mirrors assertGonePool).
func assertGoneJob(ctx SpecContext, ns, name string) {
	var got nomadv1alpha1.NomadJob
	err := k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got)
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected job %s/%s to be gone, got: %v", ns, name, err)
}
