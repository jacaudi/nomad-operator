package nomad

import (
	"errors"
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// aclBootstrapAlreadyDoneText is the substring Nomad's server embeds in the
// HTTP 400 body once /v1/acl/bootstrap has already succeeded (nomad's
// nomad/acl_endpoint.go: `structs.NewErrRPCCodedf(400, "ACL bootstrap
// already done (reset index: %d)", resetIdx)`). Pinned against the actual
// server source rather than guessed; see errors_test.go for the live
// round-trip that exercises this against the real api.Client error shape.
const aclBootstrapAlreadyDoneText = "ACL bootstrap already done"

// IsACLAlreadyBootstrapped reports whether err is the error ACLBootstrap
// returns when the cluster's ACL system has already been bootstrapped by a
// prior call. api.Client surfaces this as an api.UnexpectedResponseError
// (HTTP 400, body containing aclBootstrapAlreadyDoneText) wrapped by
// (*Client).ACLBootstrap's %w — callers use this instead of string-matching
// the wrapped error inline.
func IsACLAlreadyBootstrapped(err error) bool {
	ure, ok := errors.AsType[api.UnexpectedResponseError](err)
	if !ok {
		return false
	}
	return ure.StatusCode() == http.StatusBadRequest && strings.Contains(ure.Body(), aclBootstrapAlreadyDoneText)
}

// nodePoolNotEmptyTexts are the substrings Nomad's server embeds in the error
// body when a node pool cannot be deleted because it still has nodes or
// non-terminal jobs (nomad/node_pool_endpoint.go). Verified against Nomad
// source; the Task-10 integration spike confirms the exact v2.0.4 wording.
// Used only to choose a friendlier DeleteBlocked reason — control flow keeps the
// finalizer on ANY Delete error regardless.
var nodePoolNotEmptyTexts = []string{"has nodes in regions", "has non-terminal jobs in regions"}

// IsNotFound reports whether err is (or wraps) an api.UnexpectedResponseError
// with HTTP 404 — e.g. Info on a node pool that does not exist.
func IsNotFound(err error) bool {
	ure, ok := errors.AsType[api.UnexpectedResponseError](err)
	if !ok {
		return false
	}
	return ure.StatusCode() == http.StatusNotFound
}

// IsNodePoolNotEmpty reports whether err is Nomad's refusal to delete a node
// pool that still has nodes or non-terminal jobs.
func IsNodePoolNotEmpty(err error) bool {
	ure, ok := errors.AsType[api.UnexpectedResponseError](err)
	if !ok {
		return false
	}
	body := ure.Body()
	for _, s := range nodePoolNotEmptyTexts {
		if strings.Contains(body, s) {
			return true
		}
	}
	return false
}

// namespaceNotEmptyTexts are the substrings Nomad's server embeds in the error
// body when a namespace cannot be deleted because it still has non-terminal
// jobs. Confirmed live against Nomad v2.0.4 (2026-07-17): the refusal body is
// `namespace "<name>" has non-terminal jobs in regions: [global]`. Used only to
// choose a friendlier DeleteBlocked reason — control flow keeps the finalizer on
// ANY Delete error.
var namespaceNotEmptyTexts = []string{"has non-terminal jobs"}

// IsNamespaceNotEmpty reports whether err is Nomad's refusal to delete a
// namespace that still has non-terminal jobs.
func IsNamespaceNotEmpty(err error) bool {
	ure, ok := errors.AsType[api.UnexpectedResponseError](err)
	if !ok {
		return false
	}
	body := ure.Body()
	for _, s := range namespaceNotEmptyTexts {
		if strings.Contains(body, s) {
			return true
		}
	}
	return false
}
