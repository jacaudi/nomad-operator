package controller

import (
	"context"
	"errors"

	"github.com/jacaudi/nomad-operator/internal/nomad"
)

type fakeNomad struct {
	leader        string
	serverHealthy bool
	pingErr       error
	bootstrapErr  error
	bootstrapped  bool
	lastToken     string
	// bootstrapCalls counts every ACLBootstrap invocation, regardless of
	// outcome. Tests use it to assert a retry actually happened (or, in the
	// steady state, that it did NOT) — bootstrapped alone can't distinguish
	// "never called" from "called and failed".
	bootstrapCalls int
	// members/memberErr drive ServerHealth for status.members/quorum tests.
	members   []nomad.NomadMember
	memberErr error
}

func (f *fakeNomad) Ping(context.Context) error { return f.pingErr }
func (f *fakeNomad) Leader(context.Context) (string, error) {
	if f.leader == "" {
		return "", errors.New("no leader")
	}
	return f.leader, nil
}
func (f *fakeNomad) ServerHealthy(context.Context) (bool, error) { return f.serverHealthy, nil }
func (f *fakeNomad) ACLBootstrap(_ context.Context, token string) (string, error) {
	f.bootstrapCalls++
	if f.bootstrapErr != nil {
		return "", f.bootstrapErr
	}
	f.bootstrapped = true
	f.lastToken = token
	return token, nil // BootstrapOpts echoes the supplied secret ID
}
func (f *fakeNomad) ServerHealth(context.Context) ([]nomad.NomadMember, error) {
	return f.members, f.memberErr
}

func newFakeFactory(f *fakeNomad) NomadClientFactory {
	return func(nomad.Config) (NomadOps, error) { return f, nil }
}
