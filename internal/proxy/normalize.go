package proxy

import (
	"golang.org/x/net/idna"
	"strings"
)

// normalizeHostname converts a hostname to a canonical ASCII, lower-case form.
// - Trims spaces
// - Drops a trailing dot
// - Applies IDNA Lookup ToASCII mapping
// - Lower-cases the result
func normalizeHostname(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	host = strings.TrimSuffix(host, ".")
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" {
		ascii = host
	}
	return strings.ToLower(ascii)
}
