package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFollowUpIssuePermissions(t *testing.T) {
	_, key := testKey(t)
	for _, tc := range []struct {
		name                   string
		enabled, granted, fail bool
	}{
		{"既定4権限", false, false, false},
		{"有効化かつ権限不足", true, false, true},
		{"有効化かつ許可済み", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/app":
					return response(200, `{"id":1,"slug":"raven","owner":{"login":"org","type":"Organization"}}`), nil
				case "/app/installations/2":
					permissions := map[string]string{"metadata": "read", "pull_requests": "write", "contents": "write", "checks": "read"}
					if tc.granted {
						permissions["issues"] = "write"
					}
					body, _ := json.Marshal(map[string]any{"id": 2, "app_id": 1, "repository_selection": "all", "permissions": permissions, "account": map[string]string{"login": "org", "type": "Organization"}})
					return response(200, string(body)), nil
				case "/app/installations/2/access_tokens":
					posts++
					var body struct {
						Permissions map[string]string `json:"permissions"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					want := 4
					if tc.enabled {
						want = 5
					}
					if len(body.Permissions) != want || (body.Permissions["issues"] == "write") != tc.enabled {
						t.Fatalf("token要求=%v", body.Permissions)
					}
					return response(201, fmt.Sprintf(`{"token":"test-token","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))), nil
				default:
					t.Fatalf("想定外の要求: %s", r.URL.Path)
					return nil, nil
				}
			})}
			s, err := NewTokenSource(Config{AppID: 1, InstallationID: 2, Owner: "org", PrivateKeyPEM: key, HTTPClient: httpClient, EnableFollowUpIssues: tc.enabled})
			if err != nil {
				t.Fatal(err)
			}
			err = s.ValidateInstallation(context.Background())
			if tc.fail {
				if err == nil || !strings.Contains(err.Error(), "issues") || posts != 0 {
					t.Fatalf("err=%v posts=%d", err, posts)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Token(context.Background()); err != nil || posts != 1 {
				t.Fatalf("err=%v posts=%d", err, posts)
			}
		})
	}
}
