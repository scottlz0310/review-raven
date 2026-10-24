package main

import "testing"

func TestLoadReviewRequestConfig(t *testing.T) {
	const acl = `{"users":{"alice":["scottlz0310/*"]},"operators":["alice"]}`

	cases := []struct {
		name    string
		enabled string
		acl     string
		appMode bool
		wantACL bool
		wantErr bool
	}{
		{"off by default", "", "", false, false, false},
		{"explicitly off", "false", acl, true, false, false},
		{"off ignores a configured ACL", "off", acl, false, false, false},
		{"on in the dedicated App mode with an ACL", "true", acl, true, true, false},
		{"accepted spellings", " TRUE ", acl, true, true, false},
		{"on without the dedicated App mode", "true", acl, false, false, true},
		{"on without an ACL denies everyone, so it does not start", "true", "", true, false, true},
		{"on with an empty ACL", "true", `{"users":{}}`, true, false, true},
		{"on with a malformed ACL", "true", `{"users":{"alice":["*"]}}`, true, false, true},
		{"on with an unknown field", "true", `{"users":{"alice":["o/r"]},"extra":1}`, true, false, true},
		{"a typo in the switch is an error, not a silent off", "ture", acl, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadReviewRequestConfig(tc.enabled, tc.acl, tc.appMode)
			if (err != nil) != tc.wantErr {
				t.Fatalf("loadReviewRequestConfig() error = %v, wantErr %v", err, tc.wantErr)
			}
			if (got != nil) != tc.wantACL {
				t.Fatalf("loadReviewRequestConfig() ACL = %v, want non-nil=%v", got, tc.wantACL)
			}
		})
	}
}
