package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// clusterNomadConfig builds the per-cluster nomad.Config both reconcilers use:
// endpoint is the in-cluster API Service, TLS material is PEM bytes from the
// cert-manager Secret (never files), the token (if bootstrapped) comes from the
// token Secret, and TLSServerName is the Nomad role name. This is the single
// source of the per-cluster client-construction contract (design §4).
func clusterNomadConfig(ctx context.Context, c client.Client, nc *nomadv1alpha1.NomadCluster) (nomad.Config, error) {
	n := names(nc)
	endpoint := fmt.Sprintf("https://%s.%s.svc:%d", n.APISvc, nc.Namespace, portHTTP)

	var certSec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: nc.Spec.TLS.CertSecretRef, Namespace: nc.Namespace}, &certSec); err != nil {
		return nomad.Config{}, err
	}
	cfg := nomad.Config{
		Address:       endpoint,
		Region:        nc.Spec.Region,
		TLSServerName: "server." + nc.Spec.Region + ".nomad",
		TLS: nomad.TLSConfig{
			CACertPEM:     certSec.Data["ca.crt"],
			ClientCertPEM: certSec.Data["tls.crt"],
			ClientKeyPEM:  certSec.Data["tls.key"],
		},
	}
	var tokenSec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: n.TokenSecret, Namespace: nc.Namespace}, &tokenSec); err == nil {
		cfg.Token = string(tokenSec.Data["token"])
	}
	return cfg, nil
}
