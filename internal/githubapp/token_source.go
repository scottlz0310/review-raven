package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiVersion = "2026-03-10"

type Config struct {
	AppID          int64
	InstallationID int64
	Owner          string
	PrivateKeyPEM  []byte
	HTTPClient     *http.Client
	Now            func() time.Time
}

type TokenSource struct {
	config Config
	key    *rsa.PrivateKey
	mu     sync.Mutex
	token  string
	expiry time.Time
}

func NewTokenSource(cfg Config) (*TokenSource, error) {
	if cfg.AppID <= 0 || cfg.InstallationID <= 0 || strings.TrimSpace(cfg.Owner) == "" {
		return nil, errors.New("GitHub AppのID・Installation ID・組織名を指定してください")
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("GitHub Appの秘密鍵を解析できません: %w", err)
	}
	cfg.PrivateKeyPEM = nil
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &TokenSource{config: cfg, key: key}, nil
}

// installation tokenは利用者権限と独立するため、発行前に所有先と対象範囲を照合する。
func (s *TokenSource) ValidateInstallation(ctx context.Context) error {
	var app struct {
		ID    int64 `json:"id"`
		Owner struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"owner"`
	}
	if err := s.request(ctx, http.MethodGet, "/app", nil, http.StatusOK, &app); err != nil {
		return err
	}
	if app.ID != s.config.AppID || !strings.EqualFold(app.Owner.Login, s.config.Owner) || app.Owner.Type != "Organization" {
		return errors.New("GitHub AppのIDまたは組織所有者が設定と一致しません")
	}
	var installation struct {
		ID                  int64             `json:"id"`
		AppID               int64             `json:"app_id"`
		RepositorySelection string            `json:"repository_selection"`
		Permissions         map[string]string `json:"permissions"`
		SuspendedAt         *time.Time        `json:"suspended_at"`
		Account             struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	path := "/app/installations/" + strconv.FormatInt(s.config.InstallationID, 10)
	if err := s.request(ctx, http.MethodGet, path, nil, http.StatusOK, &installation); err != nil {
		return err
	}
	if installation.ID != s.config.InstallationID || installation.AppID != s.config.AppID ||
		!strings.EqualFold(installation.Account.Login, s.config.Owner) || installation.Account.Type != "Organization" ||
		installation.RepositorySelection != "all" || installation.SuspendedAt != nil {
		return errors.New("GitHub Appのinstallationは指定組織の全repoを対象とする有効なinstallationではありません")
	}
	for name, required := range requiredPermissions() {
		actual := installation.Permissions[name]
		if actual != required && (required != "read" || actual != "write") {
			return fmt.Errorf("GitHub Appのinstallationに%s: %s権限がありません", name, required)
		}
	}
	return nil
}

func requiredPermissions() map[string]string {
	return map[string]string{"metadata": "read", "pull_requests": "write", "contents": "write", "checks": "read"}
}

func (s *TokenSource) Token(ctx context.Context) (string, error) {
	// 発行中もロックを保持し、並行tool呼び出しで重複発行しない。
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.expiry.After(s.config.Now().Add(5*time.Minute)) {
		return s.token, nil
	}
	body, err := json.Marshal(map[string]any{"permissions": requiredPermissions()})
	if err != nil {
		return "", fmt.Errorf("installation tokenの権限をエンコードできません: %w", err)
	}
	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/app/installations/" + strconv.FormatInt(s.config.InstallationID, 10) + "/access_tokens"
	if err := s.request(ctx, http.MethodPost, path, strings.NewReader(string(body)), http.StatusCreated, &payload); err != nil {
		return "", err
	}
	if payload.Token == "" || !payload.ExpiresAt.After(s.config.Now()) {
		return "", errors.New("GitHub Appの応答に有効なinstallation tokenまたは有効期限がありません")
	}
	s.token, s.expiry = payload.Token, payload.ExpiresAt
	return s.token, nil
}

func (s *TokenSource) Invalidate(rejected string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == rejected {
		s.token, s.expiry = "", time.Time{}
	}
}

func (s *TokenSource) request(ctx context.Context, method, path string, body io.Reader, status int, out any) error {
	appJWT, err := s.signJWT()
	if err != nil {
		return fmt.Errorf("GitHub AppのJWTに署名できません: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, body)
	if err != nil {
		return fmt.Errorf("GitHub Appのリクエストを作成できません: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.config.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub App API %s %sへの接続に失敗しました: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != status {
		// tokenやJWTを含み得る応答本文はエラーへ転載しない。
		return fmt.Errorf("GitHub App API %s %sがHTTP %dを返しました", method, path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("GitHub App API %sの応答のJSON形式が不正です（%T）", path, err)
	}
	return nil
}

func (s *TokenSource) signJWT() (string, error) {
	now := s.config.Now()
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(s.config.AppID, 10),
	})
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("PEM形式の秘密鍵がありません")
	}
	if block.Type == "RSA PRIVATE KEY" {
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("秘密鍵はPKCS#1またはPKCS#8のPEM形式で指定してください")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("RSA秘密鍵を指定してください")
	}
	return key, nil
}
