package tools

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottlz0310/review-raven/internal/autherr"
	ghclient "github.com/scottlz0310/review-raven/internal/github"
	"github.com/scottlz0310/review-raven/internal/middleware"
)

type githubClientProvider func(context.Context, *mcp.CallToolRequest) (*ghclient.Client, error)

type InstallationTokens interface {
	Token(context.Context) (string, error)
	Invalidate(string)
}

func newInstallationClientProvider(threshold time.Duration, source InstallationTokens) githubClientProvider {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*ghclient.Client, error) {
		if loginFromToolRequest(ctx, req) == "" || tokenFromToolRequest(ctx, req) == "" {
			return nil, autherr.NewAuthRequired()
		}
		token, err := source.Token(ctx)
		if err != nil {
			return nil, err
		}
		return ghclient.NewClient(ctx, token, threshold, source.Invalidate), nil
	}
}

func newGitHubClientProvider(threshold time.Duration, invalidate func(string)) githubClientProvider {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*ghclient.Client, error) {
		token := tokenFromToolRequest(ctx, req)
		if token == "" {
			return nil, autherr.NewAuthRequired()
		}
		return ghclient.NewClient(ctx, token, threshold, invalidate), nil
	}
}

func loginFromToolRequest(ctx context.Context, req *mcp.CallToolRequest) string {
	if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil && req.Extra.TokenInfo.UserID != "" {
		return req.Extra.TokenInfo.UserID
	}
	return middleware.LoginFromContext(ctx)
}

func tokenFromToolRequest(ctx context.Context, req *mcp.CallToolRequest) string {
	if req != nil && req.Extra != nil {
		if token := bearerTokenFromHeader(req.Extra.Header); token != "" {
			return token
		}
	}
	return middleware.TokenFromContext(ctx)
}

func bearerTokenFromHeader(header http.Header) string {
	if header == nil {
		return ""
	}
	fields := strings.Fields(header.Get("Authorization"))
	if len(fields) == 2 && strings.EqualFold(fields[0], "bearer") {
		return fields[1]
	}
	return ""
}
