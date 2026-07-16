package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

func lbCluster(name, ns string) *nomadv1alpha1.NomadCluster {
	nc := singleServerCluster(name, ns)
	nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{
		Mode:         nomadv1alpha1.ExternalAccessLoadBalancer,
		LoadBalancer: &nomadv1alpha1.LoadBalancerSpec{},
	}
	return nc
}

func TestBuildLoadBalancerService(t *testing.T) {
	nc := lbCluster("edge", "nomad-system")
	svc := buildLoadBalancerService(nc)

	if svc.Name != "edge-lb" {
		t.Errorf("name = %q, want edge-lb", svc.Name)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("type = %q, want LoadBalancer", svc.Spec.Type)
	}
	ports := map[string]int32{}
	for _, p := range svc.Spec.Ports {
		ports[p.Name] = p.Port
	}
	if ports["rpc"] != 4647 || ports["http"] != 4646 {
		t.Errorf("ports = %+v, want rpc:4647 http:4646", ports)
	}
	if svc.Spec.Selector["app.kubernetes.io/instance"] != "edge" {
		t.Errorf("selector = %+v, want instance=edge", svc.Spec.Selector)
	}
}

func TestBuildLoadBalancerServiceClassAndAnnotations(t *testing.T) {
	nc := lbCluster("edge", "nomad-system")
	nc.Spec.ExternalAccess.LoadBalancer = &nomadv1alpha1.LoadBalancerSpec{
		LoadBalancerClass: "service.k8s.aws/nlb",
		Annotations:       map[string]string{"foo": "bar"},
	}
	svc := buildLoadBalancerService(nc)

	if svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass != "service.k8s.aws/nlb" {
		t.Errorf("loadBalancerClass = %v, want service.k8s.aws/nlb", svc.Spec.LoadBalancerClass)
	}
	if svc.Annotations["foo"] != "bar" {
		t.Errorf("annotations = %+v, want foo=bar", svc.Annotations)
	}
}

// lbReconciler builds a NomadClusterReconciler whose fake client no-ops the
// apply (Patch) and routes the LB Service read-back Get through getErr — the
// deterministic stand-in for the informer-cache lag observed on first cluster
// creation, where the just-applied Service is not yet visible to the read-back.
func lbReconciler(t *testing.T, getErr error) *NomadClusterReconciler {
	t.Helper()
	s := scheme.Scheme
	if err := nomadv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return nil // apply succeeds
			},
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				if _, ok := obj.(*corev1.Service); ok {
					return getErr // read-back races the cache
				}
				return nil
			},
		}).
		Build()
	return &NomadClusterReconciler{Client: c, Scheme: s}
}

// TestEnsureLoadBalancerNotFoundOnReadBackIsNotReady proves the reconcile-race
// fix: a NotFound from the post-apply read-back means "just created, not yet
// observable" and must return ("", false, nil) so the reconcile requeues
// cleanly, never a hard error (which logged a spurious ERROR on every LB-mode
// cluster creation).
func TestEnsureLoadBalancerNotFoundOnReadBackIsNotReady(t *testing.T) {
	nc := lbCluster("edge", "nomad-system")
	r := lbReconciler(t, apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, names(nc).LBService))

	addr, ready, err := r.ensureLoadBalancer(t.Context(), nc)
	if err != nil {
		t.Fatalf("ensureLoadBalancer returned error on NotFound read-back, want nil: %v", err)
	}
	if ready {
		t.Errorf("ready = true, want false")
	}
	if addr != "" {
		t.Errorf("addr = %q, want empty", addr)
	}
}

// TestEnsureLoadBalancerReadBackErrorPropagates confirms the fix does not
// swallow real failures: a non-NotFound error from the read-back must still
// propagate.
func TestEnsureLoadBalancerReadBackErrorPropagates(t *testing.T) {
	nc := lbCluster("edge", "nomad-system")
	boom := errors.New("apiserver exploded")
	r := lbReconciler(t, boom)

	_, _, err := r.ensureLoadBalancer(t.Context(), nc)
	if !errors.Is(err, boom) {
		t.Fatalf("ensureLoadBalancer error = %v, want it to propagate %v", err, boom)
	}
}
