package githubapp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestTokenLifecycle(t *testing.T) {
	key, keyPEM := testKey(t)
	now := time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)
	var issued atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != "https://api.github.com/app/installations/169443079/access_tokens" {
			t.Errorf("予期しないリクエスト: %s %s", r.Method, r.URL)
		}
		jwt := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(jwt) != 3 {
			t.Fatal("JWTが3部分ではありません")
		}
		digest := sha256.Sum256([]byte(jwt[0] + "." + jwt[1]))
		signature, err := base64.RawURLEncoding.DecodeString(jwt[2])
		if err != nil || rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
			t.Error("JWT署名が不正です")
		}
		claimsBytes, err := base64.RawURLEncoding.DecodeString(jwt[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims struct {
			Issuer string `json:"iss"`
			Issued int64  `json:"iat"`
			Expiry int64  `json:"exp"`
		}
		if err := json.Unmarshal(claimsBytes, &claims); err != nil {
			t.Fatal(err)
		}
		if claims.Issuer != "5184108" || claims.Issued != now.Add(-time.Minute).Unix() || claims.Expiry != now.Add(9*time.Minute).Unix() {
			t.Errorf("JWT claimsが不正です: %+v", claims)
		}
		var body struct {
			Permissions map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Permissions) != 4 || body.Permissions["checks"] != "read" || body.Permissions["contents"] != "write" {
			t.Errorf("発行権限が不正です: %v", body.Permissions)
		}
		if r.Header.Get("X-GitHub-Api-Version") != apiVersion {
			t.Error("API versionがありません")
		}
		generation := issued.Add(1)
		return response(http.StatusCreated, fmt.Sprintf(`{"token":"ghs_5184108_token_%d","expires_at":%q}`, generation, now.Add(time.Hour).Format(time.RFC3339))), nil
	})}
	source, err := NewTokenSource(Config{AppID: 5184108, InstallationID: 169443079, Owner: "scottlz0310", PrivateKeyPEM: keyPEM, HTTPClient: client, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name       string
		advance    time.Duration
		invalidate string
		want       string
		count      int32
	}{
		{"初回", 0, "", "ghs_5184108_token_1", 1},
		{"キャッシュ", 54 * time.Minute, "", "ghs_5184108_token_1", 1},
		{"期限5分前", time.Minute, "", "ghs_5184108_token_2", 2},
		{"旧世代の401", 0, "ghs_5184108_token_1", "ghs_5184108_token_2", 2},
		{"現世代の401", 0, "ghs_5184108_token_2", "ghs_5184108_token_3", 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now = now.Add(tt.advance)
			if tt.invalidate != "" {
				source.Invalidate(tt.invalidate)
			}
			token, err := source.Token(context.Background())
			if err != nil || token != tt.want || issued.Load() != tt.count {
				t.Fatalf("token=%q err=%v count=%d", token, err, issued.Load())
			}
		})
	}
	var wg sync.WaitGroup
	source.Invalidate("ghs_5184108_token_3")
	for range 16 {
		wg.Go(func() {
			if _, err := source.Token(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if issued.Load() != 4 {
		t.Errorf("並行呼び出しで重複発行しました: %d", issued.Load())
	}
}

func TestValidateInstallation(t *testing.T) {
	_, keyPEM := testKey(t)
	for _, tt := range []struct {
		name, appOwner, owner, kind, selection, permission string
		appID, installationAppID                           int64
		suspended, wantError                               bool
	}{
		{"正しい組織", "scottlz0310", "SCOTTLZ0310", "Organization", "all", "write", 5184108, 5184108, false, false},
		{"Appの所有者違い", "other", "scottlz0310", "Organization", "all", "write", 5184108, 5184108, false, true},
		{"App ID違い", "scottlz0310", "scottlz0310", "Organization", "all", "write", 1, 5184108, false, true},
		{"installationのApp違い", "scottlz0310", "scottlz0310", "Organization", "all", "write", 5184108, 1, false, true},
		{"対象組織違い", "scottlz0310", "other", "Organization", "all", "write", 5184108, 5184108, false, true},
		{"個人installation", "scottlz0310", "scottlz0310", "User", "all", "write", 5184108, 5184108, false, true},
		{"選択repo", "scottlz0310", "scottlz0310", "Organization", "selected", "write", 5184108, 5184108, false, true},
		{"停止中", "scottlz0310", "scottlz0310", "Organization", "all", "write", 5184108, 5184108, true, true},
		{"Contents権限不足", "scottlz0310", "scottlz0310", "Organization", "all", "read", 5184108, 5184108, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/app" {
					return response(200, fmt.Sprintf(`{"id":%d,"owner":{"login":%q,"type":"Organization"}}`, tt.appID, tt.appOwner)), nil
				}
				suspended := "null"
				if tt.suspended {
					suspended = `"2026-10-09T00:00:00Z"`
				}
				return response(200, fmt.Sprintf(`{"id":169443079,"app_id":%d,"account":{"login":%q,"type":%q},"repository_selection":%q,"suspended_at":%s,"permissions":{"metadata":"read","checks":"read","pull_requests":"write","contents":%q}}`, tt.installationAppID, tt.owner, tt.kind, tt.selection, suspended, tt.permission)), nil
			})}
			source, err := NewTokenSource(Config{AppID: 5184108, InstallationID: 169443079, Owner: "scottlz0310", PrivateKeyPEM: keyPEM, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			if err := source.ValidateInstallation(context.Background()); (err != nil) != tt.wantError {
				t.Fatalf("ValidateInstallation()=%v", err)
			}
		})
	}
}

func TestTokenErrorsDoNotReturnStaleCredentials(t *testing.T) {
	_, keyPEM := testKey(t)
	for _, tt := range []struct {
		name, body string
		status     int
		cancelled  bool
	}{
		{"拒否", `{"token":"secret-token"}`, 403, false},
		{"不正JSON", "{", 201, false},
		{"不正期限の秘密値", `{"token":"secret-token","expires_at":"secret-token"}`, 201, false},
		{"期限切れ", `{"token":"secret-token","expires_at":"2020-01-01T00:00:00Z"}`, 201, false},
		{"tokenなし", `{"expires_at":"2099-01-01T00:00:00Z"}`, 201, false},
		{"キャンセル", "", 201, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Context().Err() != nil {
					return nil, r.Context().Err()
				}
				return response(tt.status, tt.body), nil
			})}
			source, err := NewTokenSource(Config{AppID: 1, InstallationID: 2, Owner: "org", PrivateKeyPEM: keyPEM, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			source.token, source.expiry = "stale-secret", now.Add(time.Minute)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelled {
				cancel()
			}
			token, err := source.Token(ctx)
			if token != "" || err == nil {
				t.Fatalf("token=%q err=%v", token, err)
			}
			if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "stale-secret") {
				t.Fatal("エラーに秘密情報が含まれます")
			}
		})
	}
}

func TestPrivateKeyFormats(t *testing.T) {
	key, pkcs1 := testKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecBytes, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name      string
		pem       []byte
		wantError bool
	}{
		{"PKCS1", pkcs1, false},
		{"PKCS8", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), false},
		{"RSA以外", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecBytes}), true},
		{"不正PEM", []byte("bad-key"), true},
		{"不正DER", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("bad")}), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewTokenSource(Config{AppID: 1, InstallationID: 2, Owner: "org", PrivateKeyPEM: tt.pem})
			if (err != nil) != tt.wantError {
				t.Fatalf("NewTokenSource()=%v", err)
			}
		})
	}
}
