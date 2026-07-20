package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

const listenerNameHTTP = "http"

func listenerNameRPC(ordinal int) string { return fmt.Sprintf("rpc-%d", ordinal) }

func ptrHostname(h string) *gwapiv1.Hostname    { return new(gwapiv1.Hostname(h)) }
func ptrPortNumber(p int32) *gwapiv1.PortNumber { return new(p) }

// buildManagedGateway builds the operator-owned Gateway (Managed mode): one HTTP
// listener with TLS SNI passthrough, plus one TCP listener per RPC port (RPC 4647
// is a multiplexed mTLS stream, not SNI-routable, so each server needs its own
// listener port on the same Gateway IP).
func buildManagedGateway(nc *nomadv1alpha1.NomadCluster) *gwapiv1.Gateway {
	n := names(nc)
	listeners := make([]gwapiv1.Listener, 0, 1+len(nc.Spec.ExternalAccess.Gateway.RPCPorts))
	listeners = append(listeners, gwapiv1.Listener{
		Name:     listenerNameHTTP,
		Port:     gwapiv1.PortNumber(portHTTP),
		Protocol: gwapiv1.TLSProtocolType,
		Hostname: ptrHostname(nc.Spec.ExternalAccess.Gateway.HTTPHostname),
		TLS:      &gwapiv1.ListenerTLSConfig{Mode: new(gwapiv1.TLSModePassthrough)},
	})
	for ordinal, p := range nc.Spec.ExternalAccess.Gateway.RPCPorts {
		listeners = append(listeners, gwapiv1.Listener{
			Name:     gwapiv1.SectionName(listenerNameRPC(ordinal)),
			Port:     p,
			Protocol: gwapiv1.TCPProtocolType,
		})
	}
	return &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: n.Gateway, Namespace: nc.Namespace, Labels: n.Labels()},
		Spec: gwapiv1.GatewaySpec{
			GatewayClassName: gwapiv1.ObjectName(nc.Spec.ExternalAccess.Gateway.ClassName),
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
	if nc.Spec.ExternalAccess.Gateway.Mode == nomadv1alpha1.GatewayModeExisting && nc.Spec.ExternalAccess.Gateway.Ref != nil {
		gwName = nc.Spec.ExternalAccess.Gateway.Ref.Name
		gwNs = gwapiv1.Namespace(nc.Spec.ExternalAccess.Gateway.Ref.Namespace)
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
			Hostnames:       []gwapiv1.Hostname{gwapiv1.Hostname(nc.Spec.ExternalAccess.Gateway.HTTPHostname)},
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

// ensureGateway dispatches to the Managed or Existing gateway path based on
// spec.externalAccess.gateway.mode. Route application (buildTLSRoute/buildTCPRoutes) is
// unchanged for both modes — parentGateway already resolves to the operator's
// own Gateway (Managed) or the referenced one (Existing).
func (r *NomadClusterReconciler) ensureGateway(ctx context.Context, nc *nomadv1alpha1.NomadCluster) (string, bool, error) {
	if nc.Spec.ExternalAccess.Gateway.Mode == nomadv1alpha1.GatewayModeExisting {
		return r.ensureExistingGateway(ctx, nc)
	}
	return r.ensureManagedGateway(ctx, nc)
}

// ensureExistingGateway verifies the user-owned Gateway referenced by
// spec.externalAccess.gateway.ref: it must exist, carry a listener for the HTTP hostname and
// one TCP listener per RPC port, and admit the CR's namespace via
// allowedRoutes on those listeners. It never creates or mutates the Gateway —
// the user owns it. Returns ready=false (never an error) for any of those
// verification failures so the reconciler surfaces ExternalAccessReady=False rather
// than treating a misconfigured shared Gateway as a hard error.
func (r *NomadClusterReconciler) ensureExistingGateway(ctx context.Context, nc *nomadv1alpha1.NomadCluster) (string, bool, error) {
	ref := nc.Spec.ExternalAccess.Gateway.Ref
	// For an already-provisioned cluster (Ready/Degraded), a transient shared-
	// Gateway blip must NOT flip ExternalAccessReady to a False reason: this
	// function runs before the reconcile flap guard (#5/D4), which keeps the
	// last-known conditions intact and just requeues. Stamping a reason here
	// would be persisted by finish() and defeat that guard. During provisioning
	// (empty/Pending) the specific reason is #6's diagnostic value, so keep it.
	provisioned := nc.Status.Phase == nomadv1alpha1.PhaseReady || nc.Status.Phase == nomadv1alpha1.PhaseDegraded
	setNotReady := func(reason, msg string) {
		if provisioned {
			return
		}
		setCondition(nc, nomadv1alpha1.CondExternalAccessReady, metav1ConditionFalse, reason, msg)
	}
	var gw gwapiv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			setNotReady("GatewayNotFound",
				fmt.Sprintf("referenced Gateway %s/%s not found", ref.Namespace, ref.Name))
			return "", false, nil
		}
		return "", false, err
	}
	// Verify required listeners by NAME, not just port/protocol: the routes
	// (buildTLSRoute, buildTCPRoutes) attach via a fixed parentRef.SectionName
	// — listenerNameHTTP and listenerNameRPC(ordinal) — and Gateway API
	// matches sectionName against the listener's literal Name. A same-port
	// listener under a different name would pass a port-only check yet the
	// real Gateway controller would reject the route, so verification must
	// look up the exact name each route needs.
	byName := make(map[gwapiv1.SectionName]gwapiv1.Listener, len(gw.Spec.Listeners))
	for _, l := range gw.Spec.Listeners {
		byName[l.Name] = l
	}
	httpListener, ok := byName[listenerNameHTTP]
	if !ok || httpListener.Protocol != gwapiv1.TLSProtocolType ||
		httpListener.Hostname == nil || string(*httpListener.Hostname) != nc.Spec.ExternalAccess.Gateway.HTTPHostname {
		setNotReady("HTTPListenerInvalid",
			"referenced Gateway lacks a valid HTTPS/TLS listener for the configured hostname")
		return "", false, nil
	}
	if !listenerAdmitsNamespace(httpListener, gw.Namespace, nc.Namespace) {
		setNotReady("NamespaceNotAdmitted",
			"referenced Gateway HTTP listener does not admit namespace "+nc.Namespace)
		return "", false, nil
	}
	for ordinal, p := range nc.Spec.ExternalAccess.Gateway.RPCPorts {
		l, ok := byName[gwapiv1.SectionName(listenerNameRPC(ordinal))]
		if !ok || l.Protocol != gwapiv1.TCPProtocolType || l.Port != p {
			setNotReady("RPCListenerInvalid",
				fmt.Sprintf("referenced Gateway lacks a valid TCP listener for RPC port %d", p))
			return "", false, nil
		}
		if !listenerAdmitsNamespace(l, gw.Namespace, nc.Namespace) {
			setNotReady("NamespaceNotAdmitted",
				"referenced Gateway RPC listener does not admit namespace "+nc.Namespace)
			return "", false, nil
		}
	}
	for _, a := range gw.Status.Addresses {
		if a.Value != "" {
			return a.Value, true, nil
		}
	}
	setNotReady("GatewayNoAddress",
		"referenced Gateway has no assigned address yet")
	return "", false, nil
}

// listenerAdmitsNamespace reports whether a Gateway listener's allowedRoutes
// would admit a Route created in routeNS, given the Gateway's own namespace
// gwNS. Only the Core support levels (All, Same) are evaluated;
// Selector-based admission has no present caller and is treated as
// not-admitted (fail closed) rather than requiring an extra Namespace lookup
// with no concrete use case yet.
func listenerAdmitsNamespace(l gwapiv1.Listener, gwNS, routeNS string) bool {
	from := gwapiv1.NamespacesFromSame
	if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil && l.AllowedRoutes.Namespaces.From != nil {
		from = *l.AllowedRoutes.Namespaces.From
	}
	switch from {
	case gwapiv1.NamespacesFromAll:
		return true
	case gwapiv1.NamespacesFromSame:
		return gwNS == routeNS
	default:
		return false
	}
}
