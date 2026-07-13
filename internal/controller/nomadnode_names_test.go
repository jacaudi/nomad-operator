package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestSanitizeNodeName(t *testing.T) {
	// Exact-output cases: sanitize must produce precisely these object names.
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

	// Pathological inputs: a raw Nomad Name that would violate per-label RFC1123
	// subdomain rules (empty label, label ending in '-', label >63 chars) must
	// still sanitize to a VALID subdomain — one bad node cannot mint an
	// un-createable CR and stall the whole cluster's reflection.
	pathological := []string{
		"a..b",                  // empty middle label
		"host_.lan",             // '_' -> '-', label would end in '-'
		"--a--.--b--",           // labels with leading/trailing dashes
		strings.Repeat("x", 64), // single label exceeding 63 chars
	}
	for _, in := range pathological {
		got := sanitizeNodeName(in)
		if errs := validation.IsDNS1123Subdomain(got); len(errs) != 0 {
			t.Errorf("sanitizeNodeName(%q) = %q is not a valid RFC1123 subdomain: %v", in, got, errs)
		}
	}

	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	got := sanitizeNodeName(string(long))
	if len(got) > 253 {
		t.Errorf("sanitizeNodeName did not cap length: %d", len(got))
	}
	if errs := validation.IsDNS1123Subdomain(got); len(errs) != 0 {
		t.Errorf("sanitizeNodeName(300 x 'a') = %q is not a valid RFC1123 subdomain: %v", got, errs)
	}
}
