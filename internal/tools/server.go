package tools

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottlz0310/review-raven/internal/middleware"
	"github.com/scottlz0310/review-raven/internal/store"
	"github.com/scottlz0310/review-raven/internal/watch"
)

var schemaCache = mcp.NewSchemaCache()

// TokenInvalidator is not supported — standalone OAuth was removed in the pre-rename copilot-review-mcp lineage.

// StreamableHandler serves MCP over Streamable HTTP and owns shared background state.
type StreamableHandler struct {
	handler      http.Handler
	watchManager *watch.Manager
	server       *mcp.Server

	closeOnce sync.Once
}

// ServeHTTP proxies requests to the underlying MCP streamable handler.
func (h *StreamableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

// Close stops background review watches owned by this handler.
func (h *StreamableHandler) Close() {
	if h == nil {
		return
	}

	h.closeOnce.Do(func() {
		if h.server != nil {
			for session := range h.server.Sessions() {
				sessionID := session.ID()
				if err := session.Close(); err != nil {
					slog.Warn("failed to close MCP session", "session_id", sessionID, "err", err)
				}
			}
		}
		if h.watchManager != nil {
			h.watchManager.Close()
		}
	})
}

// BuilderOptions configures optional behaviors for BuildStreamableHandlerWithOptions.
// Unset fields fall back to defaults that preserve current behavior.
type BuilderOptions struct {
	// 専用Appモードはこのsourceを使い、Copilot系・watchを登録しない。
	InstallationTokens InstallationTokens

	// GatewayClientFactory, if non-nil, overrides the default static-token
	// watch ClientFactory. It is invoked per watch with the authenticated
	// GitHub session token and login. Used by Phase B delegated background
	// access (see scottlz0310/review-raven#29).
	GatewayClientFactory func(ctx context.Context, token, login string) watch.ReviewDataFetcher

	// TrustedCommentAuthors is the normalized allowlist of PR comment authors
	// (the TRUSTED_COMMENT_AUTHORS environment variable). It is published by
	// get_trusted_comment_authors; an empty list makes that tool fail closed.
	TrustedCommentAuthors []string
}

// BuildStreamableHandler returns a handler that serves MCP over Streamable HTTP
// in stateless mode (per-request temporary sessions, no Mcp-Session-Id), and
// only over protocol 2026-07-28 — the legacy initialize handshake is refused.
// GitHub clientは各tool呼び出しで作り、設定に応じて受信tokenまたは専用App tokenを使う。
func BuildStreamableHandler(db *store.DB, threshold time.Duration) *StreamableHandler {
	return BuildStreamableHandlerWithOptions(db, threshold, BuilderOptions{})
}

// BuildStreamableHandlerWithOptions is the option-accepting variant of
// BuildStreamableHandler. Callers that need Phase B gateway delegated
// background access should use this entry point.
func BuildStreamableHandlerWithOptions(db *store.DB, threshold time.Duration, opts BuilderOptions) *StreamableHandler {
	clientProvider := newGitHubClientProvider(threshold, nil)
	if opts.InstallationTokens != nil {
		clientProvider = newInstallationClientProvider(threshold, opts.InstallationTokens)
	}
	// Appモードではwatchを開始せず、過去のwatch URIへの購読も拒否する。
	var watchManager *watch.Manager
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "review-raven", Version: "0.6.0"},
		&mcp.ServerOptions{
			SchemaCache: schemaCache,
			SubscribeHandler: func(ctx context.Context, req *mcp.SubscribeRequest) error {
				if req == nil || req.Params == nil {
					return nil
				}
				uri := req.Params.URI
				if watchManager == nil {
					return mcp.ResourceNotFoundError(uri)
				}
				const watchPrefix = "review-raven://watch/"
				const legacyPrefix = "copilot-review://watch/"
				if strings.HasPrefix(uri, legacyPrefix) {
					return mcp.ResourceNotFoundError(uri) // legacy scheme removed in #66
				}
				if !strings.HasPrefix(uri, watchPrefix) {
					return nil // not a watch URI; allow subscription for other resource types
				}
				// URI has the watch prefix — parse it strictly so malformed URIs are rejected.
				watchID, err := parseWatchIDFromURI(uri)
				if err != nil {
					return mcp.ResourceNotFoundError(uri)
				}
				login := middleware.LoginFromContext(ctx)
				if login == "" {
					return fmt.Errorf("authenticated GitHub login is required to subscribe")
				}
				snap, ok := watchManager.GetByID(watchID)
				if !ok || snap.Login != login {
					return mcp.ResourceNotFoundError(uri)
				}
				return nil
			},
			UnsubscribeHandler: func(_ context.Context, _ *mcp.UnsubscribeRequest) error {
				return nil
			},
		},
	)
	if opts.InstallationTokens == nil {
		watchManager = watch.NewManager(db, watch.Options{
			Threshold:       threshold,
			InvalidateToken: nil,
			ClientFactory:   opts.GatewayClientFactory,
			NotifyResourceUpdated: func(uri string) {
				if err := srv.ResourceUpdated(context.Background(), &mcp.ResourceUpdatedNotificationParams{URI: uri}); err != nil {
					slog.Warn("resource updated notification failed", "uri", uri, "err", err)
				}
			},
		})
		RegisterStatusTool(srv, clientProvider, db)
		RegisterWatchTools(srv, watchManager)
		RegisterWatchResources(srv, watchManager)
		RegisterWaitTool(srv, clientProvider, db)
		RegisterRequestTool(srv, clientProvider, db)
		RegisterCycleTool(srv, clientProvider, db)
		RegisterDiagnoseTokenTool(srv)
	}
	RegisterThreadTools(srv, clientProvider)
	RegisterListCheckRunsForSHATool(srv, clientProvider)
	RegisterTrustedCommentAuthorsTool(srv, opts.TrustedCommentAuthors)

	streamableHandler := &StreamableHandler{
		watchManager: watchManager,
		server:       srv,
	}

	getServer := func(r *http.Request) *mcp.Server {
		if middleware.TokenFromContext(r.Context()) == "" {
			return nil
		}
		return srv
	}
	streamableHandler.handler = rejectLegacyInitialize(mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{
		// Stateless is required for MCP 2026-07-28 negotiation: go-sdk only
		// accepts the new protocol version on the Streamable HTTP transport when
		// Stateless is true (stateful servers negotiate down to 2025-11-25).
		// session IDを使わず、各リクエストのgateway認証に依存する。
		Stateless: true,
		// DisableLocalhostProtection is opt-in via MCP_DISABLE_LOCALHOST_PROTECTION=true.
		// Enable when the server runs behind a reverse proxy or inside a Docker network.
		DisableLocalhostProtection: os.Getenv("MCP_DISABLE_LOCALHOST_PROTECTION") == "true",
	}))
	return streamableHandler
}
