// Package trustedauthors parses and normalizes the allowlist of PR comment
// authors whose text may enter an agent's context.
//
// The allowlist is deployment configuration (the TRUSTED_COMMENT_AUTHORS
// environment variable), not part of any skill. The normalization rule is the
// same as the reviewed skill's normalize_login, so that a login compares equal
// across the REST ("name[bot]") and GraphQL ("name") spellings of a GitHub App.
package trustedauthors

import (
	"fmt"
	"regexp"
	"strings"
)

const botSuffix = "[bot]"

// loginPattern is the shape of a GitHub login after normalization: ASCII
// alphanumerics and hyphens, with no leading or trailing hyphen. It also
// rejects wildcards, whitespace and a second "[bot]" suffix.
var loginPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// Normalize lowercases ASCII letters and removes a trailing "[bot]" suffix
// exactly once. It does not trim whitespace or replace text in the middle of
// the login, so "name[bot][bot]" becomes "name[bot]" (which Parse rejects).
func Normalize(login string) string {
	b := []byte(login)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return strings.TrimSuffix(string(b), botSuffix)
}

// Parse splits a comma-separated list of logins and returns the normalized,
// de-duplicated logins in the order of first appearance. Empty entries are
// skipped. An entry that is not a valid GitHub login after normalization
// (including wildcards) is an error, so a typo fails at startup instead of
// silently weakening or breaking the gate.
func Parse(raw string) ([]string, error) {
	logins := []string{}
	seen := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		normalized := Normalize(entry)
		if !loginPattern.MatchString(normalized) {
			return nil, fmt.Errorf("TRUSTED_COMMENT_AUTHORS entry must be a GitHub login (wildcards are not allowed): %q", entry)
		}
		if _, dup := seen[normalized]; dup {
			continue
		}
		seen[normalized] = struct{}{}
		logins = append(logins, normalized)
	}
	return logins, nil
}
