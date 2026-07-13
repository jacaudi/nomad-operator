package controller

import "strings"

// sanitizeNodeName converts a Nomad node Name (which may contain uppercase or
// characters illegal in a Kubernetes object name) into a valid RFC 1123
// subdomain used as the NomadNode's metadata.name. The exact node Name is kept
// verbatim in spec.nodeName; this is only the object name. Post-sanitization
// collisions are surfaced by the reflector's duplicate guard (design §3.2).
func sanitizeNodeName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	// Collapse runs introduced by replacement so "weird_name!" -> "weird-name".
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 253 {
		out = strings.Trim(out[:253], "-.")
	}
	if out == "" {
		return "node"
	}
	return out
}
