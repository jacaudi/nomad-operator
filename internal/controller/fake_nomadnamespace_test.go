package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// fakeNomadNamespaceOps is a scriptable NomadNamespaceOps for envtest. Set the
// fields to control behavior and inspect the recorded calls.
type fakeNomadNamespaceOps struct {
	namespaces map[string]*api.Namespace // seeded state, keyed by name
	jobCount   int

	getErr    error
	upsertErr error
	deleteErr error

	registered []*api.Namespace // every UpsertNamespace arg, in order
	deleted    []string         // every DeleteNamespace name, in order
}

func newFakeNamespaceOps() *fakeNomadNamespaceOps {
	return &fakeNomadNamespaceOps{namespaces: map[string]*api.Namespace{}}
}

func (f *fakeNomadNamespaceOps) GetNamespace(_ context.Context, name string) (*api.Namespace, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.namespaces[name], nil // nil == not found, matching the real 404 mapping
}

func (f *fakeNomadNamespaceOps) UpsertNamespace(_ context.Context, ns *api.Namespace) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	cp := *ns
	f.registered = append(f.registered, &cp)
	f.namespaces[ns.Name] = &cp
	return nil
}

func (f *fakeNomadNamespaceOps) DeleteNamespace(_ context.Context, name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, name)
	delete(f.namespaces, name)
	return nil
}

func (f *fakeNomadNamespaceOps) CountNamespaceJobs(_ context.Context, _ string) (int, error) {
	return f.jobCount, nil
}

// factory returns a NomadNamespaceClientFactory that always yields this fake.
func (f *fakeNomadNamespaceOps) factory() NomadNamespaceClientFactory {
	return func(_ nomad.Config) (NomadNamespaceOps, error) { return f, nil }
}

var _ NomadNamespaceOps = (*fakeNomadNamespaceOps)(nil)
