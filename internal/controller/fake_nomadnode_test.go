package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// fakeNodeOps is a scriptable NomadNodeOps for envtest. list is returned by
// ListNodes; info maps node ID -> full node for NodeInfo; eligibility and
// drain calls are recorded for assertions.
type fakeNodeOps struct {
	list       []*api.NodeListStub
	info       map[string]*api.Node
	listErr    error
	eligErr    map[string]error // node ID -> error SetEligibility returns for it
	eligCalls  []eligCall
	drainCalls []drainCall
}

type eligCall struct {
	id       string
	eligible bool
}
type drainCall struct {
	id           string
	spec         *api.DrainSpec
	markEligible bool
}

func (f *fakeNodeOps) ListNodes(context.Context) ([]*api.NodeListStub, error) {
	return f.list, f.listErr
}
func (f *fakeNodeOps) NodeInfo(_ context.Context, id string) (*api.Node, error) {
	return f.info[id], nil
}
func (f *fakeNodeOps) SetEligibility(_ context.Context, id string, eligible bool) error {
	f.eligCalls = append(f.eligCalls, eligCall{id, eligible})
	return f.eligErr[id]
}
func (f *fakeNodeOps) UpdateDrain(_ context.Context, id string, spec *api.DrainSpec, markEligible bool) error {
	f.drainCalls = append(f.drainCalls, drainCall{id, spec, markEligible})
	return nil
}

func newFakeNodeFactory(f *fakeNodeOps) NomadNodeClientFactory {
	return func(nomad.Config) (NomadNodeOps, error) { return f, nil }
}
