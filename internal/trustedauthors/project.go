package trustedauthors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// ProjectFilePath is the repository-relative path of the project-specific
// allowlist. It is read from the pull request's base ref, never from the
// working tree or the pull request head, so a pull request cannot widen the
// allowlist that is applied to its own comments.
const ProjectFilePath = ".review-raven/trusted-comment-authors.json"

type projectFile struct {
	Version          *int      `json:"version"`
	AdditionalLogins *[]string `json:"additional_logins"`
}

// maxExcerpt bounds how much of a rejected value an error message repeats, so that a
// very long entry cannot make the message (which reaches the reader's error response)
// arbitrarily long.
const maxExcerpt = 64

func excerpt(s string) string {
	r := []rune(s)
	if len(r) > maxExcerpt {
		return strconv.Quote(string(r[:maxExcerpt])) + "..."
	}
	return strconv.Quote(s)
}

// ParseProjectFile validates the project allowlist file and returns the
// normalized logins it adds to the server allowlist.
//
// The file must be a single JSON object with version 1 and an
// additional_logins array of strings, and nothing else. A null, empty or
// non-string login, a login that is not valid after normalization (wildcards
// included), and two entries that normalize to the same login are errors: a
// malformed file must stop the gate instead of being read as "no additions".
func ParseProjectFile(data []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var file projectFile
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("%s is not a valid allowlist file: %w", ProjectFilePath, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must contain a single JSON object", ProjectFilePath)
	}
	if file.Version == nil || *file.Version != 1 {
		return nil, fmt.Errorf("%s: version must be 1", ProjectFilePath)
	}
	if file.AdditionalLogins == nil {
		return nil, fmt.Errorf("%s: additional_logins must be an array", ProjectFilePath)
	}

	logins := make([]string, 0, len(*file.AdditionalLogins))
	seen := map[string]struct{}{}
	for _, raw := range *file.AdditionalLogins {
		normalized := Normalize(raw)
		if !loginPattern.MatchString(normalized) {
			return nil, fmt.Errorf("%s: additional_logins entry must be a GitHub login (wildcards are not allowed): %s", ProjectFilePath, excerpt(raw))
		}
		if _, dup := seen[normalized]; dup {
			return nil, fmt.Errorf("%s: additional_logins has a duplicate after normalization: %s", ProjectFilePath, excerpt(raw))
		}
		seen[normalized] = struct{}{}
		logins = append(logins, normalized)
	}
	return logins, nil
}
