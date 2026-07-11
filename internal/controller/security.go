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
	// No controller ref: the gossip key is retained-by-design on CR delete (it
	// and the token Secret and Raft PVCs survive the ownerRef cascade so an
	// operator can recreate the CR without losing gossip encryption or data).
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: n.GossipSecret, Namespace: nc.Namespace, Labels: n.Labels()},
		Data:       map[string][]byte{"key": []byte(base64.StdEncoding.EncodeToString(buf))},
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

// newBootstrapToken mints a version-4 UUID string via crypto/rand, in the
// shape Nomad's ACLTokens().BootstrapOpts expects for an operator-supplied
// bootstrap token. This is a small hand-rolled helper rather than a
// github.com/google/uuid import: uuid is currently only an indirect
// dependency of this module, and minting a token here doesn't warrant
// promoting it to direct.
func newBootstrapToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("bootstrap token rand: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC-4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ensureBootstrapToken is idempotent: if the token Secret already exists, it is
// the source of truth and no bootstrap is attempted. Otherwise it generates a
// token, WRITES THE SECRET FIRST, then calls BootstrapOpts with that token.
func (r *NomadClusterReconciler) ensureBootstrapToken(ctx context.Context, nc *nomadv1alpha1.NomadCluster, ops NomadOps) error {
	n := names(nc)
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: n.TokenSecret, Namespace: nc.Namespace}, &existing)
	if err == nil {
		return nil // Secret is the source of truth; already bootstrapped
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	token, err := newBootstrapToken()
	if err != nil {
		return err
	}
	// No controller ref: the token Secret is retained-by-design on CR delete
	// (see ensureGossipKey) so the ACL bootstrap token survives CR deletion.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: n.TokenSecret, Namespace: nc.Namespace, Labels: n.Labels()},
		Data:       map[string][]byte{"token": []byte(token)},
	}
	if err := r.Create(ctx, sec); err != nil {
		return fmt.Errorf("write token secret: %w", err)
	}

	if _, err := ops.ACLBootstrap(ctx, token); err != nil {
		// "already bootstrapped" out of band: the Secret we just wrote is
		// authoritative for OUR token; surface but do not delete the Secret.
		return fmt.Errorf("acl bootstrap: %w", err)
	}
	return nil
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
