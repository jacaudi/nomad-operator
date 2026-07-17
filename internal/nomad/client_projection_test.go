package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/api"
)

func TestToMembers_MapsServerHealthFields(t *testing.T) {
	in := []api.ServerHealth{
		{Name: "srv-0", Address: "10.0.0.5:14647", SerfStatus: "alive", Leader: true, Voter: true},
		{Name: "srv-1", Address: "10.0.0.6:24647", SerfStatus: "failed", Leader: false, Voter: false},
	}
	got := toMembers(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0] != (NomadMember{Name: "srv-0", Addr: "10.0.0.5:14647", Status: "alive", Leader: true, Voter: true}) {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1] != (NomadMember{Name: "srv-1", Addr: "10.0.0.6:24647", Status: "failed", Leader: false, Voter: false}) {
		t.Errorf("got[1] = %+v", got[1])
	}
}
