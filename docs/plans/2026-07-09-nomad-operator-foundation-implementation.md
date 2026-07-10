# nomad-operator Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up a buildable, verifiable `nomad-operator` skeleton that compiles against a pinned Nomad v2.0.4 `api`, guards that pin with a compile-time contract, and proves it can read from a live Nomad via a hermetic dev-agent test — with no business CRDs or controllers.

**Architecture:** A Kubebuilder-scaffolded controller-runtime project whose manager starts idle (no controllers). A small `internal/nomad` package wraps `*api.Client` per-endpoint with a read-only surface; `internal/nomad/contract.go` references every `api` symbol used so signature drift breaks the build. Toolchain targets (`nomad-pin`, `generate`, `verify`) gate correctness.

**Tech Stack:** Go 1.26.4 · Kubebuilder / controller-runtime · `github.com/hashicorp/nomad/api` pinned at commit `5b83b133998a` (release v2.0.4) · standard-library `net`, `net/http/httptest`, `os/exec` for tests.

> **For Claude:** REQUIRED EXECUTION WORKFLOW (follow in order):
> 1. `superpowers:using-git-worktrees` — Isolate work in a dedicated worktree
> 2. `superpowers:subagent-driven-development` — Dispatch a fresh subagent per task
> 3. `superpowers:test-driven-development` — All subagents use TDD
> 4. `superpowers:verification-before-completion` — Verify all tests pass per task
> 5. `superpowers:requesting-code-review` — Code review after each task (built in)
> 6. After all tasks: comprehensive code review on full diff from branch point (automatic)
> 7. `superpowers:finishing-a-development-branch` — Complete the branch
>
> Skills carry their own model and effort settings. Do not override them.
>
> **Greenfield note:** the repo is not yet initialized. Task 1 runs `git init` and makes the first commit; the worktree/branch isolation in step 1 applies from Task 2 onward. If `using-git-worktrees` cannot operate before a repo exists, run Task 1 on a `foundation` branch in the main workspace, then isolate.

## Global Constraints

- **Go toolchain:** `go 1.26.4` (the `api` module requires ≥ 1.25). `go.mod` `go` directive = `1.26.4`.
- **Nomad api pin:** `github.com/hashicorp/nomad/api@5b83b133998a` → resolves to pseudo-version `v0.0.0-20260707172059-5b83b133998a` == release **v2.0.4**. The `api` submodule has **no semver tags**; never pin via `@v2.0.x`.
- **Module path:** `github.com/jacaudi/nomad-operator`.
- **API domain:** `operator.io` (group `nomad` → group-version `nomad.operator.io/v1alpha1` is realized only when the first CRD is created in a later slice; Foundation creates no CRDs).
- **Read-only:** no Nomad write calls anywhere in this slice.
- **No env inheritance:** clients are built from an explicit `Config`, never from `api.DefaultConfig()` — a per-endpoint client must not silently absorb the operator process's `NOMAD_*` env (this is load-bearing for the slice-2 per-cluster seam).
- **No `jobspec2`, no `nomad-openapi`:** keep the dependency graph to what the `api` package needs.
- **Build gate:** `make nomad-pin && make generate && make verify` must pass.

---

## File Structure

| File | Responsibility | Task |
|------|----------------|------|
| `go.mod`, `PROJECT`, `cmd/main.go`, `Makefile`, `config/**`, `.gitignore` | Kubebuilder scaffold; idle manager with health/ready probes; toolchain targets | 1 |
| `internal/nomad/contract.go` | Compile-time contract pinning every `api` symbol used; first `api` importer (materializes the pin) | 2 |
| `internal/nomad/config.go` | `Config`/`TLSConfig` value types + `Validate()` | 3 |
| `internal/nomad/config_test.go` | Unit tests for `Config.Validate()` | 3 |
| `internal/nomad/client.go` | Per-endpoint `Client` wrapping `*api.Client`; read methods | 4 |
| `internal/nomad/client_test.go` | Unit tests for client reads against `httptest` | 4 |
| `internal/nomad/client_integration_test.go` | Hermetic `nomad agent -dev` read test (build-tagged) | 5 |

**Task order rationale:** `contract.go` (Task 2) is the exhaustive `api` importer, so it is created *before* `config`/`client`. This materializes the api pin at the earliest point (de-risking the whole slice) and prevents `go mod tidy` from pruning the `require` when no package imports `api` yet.

---

