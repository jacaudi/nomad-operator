package nomad

import (
	"context"
	"fmt"

	"github.com/hashicorp/nomad/api"
)

// Client is a thin, read-oriented wrapper over the Nomad API client for a
// single endpoint. Foundation exposes reads only; writes arrive in later slices.
type Client struct {
	api *api.Client
}

// New builds a Client from Config. It validates the config and constructs the
// underlying *api.Client from an explicit api.Config (never api.DefaultConfig,
// which would absorb the process's NOMAD_* environment).
func New(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	apiCfg := &api.Config{
		Address:  cfg.Address,
		Region:   cfg.Region,
		SecretID: cfg.Token,
	}
	if cfg.TLS != (TLSConfig{}) {
		apiCfg.TLSConfig = &api.TLSConfig{
			CACert:     cfg.TLS.CACert,
			ClientCert: cfg.TLS.ClientCert,
			ClientKey:  cfg.TLS.ClientKey,
			Insecure:   cfg.TLS.Insecure,
		}
	}
	c, err := api.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("nomad: new client: %w", err)
	}
	return &Client{api: c}, nil
}

func queryOpts(ctx context.Context) *api.QueryOptions {
	return (&api.QueryOptions{}).WithContext(ctx)
}

// ListNodes returns the node stubs registered with Nomad.
func (c *Client) ListNodes(ctx context.Context) ([]*api.NodeListStub, error) {
	nodes, _, err := c.api.Nodes().List(queryOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("nomad: list nodes: %w", err)
	}
	return nodes, nil
}

// NodeInfo returns the full node record for a node ID.
func (c *Client) NodeInfo(ctx context.Context, id string) (*api.Node, error) {
	node, _, err := c.api.Nodes().Info(id, queryOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("nomad: node info %q: %w", id, err)
	}
	return node, nil
}

// Ping verifies connectivity and auth by reading agent self.
//
// api.Agent().Self() takes no QueryOptions, so ctx is not threaded through the
// call today; it is retained for a uniform, cancellation-ready signature. Go
// permits unused parameters and `go vet` does not flag them; if golangci-lint
// (unparam) is later added, either route this through a QueryOptions-accepting
// call or annotate deliberately.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.api.Agent().Self(); err != nil {
		return fmt.Errorf("nomad: ping: %w", err)
	}
	return nil
}
