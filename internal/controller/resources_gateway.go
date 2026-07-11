package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

const listenerNameHTTP = "http"

func listenerNameRPC(ordinal int) string { return fmt.Sprintf("rpc-%d", ordinal) }

func ptrHostname(h string) *gwapiv1.Hostname    { return new(gwapiv1.Hostname(h)) }
func ptrPortNumber(p int32) *gwapiv1.PortNumber { return new(gwapiv1.PortNumber(p)) }

// buildManagedGateway builds the operator-owned Gateway (Managed mode): one HTTP
// listener with TLS SNI passthrough, plus one TCP listener per RPC port (RPC 4647
// is a multiplexed mTLS stream, not SNI-routable, so each server needs its own
// listener port on the same Gateway IP).
func buildManagedGateway(nc *nomadv1alpha1.NomadCluster) *gwapiv1.Gateway {
	n := names(nc)
	listeners := []gwapiv1.Listener{{
		Name:     listenerNameHTTP,
		Port:     gwapiv1.PortNumber(portHTTP),
		Protocol: gwapiv1.TLSProtocolType,
		Hostname: ptrHostname(nc.Spec.Gateway.HTTPHostname),
		TLS:      &gwapiv1.GatewayTLSConfig{Mode: new(gwapiv1.TLSModePassthrough)},
	}}
	for ordinal, p := range nc.Spec.Gateway.RPCPorts {
		listeners = append(listeners, gwapiv1.Listener{
			Name:     gwapiv1.SectionName(listenerNameRPC(ordinal)),
			Port:     gwapiv1.PortNumber(p),
			Protocol: gwapiv1.TCPProtocolType,
		})
	}
	return &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: n.Gateway, Namespace: nc.Namespace, Labels: n.Labels()},
		Spec: gwapiv1.GatewaySpec{
			GatewayClassName: gwapiv1.ObjectName(nc.Spec.Gateway.ClassName),
			Listeners:        listeners,
		},
	}
}

// parentGateway returns the parentRef the routes attach to (Managed: the created
// Gateway; Existing: the referenced one).
func parentGateway(nc *nomadv1alpha1.NomadCluster) gwapiv1.ParentReference {
	n := names(nc)
	gwName := n.Gateway
	gwNs := gwapiv1.Namespace(nc.Namespace)
	if nc.Spec.Gateway.Mode == nomadv1alpha1.GatewayModeExisting && nc.Spec.Gateway.Ref != nil {
		gwName = nc.Spec.Gateway.Ref.Name
		gwNs = gwapiv1.Namespace(nc.Spec.Gateway.Ref.Namespace)
	}
	return gwapiv1.ParentReference{
		Group:     new(gwapiv1.Group("gateway.networking.k8s.io")),
		Kind:      new(gwapiv1.Kind("Gateway")),
		Name:      gwapiv1.ObjectName(gwName),
		Namespace: &gwNs,
	}
}

// buildTLSRoute builds the HTTP front door: SNI == httpHostname routed via TLS
// passthrough to the API service.
func buildTLSRoute(nc *nomadv1alpha1.NomadCluster) *gwapiv1a2.TLSRoute {
	n := names(nc)
	parent := parentGateway(nc)
	parent.SectionName = new(gwapiv1.SectionName(listenerNameHTTP))
	return &gwapiv1a2.TLSRoute{
		ObjectMeta: metav1.ObjectMeta{Name: n.TLSRoute, Namespace: nc.Namespace, Labels: n.Labels()},
		Spec: gwapiv1a2.TLSRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{parent}},
			Hostnames:       []gwapiv1.Hostname{gwapiv1.Hostname(nc.Spec.Gateway.HTTPHostname)},
			Rules: []gwapiv1a2.TLSRouteRule{{BackendRefs: []gwapiv1.BackendRef{{
				BackendObjectReference: gwapiv1.BackendObjectReference{Name: gwapiv1.ObjectName(n.APISvc), Port: ptrPortNumber(portHTTP)},
			}}}},
		},
	}
}

// buildTCPRoutes builds one TCPRoute per server: each attaches to its own
// listener sectionName (rpc-<ordinal>) and backends the per-pod ClusterIP
// service for that ordinal.
func buildTCPRoutes(nc *nomadv1alpha1.NomadCluster) []*gwapiv1a2.TCPRoute {
	n := names(nc)
	routes := make([]*gwapiv1a2.TCPRoute, 0, nc.Spec.Servers)
	for ordinal := range int(nc.Spec.Servers) {
		parent := parentGateway(nc)
		parent.SectionName = new(gwapiv1.SectionName(listenerNameRPC(ordinal)))
		routes = append(routes, &gwapiv1a2.TCPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: n.TCPRoute(ordinal), Namespace: nc.Namespace, Labels: n.Labels()},
			Spec: gwapiv1a2.TCPRouteSpec{
				CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{parent}},
				Rules: []gwapiv1a2.TCPRouteRule{{BackendRefs: []gwapiv1.BackendRef{{
					BackendObjectReference: gwapiv1.BackendObjectReference{Name: gwapiv1.ObjectName(n.PodSvc(ordinal)), Port: ptrPortNumber(portRPC)},
				}}}},
			},
		})
	}
	return routes
}

// ensureManagedGateway applies the operator-owned Gateway and re-reads it to
// observe the address assigned by the Gateway controller. envtest runs no
// Gateway controller, so tests assign Status.Addresses directly to simulate it.
func (r *NomadClusterReconciler) ensureManagedGateway(ctx context.Context, nc *nomadv1alpha1.NomadCluster) (string, bool, error) {
	gw := buildManagedGateway(nc)
	if err := r.apply(ctx, nc, gw); err != nil {
		return "", false, err
	}
	// Re-read to observe the assigned address.
	var current gwapiv1.Gateway
	if err := r.Get(ctx, client.ObjectKeyFromObject(gw), &current); err != nil {
		return "", false, err
	}
	for _, a := range current.Status.Addresses {
		if a.Value != "" {
			return a.Value, true, nil
		}
	}
	return "", false, nil
}
