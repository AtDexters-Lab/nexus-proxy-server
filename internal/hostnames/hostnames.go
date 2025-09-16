package hostnames

import (
    "strings"
    "golang.org/x/net/idna"
)

// Normalize converts a hostname to its canonical ASCII lower-case form.
// - Trims spaces
// - Drops a trailing dot
// - Applies IDNA Lookup ToASCII mapping
// - Lower-cases the result
func Normalize(host string) string {
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

// NormalizeOrWildcard behaves like Normalize but preserves a leading
// single-label wildcard pattern "*." if present.
func NormalizeOrWildcard(host string) string {
    host = strings.TrimSpace(host)
    if host == "" {
        return ""
    }
    if strings.HasPrefix(host, "*.") {
        base := Normalize(host[2:])
        if base == "" {
            return ""
        }
        return "*." + base
    }
    return Normalize(host)
}

// IsWildcard returns true if the pattern begins with a single-label wildcard ("*.").
func IsWildcard(pattern string) bool {
    return strings.HasPrefix(pattern, "*.")
}

// IsValidWildcard returns true if the pattern is a single-label wildcard and
// the part after "*." contains at least two labels (e.g., "example.com").
// This intentionally rejects broad patterns like "*.com".
func IsValidWildcard(pattern string) bool {
    if !IsWildcard(pattern) {
        return false
    }
    rest := pattern[2:]
    // Must contain at least one dot and no empty labels
    if !strings.Contains(rest, ".") {
        return false
    }
    // Disallow additional '*' anywhere else
    if strings.Contains(rest, "*") {
        return false
    }
    // No empty labels around dots
    if strings.HasPrefix(rest, ".") || strings.HasSuffix(rest, ".") || strings.Contains(rest, "..") {
        return false
    }
    return true
}

// WildcardSuffix returns the canonical suffix (including the leading dot),
// e.g., for "*.example.com" it returns ".example.com". The boolean is false
// if the pattern is not a valid single-label wildcard.
func WildcardSuffix(pattern string) (string, bool) {
    p := NormalizeOrWildcard(pattern)
    if !IsValidWildcard(p) {
        return "", false
    }
    return p[1:], true // keep leading dot
}

// FirstDotSuffix returns the suffix of a hostname starting from the first dot,
// e.g., "a.example.com" -> ".example.com". Returns false if there is no dot.
func FirstDotSuffix(host string) (string, bool) {
    h := Normalize(host)
    if i := strings.Index(h, "."); i > 0 {
        return h[i:], true
    }
    return "", false
}

