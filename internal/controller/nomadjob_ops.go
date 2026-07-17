package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// NomadJobOps is the subset of the Nomad client the job reconciler needs,
// defined at the consumer (Go convention). *nomad.Client satisfies it, and
// envtest injects a fake. It is intentionally separate from NomadOps (slice 2),
// NomadNodeOps (slice 3), and NomadPoolOps (slice 4) so the controllers' test
// seams stay decoupled.
type NomadJobOps interface {
	GetJob(ctx context.Context, namespace, jobID string) (*api.Job, error)
	PlanJob(ctx context.Context, job *api.Job) (bool, error)
	RegisterJob(ctx context.Context, job *api.Job) (string, error)
	DeregisterJob(ctx context.Context, namespace, jobID string, purge bool) error
	JobGroupSummary(ctx context.Context, namespace, jobID string) (map[string]api.TaskGroupSummary, error)
}

// NomadJobClientFactory builds a NomadJobOps from a per-cluster Config.
type NomadJobClientFactory func(cfg nomad.Config) (NomadJobOps, error)

// DefaultNomadJobClientFactory constructs the real client.
func DefaultNomadJobClientFactory(cfg nomad.Config) (NomadJobOps, error) {
	return nomad.New(cfg)
}
