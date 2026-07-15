/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

// newValidNomadJob is the single source of truth for a minimal, admissible
// NomadJob: a valid jobID (matches the pattern), a clusterRef, and a schemaless
// job body. The immutability tests mutate a copy so the ONLY reason Update fails
// is the CEL rule under test — not an incidentally-invalid field.
func newValidNomadJob(namespace, name string) *nomadv1alpha1.NomadJob {
	return &nomadv1alpha1.NomadJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: nomadv1alpha1.NomadJobSpec{
			ClusterRef: nomadv1alpha1.JobClusterRef{Name: "prod"},
			JobID:      name,
			Job:        runtime.RawExtension{Raw: []byte(`{"type":"service"}`)},
		},
	}
}

var _ = Describe("NomadJob CRD", func() {
	// Positive control: a valid NomadJob is admitted. This distinguishes the
	// rejection tests below from a CRD that rejects everything.
	It("admits a valid NomadJob", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-crd-valid-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nj := newValidNomadJob(ns.Name, "web")
		Expect(k8s.Create(ctx, nj)).To(Succeed())
	})

	It("rejects mutating jobID after creation (immutable)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-crd-jobid-immut-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nj := newValidNomadJob(ns.Name, "web")
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		// "web2" is a VALID jobID (passes the pattern), so the only reason the
		// Update can fail is the immutability CEL rule.
		nj.Spec.JobID = "web2"
		err := k8s.Update(ctx, nj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("jobID is immutable"))
	})

	It("rejects mutating clusterRef.name after creation (immutable)", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-crd-cluster-immut-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nj := newValidNomadJob(ns.Name, "web")
		Expect(k8s.Create(ctx, nj)).To(Succeed())

		// "staging" is a valid clusterRef.name, so the only reason the Update can
		// fail is the immutability CEL rule.
		nj.Spec.ClusterRef.Name = "staging"
		err := k8s.Update(ctx, nj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("clusterRef.name is immutable"))
	})

	It("rejects a jobID that violates the name pattern", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nj-crd-jobid-pattern-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nj := newValidNomadJob(ns.Name, "bad")
		nj.Spec.JobID = "has spaces"
		err := k8s.Create(ctx, nj)
		Expect(err).To(HaveOccurred(), "jobID with spaces must be rejected by the pattern")
		// The rejection must specifically reference the jobID pattern validation,
		// not some unrelated admission failure.
		Expect(err.Error()).To(ContainSubstring("spec.jobID"))
		Expect(err.Error()).To(ContainSubstring("should match"))
	})
})
