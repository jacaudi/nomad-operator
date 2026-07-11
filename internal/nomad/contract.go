// Package nomad is the operator's boundary to a single Nomad endpoint. It wraps
// github.com/hashicorp/nomad/api with a small, read-oriented surface and this
// compile-time contract, which pins the exact api symbols used.
package nomad

// This file references every github.com/hashicorp/nomad/api symbol the operator
// depends on, so a version bump that renames or reshapes any of them breaks
// `go build` here — loudly and early — instead of failing at runtime. Extend it
// as later slices bind to more of the api. Nothing here is executed; it is
// type-checked only.

import "github.com/hashicorp/nomad/api"

// Type pins — the structs and clients the operator reads.
var (
	_ api.Config
	_ api.TLSConfig
	_ api.QueryOptions
	_ api.Client
	_ api.Nodes
	_ api.Agent
	_ api.NodeListStub
	_ api.Node
	_ api.DriverInfo
	_ api.Status
	_ api.ACLTokens
	_ api.ACLToken
	_ api.AgentHealthResponse
	_ api.AgentHealth
	_ api.WriteOptions
	_ api.WriteMeta
)

// Method / constructor signature pins (method expressions; receiver never evaluated).
var (
	_ = api.NewClient
	_ = (*api.Client).Nodes
	_ = (*api.Client).Agent
	_ = (*api.Client).Status
	_ = (*api.Client).ACLTokens
	_ = (*api.Nodes).List
	_ = (*api.Nodes).Info
	_ = (*api.Agent).Self
	_ = (*api.Agent).Health
	_ = (*api.Status).Leader
	_ = (*api.ACLTokens).BootstrapOpts
	_ = (*api.QueryOptions).WithContext
	_ = (*api.WriteOptions).WithContext
)

// Constant pins — the node status and eligibility value set the operator maps.
var (
	_ = api.NodeStatusInit
	_ = api.NodeStatusReady
	_ = api.NodeStatusDown
	_ = api.NodeStatusDisconnected
	_ = api.NodeSchedulingEligible
	_ = api.NodeSchedulingIneligible
)
