package ghclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetPRContextDoesNotSelectBodies(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(map[string]any)
		wantError bool
	}{
		{"OPEN", func(map[string]any) {}, false},
		{"CLOSED", func(p map[string]any) { p["state"] = "CLOSED" }, false},
		{"MERGED", func(p map[string]any) { p["state"] = "MERGED" }, false},
		{"参照消失", func(p map[string]any) { p["baseRef"] = nil }, true},
		{"不正SHA", func(p map[string]any) { p["headRefOid"] = "bad" }, true},
		{"PR消失", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req gqlRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if strings.Contains(strings.ToLower(req.Query), "body") || strings.Contains(req.Query, "comments") || strings.Contains(req.Query, "reviews") {
					t.Errorf("本文を選択: %s", req.Query)
				}
				var p map[string]any
				if tc.change != nil {
					p = map[string]any{"url": "https://github.com/o/r/pull/1", "state": "OPEN", "headRefOid": strings.Repeat("1", 40), "baseRefOid": strings.Repeat("2", 40), "baseRefName": "main", "baseRef": policyRef("main", "refs/heads/", "Commit", strings.Repeat("4", 40))}
					tc.change(p)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": p}}})
			}))
			defer server.Close()
			client, err := NewWithHTTPClientAndURLFull(server.Client(), server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			out, err := client.GetPRContext(context.Background(), DescriptionTarget{Owner: "o", Repo: "r", PR: 1})
			if (err != nil) != tc.wantError {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if !tc.wantError && (out.PolicyBaseSHA == out.BaseSHA || out.PolicyBaseSHA != strings.Repeat("4", 40)) {
				t.Fatalf("比較元SHAを宣言へ代用: %+v", out)
			}
		})
	}
}
