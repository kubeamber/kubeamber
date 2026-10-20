package policy

import (
	"regexp"
	"strings"
)

// Glob reports whether s matches pattern, where '*' matches any run of
// characters (including none) and every other character matches itself.
// Unlike path.Match, '*' also matches '/' and ':', which appear in Kubernetes
// usernames.
func Glob(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return strings.HasSuffix(s, last)
}

// GlobAny reports whether s matches any of the patterns.
func GlobAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if Glob(p, s) {
			return true
		}
	}
	return false
}

// GlobRegexp returns an anchored RE2 expression equivalent to Glob(pattern, ·).
func GlobRegexp(pattern string) string {
	parts := strings.Split(pattern, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return "^" + strings.Join(parts, ".*") + "$"
}
