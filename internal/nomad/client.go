package nomad

import (
	"context"
	"fmt"

	"github.com/hashicorp/nomad/api"
)

// Client is a thin wrapper over the Nomad API client for a single endpoint.
// It exposes reads (nodes, agent health, leader) and a small write surface
// (ACL bootstrap) needed by the operator's reconcile loop.
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
	if cfg.hasTLSMaterial() {
		apiCfg.TLSConfig = &api.TLSConfig{
			CACert:        cfg.TLS.CACert,
			ClientCert:    cfg.TLS.ClientCert,
			ClientKey:     cfg.TLS.ClientKey,
			Insecure:      cfg.TLS.Insecure,
			TLSServerName: cfg.TLSServerName,
			CACertPEM:     cfg.TLS.CACertPEM,
			ClientCertPEM: cfg.TLS.ClientCertPEM,
			ClientKeyPEM:  cfg.TLS.ClientKeyPEM,
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

// Leader returns the current Raft leader's "ip:port" RPC address, or an error
// if no leader is known. Status().Leader takes no QueryOptions, so ctx is
// retained for a uniform signature but not threaded (same as Ping).
func (c *Client) Leader(ctx context.Context) (string, error) {
	leader, err := c.api.Status().Leader()
	if err != nil {
		return "", fmt.Errorf("nomad: leader: %w", err)
	}
	return leader, nil
}

// ServerHealthy reports whether this agent's server subsystem is healthy
// (which, for Nomad, requires a known leader). Agent().Health takes no
// QueryOptions; ctx is retained for signature uniformity.
func (c *Client) ServerHealthy(ctx context.Context) (bool, error) {
	h, err := c.api.Agent().Health()
	if err != nil {
		return false, fmt.Errorf("nomad: health: %w", err)
	}
	return h.Server != nil && h.Server.Ok, nil
}

// ACLBootstrap bootstraps the ACL system with an operator-supplied management
// token (Nomad's BootstrapOpts form) and returns the resulting secret ID. Using
// a supplied token makes bootstrap idempotent: the caller persists the token
// first, then calls this, so a crash-and-retry re-submits the same token.
func (c *Client) ACLBootstrap(ctx context.Context, bootstrapToken string) (string, error) {
	tok, _, err := c.api.ACLTokens().BootstrapOpts(bootstrapToken, (&api.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("nomad: acl bootstrap: %w", err)
	}
	return tok.SecretID, nil
}

// SetEligibility toggles a node's scheduling eligibility (Nomad's cordon knob).
func (c *Client) SetEligibility(ctx context.Context, nodeID string, eligible bool) error {
	if _, err := c.api.Nodes().ToggleEligibility(nodeID, eligible, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: set eligibility %q: %w", nodeID, err)
	}
	return nil
}

// UpdateDrain sets or cancels a node's drain. A nil spec cancels an active
// drain; markEligible marks the node eligible when canceling.
func (c *Client) UpdateDrain(ctx context.Context, nodeID string, spec *api.DrainSpec, markEligible bool) error {
	if _, err := c.api.Nodes().UpdateDrain(nodeID, spec, markEligible, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: update drain %q: %w", nodeID, err)
	}
	return nil
}
