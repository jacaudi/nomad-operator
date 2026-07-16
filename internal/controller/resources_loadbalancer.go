package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

// buildLoadBalancerService builds the single type: LoadBalancer Service that
// fronts a single-node (servers: 1) control plane in LoadBalancer mode. It
// exposes RPC 4647 and HTTP 4646 and selects the server pods directly (no
// per-pod backend Services, no Gateway). North-south only — servers: 1 has no
// east-west Raft.
func buildLoadBalancerService(nc *nomadv1alpha1.NomadCluster) *corev1.Service {
	n := names(nc)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: n.LBService, Namespace: nc.Namespace, Labels: n.Labels()},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: n.Labels(),
			Ports: []corev1.ServicePort{
				{Name: "rpc", Port: portRPC, TargetPort: intstr.FromInt32(portRPC), Protocol: corev1.ProtocolTCP},
				{Name: "http", Port: portHTTP, TargetPort: intstr.FromInt32(portHTTP), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if lb := nc.Spec.ExternalAccess.LoadBalancer; lb != nil {
		if lb.LoadBalancerClass != "" {
			svc.Spec.LoadBalancerClass = &lb.LoadBalancerClass
		}
		if len(lb.Annotations) > 0 {
			svc.Annotations = lb.Annotations
		}
	}
	return svc
}

// ensureLoadBalancer applies the LoadBalancer Service and re-reads it to observe
// the ingress address the LB provider assigns (status.loadBalancer.ingress). It
// mirrors ensureManagedGateway: envtest runs no LB provider, so tests stub the
// ingress directly. Returns ready=false (never an error) until an address lands,
// so the reconciler surfaces ExternalAccessReady=False and requeues.
func (r *NomadClusterReconciler) ensureLoadBalancer(ctx context.Context, nc *nomadv1alpha1.NomadCluster) (string, bool, error) {
	svc := buildLoadBalancerService(nc)
	if err := r.apply(ctx, nc, svc); err != nil {
		return "", false, err
	}
	var current corev1.Service
	if err := r.Get(ctx, client.ObjectKeyFromObject(svc), &current); err != nil {
		// On first creation the informer cache may not yet have observed the
		// just-applied Service, so the read-back returns NotFound. Treat that as
		// "not ready yet" and requeue cleanly (same as no ingress assigned)
		// rather than surfacing a spurious reconcile error.
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, ing := range current.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP, true, nil
		}
		if ing.Hostname != "" {
			return ing.Hostname, true, nil
		}
	}
	return "", false, nil
}
