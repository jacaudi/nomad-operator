package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// NomadNamespaceOps is the subset of the Nomad client the namespace reconciler
// needs, defined at the consumer (Go convention). *nomad.Client satisfies it,
// and envtest injects a fake. It is intentionally separate from the other *Ops
// interfaces so the controllers' test seams stay decoupled.
type NomadNamespaceOps interface {
	GetNamespace(ctx context.Context, name string) (*api.Namespace, error)
	UpsertNamespace(ctx context.Context, ns *api.Namespace) error
	DeleteNamespace(ctx context.Context, name string) error
	CountNamespaceJobs(ctx context.Context, name string) (int, error)
}

// NomadNamespaceClientFactory builds a NomadNamespaceOps from a per-cluster Config.
type NomadNamespaceClientFactory func(cfg nomad.Config) (NomadNamespaceOps, error)

// DefaultNomadNamespaceClientFactory constructs the real client.
func DefaultNomadNamespaceClientFactory(cfg nomad.Config) (NomadNamespaceOps, error) {
	return nomad.New(cfg)
}
