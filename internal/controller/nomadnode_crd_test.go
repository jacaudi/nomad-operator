package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

var _ = Describe("NomadNode CRD", func() {
	It("defaults eligible to true and rejects nodeName mutation", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-crd-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		// Defaulting is only observable when `eligible` is ABSENT from the
		// submitted JSON. NomadNodeSpec.Eligible is deliberately NOT omitempty —
		// a reflector-seeded false must always be sent so the apiserver never
		// re-defaults an intentionally-ineligible node back to true — which means
		// the typed Go client always marshals `eligible: false` for a zero value
		// and the CRD default never fires through it. So we exercise the default
		// via an unstructured create that omits the field, matching the
		// human/`kubectl apply` path that the `default: true` is there to serve.
		probe := &unstructured.Unstructured{}
		probe.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "nomad.operator.io", Version: "v1alpha1", Kind: "NomadNode",
		})
		probe.SetName("defaults-probe")
		probe.SetNamespace(ns.Name)
		probe.Object["spec"] = map[string]any{
			"clusterRef": map[string]any{"name": "prod"},
			"nodeName":   "defaults-probe",
		}
		Expect(k8s.Create(ctx, probe)).To(Succeed())
		eligible, found, err := unstructured.NestedBool(probe.Object, "spec", "eligible")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue(), "eligible should be defaulted onto the object")
		Expect(eligible).To(BeTrue(), "eligible should default true")

		nn := &nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: "box-1", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: "prod"},
				NodeName:   "box-1",
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		nn.Spec.NodeName = "box-2"
		err = k8s.Update(ctx, nn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("nodeName is immutable"))

		Expect(k8s.Delete(ctx, nn)).To(Succeed())
	})
})