## Task 1: Bootstrap project and toolchain

**Files:**
- Create: repo scaffold via Kubebuilder (`go.mod`, `PROJECT`, `cmd/main.go`, `config/**`, `.gitignore`, `hack/boilerplate.go.txt`)
- Modify: `go.mod` (`go` directive → `1.26.4`), `Makefile` (add `nomad-pin`, `verify`, and later `test-integration`)

**Interfaces:**
- Consumes: nothing (first task).
- Produces: a green build gate on the bare scaffold and the `nomad-pin`/`verify` Makefile targets. The api pin itself is *asserted* in Task 2 (the first importer). No Go symbols exported to other tasks.

**Prerequisites:** `kubebuilder` and `go 1.26.4` installed. Check with `kubebuilder version` and `go version`; if `kubebuilder` is missing, install per its docs before starting.

- [ ] **Step 1: Initialize the repo and Kubebuilder scaffold (move `docs/` aside first)**

`kubebuilder init` **refuses a non-empty directory** — its allowlist is only `go.mod`, `go.sum`, `LICENSE`, `README.md`, and dot-prefixed entries. The repo root holds `docs/`, which is not allowed. Temporarily relocate it. Run from the repo root (`/Users/user/Projects/github/nomad-operator`):

```bash
git init
mv docs .docs.tmp    # dot-prefixed → on kubebuilder's allowlist
kubebuilder init --domain operator.io --repo github.com/jacaudi/nomad-operator
mv .docs.tmp docs
```

Expected: Kubebuilder writes `go.mod`, `PROJECT`, `cmd/main.go`, `Makefile`, `config/`, `.gitignore`, `hack/boilerplate.go.txt`, and runs an initial `go mod tidy`. `PROJECT` shows `domain: operator.io` and `repo: github.com/jacaudi/nomad-operator`. `docs/` is back in place.

- [ ] **Step 2: Set the Go toolchain version**

Edit `go.mod` so the `go` directive reads exactly:

```
go 1.26.4
```

(Replace whatever version Kubebuilder wrote. Leave `require`/`toolchain` lines otherwise intact; `go mod tidy` may add a matching `toolchain go1.26.4` line — that is expected.)

- [ ] **Step 3: Add the `nomad-pin` and `verify` targets to the Makefile**

Append to `Makefile`:

```makefile
##@ Nomad

# The api submodule has NO semver tags — pin by commit. 5b83b133998a == release v2.0.4.
NOMAD_API_REF ?= 5b83b133998a

.PHONY: nomad-pin
nomad-pin: ## Pin github.com/hashicorp/nomad/api to NOMAD_API_REF (default v2.0.4 / 5b83b13).
	go get github.com/hashicorp/nomad/api@$(NOMAD_API_REF)
	go mod tidy

.PHONY: verify
verify: ## Build, vet, and test everything (the build gate).
	go build ./...
	go vet ./...
	go test ./...
```

> Do **not** run `make nomad-pin` yet. With no package importing `api`, the `go mod tidy` inside it would prune the `require` again. The pin is added and asserted in Task 2, which introduces the first importer (`contract.go`).

- [ ] **Step 4: Confirm the idle manager builds and the bare gate is green**

The Kubebuilder-scaffolded `cmd/main.go` already constructs a manager with `/healthz` and `/readyz` and registers **no controllers** — exactly the Foundation "idle manager." Do not add controllers. Run:

```bash
make generate
make verify
```

Expected: `make generate` fetches `controller-gen` into `bin/` on first run (network access needed once) and is a codegen no-op with no API types; exits 0. `make verify` runs `go build ./...`, `go vet ./...`, `go test ./...` all clean (no test files yet → `no test files`, exit 0).

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "chore: bootstrap kubebuilder scaffold with go 1.26.4 and nomad-pin target

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: `internal/nomad/contract.go` compile-time contract + materialize the api pin

**Files:**
- Create: `internal/nomad/contract.go`
- Modify: `go.mod`/`go.sum` (via `make nomad-pin`)

**Interfaces:**
- Consumes: `github.com/hashicorp/nomad/api` only.
- Produces: no exported symbols. Its jobs are (a) to fail `go build` if any pinned `api` symbol changes shape, and (b) to be the first `api` importer so the pin survives `go mod tidy`.

- [ ] **Step 1: Write the contract file**

Create `internal/nomad/contract.go`:

