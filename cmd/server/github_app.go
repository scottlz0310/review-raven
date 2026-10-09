package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/scottlz0310/review-raven/internal/githubapp"
)

type appAuthConfig struct {
	githubapp.Config
	ProxySecret string
}

func loadGitHubAppConfig(getenv func(string) string, readFile func(string) ([]byte, error)) (string, *appAuthConfig, error) {
	mode := strings.TrimSpace(getenv("REVIEW_RAVEN_AUTH_MODE"))
	if mode == "" {
		mode = "gateway"
	}
	if mode == "gateway" {
		return mode, nil, nil
	}
	if mode != "github-app" {
		return "", nil, errors.New("REVIEW_RAVEN_AUTH_MODEはgatewayまたはgithub-appを指定してください")
	}
	if strings.TrimSpace(getenv("REVIEW_RAVEN_GATEWAY_INTERNAL_URL")) != "" || strings.TrimSpace(getenv("REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET")) != "" {
		return "", nil, errors.New("github-appモードではdelegated background accessの設定を外してください")
	}
	proxySecret := strings.TrimSpace(getenv("REVIEW_RAVEN_PROXY_SECRET"))
	if len(proxySecret) < 32 {
		return "", nil, errors.New("REVIEW_RAVEN_PROXY_SECRETに32文字以上の専用共有シークレットを指定してください")
	}
	id := func(name string) (int64, error) {
		value, err := strconv.ParseInt(strings.TrimSpace(getenv(name)), 10, 64)
		if err != nil || value <= 0 {
			return 0, fmt.Errorf("%sは正の整数で指定してください", name)
		}
		return value, nil
	}
	appID, err := id("REVIEW_RAVEN_GITHUB_APP_ID")
	if err != nil {
		return "", nil, err
	}
	installationID, err := id("REVIEW_RAVEN_GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return "", nil, err
	}
	owner := strings.TrimSpace(getenv("REVIEW_RAVEN_GITHUB_APP_OWNER"))
	if owner == "" {
		return "", nil, errors.New("REVIEW_RAVEN_GITHUB_APP_OWNERに対象組織を指定してください")
	}
	encoded := strings.TrimSpace(getenv("REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_B64"))
	file := strings.TrimSpace(getenv("REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_FILE"))
	if (encoded == "") == (file == "") {
		return "", nil, errors.New("専用Appの秘密鍵はPRIVATE_KEY_B64またはPRIVATE_KEY_FILEの片方だけを指定してください")
	}
	var key []byte
	if encoded != "" {
		key, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", nil, errors.New("REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_B64が有効なbase64ではありません")
		}
	} else {
		key, err = readFile(file)
		if err != nil {
			return "", nil, fmt.Errorf("専用Appの秘密鍵ファイルを読み込めません: %w", err)
		}
	}
	return mode, &appAuthConfig{
		Config:      githubapp.Config{AppID: appID, InstallationID: installationID, Owner: owner, PrivateKeyPEM: key},
		ProxySecret: proxySecret,
	}, nil
}

func loadProcessGitHubAppConfig() (string, *appAuthConfig) {
	mode, cfg, err := loadGitHubAppConfig(os.Getenv, os.ReadFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review-raven: GitHub認証設定が不正です: %v\n", err)
		os.Exit(1)
	}
	return mode, cfg
}
