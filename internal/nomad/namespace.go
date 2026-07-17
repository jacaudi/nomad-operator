package nomad

import (
	"context"
	"fmt"

	"github.com/hashicorp/nomad/api"
)

// GetNamespace returns the namespace by name, or (nil, nil) if it does not exist.
func (c *Client) GetNamespace(ctx context.Context, name string) (*api.Namespace, error) {
	ns, _, err := c.api.Namespaces().Info(name, queryOpts(ctx))
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("nomad: get namespace %q: %w", name, err)
	}
	return ns, nil
}

// UpsertNamespace creates or updates a namespace (Nomad's Register is an upsert).
func (c *Client) UpsertNamespace(ctx context.Context, ns *api.Namespace) error {
	if _, err := c.api.Namespaces().Register(ns, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: upsert namespace %q: %w", ns.Name, err)
	}
	return nil
}

// DeleteNamespace deletes a namespace by name. Nomad refuses to delete a
// namespace that still has non-terminal jobs (see IsNamespaceNotEmpty).
func (c *Client) DeleteNamespace(ctx context.Context, name string) error {
	if _, err := c.api.Namespaces().Delete(name, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("nomad: delete namespace %q: %w", name, err)
	}
	return nil
}

// CountNamespaceJobs returns how many jobs exist in the namespace. Note:
// Jobs().List is unfiltered (includes terminal jobs); this is a raw total, used
// only for the informational status.jobCount (the delete gate is the Delete
// refusal, not this count).
func (c *Client) CountNamespaceJobs(ctx context.Context, name string) (int, error) {
	jobs, _, err := c.api.Jobs().List((&api.QueryOptions{Namespace: name}).WithContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("nomad: list namespace %q jobs: %w", name, err)
	}
	return len(jobs), nil
}
