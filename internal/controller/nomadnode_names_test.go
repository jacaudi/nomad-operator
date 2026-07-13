package controller

import "testing"

func TestSanitizeNodeName(t *testing.T) {
	cases := map[string]string{
		"truenas-01":  "truenas-01",
		"TrueNAS-01":  "truenas-01",
		"host.lan":    "host.lan",
		"weird_name!": "weird-name",
		"--edge--":    "edge",
		"":            "node",
	}
	for in, want := range cases {
		if got := sanitizeNodeName(in); got != want {
			t.Errorf("sanitizeNodeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	if got := sanitizeNodeName(string(long)); len(got) > 253 {
		t.Errorf("sanitizeNodeName did not cap length: %d", len(got))
	}
}
