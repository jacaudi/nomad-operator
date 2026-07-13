package controller

import (
	"fmt"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

// resourceNames is the single source of truth for every object name and
// label set derived from a NomadCluster. Callers must go through names(nc)
// rather than formatting names themselves, so the naming contract stays
// single-sourced across the controller.
type resourceNames struct {
	nc           *nomadv1alpha1.NomadCluster
	StatefulSet  string
	HeadlessSvc  string
	APISvc       string
	ConfigMap    string
	PDB          string
	Gateway      string
	GossipSecret string
	TokenSecret  string
	TLSRoute     string
	LBService    string
}

func names(nc *nomadv1alpha1.NomadCluster) resourceNames {
	base := nc.Name + "-server"
	apiSvc := nc.Name + "-nomad"
	return resourceNames{
		nc:           nc,
		StatefulSet:  base,
		HeadlessSvc:  base + "-headless",
		APISvc:       apiSvc,
		ConfigMap:    base + "-config",
		PDB:          base + "-pdb",
		Gateway:      nc.Name + "-gateway",
		GossipSecret: nc.Name + "-nomad-gossip-key",
		TokenSecret:  nc.Name + "-nomad-bootstrap-token",
		TLSRoute:     apiSvc + "-tls",
		LBService:    nc.Name + "-lb",
	}
}

func (r resourceNames) PodName(ordinal int) string {
	return fmt.Sprintf("%s-%d", r.StatefulSet, ordinal)
}
func (r resourceNames) PodSvc(ordinal int) string {
	return fmt.Sprintf("%s-%d-rpc", r.StatefulSet, ordinal)
}
func (r resourceNames) TCPRoute(ordinal int) string {
	return fmt.Sprintf("%s-rpc-%d", r.nc.Name, ordinal)
}

func (r resourceNames) Labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "nomad",
		"app.kubernetes.io/instance":   r.nc.Name,
		"app.kubernetes.io/managed-by": "nomad-operator",
	}
}
