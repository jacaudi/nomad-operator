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

// jobWriteOpts builds WriteOptions carrying the job's namespace (nil-safe). The
// controller injects job.Namespace before Plan/Register, but the deref is
// guarded so a nil Namespace from any caller cannot panic.
func jobWriteOpts(ctx context.Context, job *api.Job) *api.WriteOptions {
	w := &api.WriteOptions{}
	if job != nil && job.Namespace != nil {
		w.Namespace = *job.Namespace
	}
	return w.WithContext(ctx)
}

// nsQueryOpts builds QueryOptions scoped to a Nomad namespace.
func nsQueryOpts(ctx context.Context, namespace string) *api.QueryOptions {
	return (&api.QueryOptions{Namespace: namespace}).WithContext(ctx)
}

// writeOpts builds plain WriteOptions carrying the context.
func writeOpts(ctx context.Context) *api.WriteOptions {
	return (&api.WriteOptions{}).WithContext(ctx)
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

// NomadMember is the operator's projection of a Nomad server's autopilot health,
// decoupled from *api.ServerHealth so no api type leaks past this package.
type NomadMember struct {
	Name   string // ServerHealth.Name (node name)
	Addr   string // ServerHealth.Address (advertise.rpc, "ip:port")
	Status string // ServerHealth.SerfStatus ("alive"/"failed"/"left")
	Leader bool   // ServerHealth.Leader
	Voter  bool   // ServerHealth.Voter
}

func toMembers(in []api.ServerHealth) []NomadMember {
	out := make([]NomadMember, 0, len(in))
	for _, s := range in {
		out = append(out, NomadMember{
			Name:   s.Name,
			Addr:   s.Address,
			Status: s.SerfStatus,
			Leader: s.Leader,
			Voter:  s.Voter,
		})
	}
	return out
}

// ServerHealth returns the per-server autopilot health for the cluster's
// servers (name, advertise.rpc address, serf status, leader, voter). It reads
// Operator().AutopilotServerHealth (GET /v1/operator/autopilot/health), an
// operator endpoint that is not namespace-scoped.
func (c *Client) ServerHealth(ctx context.Context) ([]NomadMember, error) {
	reply, _, err := c.api.Operator().AutopilotServerHealth(queryOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("nomad: autopilot server health: %w", err)
	}
	return toMembers(reply.Servers), nil
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

// GetNodePool returns the node pool by name, or (nil, nil) if it does not exist.
func (c *Client) GetNodePool(ctx context.Context, name string) (*api.NodePool, error) {
	pool, _, err := c.api.NodePools().Info(name, queryOpts(ctx))
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("nomad: get node pool %q: %w", name, err)
	}
	return pool, nil
}

// UpsertNodePool creates or updates a node pool (Nomad's Register is an upsert).
func (c *Client) UpsertNodePool(ctx context.Context, pool *api.NodePool) error {
	if _, err := c.api.NodePools().Register(pool, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: upsert node pool %q: %w", pool.Name, err)
	}
	return nil
}

// DeleteNodePool deletes a node pool by name. Nomad refuses to delete a pool
// that still has nodes or non-terminal jobs (see IsNodePoolNotEmpty).
func (c *Client) DeleteNodePool(ctx context.Context, name string) error {
	if _, err := c.api.NodePools().Delete(name, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: delete node pool %q: %w", name, err)
	}
	return nil
}

// CountNodePoolNodes returns how many nodes are registered in the pool.
func (c *Client) CountNodePoolNodes(ctx context.Context, name string) (int, error) {
	nodes, _, err := c.api.NodePools().ListNodes(name, queryOpts(ctx))
	if err != nil {
		return 0, fmt.Errorf("nomad: list node pool %q nodes: %w", name, err)
	}
	return len(nodes), nil
}

// CountNodePoolJobs returns how many jobs target the pool.
func (c *Client) CountNodePoolJobs(ctx context.Context, name string) (int, error) {
	jobs, _, err := c.api.NodePools().ListJobs(name, queryOpts(ctx))
	if err != nil {
		return 0, fmt.Errorf("nomad: list node pool %q jobs: %w", name, err)
	}
	return len(jobs), nil
}

// jobDiffNone is Nomad's JobDiff.Type value when a Plan finds no changes.
const jobDiffNone = "None"

// GetJob returns the job by ID within the given Nomad namespace, or (nil, nil)
// if it does not exist.
func (c *Client) GetJob(ctx context.Context, namespace, jobID string) (*api.Job, error) {
	job, _, err := c.api.Jobs().Info(jobID, nsQueryOpts(ctx, namespace))
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("nomad: get job %q: %w", jobID, err)
	}
	return job, nil
}

// PlanJob dry-runs a register and reports whether applying the job would change
// anything (JobDiff.Type != "None"). A not-yet-registered job plans as "Added".
// The job must have a non-nil ID (Nomad's PlanOpts requires it); the caller
// injects it.
func (c *Client) PlanJob(ctx context.Context, job *api.Job) (bool, error) {
	resp, _, err := c.api.Jobs().Plan(job, true, jobWriteOpts(ctx, job))
	if err != nil {
		return false, fmt.Errorf("nomad: plan job: %w", err)
	}
	if resp.Diff == nil {
		return true, nil // no diff computed → treat as changed (safe: at worst one extra Register)
	}
	return resp.Diff.Type != jobDiffNone, nil
}

// RegisterJob upserts the job (Nomad's Register is an upsert) and returns any
// server warnings (e.g. deprecation notices).
func (c *Client) RegisterJob(ctx context.Context, job *api.Job) (string, error) {
	resp, _, err := c.api.Jobs().Register(job, jobWriteOpts(ctx, job))
	if err != nil {
		return "", fmt.Errorf("nomad: register job: %w", err)
	}
	return resp.Warnings, nil
}

// DeregisterJob stops and removes a job within the given Nomad namespace.
// purge=true fully removes the job record (vs leaving a queryable dead record
// that would collide with a re-create).
func (c *Client) DeregisterJob(ctx context.Context, namespace, jobID string, purge bool) error {
	if _, _, err := c.api.Jobs().Deregister(jobID, purge, (&api.WriteOptions{Namespace: namespace}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: deregister job %q: %w", jobID, err)
	}
	return nil
}

// JobGroupSummary returns per-task-group allocation summaries for the job within
// the given Nomad namespace.
func (c *Client) JobGroupSummary(ctx context.Context, namespace, jobID string) (map[string]api.TaskGroupSummary, error) {
	summary, _, err := c.api.Jobs().Summary(jobID, nsQueryOpts(ctx, namespace))
	if err != nil {
		return nil, fmt.Errorf("nomad: job summary %q: %w", jobID, err)
	}
	return summary.Summary, nil
}
