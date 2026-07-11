package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

const (
	metav1ConditionTrue  = metav1.ConditionTrue
	metav1ConditionFalse = metav1.ConditionFalse
)

// setCondition upserts a status condition by type.
func setCondition(nc *nomadv1alpha1.NomadCluster, condType string, status metav1.ConditionStatus, reason, msg string) {
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: nc.Generation,
		LastTransitionTime: metav1.Now(),
	}
	for i := range nc.Status.Conditions {
		if nc.Status.Conditions[i].Type == condType {
			if nc.Status.Conditions[i].Status == status {
				cond.LastTransitionTime = nc.Status.Conditions[i].LastTransitionTime
			}
			nc.Status.Conditions[i] = cond
			return
		}
	}
	nc.Status.Conditions = append(nc.Status.Conditions, cond)
}
