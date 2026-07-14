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

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

var _ = Describe("NomadPool CRD", func() {
	It("rejects the built-in poolNames 'default' and 'all'", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-crd-builtin-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		for _, name := range []string{"default", "all"} {
			np := &nomadv1alpha1.NomadPool{
				ObjectMeta: metav1.ObjectMeta{Name: "np-" + name, Namespace: ns.Name},
				Spec: nomadv1alpha1.NomadPoolSpec{
					ClusterRef: nomadv1alpha1.PoolClusterRef{Name: "prod"},
					PoolName:   name,
				},
			}
			err := k8s.Create(ctx, np)
			Expect(err).To(HaveOccurred(), "poolName %q must be rejected by CEL", name)
			Expect(err.Error()).To(ContainSubstring("built-in and cannot be managed"))
		}
	})

	It("rejects mutating poolName after creation", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-crd-immutable-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadPoolSpec{
				ClusterRef: nomadv1alpha1.PoolClusterRef{Name: "prod"},
				PoolName:   "gpu",
			},
		}
		Expect(k8s.Create(ctx, np)).To(Succeed())

		np.Spec.PoolName = "gpu2"
		err := k8s.Update(ctx, np)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("poolName is immutable"))
	})

	It("rejects a poolName that doesn't match the allowed pattern", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "np-crd-pattern-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		np := &nomadv1alpha1.NomadPool{
			ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadPoolSpec{
				ClusterRef: nomadv1alpha1.PoolClusterRef{Name: "prod"},
				PoolName:   "has spaces",
			},
		}
		err := k8s.Create(ctx, np)
		Expect(err).To(HaveOccurred(), "poolName with spaces must be rejected by the pattern")
	})
})
