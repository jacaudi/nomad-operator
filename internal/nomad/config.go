package nomad

import "errors"

// Config describes how to reach one Nomad endpoint. It is intentionally
// per-endpoint (not a process-wide singleton) so a future NomadCluster
// reconciler can build one Client per cluster from a custom resource.
type Config struct {
	Address string // e.g. http://127.0.0.1:4646
	Region  string // optional
	Token   string // ACL token; empty in dev mode
	// TLSServerName overrides the server name verified during the TLS handshake.
	// Nomad verifies role/region names (e.g. "server.<region>.nomad"), not the
	// dialed address, so callers set this rather than relying on IP/DNS SANs.
	TLSServerName string
	TLS           TLSConfig
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
