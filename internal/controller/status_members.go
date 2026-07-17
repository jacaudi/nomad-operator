package controller

import (
	"fmt"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// toMemberStatus projects the Nomad server health into the CRD status shape.
func toMemberStatus(members []nomad.NomadMember) []nomadv1alpha1.MemberStatus {
	out := make([]nomadv1alpha1.MemberStatus, 0, len(members))
	for _, m := range members {
		out = append(out, nomadv1alpha1.MemberStatus{
			Name:   m.Name,
			Addr:   m.Addr,
			Status: m.Status,
			Leader: m.Leader,
			Voter:  m.Voter,
		})
	}
	return out
}

// quorumString reports "<voters>/<total>" from the observed member set.
func quorumString(members []nomad.NomadMember) string {
	voters := 0
	for _, m := range members {
		if m.Voter {
			voters++
		}
	}
	return fmt.Sprintf("%d/%d", voters, len(members))
}
