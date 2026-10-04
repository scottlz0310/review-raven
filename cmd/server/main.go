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
	"github.com/scottlz0310/review-raven/internal/middleware"
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

	slog.Info("auth mode: gateway (trusting X-Authenticated-User header from mcp-gateway)")

	authMiddleware := middleware.Auth()

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
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		mcpHandler.Close()
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

type config struct {
	port                   string
	bindAddr               string
	logLevel               string
	sqlitePath             string
	inProgressThresholdSec int
	gatewayInternalURL     string
	gatewayInternalSecret  string
	trustedCommentAuthors  []string
}

func loadConfig() config {
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
	return config{
		port:                   getEnv("MCP_PORT", "8083"),
		bindAddr:               getEnv("BIND_ADDR", "127.0.0.1"),
		logLevel:               getEnv("LOG_LEVEL", "info"),
		sqlitePath:             getEnv("SQLITE_PATH", "/data/review-raven.db"),
		inProgressThresholdSec: getEnvInt("IN_PROGRESS_THRESHOLD_SEC", 30),
		gatewayInternalURL:     gatewayURL,
		gatewayInternalSecret:  gatewaySecret,
		trustedCommentAuthors:  trusted,
	}
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
