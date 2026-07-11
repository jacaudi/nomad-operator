package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

// ensureGossipKey creates a 32-byte base64-encoded gossip encryption key Secret
// if one is not already present. Secret existence is the idempotency gate: once
// created, the key is never regenerated (regenerating it would split-brain the
// Serf gossip pool).
func (r *NomadClusterReconciler) ensureGossipKey(ctx context.Context, nc *nomadv1alpha1.NomadCluster) (string, error) {
	n := names(nc)
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: n.GossipSecret, Namespace: nc.Namespace}, &existing)
	if err == nil {
		return n.GossipSecret, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("gossip key rand: %w", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: n.GossipSecret, Namespace: nc.Namespace, Labels: n.Labels()},
		Data:       map[string][]byte{"key": []byte(base64.StdEncoding.EncodeToString(buf))},
	}
	if err := controllerutil.SetControllerReference(nc, sec, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", err
	}
	return n.GossipSecret, nil
}

// certSecretReady reports whether the user-provided cert-manager Secret exists
// and carries the three TLS keys the server container mounts. The StatefulSet
// must not be provisioned before this gates true.
func (r *NomadClusterReconciler) certSecretReady(ctx context.Context, nc *nomadv1alpha1.NomadCluster) (bool, error) {
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: nc.Spec.TLS.CertSecretRef, Namespace: nc.Namespace}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, k := range []string{"tls.crt", "tls.key", "ca.crt"} {
		if len(s.Data[k]) == 0 {
			return false, nil
		}
	}
	return true, nil
}

// apply sets the controller ref and Server-Side-Applies the object. SSA is used
// instead of Get+Update because a naive update sends empty apiserver-populated
// immutable fields (notably Service.clusterIP) and is rejected on the second
// reconcile; SSA merges by field ownership and needs no resourceVersion dance.
func (r *NomadClusterReconciler) apply(ctx context.Context, nc *nomadv1alpha1.NomadCluster, obj client.Object) error {
	if err := controllerutil.SetControllerReference(nc, obj, r.Scheme); err != nil {
		return err
	}
	gvk, err := apiutil.GVKForObject(obj, r.Scheme)
	if err != nil {
		return err
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk) // SSA requires apiVersion/kind in the body
	// client.Apply is deprecated (SA1019) in favor of the typed Client.Apply(ctx,
	// runtime.ApplyConfiguration) API, but that API requires generated apply
	// configuration types per object kind (client-gen applyconfiguration). We
	// don't have those generated for the Gateway API types (Gateway/TLSRoute/
	// TCPRoute) this controller applies, so the Patch-based SSA form remains the
	// correct, working mechanism here.
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner("nomad-operator"), client.ForceOwnership) //nolint:staticcheck
}
