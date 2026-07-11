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
	if f.bootstrapErr != nil {
		return "", f.bootstrapErr
	}
	f.bootstrapped = true
	f.lastToken = token
	return token, nil // BootstrapOpts echoes the supplied secret ID
}

func newFakeFactory(f *fakeNomad) NomadClientFactory {
	return func(nomad.Config) (NomadOps, error) { return f, nil }
}