```go
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
)

// Method / constructor signature pins (method expressions; receiver never evaluated).
var (
	_ = api.NewClient
	_ = (*api.Client).Nodes
	_ = (*api.Client).Agent
	_ = (*api.Nodes).List
	_ = (*api.Nodes).Info
	_ = (*api.Agent).Self
	_ = (*api.QueryOptions).WithContext
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
```

- [ ] **Step 2: Materialize the api pin (now that an importer exists)**

Run:

```bash
make nomad-pin
```

Expected: `go get` adds `require github.com/hashicorp/nomad/api v0.0.0-20260707172059-5b83b133998a`; because `contract.go` imports the package, `go mod tidy` **keeps** it. Assert the pin:

```bash
go list -m github.com/hashicorp/nomad/api
```

Expected output: `github.com/hashicorp/nomad/api v0.0.0-20260707172059-5b83b133998a`

- [ ] **Step 3: Verify the contract compiles against the pinned api**

Run: `go build ./internal/nomad/`
Expected: exit 0. If any line fails to compile, the pinned `api` differs from what this slice assumes — fix the reference to match the real symbol and note the delta in the design's Appendix A.

- [ ] **Step 4: Verify the full gate**

Run: `make verify`
Expected: `go build`, `go vet`, `go test ./...` all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/nomad/contract.go go.mod go.sum
git commit -m "feat(nomad): add compile-time api contract and pin api to v2.0.4

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: `internal/nomad` Config

**Files:**
- Create: `internal/nomad/config.go`
- Test: `internal/nomad/config_test.go`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces:
  - `type Config struct { Address, Region, Token string; TLS TLSConfig }`
  - `type TLSConfig struct { CACert, ClientCert, ClientKey string; Insecure bool }`
  - `func (Config) Validate() error`

- [ ] **Step 1: Write the failing test**

Create `internal/nomad/config_test.go`:

```go
package nomad

import "testing"

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"empty address", Config{}, true},
		{"address only", Config{Address: "http://127.0.0.1:4646"}, false},
		{"client cert without key", Config{Address: "https://n:4646", TLS: TLSConfig{ClientCert: "c.pem"}}, true},
		{"client key without cert", Config{Address: "https://n:4646", TLS: TLSConfig{ClientKey: "k.pem"}}, true},
		{"client cert and key", Config{Address: "https://n:4646", TLS: TLSConfig{ClientCert: "c.pem", ClientKey: "k.pem"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/nomad/ -run TestConfigValidate -v`
Expected: compile failure — `undefined: Config` / `undefined: TLSConfig`.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/nomad/config.go`:

```go
package nomad

import "errors"

// Config describes how to reach one Nomad endpoint. It is intentionally
// per-endpoint (not a process-wide singleton) so a future NomadCluster
// reconciler can build one Client per cluster from a custom resource.
type Config struct {
	Address string // e.g. http://127.0.0.1:4646
	Region  string // optional
	Token   string // ACL token; empty in dev mode
	TLS     TLSConfig
}

// TLSConfig holds optional TLS material for talking to Nomad over HTTPS.
type TLSConfig struct {
	CACert     string // path to CA cert file
	ClientCert string // path to client cert file
	ClientKey  string // path to client key file
	Insecure   bool
}

// Validate reports whether the Config can be used to build a Client.
func (c Config) Validate() error {
	if c.Address == "" {
		return errors.New("nomad: Address is required")
	}
	if (c.TLS.ClientCert == "") != (c.TLS.ClientKey == "") {
		return errors.New("nomad: ClientCert and ClientKey must be set together")
	}
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/nomad/ -run TestConfigValidate -v`
Expected: `PASS` — all five subtests OK.

- [ ] **Step 5: Commit**

```bash
git add internal/nomad/config.go internal/nomad/config_test.go
git commit -m "feat(nomad): add per-endpoint Config with validation

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: `internal/nomad` Client (read-only wrapper)

**Files:**
- Create: `internal/nomad/client.go`
- Test: `internal/nomad/client_test.go`

**Interfaces:**
- Consumes: `Config` (Task 3).
- Produces:
  - `type Client struct { ... }`
  - `func New(cfg Config) (*Client, error)`
  - `func (*Client) ListNodes(ctx context.Context) ([]*api.NodeListStub, error)`
  - `func (*Client) NodeInfo(ctx context.Context, id string) (*api.Node, error)`
  - `func (*Client) Ping(ctx context.Context) error`

- [ ] **Step 1: Write the failing test**

Create `internal/nomad/client_test.go`:

```go
package nomad

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeNomad serves the minimal endpoints the Client reads.
func fakeNomad(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"ID":"abc123","Name":"n1","Status":"ready","SchedulingEligibility":"eligible"}]`))
	})
	mux.HandleFunc("/v1/node/abc123", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ID":"abc123","Name":"n1","Status":"ready"}`))
	})
	mux.HandleFunc("/v1/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"Region":"global"},"member":{},"stats":{}}`))
	})
	return httptest.NewServer(mux)
}

