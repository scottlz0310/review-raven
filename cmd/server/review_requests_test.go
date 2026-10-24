package main

import (
	"testing"
	"time"
)

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

func TestLoadDeliveryConfig(t *testing.T) {
	const (
		url   = "https://gateway.example/mcp/thread-owl-internal"
		token = "service-credential-0123456789abcdef"
		// a private container network reaches the gateway by its service name
		serviceURL = "http://mcp-gateway:8080/mcp/thread-owl-internal"
	)

	cases := []struct {
		name         string
		url          string
		token        string
		interval     string
		plain        string
		ledger       bool
		wantConfig   bool
		wantInterval time.Duration
		wantPlain    bool
		wantErr      bool
	}{
		{"off by default", "", "", "", "", false, false, 0, false, false},
		{"on with the default interval", url, token, "", "", true, true, 5 * time.Second, false, false},
		{"on with an interval", url, token, "30", "", true, true, 30 * time.Second, false, false},
		{"loopback http is allowed", "http://127.0.0.1:8084/mcp", token, "", "", true, true, 5 * time.Second, false, false},
		{"values are trimmed", "  " + url + "  ", "  " + token + "  ", " 10 ", "", true, true, 10 * time.Second, false, false},
		{"url without a credential", url, "", "", "", true, false, 0, false, true},
		{"credential without a url", "", token, "", "", true, false, 0, false, true},
		{"without the review-request ledger", url, token, "", "", false, false, 0, false, true},
		{"plain http to another host", "http://gateway.example/mcp", token, "", "", true, false, 0, false, true},
		{"plain http to a service name is refused by default", serviceURL, token, "", "", true, false, 0, false, true},
		{"plain http to a service name, opted in", serviceURL, token, "", "true", true, true, 5 * time.Second, true, false},
		{"opt-in accepts 1 and on", serviceURL, token, "", " 1 ", true, true, 5 * time.Second, true, false},
		{"opt-in off is the default", serviceURL, token, "", "false", true, false, 0, false, true},
		{"opt-in with an unrecognised value", serviceURL, token, "", "yes", true, false, 0, false, true},
		{"opt-in with an unrecognised value even when the url is https", url, token, "", "maybe", true, false, 0, false, true},
		{"opt-in does not relax credentials in the url", "http://user:pass@mcp-gateway:8080/mcp", token, "", "true", true, false, 0, false, true},
		{"opt-in does not relax the scheme", "ftp://mcp-gateway/mcp", token, "", "true", true, false, 0, false, true},
		{"opt-in alone enables nothing", "", "", "", "true", true, false, 0, false, false},
		{"credentials embedded in the url", "https://user:pass@gateway.example/mcp", token, "", "", true, false, 0, false, true},
		{"interval not a number", url, token, "fast", "", true, false, 0, false, true},
		{"interval zero", url, token, "0", "", true, false, 0, false, true},
		{"interval too long", url, token, "3601", "", true, false, 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, interval, err := loadDeliveryConfig(tc.url, tc.token, tc.interval, tc.plain, tc.ledger)
			if (err != nil) != tc.wantErr {
				t.Fatalf("loadDeliveryConfig() error = %v, wantErr %v", err, tc.wantErr)
			}
			if (cfg != nil) != tc.wantConfig {
				t.Fatalf("loadDeliveryConfig() config = %v, want non-nil=%v", cfg, tc.wantConfig)
			}
			if interval != tc.wantInterval {
				t.Fatalf("interval = %v, want %v", interval, tc.wantInterval)
			}
			if cfg != nil && cfg.Token != token {
				t.Fatalf("the credential must be trimmed, got %q", cfg.Token)
			}
			if cfg != nil && cfg.AllowPlainHTTP != tc.wantPlain {
				t.Fatalf("AllowPlainHTTP = %v, want %v", cfg.AllowPlainHTTP, tc.wantPlain)
			}
		})
	}
}
