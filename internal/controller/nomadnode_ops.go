package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// NomadNodeOps is the subset of the Nomad client the node reflector needs,
// defined at the consumer (Go convention). *nomad.Client satisfies it, and
// envtest injects a fake. It is intentionally separate from the cluster
// reconciler's NomadOps so the two controllers' test seams stay decoupled.
type NomadNodeOps interface {
	ListNodes(ctx context.Context) ([]*api.NodeListStub, error)
	NodeInfo(ctx context.Context, id string) (*api.Node, error)
	SetEligibility(ctx context.Context, nodeID string, eligible bool) error
	UpdateDrain(ctx context.Context, nodeID string, spec *api.DrainSpec, markEligible bool) error
}

// NomadNodeClientFactory builds a NomadNodeOps from a per-cluster Config.
type NomadNodeClientFactory func(cfg nomad.Config) (NomadNodeOps, error)

// DefaultNomadNodeClientFactory constructs the real client.
func DefaultNomadNodeClientFactory(cfg nomad.Config) (NomadNodeOps, error) {
	return nomad.New(cfg)
}
