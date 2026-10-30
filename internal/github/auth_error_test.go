package ghclient

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-github/v93/github"
)

func TestIsAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "REST 401 error response",
			err: &github.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusUnauthorized},
			},
			want: true,
		},
		{
			name: "REST non-401 error response",
			err: &github.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusForbidden},
			},
			want: false,
		},
		{
			name: "githubv4 plain 401 error string",
			err:  errors.New("non-200 OK status code: 401 Unauthorized body: {\"message\":\"Bad credentials\"}"),
			want: true,
		},
		{
			name: "nearby non-401 error string",
			err:  errors.New("non-200 OK status code: 403 Forbidden body: {\"message\":\"Resource not accessible\"}"),
			want: false,
		},
		{
			name: "generic non-auth error",
			err:  errors.New("network timeout"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAuthError(tt.err); got != tt.want {
				t.Fatalf("IsAuthError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsGatewayAuthError(t *testing.T) {
	wrapped := func(sentinel error) error {
		return fmt.Errorf("outer: %w", sentinel)
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrGatewaySubjectGone direct", ErrGatewaySubjectGone, true},
		{"ErrGatewayRotationFailed direct", ErrGatewayRotationFailed, true},
		{"ErrGatewaySubjectGone wrapped", wrapped(ErrGatewaySubjectGone), true},
		{"ErrGatewayRotationFailed wrapped", wrapped(ErrGatewayRotationFailed), true},
		// ErrGatewayUpstreamFailure is transient; must NOT map to auth error.
		{"ErrGatewayUpstreamFailure direct", ErrGatewayUpstreamFailure, false},
		{"ErrGatewayUpstreamFailure wrapped", wrapped(ErrGatewayUpstreamFailure), false},
		{"ErrGatewayUnauthorized", ErrGatewayUnauthorized, false},
		{"ErrGatewayBadRequest", ErrGatewayBadRequest, false},
		{"unrelated error", errors.New("network timeout"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsGatewayAuthError(tt.err); got != tt.want {
				t.Fatalf("IsGatewayAuthError() = %v, want %v", got, tt.want)
			}
		})
	}
}
