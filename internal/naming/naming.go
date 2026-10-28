// Package naming holds helpers for building Kubernetes-safe object and request names.
package naming

import "strings"

// Sanitize converts s into a DNS-label-safe string: it lowercases A-Z, maps
// every character outside [a-z0-9-] to '-', collapses consecutive dashes and
// trims leading/trailing dashes. It returns fallback if the result is empty.
// It does not enforce a length limit.
func Sanitize(s, fallback string) string {
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, s)

	for strings.Contains(sanitized, "--") {
		sanitized = strings.ReplaceAll(sanitized, "--", "-")
	}
	sanitized = strings.Trim(sanitized, "-")

	if sanitized == "" {
		return fallback
	}
	return sanitized
}
