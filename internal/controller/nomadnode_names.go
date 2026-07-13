package controller

import "strings"

// sanitizeNodeName converts a Nomad node Name (which may contain uppercase or
// characters illegal in a Kubernetes object name) into a valid RFC 1123
// subdomain used as the NomadNode's metadata.name. The output is ALWAYS a valid
// subdomain for any input: each dot-separated label is non-empty, at most 63
// characters, and starts/ends alphanumeric. The exact node Name is kept verbatim
// in spec.nodeName; this is only the object name. Post-sanitization collisions
// are surfaced by the reflector's duplicate guard (design §3.2).
func sanitizeNodeName(name string) string {
	var labels []string
	for raw := range strings.SplitSeq(strings.ToLower(name), ".") {
		if label := sanitizeLabel(raw); label != "" {
			labels = append(labels, label)
		}
	}
	out := strings.Join(labels, ".")
	if len(out) > 253 {
		out = strings.Trim(out[:253], "-.")
	}
	if out == "" {
		return "node"
	}
	return out
}

// sanitizeLabel maps one dot-separated label to a valid RFC 1123 label: illegal
// runes become '-', collapsed runs are flattened, leading/trailing '-' trimmed,
// and the result capped at 63 chars (re-trimming any '-' the cut exposes). May
// return "" when the label reduces to nothing; the caller drops empty labels.
func sanitizeLabel(label string) string {
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	// Collapse runs introduced by replacement so "weird_name!" -> "weird-name".
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	out = strings.Trim(out, "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}
