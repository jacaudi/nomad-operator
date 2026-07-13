package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

func TestClusterNomadConfig(t *testing.T) {
	nc := &nomadv1alpha1.NomadCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ns"},
		Spec: nomadv1alpha1.NomadClusterSpec{
			Region: "global",
			TLS:    nomadv1alpha1.TLSSpec{CertSecretRef: "cert"},
		},
	}
	cert := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cert", Namespace: "ns"},
		Data:       map[string][]byte{"ca.crt": []byte("CA"), "tls.crt": []byte("CRT"), "tls.key": []byte("KEY")},
	}
	tok := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: names(nc).TokenSecret, Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("t0ken")},
	}
	c := fake.NewClientBuilder().WithObjects(cert, tok).Build()

	cfg, err := clusterNomadConfig(t.Context(), c, nc)
	if err != nil {
		t.Fatalf("clusterNomadConfig: %v", err)
	}
	if cfg.TLSServerName != "server.global.nomad" {
		t.Errorf("TLSServerName = %q", cfg.TLSServerName)
	}
	if string(cfg.TLS.CACertPEM) != "CA" || string(cfg.TLS.ClientKeyPEM) != "KEY" {
		t.Errorf("PEM material not wired: %+v", cfg.TLS)
	}
	if cfg.Token != "t0ken" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.Address == "" {
		t.Error("Address empty")
	}
}
