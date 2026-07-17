package controller

import (
	"context"

	"github.com/hashicorp/nomad/api"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// fakeNomadJobOps is a scriptable NomadJobOps for envtest. Set the fields to
// control behavior and inspect the recorded calls.
type fakeNomadJobOps struct {
	jobs        map[string]*api.Job             // seeded Info state, keyed by jobID
	summary     map[string]api.TaskGroupSummary // JobGroupSummary result
	planChanged bool                            // PlanJob result
	warnings    string                          // RegisterJob warnings

	getErr, planErr, registerErr, deregisterErr, summaryErr error

	registered     []*api.Job // every RegisterJob arg, in order
	deregistered   []string   // every DeregisterJob jobID, in order
	deregisteredNS []string   // the namespace for each DeregisterJob, in order
	purged         []bool     // the purge flag for each DeregisterJob, in order
	getNS          []string   // the namespace for each GetJob, in order
	summaryNS      []string   // the namespace for each JobGroupSummary, in order
}

func newFakeJobOps() *fakeNomadJobOps {
	return &fakeNomadJobOps{jobs: map[string]*api.Job{}, summary: map[string]api.TaskGroupSummary{}}
}

func (f *fakeNomadJobOps) GetJob(_ context.Context, namespace, jobID string) (*api.Job, error) {
	f.getNS = append(f.getNS, namespace)
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.jobs[jobID], nil // nil == not found, matching the real 404 mapping
}

func (f *fakeNomadJobOps) PlanJob(_ context.Context, _ *api.Job) (bool, error) {
	if f.planErr != nil {
		return false, f.planErr
	}
	return f.planChanged, nil
}

func (f *fakeNomadJobOps) RegisterJob(_ context.Context, job *api.Job) (string, error) {
	if f.registerErr != nil {
		return "", f.registerErr
	}
	cp := *job
	f.registered = append(f.registered, &cp)
	if cp.ID != nil {
		f.jobs[*cp.ID] = &cp
	}
	return f.warnings, nil
}

func (f *fakeNomadJobOps) DeregisterJob(_ context.Context, namespace, jobID string, purge bool) error {
	if f.deregisterErr != nil {
		return f.deregisterErr
	}
	f.deregistered = append(f.deregistered, jobID)
	f.deregisteredNS = append(f.deregisteredNS, namespace)
	f.purged = append(f.purged, purge)
	delete(f.jobs, jobID)
	return nil
}

func (f *fakeNomadJobOps) JobGroupSummary(_ context.Context, namespace, _ string) (map[string]api.TaskGroupSummary, error) {
	f.summaryNS = append(f.summaryNS, namespace)
	if f.summaryErr != nil {
		return nil, f.summaryErr
	}
	return f.summary, nil
}

// factory returns a NomadJobClientFactory that always yields this fake.
func (f *fakeNomadJobOps) factory() NomadJobClientFactory {
	return func(_ nomad.Config) (NomadJobOps, error) { return f, nil }
}

var _ NomadJobOps = (*fakeNomadJobOps)(nil)
