package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppProxyAuth(t *testing.T) {
	secret := strings.Repeat("s", 32)
	for _, tt := range []struct {
		name, identity, token, configured string
		want                              int
	}{
		{"認証済みproxy", "alice", secret, secret, 200},
		{"任意Bearerの偽装", "alice", "invalid-token", secret, 401},
		{"別proxyの鍵", "alice", strings.Repeat("x", 32), secret, 401},
		{"Bearer欠落", "alice", "", secret, 401},
		{"identity欠落", "", secret, secret, 401},
		{"設定鍵欠落", "alice", secret, "", 401},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			handler := AppProxyAuth(tt.configured)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if !ProxyVerified(r.Context()) || LoginFromContext(r.Context()) != "alice" {
					t.Error("検証済みproxyのcontextがありません")
				}
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.Header.Set("X-Authenticated-User", tt.identity)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tt.want || called != (tt.want == 200) {
				t.Fatalf("status=%d called=%v", recorder.Code, called)
			}
			if strings.Contains(recorder.Body.String(), secret) || strings.Contains(recorder.Body.String(), "invalid-token") {
				t.Fatal("認証失敗応答がBearerを露出しています")
			}
		})
	}
}
