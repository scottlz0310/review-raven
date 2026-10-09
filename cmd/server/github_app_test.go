package main

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestLoadGitHubAppConfig(t *testing.T) {
	for _, tt := range []struct {
		name, mode, change, value string
		wantError                 bool
	}{
		{"既定gateway", "", "", "", false},
		{"明示gateway", "gateway", "", "", false},
		{"base64鍵", "github-app", "", "", false},
		{"ファイル鍵", "github-app", "file", "key.pem", false},
		{"不明モード", "other", "", "", true},
		{"App ID未設定", "github-app", "REVIEW_RAVEN_GITHUB_APP_ID", "", true},
		{"不正Installation ID", "github-app", "REVIEW_RAVEN_GITHUB_APP_INSTALLATION_ID", "-1", true},
		{"組織未設定", "github-app", "REVIEW_RAVEN_GITHUB_APP_OWNER", "", true},
		{"不正base64", "github-app", "REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_B64", "secret-invalid!", true},
		{"鍵未設定", "github-app", "REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_B64", "", true},
		{"鍵の二重指定", "github-app", "REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_FILE", "key.pem", true},
		{"ファイル読み込み失敗", "github-app", "file", "missing.pem", true},
		{"delegated URL混在", "github-app", "REVIEW_RAVEN_GATEWAY_INTERNAL_URL", "http://127.0.0.1", true},
		{"delegated secret混在", "github-app", "REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET", "secret", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{
				"REVIEW_RAVEN_AUTH_MODE":     tt.mode,
				"REVIEW_RAVEN_GITHUB_APP_ID": "5184108", "REVIEW_RAVEN_GITHUB_APP_INSTALLATION_ID": "169443079",
				"REVIEW_RAVEN_GITHUB_APP_OWNER":           "scottlz0310",
				"REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_B64": base64.StdEncoding.EncodeToString([]byte("test-key")),
			}
			if tt.change == "file" {
				env["REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_B64"] = ""
				env["REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_FILE"] = tt.value
			} else if tt.change != "" {
				env[tt.change] = tt.value
			}
			mode, cfg, err := loadGitHubAppConfig(func(name string) string { return env[name] }, func(path string) ([]byte, error) {
				if path == "missing.pem" {
					return nil, errors.New("ファイルなし")
				}
				return []byte("test-key"), nil
			})
			if (err != nil) != tt.wantError {
				t.Fatalf("loadGitHubAppConfig()=%v", err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "secret-invalid!") {
					t.Fatal("秘密鍵をエラーへ転載しました")
				}
				return
			}
			if tt.mode == "github-app" {
				if mode != tt.mode || cfg == nil || cfg.AppID != 5184108 || cfg.InstallationID != 169443079 || cfg.Owner != "scottlz0310" || string(cfg.PrivateKeyPEM) != "test-key" {
					t.Fatalf("App設定が不正です: mode=%s", mode)
				}
			} else if mode != "gateway" || cfg != nil {
				t.Fatal("既存gatewayモードが変わりました")
			}
		})
	}
}
