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

func TestLoadInternalSurfaceConfig(t *testing.T) {
	const (
		acl    = `{"services":{"svc-owl":{"actions":["ingest","publish_result"],"repos":["scottlz0310/*"]}}}`
		secret = "internal-secret-0123456789abcdef0123"
		public = "public-secret-0123456789abcdef01234"
	)

	cases := []struct {
		name    string
		enabled string
		acl     string
		secret  string
		public  string
		ledger  bool
		wantACL bool
		wantErr bool
	}{
		{"off by default", "", "", "", public, true, false, false},
		{"explicitly off ignores the rest", "false", acl, secret, public, false, false, false},
		{"on with everything configured", "true", acl, secret, public, true, true, false},
		{"secret is trimmed", "true", acl, "  " + secret + "  ", public, true, true, false},
		{"on without the review-request ledger", "true", acl, secret, public, false, false, true},
		{"on without a secret", "true", acl, "", public, true, false, true},
		{"on with a short secret", "true", acl, "short", public, true, false, true},
		{"on with the public proxy secret", "true", acl, public, public, true, false, true},
		{"on without an ACL denies every service, so it does not start", "true", "", secret, public, true, false, true},
		{"on with a malformed ACL", "true", `{"services":{"s":{"actions":["request"],"repos":["o/*"]}}}`, secret, public, true, false, true},
		{"a typo in the switch is an error, not a silent off", "ture", acl, secret, public, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, gotSecret, err := loadInternalSurfaceConfig(tc.enabled, tc.acl, tc.secret, tc.public, tc.ledger)
			if (err != nil) != tc.wantErr {
				t.Fatalf("loadInternalSurfaceConfig() error = %v, wantErr %v", err, tc.wantErr)
			}
			if (got != nil) != tc.wantACL {
				t.Fatalf("loadInternalSurfaceConfig() ACL = %v, want non-nil=%v", got, tc.wantACL)
			}
			if tc.wantACL && gotSecret != secret {
				t.Fatalf("secret = %q, want the trimmed internal secret", gotSecret)
			}
			if !tc.wantACL && gotSecret != "" {
				t.Fatalf("a disabled or failed surface must not return a secret, got %q", gotSecret)
			}
		})
	}
}
