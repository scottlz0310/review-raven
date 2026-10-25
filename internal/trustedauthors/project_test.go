package trustedauthors

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseProjectFile(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr string
	}{
		{name: "adds normalized logins", input: `{"version":1,"additional_logins":["Cloudflare-Workers-And-Pages[bot]","ci-bot"]}`, want: []string{"cloudflare-workers-and-pages", "ci-bot"}},
		{name: "empty list is valid", input: `{"version":1,"additional_logins":[]}`, want: []string{}},
		{name: "not json", input: `nope`, wantErr: "not a valid allowlist file"},
		{name: "not an object", input: `["a"]`, wantErr: "not a valid allowlist file"},
		{name: "trailing document", input: `{"version":1,"additional_logins":[]} {}`, wantErr: "single JSON object"},
		{name: "missing version", input: `{"additional_logins":[]}`, wantErr: "version must be 1"},
		{name: "wrong version", input: `{"version":2,"additional_logins":[]}`, wantErr: "version must be 1"},
		{name: "missing logins", input: `{"version":1}`, wantErr: "additional_logins must be an array"},
		{name: "null logins", input: `{"version":1,"additional_logins":null}`, wantErr: "additional_logins must be an array"},
		{name: "unknown key", input: `{"version":1,"additional_logins":[],"extra":true}`, wantErr: "not a valid allowlist file"},
		{name: "non-string login", input: `{"version":1,"additional_logins":[1]}`, wantErr: "not a valid allowlist file"},
		{name: "null login", input: `{"version":1,"additional_logins":[null]}`, wantErr: "GitHub login"},
		{name: "empty login", input: `{"version":1,"additional_logins":[""]}`, wantErr: "GitHub login"},
		{name: "wildcard", input: `{"version":1,"additional_logins":["*"]}`, wantErr: "wildcards are not allowed"},
		{name: "double bot suffix", input: `{"version":1,"additional_logins":["name[bot][bot]"]}`, wantErr: "GitHub login"},
		{name: "duplicate after normalization", input: `{"version":1,"additional_logins":["Name","name[bot]"]}`, wantErr: "duplicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseProjectFile([]byte(tt.input))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("logins = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseProjectFileErrorRepeatsOnlyAnExcerptOfALongValue(t *testing.T) {
	long := strings.Repeat("!", 600)
	for _, tt := range []struct {
		name  string
		input string
	}{
		{name: "invalid login", input: `{"version":1,"additional_logins":["` + long + `"]}`},
		{name: "duplicate", input: `{"version":1,"additional_logins":["` + strings.Repeat("a", 600) + `","` + strings.Repeat("A", 600) + `"]}`},
		{name: "multibyte", input: `{"version":1,"additional_logins":["` + strings.Repeat("あ", 600) + `"]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseProjectFile([]byte(tt.input))
			if err == nil {
				t.Fatal("expected an error")
			}
			if n := len([]rune(err.Error())); n > 300 {
				t.Fatalf("error message has %d characters; it must not repeat the whole value", n)
			}
		})
	}
}
