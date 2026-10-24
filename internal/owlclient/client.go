// Package owlclient is the review-raven → thread-owl delivery client: it calls
// the reviewer side's internal MCP tools accept_review_candidate and
// cancel_review_candidate and implements reviewrequest.Deliverer.
//
// Every failure is returned as a contract error (reviewrequest.Error). A lost
// acknowledgement or a 5xx-like failure is UNAVAILABLE (retryable); a refused
// credential, a rejected request or an error answered by thread-owl is final and
// keeps the code thread-owl gave, so nothing is retried through another route.
package owlclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottlz0310/review-raven/internal/reviewrequest"
)

// Config configures a Client.
type Config struct {
	// Endpoint is the MCP endpoint of thread-owl's internal surface, normally the
	// gateway route. See ValidateEndpoint for what is accepted.
	Endpoint string
	// Token is the service credential presented as a Bearer token. The gateway
	// maps it to the service principal; it is never logged.
	Token string
	// HTTPClient overrides the HTTP client (tests, proxies). Nil uses a client
	// without a global timeout: each call is bounded by its context instead.
	HTTPClient *http.Client
}

// Client delivers outbox messages to thread-owl.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

var _ reviewrequest.Deliverer = (*Client)(nil)

// ValidateEndpoint accepts an https URL, or an http URL only for a loopback host
// (the credential must not cross a network in clear text). It returns the
// normalized URL.
func ValidateEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid thread-owl endpoint %q", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("the thread-owl endpoint must not carry credentials, a query or a fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", errors.New("a plain http thread-owl endpoint is only allowed on a loopback host")
		}
	default:
		return "", fmt.Errorf("unsupported thread-owl endpoint scheme %q", u.Scheme)
	}
	return u.String(), nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	endpoint, err := ValidateEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("owlclient: a service credential is required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{endpoint: endpoint, token: strings.TrimSpace(cfg.Token), http: httpClient}, nil
}

// Accept implements reviewrequest.Deliverer.
func (c *Client) Accept(ctx context.Context, msg reviewrequest.AcceptMessage) (reviewrequest.AcceptResult, error) {
	var out struct {
		CandidateID string `json:"candidateId"`
		Duplicate   bool   `json:"duplicate"`
	}
	if err := c.call(ctx, "accept_review_candidate", msg, &out); err != nil {
		return reviewrequest.AcceptResult{}, err
	}
	if out.CandidateID == "" {
		return reviewrequest.AcceptResult{}, unavailable("thread-owl answered without a candidateId")
	}
	return reviewrequest.AcceptResult{CandidateID: out.CandidateID, Duplicate: out.Duplicate}, nil
}

// Cancel implements reviewrequest.Deliverer.
func (c *Client) Cancel(ctx context.Context, msg reviewrequest.CancelMessage) (reviewrequest.CancelResult, error) {
	var out struct {
		CancelID        string `json:"cancelId"`
		Acknowledged    bool   `json:"acknowledged"`
		CandidateStatus string `json:"candidateStatus"`
		// 欠落と false を区別する（契約は const:true）。
		GithubWriteReconciled *bool `json:"githubWriteReconciled"`
	}
	if err := c.call(ctx, "cancel_review_candidate", msg, &out); err != nil {
		return reviewrequest.CancelResult{}, err
	}
	// ACK でないもの、旧実行の GitHub 書込みが未確定のものを確定として扱わない
	// （取消の確定は旧候補の無効化と書込み結果の確定が前提）。
	if !out.Acknowledged || out.CancelID != msg.CancelID {
		return reviewrequest.CancelResult{}, unavailable("thread-owl did not acknowledge the cancellation")
	}
	if out.GithubWriteReconciled == nil || !*out.GithubWriteReconciled {
		return reviewrequest.CancelResult{}, &reviewrequest.Error{
			Code: reviewrequest.CodeCancelUnconfirmed, Message: "thread-owl has not reconciled the GitHub write of the cancelled candidate", Retryable: true,
		}
	}
	return reviewrequest.CancelResult{CancelID: out.CancelID, CandidateStatus: out.CandidateStatus}, nil
}

func (c *Client) call(ctx context.Context, tool string, args, out any) error {
	rec := &statusRecorder{base: c.http.Transport, token: c.token}
	httpClient := *c.http
	httpClient.Transport = rec
	// 資格情報を付けた要求の転送先は ValidateEndpoint で検証できないため、追わない
	// （転送は 3xx のまま UNAVAILABLE になり、試行上限で止まる）。
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	client := mcp.NewClient(&mcp.Implementation{Name: "review-raven-delivery", Version: "0.6.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: c.endpoint, HTTPClient: &httpClient, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		return rec.classify(err)
	}
	defer func() { _ = session.Close() }()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return rec.classify(err)
	}
	if result.IsError {
		return peerError(result)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil || result.StructuredContent == nil {
		return unavailable("thread-owl answered without structured content")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return unavailable("thread-owl answered with an unreadable result")
	}
	return nil
}

// peerError turns an error result of thread-owl into a contract error. An
// answer that is not a contract envelope is a misbehaving peer, treated like a
// lost acknowledgement (bounded by the attempt limit).
func peerError(result *mcp.CallToolResult) error {
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return unavailable("thread-owl answered with an unreadable error")
	}
	var envelope reviewrequest.ErrorEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Error.Code == "" {
		return unavailable("thread-owl answered with an error that is not a contract error")
	}
	code := envelope.Error.Code
	switch code {
	case reviewrequest.CodeCancelUnconfirmed, reviewrequest.CodeUnavailable:
		return &reviewrequest.Error{Code: code, Message: envelope.Error.Message, Retryable: true}
	}
	return &reviewrequest.Error{Code: code, Message: envelope.Error.Message, Details: envelope.Error.Details}
}

func unavailable(message string) *reviewrequest.Error {
	return &reviewrequest.Error{Code: reviewrequest.CodeUnavailable, Message: message, Retryable: true}
}

// statusRecorder adds the credential to every request and remembers the last
// HTTP status, which is the only way to tell a refused credential from a lost
// connection once the SDK has wrapped the failure.
type statusRecorder struct {
	base  http.RoundTripper
	token string

	mu     sync.Mutex
	status int
}

func (r *statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+r.token)
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(cloned)
	if err == nil {
		r.mu.Lock()
		r.status = resp.StatusCode
		r.mu.Unlock()
	}
	return resp, err
}

// classify maps a transport-level failure to a contract error. A refused
// credential and a rejected request are final; everything else is a lost
// acknowledgement. The error text never includes the credential.
func (r *statusRecorder) classify(err error) error {
	r.mu.Lock()
	status := r.status
	r.mu.Unlock()
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &reviewrequest.Error{
			Code:    reviewrequest.CodePermissionDenied,
			Message: fmt.Sprintf("thread-owl refused the service credential (HTTP %d)", status),
		}
	case status == http.StatusTooManyRequests || status >= 500 || status == 0 || status < 400:
		return unavailable(fmt.Sprintf("could not reach thread-owl: %v", err))
	default:
		return &reviewrequest.Error{
			Code:    reviewrequest.CodeInvalidArgument,
			Message: fmt.Sprintf("thread-owl rejected the request (HTTP %d)", status),
		}
	}
}