func TestClientReads(t *testing.T) {
	srv := fakeNomad(t)
	defer srv.Close()

	c, err := New(Config{Address: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	nodes, err := c.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "n1" || nodes[0].Status != "ready" {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}

	node, err := c.NodeInfo(ctx, "abc123")
	if err != nil {
		t.Fatalf("NodeInfo: %v", err)
	}
	if node.ID != "abc123" {
		t.Fatalf("unexpected node: %+v", node)
	}

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error for empty Address")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/nomad/ -run TestClient -v`
Expected: compile failure — `undefined: New`.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/nomad/client.go`. Note `New` builds an **explicit** `*api.Config` and does **not** call `api.DefaultConfig()` — that function reads `NOMAD_*` env vars, which must never leak into a per-endpoint client.

```go
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/nomad/ -run TestClient -v && go test ./internal/nomad/ -run TestNewRejectsBadConfig -v`
Expected: `PASS` for both. If the build fails on `WithContext` or a field name, that is the contract catching real api drift — reconcile against the pinned `api` source before proceeding.

- [ ] **Step 5: Format and re-verify**

Run: `gofmt -w internal/nomad/ && make verify`
Expected: no diff surprises; full gate green.

- [ ] **Step 6: Commit**

```bash
git add internal/nomad/client.go internal/nomad/client_test.go
git commit -m "feat(nomad): add read-only Client wrapper over api.Client

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Hermetic dev-agent integration test

**Files:**
- Create: `internal/nomad/client_integration_test.go`
- Modify: `Makefile` (add `test-integration`)

**Interfaces:**
- Consumes: `New`, `ListNodes`, `NodeInfo`, `Ping` (Task 4); `api.NodeListStub`, node-status constants (api).
- Produces: nothing consumed by other tasks.

**Prerequisite:** a `nomad` **v2.0.4** binary in `PATH`. The test skips cleanly when absent or when port 4646 is already in use. On some Linux/CI hosts `nomad agent -dev` needs elevated privileges to fingerprint the client and reach `ready`; run the target with sufficient privileges there. This package must stay non-parallel (the dev agent binds fixed ports 4646/4647/4648).

- [ ] **Step 1: Write the integration test**

Create `internal/nomad/client_integration_test.go`:

```go
//go:build integration

package nomad

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
)

const devAddr = "http://127.0.0.1:4646"

// startDevAgent boots `nomad agent -dev` and returns its address plus a stop
// func. It skips (not fails) when no nomad binary exists or 4646 is occupied.
func startDevAgent(t *testing.T) (addr string, stop func()) {
	t.Helper()
	if _, err := exec.LookPath("nomad"); err != nil {
		t.Skip("nomad binary not found in PATH; skipping hermetic integration test")
	}
	// Pre-flight: the dev agent binds fixed ports. If 4646 is already taken,
	// skip rather than clobber a running Nomad or a leaked prior run.
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:4646", 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Skip("127.0.0.1:4646 already in use; skipping to avoid clobbering a running Nomad")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "nomad", "agent", "-dev")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start nomad dev agent: %v", err)
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(devAddr + "/v1/agent/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return devAddr, func() { cancel(); _ = cmd.Wait() }
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	cancel()
	_ = cmd.Wait()
	t.Fatalf("nomad dev agent did not become ready within 60s")
	return "", func() {}
}

func TestDevAgentReadPath(t *testing.T) {
	addr, stop := startDevAgent(t)
	defer stop()

	c, err := New(Config{Address: addr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// The dev agent registers its client node asynchronously and transitions
	// initializing -> ready. Poll until exactly one node reports ready.
	var nodes []*api.NodeListStub
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		nodes, err = c.ListNodes(ctx)
		if err == nil && len(nodes) == 1 && nodes[0].Status == api.NodeStatusReady {
			break
		}
		time.Sleep(time.Second)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected exactly one dev node, got %d (err=%v)", len(nodes), err)
	}
	if nodes[0].Status != api.NodeStatusReady {
		t.Fatalf("node status = %q, want %q", nodes[0].Status, api.NodeStatusReady)
	}

	// Exercise the full read surface against a real Nomad.
	node, err := c.NodeInfo(ctx, nodes[0].ID)
	if err != nil {
		t.Fatalf("NodeInfo: %v", err)
	}
	if node.ID != nodes[0].ID {
		t.Fatalf("NodeInfo returned %q, want %q", node.ID, nodes[0].ID)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// Closes design open-item #1: confirm the observed status is in the
	// documented set. Fail loudly on an undocumented value.
	switch node.Status {
	case api.NodeStatusInit, api.NodeStatusReady, api.NodeStatusDown, api.NodeStatusDisconnected:
	default:
		t.Fatalf("undocumented Node.Status observed on 2.0.4: %q", node.Status)
	}
}
```

- [ ] **Step 2: Add the `test-integration` Makefile target**

Append to `Makefile` under the `##@ Nomad` group:

```makefile
.PHONY: test-integration
test-integration: ## Run hermetic Nomad integration tests (requires a nomad v2.0.4 binary in PATH; may need elevated privileges on Linux).
	go test -tags integration ./internal/nomad/... -run TestDevAgent -v
```

- [ ] **Step 3: Verify it compiles even without a running agent**

Run: `go vet -tags integration ./internal/nomad/`
Expected: exit 0 (compiles under the `integration` build tag).

- [ ] **Step 4: Run the integration test against a real dev agent**

Ensure a `nomad` v2.0.4 binary is on `PATH` (`nomad version` → `Nomad v2.0.4`) and port 4646 is free, then run:

```bash
make test-integration
```

Expected: `PASS` — one node observed, status `ready`, `NodeInfo`/`Ping` succeed, status is in the documented set. If `nomad` is absent or 4646 is busy, the test SKIPS with the documented message (still exit 0).

- [ ] **Step 5: Confirm the default gate is unaffected**

Run: `make verify`
Expected: PASS. The integration test is excluded (no `integration` tag), so `go build`/`go vet`/`go test ./...` do not require a nomad binary.

- [ ] **Step 6: Commit**

```bash
git add internal/nomad/client_integration_test.go Makefile
git commit -m "test(nomad): add hermetic dev-agent read integration test

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Definition of Done (verify before finishing the branch)

- [ ] `make nomad-pin && make generate && make verify` is green; `go list -m github.com/hashicorp/nomad/api` shows `v0.0.0-20260707172059-5b83b133998a`.
- [ ] `internal/nomad/contract.go` compiles against the pin with no edits.
- [ ] `cmd/main.go` manager builds, registers zero controllers, and wires `/healthz` + `/readyz` (Kubebuilder default).
- [ ] Unit tests (Tasks 3–4) pass under `make verify`.
- [ ] `make test-integration` passes with a nomad v2.0.4 binary present (or skips cleanly without one / when 4646 is busy).

## Notes & intentional deviations from the spec

- **Config plumbing (spec §4.5).** The spec said "flags + env now." This plan makes two reasoned refinements: (1) it ships the reusable `Config` + `New` seam but does **not** wire address flags into `cmd/main.go` — the idle manager has no `Client` consumer yet, so flag wiring would be speculative (YAGNI); it lands in the NomadNode slice. (2) It **drops env fallback** for Foundation: `New` builds an explicit `api.Config` instead of `api.DefaultConfig()`, because a per-endpoint client silently inheriting the operator's `NOMAD_*` env is wrong for the slice-2 per-cluster seam (would cross-contaminate clusters). If env-sourced config is wanted later, resolve env → `Config` explicitly in one place (e.g. `cmd/main.go`), not via `DefaultConfig()`'s implicit, partial inheritance. *The design's §4.5 wording is updated to match.*
- **Open items #2 and #3** (system-job `DeploymentState` population; the 200-empty-body `LatestDeployment` contract) require registering a job and belong to the NomadJob/readiness slices; Task 5 closes only open item #1 (the node status value set).
- **Review provenance:** blockers B1 (kubebuilder non-empty dir) and B2 (`go mod tidy` pruning the pin), should-fixes S1 (env leakage) and S2 (integration-test hardening), and nits N1/N3 from Fable's plan review are incorporated; every `api` symbol and the httptest fakes were confirmed against the pinned v2.0.4 source.
