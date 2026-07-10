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
