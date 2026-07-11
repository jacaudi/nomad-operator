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
