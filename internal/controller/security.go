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
	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// annotationACLBootstrapped is the DURABLE marker on the token Secret that
// records a CONFIRMED-successful ACL bootstrap. It is set only after
// ops.ACLBootstrap has actually succeeded (or told us the cluster was
// already bootstrapped) — never merely because the Secret exists. Status
// conditions are not durable (they're wiped/recomputed across reconciles),
// so the annotation is the only safe idempotency gate; see
// ensureBootstrapToken.
const annotationACLBootstrapped = "nomad.operator.io/acl-bootstrapped"

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

// ensureBootstrapToken is idempotent-with-retry: the durable confirmation
// marker (annotationACLBootstrapped on the token Secret) is the ONLY signal
// that bootstrap has actually succeeded — Secret EXISTENCE alone is not
// (design §3.3's retry guarantee requires this: a crash/error between
// writing the Secret and confirming ACLBootstrap must re-attempt on the next
// reconcile, not silently report success).
//
//   - Secret absent            → mint a token, Create the Secret (unannotated).
//   - Secret present, annotated → durably confirmed; return nil, no API call.
//   - Secret present, not yet
//     annotated                → re-attempt bootstrap with the Secret's token
//     (same token: crash-and-retry re-submits it, design §3.3).
//
// Either way, ops.ACLBootstrap is then called; on success — or on the
// already-bootstrapped self-heal case — the Secret is annotated and this
// returns nil. Any other error is returned so the caller requeues and
// RE-ATTEMPTS on the next reconcile instead of wrongly reporting Ready.
func (r *NomadClusterReconciler) ensureBootstrapToken(ctx context.Context, nc *nomadv1alpha1.NomadCluster, ops NomadOps) error {
	n := names(nc)
	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: n.TokenSecret, Namespace: nc.Namespace}, &sec)
	switch {
	case err == nil:
		if sec.Annotations[annotationACLBootstrapped] == "true" {
			return nil // durably confirmed; steady state, no API call
		}
		// Secret exists but a prior attempt never confirmed bootstrap succeeded
		// (e.g. a leader flap right after ACLBootstrap was called, or the
		// operator crashed before writing the marker) — retry below with its
		// existing token.
	case apierrors.IsNotFound(err):
		token, terr := newBootstrapToken()
		if terr != nil {
			return terr
		}
		// No controller ref: the token Secret is retained-by-design on CR delete
		// (see ensureGossipKey) so the ACL bootstrap token survives CR deletion.
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: n.TokenSecret, Namespace: nc.Namespace, Labels: n.Labels()},
			Data:       map[string][]byte{"token": []byte(token)},
		}
		if cerr := r.Create(ctx, &sec); cerr != nil {
			return fmt.Errorf("write token secret: %w", cerr)
		}
	default:
		return err
	}

	if _, err := ops.ACLBootstrap(ctx, string(sec.Data["token"])); err != nil {
		if !nomad.IsACLAlreadyBootstrapped(err) {
			return fmt.Errorf("acl bootstrap: %w", err)
		}
		// Already bootstrapped out of band (e.g. a prior attempt's ACLBootstrap
		// call succeeded but the operator crashed before writing the
		// confirmation annotation): the cluster IS bootstrapped, so self-heal by
		// marking the Secret confirmed instead of retrying forever.
	}

	if sec.Annotations == nil {
		sec.Annotations = map[string]string{}
	}
	sec.Annotations[annotationACLBootstrapped] = "true"
	if err := r.Update(ctx, &sec); err != nil {
		return fmt.Errorf("mark token secret bootstrapped: %w", err)
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
