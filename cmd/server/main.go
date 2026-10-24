package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	ghclient "github.com/scottlz0310/review-raven/internal/github"
	"github.com/scottlz0310/review-raven/internal/githubapp"
	"github.com/scottlz0310/review-raven/internal/middleware"
	"github.com/scottlz0310/review-raven/internal/owlclient"
	"github.com/scottlz0310/review-raven/internal/reviewrequest"
	"github.com/scottlz0310/review-raven/internal/store"
	"github.com/scottlz0310/review-raven/internal/tools"
	"github.com/scottlz0310/review-raven/internal/trustedauthors"
	"github.com/scottlz0310/review-raven/internal/watch"
)

func main() {
	cfg := loadConfig()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.logLevel),
	})))

	// Open (or create) the SQLite trigger_log database.
	db, err := store.Open(cfg.sqlitePath)
	if err != nil {
		slog.Error("failed to open SQLite database", "path", cfg.sqlitePath, "err", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	slog.Info("GitHub認証モード", "mode", cfg.authMode)

	authMiddleware := middleware.Auth()
	if cfg.githubApp != nil {
		authMiddleware = middleware.AppProxyAuth(cfg.githubApp.ProxySecret)
	}

	mux := http.NewServeMux()

	// OAuth endpoints are not supported; return 410 Gone with migration guidance.
	goneHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = fmt.Fprintln(w, `{"error":"oauth_removed","detail":"Standalone OAuth is not supported in review-raven. Connect via mcp-gateway instead. See https://github.com/scottlz0310/review-raven#readme"}`)
	}
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", goneHandler)
	mux.HandleFunc("GET /authorize", goneHandler)
	mux.HandleFunc("GET /callback", goneHandler)
	mux.HandleFunc("POST /token", goneHandler)
	mux.HandleFunc("POST /register", goneHandler)

	// Health check (no auth required)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintln(w, `{"status":"ok"}`)
	})

	// MCP endpoints (auth required) — Streamable HTTP transport (stateless, MCP 2026-07-28)
	threshold := time.Duration(cfg.inProgressThresholdSec) * time.Second
	builderOpts := tools.BuilderOptions{TrustedCommentAuthors: cfg.trustedCommentAuthors}
	var ledger *reviewrequest.Service
	stopDelivery := context.CancelFunc(func() {})
	if cfg.githubApp != nil {
		source, err := githubapp.NewTokenSource(cfg.githubApp.Config)
		if err != nil {
			slog.Error("専用Appの認証を初期化できません", "err", err)
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = source.ValidateInstallation(ctx)
		cancel()
		if err != nil {
			slog.Error("専用Appのinstallationを確認できません", "err", err)
			os.Exit(1)
		}
		builderOpts.InstallationTokens = source
		slog.Info("専用Appのinstallationを確認しました", "app_id", cfg.githubApp.AppID,
			"installation_id", cfg.githubApp.InstallationID, "owner", cfg.githubApp.Owner)

		if cfg.reviewRequestACL != nil {
			// 依頼の台帳は専用Appモードだけで、保護設定の ACL を認可に使う。内部面（owl / Squirrel）を
			// 有効にしない限り、観測を送れる producer は存在しない。
			var authorizer reviewrequest.Authorizer = cfg.reviewRequestACL
			var producers reviewrequest.ProducerPolicy = reviewrequest.DenyAllProducers{}
			if cfg.serviceACL != nil {
				authorizer, err = reviewrequest.NewSplitAuthorizer(cfg.reviewRequestACL, cfg.serviceACL)
				if err != nil {
					slog.Error("利用者とサービスのアクセスリストが不正です", "err", err)
					os.Exit(1)
				}
				producers = cfg.serviceACL
			}
			ledger, err = reviewrequest.NewService(context.Background(), db.SQL(), reviewrequest.Deps{
				Authorizer: authorizer,
				Heads:      tools.NewInstallationHeadResolver(threshold, source),
				Producers:  producers,
				Settings:   reviewrequest.DefaultSettings(),
			})
			if err != nil {
				slog.Error("レビュー依頼の台帳を初期化できません", "err", err)
				os.Exit(1)
			}
			builderOpts.ReviewRequests = ledger
			slog.Info("レビュー依頼の公開面を有効にしました", "acl_users", len(cfg.reviewRequestACL.Users()))
		}
	}
	if len(cfg.trustedCommentAuthors) == 0 {
		slog.Warn("TRUSTED_COMMENT_AUTHORS is not set: get_trusted_comment_authors returns an error, so no comment author is trusted. Set TRUSTED_COMMENT_AUTHORS to the GitHub logins whose comments may be read (comma-separated).")
	} else {
		slog.Info("trusted comment authors configured", "count", len(cfg.trustedCommentAuthors))
	}
	if cfg.gatewayInternalURL != "" {
		slog.Info("phase B gateway delegated background access enabled",
			"endpoint", cfg.gatewayInternalURL)
		builderOpts.GatewayClientFactory = buildGatewayClientFactory(cfg.gatewayInternalURL, cfg.gatewayInternalSecret, threshold)
	}
	mcpHandler := tools.BuildStreamableHandlerWithOptions(db, threshold, builderOpts)
	defer mcpHandler.Close()
	mux.Handle("/mcp", authMiddleware(mcpHandler))
	mux.Handle("/mcp/", authMiddleware(mcpHandler))

	// 内部面（サービス主体向け）は agent 向けの /mcp とは別のサーバー・別の proxy 秘密にする。
	var internalHandler *tools.StreamableHandler
	if cfg.serviceACL != nil {
		internalHandler = tools.BuildInternalHandler(ledger)
		defer internalHandler.Close()
		internalAuth := middleware.AppProxyAuth(cfg.internalProxySecret)
		mux.Handle("/internal/mcp", internalAuth(internalHandler))
		mux.Handle("/internal/mcp/", internalAuth(internalHandler))
		slog.Info("レビュー依頼の内部面を有効にしました", "services", cfg.serviceACL.Services())
	}

	// outbox の配送 worker。レビュー側（thread-owl）の内部面が設定されたときだけ動かす。
	if cfg.owlDelivery != nil {
		owl, err := owlclient.New(*cfg.owlDelivery)
		if err != nil {
			slog.Error("thread-owl 向けの配送クライアントを作成できません", "err", err)
			os.Exit(1)
		}
		var deliveryCtx context.Context
		deliveryCtx, stopDelivery = context.WithCancel(context.Background())
		defer stopDelivery()
		go func() {
			if err := ledger.RunDelivery(deliveryCtx, owl, cfg.deliveryInterval); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("配送 worker が停止しました", "err", err)
			}
		}()
		slog.Info("レビュー側への配送 worker を開始しました", "endpoint", cfg.owlDelivery.Endpoint, "interval", cfg.deliveryInterval.String())
	}

	addr := net.JoinHostPort(cfg.bindAddr, cfg.port)
	slog.Info("review-raven starting", "addr", addr)
	// WriteTimeout remains unlimited because legacy wait_for_copilot_review still exists
	// as a blocking fallback and may occupy one tool call for up to 30 minutes.
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
	server.RegisterOnShutdown(mcpHandler.Close)
	server.RegisterOnShutdown(stopDelivery)
	if internalHandler != nil {
		server.RegisterOnShutdown(internalHandler.Close)
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		mcpHandler.Close()
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

type config struct {
	authMode               string
	githubApp              *appAuthConfig
	port                   string
	bindAddr               string
	logLevel               string
	sqlitePath             string
	inProgressThresholdSec int
	gatewayInternalURL     string
	gatewayInternalSecret  string
	trustedCommentAuthors  []string
	// reviewRequestACL is non-nil only when the review-request tools are enabled.
	reviewRequestACL *reviewrequest.ACL
	// serviceACL and internalProxySecret are set only when the internal
	// (service-to-service) surface is enabled.
	serviceACL          *reviewrequest.ServiceACL
	internalProxySecret string
	// owlDelivery is non-nil only when delivery to thread-owl is enabled.
	owlDelivery      *owlclient.Config
	deliveryInterval time.Duration
}

func loadConfig() config {
	mode, appConfig := loadProcessGitHubAppConfig()
	gatewayURL := strings.TrimSpace(os.Getenv("REVIEW_RAVEN_GATEWAY_INTERNAL_URL"))
	gatewaySecret := strings.TrimSpace(os.Getenv("REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET"))
	// Fail-closed: both env vars must be set together. Configuring only one is
	// almost always a deployment mistake (e.g., secret leaked but URL forgotten),
	// so refuse to start rather than silently falling back to static tokens.
	//
	// Note: this runs before slog.SetDefault is configured in main(), so we
	// write directly to stderr instead of via slog. Otherwise startup-time
	// config failures would be logged with the default slog handler/format
	// rather than the intended JSON handler + LOG_LEVEL.
	if (gatewayURL == "") != (gatewaySecret == "") {
		fmt.Fprintf(os.Stderr,
			"review-raven: phase B gateway config is incomplete: set both REVIEW_RAVEN_GATEWAY_INTERNAL_URL and REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET, or neither (url_set=%t secret_set=%t)\n",
			gatewayURL != "", gatewaySecret != "")
		os.Exit(1)
	}
	// When both are set, validate URL/secret at startup so misconfiguration
	// (bad scheme, non-loopback host) fails fast instead of silently degrading
	// every watch to static tokens at runtime.
	if gatewayURL != "" {
		if err := ghclient.ValidateGatewayEndpoint(gatewayURL, gatewaySecret); err != nil {
			fmt.Fprintf(os.Stderr, "review-raven: phase B gateway config rejected at startup: %v\n", err)
			os.Exit(1)
		}
	}
	// Fail-fast: a malformed entry (wildcard, typo) must not silently weaken or
	// break the comment-author gate that reads this list.
	trusted, err := trustedauthors.Parse(os.Getenv("TRUSTED_COMMENT_AUTHORS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "review-raven: invalid TRUSTED_COMMENT_AUTHORS: %v\n", err)
		os.Exit(1)
	}
	reviewACL, err := loadReviewRequestConfig(os.Getenv("REVIEW_RAVEN_REVIEW_REQUESTS"), os.Getenv("REVIEW_RAVEN_REQUEST_ACL"), appConfig != nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review-raven: invalid review request configuration: %v\n", err)
		os.Exit(1)
	}
	publicProxySecret := ""
	if appConfig != nil {
		publicProxySecret = appConfig.ProxySecret
	}
	serviceACL, internalSecret, err := loadInternalSurfaceConfig(
		os.Getenv("REVIEW_RAVEN_INTERNAL_SURFACE"), os.Getenv("REVIEW_RAVEN_SERVICE_ACL"),
		os.Getenv("REVIEW_RAVEN_INTERNAL_PROXY_SECRET"), publicProxySecret, reviewACL != nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review-raven: invalid internal surface configuration: %v\n", err)
		os.Exit(1)
	}
	owlDelivery, deliveryInterval, err := loadDeliveryConfig(
		os.Getenv("REVIEW_RAVEN_OWL_INTERNAL_URL"), os.Getenv("REVIEW_RAVEN_OWL_INTERNAL_TOKEN"),
		os.Getenv("REVIEW_RAVEN_DELIVERY_INTERVAL_SEC"), reviewACL != nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review-raven: invalid delivery configuration: %v\n", err)
		os.Exit(1)
	}
	return config{
		authMode:               mode,
		githubApp:              appConfig,
		port:                   getEnv("MCP_PORT", "8083"),
		bindAddr:               getEnv("BIND_ADDR", "127.0.0.1"),
		logLevel:               getEnv("LOG_LEVEL", "info"),
		sqlitePath:             getEnv("SQLITE_PATH", "/data/review-raven.db"),
		inProgressThresholdSec: getEnvInt("IN_PROGRESS_THRESHOLD_SEC", 30),
		gatewayInternalURL:     gatewayURL,
		gatewayInternalSecret:  gatewaySecret,
		trustedCommentAuthors:  trusted,
		reviewRequestACL:       reviewACL,
		serviceACL:             serviceACL,
		internalProxySecret:    internalSecret,
		owlDelivery:            owlDelivery,
		deliveryInterval:       deliveryInterval,
	}
}

// defaultDeliveryInterval is how often the delivery worker looks for due messages.
const defaultDeliveryInterval = 5 * time.Second

// loadDeliveryConfig reads the endpoint and credential of thread-owl's internal
// surface and the polling interval of the delivery worker. It returns a nil
// config when delivery is off (neither value is set).
//
// The URL and the credential are set together or not at all, delivery needs the
// review-request ledger, and the endpoint must be https (or loopback http):
// a half-configured or clear-text delivery is a startup error, never a fallback.
func loadDeliveryConfig(rawURL, token, rawInterval string, ledgerEnabled bool) (*owlclient.Config, time.Duration, error) {
	rawURL, token = strings.TrimSpace(rawURL), strings.TrimSpace(token)
	if rawURL == "" && token == "" {
		return nil, 0, nil
	}
	if (rawURL == "") != (token == "") {
		return nil, 0, errors.New("set both REVIEW_RAVEN_OWL_INTERNAL_URL and REVIEW_RAVEN_OWL_INTERNAL_TOKEN, or neither")
	}
	if !ledgerEnabled {
		return nil, 0, errors.New("delivery to thread-owl requires REVIEW_RAVEN_REVIEW_REQUESTS=true")
	}
	endpoint, err := owlclient.ValidateEndpoint(rawURL)
	if err != nil {
		return nil, 0, fmt.Errorf("REVIEW_RAVEN_OWL_INTERNAL_URL: %w", err)
	}
	interval := defaultDeliveryInterval
	if raw := strings.TrimSpace(rawInterval); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 3600 {
			return nil, 0, fmt.Errorf("REVIEW_RAVEN_DELIVERY_INTERVAL_SEC must be an integer within 1..3600, got %q", raw)
		}
		interval = time.Duration(seconds) * time.Second
	}
	return &owlclient.Config{Endpoint: endpoint, Token: token}, interval, nil
}

// loadInternalSurfaceConfig reads the opt-in switch, the service ACL and the
// proxy secret of the internal (service-to-service) surface. It returns a nil
// ACL when the surface is off.
//
// The surface needs the review-request ledger, a service ACL, and its own proxy
// secret that differs from the public one: a caller holding the public secret
// must not be able to reach the internal route. Enabling it without any of
// these is a startup error, never a fallback.
func loadInternalSurfaceConfig(enabled, aclJSON, secret, publicSecret string, ledgerEnabled bool) (*reviewrequest.ServiceACL, string, error) {
	switch strings.ToLower(strings.TrimSpace(enabled)) {
	case "", "false", "0", "off":
		return nil, "", nil
	case "true", "1", "on":
	default:
		return nil, "", fmt.Errorf("REVIEW_RAVEN_INTERNAL_SURFACE must be true or false, got %q", enabled)
	}
	if !ledgerEnabled {
		return nil, "", errors.New("REVIEW_RAVEN_INTERNAL_SURFACE=true requires REVIEW_RAVEN_REVIEW_REQUESTS=true")
	}
	secret = strings.TrimSpace(secret)
	if len(secret) < 32 {
		return nil, "", errors.New("REVIEW_RAVEN_INTERNAL_PROXY_SECRET must be a dedicated shared secret of at least 32 characters")
	}
	if secret == strings.TrimSpace(publicSecret) {
		return nil, "", errors.New("REVIEW_RAVEN_INTERNAL_PROXY_SECRET must differ from REVIEW_RAVEN_PROXY_SECRET")
	}
	acl, err := reviewrequest.ParseServiceACL(aclJSON)
	if err != nil {
		return nil, "", fmt.Errorf("REVIEW_RAVEN_SERVICE_ACL: %w", err)
	}
	return acl, secret, nil
}

// loadReviewRequestConfig reads the opt-in switch and the access list of the
// review-request tools. It returns a nil ACL when the feature is off.
//
// The feature needs the dedicated GitHub App mode (the caller identity must be
// proxy-verified) and a non-empty ACL: enabling it without either is a startup
// error, never a silent fallback to "allow everyone" or to an unverified identity.
func loadReviewRequestConfig(enabled, aclJSON string, appMode bool) (*reviewrequest.ACL, error) {
	switch strings.ToLower(strings.TrimSpace(enabled)) {
	case "", "false", "0", "off":
		return nil, nil
	case "true", "1", "on":
	default:
		return nil, fmt.Errorf("REVIEW_RAVEN_REVIEW_REQUESTS must be true or false, got %q", enabled)
	}
	if !appMode {
		return nil, errors.New("REVIEW_RAVEN_REVIEW_REQUESTS=true requires the dedicated GitHub App mode")
	}
	acl, err := reviewrequest.ParseACL(aclJSON)
	if err != nil {
		return nil, fmt.Errorf("REVIEW_RAVEN_REQUEST_ACL: %w", err)
	}
	return acl, nil
}

// buildGatewayClientFactory returns a watch ClientFactory that resolves the
// access token for the authenticated GitHub login from the gateway's
// /internal/v1/whoami endpoint. The returned source is wrapped in
// oauth2.ReuseTokenSource per watch so whoami is only hit when the cached
// token is near expiry.
//
// A single *http.Client is constructed once and shared across every watch's
// token source. The underlying http.Transport is safe for concurrent reuse
// and sharing it avoids allocating a fresh transport (and its idle-connection
// pool) per watch.
//
// Endpoint URL and shared secret are validated at startup in loadConfig, so
// the only failure remaining at watch creation is an empty GitHub login
// (which would indicate a session-binding bug, not a config error). In that
// case we log at Error level and keep using the request-scoped token supplied
// by mcp-gateway. No process-wide credential fallback exists.
func buildGatewayClientFactory(endpointURL, secret string, threshold time.Duration) func(ctx context.Context, token, login string) watch.ReviewDataFetcher {
	sharedHTTP := &http.Client{Timeout: ghclient.DefaultGatewayTimeout}
	return func(ctx context.Context, token, login string) watch.ReviewDataFetcher {
		ts, err := ghclient.NewGatewayTokenSource(ghclient.GatewayTokenSourceConfig{
			EndpointURL: endpointURL,
			Secret:      secret,
			Subject:     login,
			HTTPClient:  sharedHTTP,
			Context:     ctx,
		})
		if err != nil {
			slog.Error("gateway token source unavailable; phase B disabled for this watch, using request-scoped token",
				"login", login, "err", err)
			return ghclient.NewClient(ctx, token, threshold, nil)
		}
		return ghclient.NewClientWithTokenSource(ctx, oauth2.ReuseTokenSource(nil, ts), threshold)
	}
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
