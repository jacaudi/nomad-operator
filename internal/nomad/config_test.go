package nomad

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

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

func TestConfigTLSServerNameOptional(t *testing.T) {
	// Empty TLSServerName must remain valid (additive, backward compatible).
	cfg := Config{Address: "https://127.0.0.1:4646"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	// A Config carrying TLSServerName must construct a client without error.
	cfg2 := Config{Address: "https://127.0.0.1:4646", TLSServerName: "server.global.nomad"}
	if _, err := New(cfg2); err != nil {
		t.Fatalf("New() with TLSServerName = %v, want nil", err)
	}
}

func TestConfigTLSPEMBytes(t *testing.T) {
	// The operator holds PEM bytes read from a k8s Secret, not file paths, so
	// TLSConfig must accept PEM byte fields and New must construct a client
	// from them without error.
	certPEM, keyPEM := generateSelfSignedPEM(t)
	cfg := Config{
		Address:       "https://127.0.0.1:4646",
		TLSServerName: "server.global.nomad",
		TLS: TLSConfig{
			CACertPEM:     certPEM,
			ClientCertPEM: certPEM,
			ClientKeyPEM:  keyPEM,
		},
	}
	if _, err := New(cfg); err != nil {
		t.Fatalf("New() with TLS PEM bytes = %v, want nil", err)
	}
}

// generateSelfSignedPEM returns a self-signed cert/key PEM pair for tests
// that need real (parseable) TLS material rather than a file path.
func generateSelfSignedPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
