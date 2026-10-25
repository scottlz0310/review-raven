package reader_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/scottlz0310/review-raven/internal/reader"
	"github.com/scottlz0310/review-raven/internal/reader/readertest"
	"github.com/scottlz0310/review-raven/internal/trustedauthors"
)

func TestScenarios(t *testing.T) {
	outputSchema := readertest.Schema(t, "read_pr_comments.output.schema.json")
	errorSchema := readertest.Schema(t, "read_pr_comments.error.schema.json")

	for _, sc := range readertest.Load(t) {
		t.Run(sc.Name, func(t *testing.T) {
			src := readertest.NewSource(t, sc)
			result, err := reader.Read(context.Background(), src, sc.ServerAllowlist)

			if src.Calls != sc.Expect.FetchCalls || src.BodyCalls != sc.Expect.BodyFetchCalls {
				t.Fatalf("fetch calls = %d (with bodies %d), want %d (with bodies %d)", src.Calls, src.BodyCalls, sc.Expect.FetchCalls, sc.Expect.BodyFetchCalls)
			}

			if sc.Expect.Outcome == "ok" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got, want := readertest.Keys(result.Snapshot), readertest.Sorted(sc.Expect.CommentIDs); !readertest.Equal(got, want) {
					t.Fatalf("comments = %v, want %v", got, want)
				}
				if want := sc.Expect.Allowlist; want != nil {
					got := result.Allowlist
					if got.ServerLogins != want.ServerLogins || got.ProjectFile != want.ProjectFile || got.ProjectLogins != want.ProjectLogins {
						t.Fatalf("allowlist = %+v, want %+v", got, *want)
					}
				}
				readertest.Validate(t, outputSchema, reader.ToOutput(result))
				return
			}

			var gateErr *reader.Error
			if !errors.As(err, &gateErr) {
				t.Fatalf("error = %v, want a gate error %s", err, sc.Expect.Code)
			}
			if gateErr.Code != sc.Expect.Code {
				t.Fatalf("code = %s, want %s", gateErr.Code, sc.Expect.Code)
			}
			var refs []string
			for _, c := range gateErr.Comments {
				refs = append(refs, readertest.Key(c.Kind, c.ID))
			}
			if got, want := readertest.Sorted(refs), readertest.Sorted(sc.Expect.ErrorCommentIDs); !readertest.Equal(got, want) {
				t.Fatalf("error comments = %v, want %v", got, want)
			}
			envelope := reader.ToErrorEnvelope(gateErr)
			readertest.Validate(t, errorSchema, envelope)

			// 失敗の応答に、どのコメントの本文も含まれない。
			raw, _ := json.Marshal(envelope)
			for _, body := range readertest.Bodies(sc) {
				if body != "" && strings.Contains(string(raw), body) {
					t.Fatalf("error response leaks comment text %q: %s", body, raw)
				}
			}
		})
	}
}

// 検証で止まったシナリオでは、本文を要求した回数が、検証を通過した試行の分だけである。
// 取り直した試行は「メタデータ→本文」の組、最後に止まった試行は「メタデータ」だけなので、
// メタデータの呼び出し回数 = 本文の呼び出し回数 + 1 になる（GitHub を呼ばずに止まる場合は 0 回）。
func TestBodiesAreNeverRequestedBeforeTheAuthorCheckPasses(t *testing.T) {
	for _, sc := range readertest.Load(t) {
		switch sc.Expect.Code {
		case reader.CodeUntrustedComment, reader.CodeAuthorCheckFailed, reader.CodeProjectAllowlistInvalid:
			if sc.Expect.FetchCalls != 2*sc.Expect.BodyFetchCalls+1 {
				t.Fatalf("%s: fetch calls = %d, body calls = %d: a blocked attempt must stop before it asks for any text", sc.Name, sc.Expect.FetchCalls, sc.Expect.BodyFetchCalls)
			}
		case reader.CodeTrustedAuthorsNotConfigured:
			if sc.Expect.FetchCalls != 0 {
				t.Fatalf("%s: an empty allowlist must stop before calling GitHub", sc.Name)
			}
		}
	}
}

// 不正なプロジェクト allowlist のエラーが長くても、失敗の応答は契約（message は 500 文字以内）を満たす。
func TestProjectAllowlistErrorStaysWithinTheContract(t *testing.T) {
	errorSchema := readertest.Schema(t, "read_pr_comments.error.schema.json")
	for _, value := range []string{strings.Repeat("!", 600), strings.Repeat("あ", 600)} {
		_, parseErr := trustedauthors.ParseProjectFile([]byte(`{"version":1,"additional_logins":["` + value + `"]}`))
		if parseErr == nil {
			t.Fatal("expected the project file to be rejected")
		}
		src := &failingProjectSource{err: errors.Join(reader.ErrProjectAllowlistInvalid, parseErr)}
		_, err := reader.Read(context.Background(), src, []string{"thread-owl"})

		var gateErr *reader.Error
		if !errors.As(err, &gateErr) || gateErr.Code != reader.CodeProjectAllowlistInvalid {
			t.Fatalf("error = %v, want %s", err, reader.CodeProjectAllowlistInvalid)
		}
		readertest.Validate(t, errorSchema, reader.ToErrorEnvelope(gateErr))
	}
}

// 上流のエラーがどれだけ長くても、契約の長さを超えず、文字の途中では切らない。
func TestErrorEnvelopeClampsLongMessagesOnACharacterBoundary(t *testing.T) {
	errorSchema := readertest.Schema(t, "read_pr_comments.error.schema.json")
	for _, msg := range []string{strings.Repeat("x", 501), strings.Repeat("あ", 2000), strings.Repeat("x", 500)} {
		envelope := reader.ToErrorEnvelope(&reader.Error{Code: reader.CodeSnapshotChanged, Message: msg})
		got := envelope.Error.Message
		if n := len([]rune(got)); n > reader.MaxMessageLength {
			t.Fatalf("message has %d characters, want at most %d", n, reader.MaxMessageLength)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("message is not valid UTF-8 after clamping: %q", got)
		}
		if len([]rune(msg)) <= reader.MaxMessageLength && got != msg {
			t.Fatalf("a message within the limit must not change: %q", got)
		}
		readertest.Validate(t, errorSchema, envelope)
	}
}

type failingProjectSource struct{ err error }

func (s *failingProjectSource) Fetch(context.Context, bool) (reader.Snapshot, error) {
	return reader.Snapshot{HeadSHA: strings.Repeat("1", 40), BaseSHA: strings.Repeat("2", 40)}, nil
}

func (s *failingProjectSource) ProjectAllowlist(context.Context, string) (reader.ProjectAllowlist, error) {
	return reader.ProjectAllowlist{}, s.err
}
