package trustedauthors

import (
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name  string
		login string
		want  string
	}{
		{name: "plain login", login: "scottlz0310-user", want: "scottlz0310-user"},
		{name: "uppercase is lowercased", login: "Thread-Owl", want: "thread-owl"},
		{name: "bot suffix is removed", login: "thread-owl[bot]", want: "thread-owl"},
		{name: "uppercase bot suffix is removed after lowercasing", login: "THREAD-OWL[BOT]", want: "thread-owl"},
		{name: "suffix is removed only once", login: "thread-owl[bot][bot]", want: "thread-owl[bot]"},
		{name: "bot in the middle is kept", login: "thread[bot]owl", want: "thread[bot]owl"},
		{name: "whitespace is not trimmed", login: " thread-owl ", want: " thread-owl "},
		{name: "non-ASCII letters are not lowercased", login: "Ünicode", want: "Ünicode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Normalize(tt.login); got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.login, got, tt.want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "empty string is an empty list", raw: "", want: []string{}},
		{name: "only separators and spaces", raw: " , ,, ", want: []string{}},
		{name: "single login", raw: "thread-owl", want: []string{"thread-owl"}},
		{name: "order of first appearance is kept", raw: "codecov,thread-owl,copilot", want: []string{"codecov", "thread-owl", "copilot"}},
		{name: "entries are trimmed", raw: " codecov , thread-owl ", want: []string{"codecov", "thread-owl"}},
		{name: "bot suffix and uppercase are normalized", raw: "Thread-Owl[bot],CODECOV", want: []string{"thread-owl", "codecov"}},
		{name: "spellings of one App are de-duplicated", raw: "thread-owl,thread-owl[bot],THREAD-OWL[BOT]", want: []string{"thread-owl"}},
		{name: "wildcard is rejected", raw: "scottlz0310-*", wantErr: true},
		{name: "bare wildcard is rejected", raw: "*", wantErr: true},
		{name: "double bot suffix is rejected", raw: "thread-owl[bot][bot]", wantErr: true},
		{name: "leading hyphen is rejected", raw: "-owl", wantErr: true},
		{name: "trailing hyphen is rejected", raw: "owl-", wantErr: true},
		{name: "inner whitespace is rejected", raw: "thread owl", wantErr: true},
		{name: "slash is rejected", raw: "owner/repo", wantErr: true},
		{name: "one bad entry rejects the whole value", raw: "codecov,bad name", wantErr: true},
		{name: "bot suffix alone is rejected", raw: "[bot]", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %v, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}
