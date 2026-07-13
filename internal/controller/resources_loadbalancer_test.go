package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

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
