package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// NomadPoolOps is the subset of the Nomad client the pool reconciler needs,
// defined at the consumer (Go convention). *nomad.Client satisfies it, and
// envtest injects a fake. It is intentionally separate from NomadOps (slice 2)
// and NomadNodeOps (slice 3) so the controllers' test seams stay decoupled.
type NomadPoolOps interface {
	GetNodePool(ctx context.Context, name string) (*api.NodePool, error)
	UpsertNodePool(ctx context.Context, pool *api.NodePool) error
	DeleteNodePool(ctx context.Context, name string) error
	CountNodePoolNodes(ctx context.Context, name string) (int, error)
	CountNodePoolJobs(ctx context.Context, name string) (int, error)
}

// NomadPoolClientFactory builds a NomadPoolOps from a per-cluster Config.
type NomadPoolClientFactory func(cfg nomad.Config) (NomadPoolOps, error)

// DefaultNomadPoolClientFactory constructs the real client.
func DefaultNomadPoolClientFactory(cfg nomad.Config) (NomadPoolOps, error) {
	return nomad.New(cfg)
}
