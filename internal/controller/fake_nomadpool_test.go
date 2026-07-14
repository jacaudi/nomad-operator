package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// fakeNomadPoolOps is a scriptable NomadPoolOps for envtest. Set the *Fn hooks
// to control behavior and inspect the recorded calls.
type fakeNomadPoolOps struct {
	pools     map[string]*api.NodePool // seeded pool state, keyed by name
	nodeCount int
	jobCount  int

	getErr    error
	upsertErr error
	deleteErr error

	registered []*api.NodePool // every UpsertNodePool arg, in order
	deleted    []string        // every DeleteNodePool name, in order
}

func newFakePoolOps() *fakeNomadPoolOps {
	return &fakeNomadPoolOps{pools: map[string]*api.NodePool{}}
}

func (f *fakeNomadPoolOps) GetNodePool(_ context.Context, name string) (*api.NodePool, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.pools[name], nil // nil == not found, matching the real 404 mapping
}

func (f *fakeNomadPoolOps) UpsertNodePool(_ context.Context, pool *api.NodePool) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	cp := *pool
	f.registered = append(f.registered, &cp)
	f.pools[pool.Name] = &cp
	return nil
}

func (f *fakeNomadPoolOps) DeleteNodePool(_ context.Context, name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, name)
	delete(f.pools, name)
	return nil
}

func (f *fakeNomadPoolOps) CountNodePoolNodes(_ context.Context, _ string) (int, error) {
	return f.nodeCount, nil
}

func (f *fakeNomadPoolOps) CountNodePoolJobs(_ context.Context, _ string) (int, error) {
	return f.jobCount, nil
}

// factory returns a NomadPoolClientFactory that always yields this fake.
func (f *fakeNomadPoolOps) factory() NomadPoolClientFactory {
	return func(_ nomad.Config) (NomadPoolOps, error) { return f, nil }
}

var _ NomadPoolOps = (*fakeNomadPoolOps)(nil)
