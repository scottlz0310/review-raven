package tools

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// errTrustedAuthorsNotConfigured is the error_type returned when the
// TRUSTED_COMMENT_AUTHORS allowlist is empty (unset). An empty allowlist trusts
// nobody, so the caller must stop instead of reading it as "nothing to check".
const errTrustedAuthorsNotConfigured = "TRUSTED_AUTHORS_NOT_CONFIGURED"

// TrustedCommentAuthorsInput is the (empty) input schema for get_trusted_comment_authors.
type TrustedCommentAuthorsInput struct{}

// TrustedCommentAuthorsOutput is the output schema for get_trusted_comment_authors.
type TrustedCommentAuthorsOutput struct {
	// Logins are the normalized logins (lowercase, without a "[bot]" suffix)
	// whose comments may enter an agent's context.
	Logins []string `json:"logins"`
}

var trustedCommentAuthorsTool = &mcp.Tool{
	Name: "get_trusted_comment_authors",
	Description: "PR のコメント・レビューの本文を、エージェントの文脈に入れてよい投稿者の許可リストを返す。" +
		"正本は、サーバーの環境変数 TRUSTED_COMMENT_AUTHORS である。" +
		"login は正規化済み(ASCII の小文字、末尾の [bot] なし)で、呼び出し側は、投稿者の login を同じ規則で正規化して、完全一致で比較する。" +
		"read-only で、GitHub へは通信しない。許可リストは秘密情報ではない。" +
		"未設定(空)のときは、空のリストを返さず、エラー TRUSTED_AUTHORS_NOT_CONFIGURED を返す(誰も信頼しないので、サイクルを進めてはならない)。",
}

type trustedAuthorsError struct {
	ErrorType string `json:"error_type"`
	Message   string `json:"message"`
}

func trustedAuthorsHandler(logins []string) func(context.Context, *mcp.CallToolRequest, TrustedCommentAuthorsInput) (*mcp.CallToolResult, TrustedCommentAuthorsOutput, error) {
	// Copy so that a caller mutating the slice it passed in cannot change the allowlist.
	configured := append([]string{}, logins...)
	return func(_ context.Context, _ *mcp.CallToolRequest, _ TrustedCommentAuthorsInput) (*mcp.CallToolResult, TrustedCommentAuthorsOutput, error) {
		if len(configured) == 0 {
			e := trustedAuthorsError{
				ErrorType: errTrustedAuthorsNotConfigured,
				Message:   "TRUSTED_COMMENT_AUTHORS is not set on the server, so no comment author is trusted. Set it to the GitHub logins whose comments may be read (comma-separated).",
			}
			b, _ := json.Marshal(e)
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
				StructuredContent: e,
				IsError:           true,
			}, TrustedCommentAuthorsOutput{Logins: []string{}}, nil
		}
		return nil, TrustedCommentAuthorsOutput{Logins: append([]string{}, configured...)}, nil
	}
}

// RegisterTrustedCommentAuthorsTool adds get_trusted_comment_authors to the MCP
// server. logins must already be normalized (see internal/trustedauthors).
func RegisterTrustedCommentAuthorsTool(server *mcp.Server, logins []string) {
	mcp.AddTool(server, trustedCommentAuthorsTool, trustedAuthorsHandler(logins))
}
