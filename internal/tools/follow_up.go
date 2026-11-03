package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/scottlz0310/review-raven/internal/autherr"
	ghclient "github.com/scottlz0310/review-raven/internal/github"
	"github.com/scottlz0310/review-raven/internal/reader"
	"github.com/scottlz0310/review-raven/internal/trustedauthors"
)

type followUpSourceRef struct {
	Kind     reader.Kind `json:"kind"`
	ID       string      `json:"id"`
	ThreadID string      `json:"threadId,omitempty"`
	URL      string      `json:"url,omitempty"`
}
type followUpTarget struct {
	ghclient.DescriptionTarget
	ExpectedHeadSHA string            `json:"expectedHeadSha"`
	Source          followUpSourceRef `json:"source"`
}
type followUpWrite struct {
	followUpTarget
	ExpectedSnapshotID string `json:"expectedSnapshotId"`
	Body               string `json:"body"`
}
type followUpReplyInput struct{ followUpWrite }
type followUpIssueInput struct {
	followUpWrite
	Reason string   `json:"reason"`
	Title  string   `json:"title"`
	Labels []string `json:"labels,omitempty"`
}
type followUpReadInput struct {
	followUpTarget
	IssueNumber int `json:"issueNumber"`
}
type followUpResult struct {
	SchemaVersion       int                       `json:"schemaVersion"`
	Status              string                    `json:"status"`
	Source              followUpSourceRef         `json:"source"`
	Comment             *ghclient.FollowUpComment `json:"comment,omitempty"`
	Issue               *ghclient.FollowUpIssue   `json:"issue,omitempty"`
	VerifiedHeadSHA     string                    `json:"verifiedHeadSha"`
	VerifiedSnapshotID  string                    `json:"verifiedSnapshotId,omitempty"`
	PolicyBaseSHA       string                    `json:"policyBaseSha"`
	PRState             string                    `json:"prState"`
	MatchesExpectedHead bool                      `json:"matchesExpectedHead"`
}
type followUpResource struct {
	ID     string `json:"id,omitempty"`
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
}
type followUpError struct {
	Code            string             `json:"code"`
	Message         string             `json:"message"`
	MayHaveChanged  bool               `json:"mayHaveChanged"`
	CreatedResource *followUpResource  `json:"createdResource,omitempty"`
	Candidates      []followUpResource `json:"candidates,omitempty"`
}

func (e *followUpError) Error() string { return e.Code + ": " + e.Message }

type followUpErrorEnvelope struct {
	Error *followUpError `json:"error"`
}

func followUpFailure(code, message string) error { return &followUpError{Code: code, Message: message} }

var followUpIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)
var followUpHeadPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var followUpSnapshotPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (in followUpTarget) validate(thread bool) error {
	if err := in.Validate(); err != nil {
		return followUpFailure("INVALID_INPUT", "owner/repo/prが不正です")
	}
	if in.PR > math.MaxInt32 || !followUpHeadPattern.MatchString(in.ExpectedHeadSHA) || !followUpIDPattern.MatchString(in.Source.ID) || in.Source.URL != "" {
		return followUpFailure("INVALID_INPUT", "完全なHEAD・readerの元指摘IDを指定してください。URLはserverが取得します")
	}
	switch in.Source.Kind {
	case reader.KindReviewBody, reader.KindIssueComment:
		if in.Source.ThreadID != "" {
			return followUpFailure("INVALID_INPUT", "thread以外にはthreadIdを指定できません")
		}
	case reader.KindThreadComment:
		if !thread || in.Source.ThreadID == "" {
			return followUpFailure("INVALID_INPUT", "thread指摘の所属が不正です。返信には既存thread APIを使ってください")
		}
	default:
		return followUpFailure("INVALID_INPUT", "元指摘のkindが不正です")
	}
	return nil
}
func validateFollowUpBody(body string) error {
	if !utf8.ValidString(body) || strings.TrimSpace(body) == "" {
		return followUpFailure("INVALID_INPUT", "空白以外のUTF-8本文を指定してください")
	}
	for _, reserved := range []string{"review verdict", "@thread-owl re-review requested", "## レビュー対応サマリ（thread-owl）", "### サイクル状態", "review-raven-source:"} {
		if strings.Contains(strings.ToLower(body), reserved) {
			return followUpFailure("INVALID_INPUT", "定型投稿・Verdict・再レビュー起動・固定footerは返信本文へ含められません")
		}
	}
	return nil
}
func (in followUpWrite) validate(thread bool) error {
	if err := in.followUpTarget.validate(thread); err != nil {
		return err
	}
	if !followUpSnapshotPattern.MatchString(in.ExpectedSnapshotID) {
		return followUpFailure("INVALID_INPUT", "読取済みsnapshotIdを指定してください")
	}
	return validateFollowUpBody(in.Body)
}
func (in followUpIssueInput) validate() error {
	if err := in.followUpWrite.validate(true); err != nil {
		return err
	}
	if in.Reason != "out-of-scope" && in.Reason != "deferred" && in.Reason != "follow-up" {
		return followUpFailure("INVALID_INPUT", "reasonが不正です")
	}
	if !utf8.ValidString(in.Title) || strings.TrimSpace(in.Title) == "" {
		return followUpFailure("INVALID_INPUT", "UTF-8のtitleを指定してください")
	}
	seen := map[string]bool{}
	for _, label := range in.Labels {
		if !utf8.ValidString(label) || strings.TrimSpace(label) == "" || seen[label] {
			return followUpFailure("INVALID_INPUT", "labelが空・不正・重複しています")
		}
		seen[label] = true
	}
	return nil
}

type followUpSource interface {
	reader.Source
	Context(context.Context) (ghclient.PRContext, error)
	Bot(context.Context, string) (ghclient.DescriptionActor, error)
	PostReply(context.Context, string) (ghclient.FollowUpComment, error)
	ReadReply(context.Context, string) (ghclient.FollowUpComment, error)
	PostIssue(context.Context, string, string, []string) (ghclient.FollowUpIssue, error)
	Issue(context.Context, int, bool) (ghclient.FollowUpIssue, error)
	FindIssues(context.Context, string) ([]int, error)
	Labels(context.Context, []string) error
}
type githubFollowUpSource struct{ *githubCommentSource }

func (s *githubFollowUpSource) target() ghclient.DescriptionTarget {
	return ghclient.DescriptionTarget{Owner: s.owner, Repo: s.repo, PR: s.pr}
}
func (s *githubFollowUpSource) Bot(ctx context.Context, login string) (ghclient.DescriptionActor, error) {
	return s.gh.FollowUpBot(ctx, login)
}
func (s *githubFollowUpSource) PostReply(ctx context.Context, body string) (ghclient.FollowUpComment, error) {
	return s.gh.CreateFollowUpReply(ctx, s.target(), body)
}
func (s *githubFollowUpSource) ReadReply(ctx context.Context, id string) (ghclient.FollowUpComment, error) {
	return s.gh.GetFollowUpReply(ctx, s.target(), id)
}
func (s *githubFollowUpSource) PostIssue(ctx context.Context, title, body string, labels []string) (ghclient.FollowUpIssue, error) {
	return s.gh.CreateFollowUpIssue(ctx, s.target(), title, body, labels)
}
func (s *githubFollowUpSource) Issue(ctx context.Context, number int, body bool) (ghclient.FollowUpIssue, error) {
	return s.gh.GetFollowUpIssue(ctx, s.target(), number, body)
}
func (s *githubFollowUpSource) FindIssues(ctx context.Context, marker string) ([]int, error) {
	return s.gh.FindFollowUpIssues(ctx, s.target(), marker)
}
func (s *githubFollowUpSource) Labels(ctx context.Context, labels []string) error {
	return s.gh.CheckFollowUpLabels(ctx, s.target(), labels)
}

type followUpService struct {
	src      followUpSource
	logins   []string
	appLogin string
}

func locateFollowUp(s reader.Snapshot, ref followUpSourceRef) (followUpSourceRef, bool) {
	match := func(c reader.Comment, thread string) bool {
		return c.ID == ref.ID && c.Kind == ref.Kind && thread == ref.ThreadID
	}
	for _, t := range s.Threads {
		for _, c := range t.Comments {
			if match(c, t.ID) {
				ref.URL = c.URL
				return ref, c.URL != ""
			}
		}
	}
	for _, comments := range [][]reader.Comment{s.Reviews, s.IssueComments} {
		for _, c := range comments {
			if match(c, "") {
				ref.URL = c.URL
				return ref, c.URL != ""
			}
		}
	}
	return followUpSourceRef{}, false
}
func refsMatch(s reader.Snapshot, p ghclient.PRContext) bool {
	return s.HeadSHA == p.HeadSHA && s.BaseSHA == p.BaseSHA && s.BaseRefName == p.BaseRefName && s.PolicyBaseSHA == p.PolicyBaseSHA
}
func (s followUpService) prepare(ctx context.Context, in followUpTarget, snapshot string) (reader.Result, ghclient.PRContext, followUpSourceRef, error) {
	r, err := reader.ReadMetadata(ctx, s.src, s.logins)
	if err != nil {
		return r, ghclient.PRContext{}, followUpSourceRef{}, err
	}
	p, err := s.src.Context(ctx)
	if err != nil {
		return r, p, followUpSourceRef{}, err
	}
	ref, found := locateFollowUp(r.Snapshot, in.Source)
	if !found || !refsMatch(r.Snapshot, p) || (snapshot != "" && (r.SnapshotID != snapshot || p.State != "OPEN" || p.HeadSHA != in.ExpectedHeadSHA)) {
		return r, p, ref, followUpFailure("PRECONDITION_FAILED", "元指摘所属・OPEN・HEAD・snapshot・宣言参照が一致しません")
	}
	return r, p, ref, nil
}
func (s followUpService) checkRefs(ctx context.Context, before ghclient.PRContext) error {
	after, err := s.src.Context(ctx)
	if err != nil {
		return err
	}
	if before != after {
		return followUpFailure("PRECONDITION_FAILED", "PRのstate/HEAD/base/宣言参照が変化しました")
	}
	return nil
}
func followUpMarker(in followUpTarget) string {
	return fmt.Sprintf("Review-Raven-Source: %s/%s#%d %s:%s", strings.ToLower(in.Owner), strings.ToLower(in.Repo), in.PR, in.Source.Kind, in.Source.ID)
}
func hasFollowUpMarker(body, marker string) bool {
	return slices.Contains(strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n"), marker)
}
func sameFollowUpBot(a ghclient.DescriptionActor, b ghclient.DescriptionActor) bool {
	return a.NodeID != "" && a.NodeID == b.NodeID && a.Type == "Bot" && b.Type == "Bot" && trustedauthors.Normalize(a.Login) == trustedauthors.Normalize(b.Login)
}
func (s followUpService) bot(ctx context.Context) (ghclient.DescriptionActor, error) {
	b, err := s.src.Bot(ctx, s.appLogin)
	if err != nil || b.NodeID == "" || b.Type != "Bot" || s.appLogin == "" || trustedauthors.Normalize(b.Login) != trustedauthors.Normalize(s.appLogin) {
		return b, followUpFailure("APP_IDENTITY_FAILED", "認証済みAppのBot identityを確認できません")
	}
	return b, nil
}
func postFollowUpError(code string, resource *followUpResource) error {
	return &followUpError{Code: code, Message: "POST後の結果を確定できません。readで照合し、利用者の再投稿許可なしに再試行しないでください", MayHaveChanged: true, CreatedResource: resource}
}
func followUpOutput(r reader.Result, p ghclient.PRContext, ref followUpSourceRef, expected string) followUpResult {
	return followUpResult{SchemaVersion: 1, Status: "created", Source: ref, VerifiedHeadSHA: p.HeadSHA, VerifiedSnapshotID: r.SnapshotID, PolicyBaseSHA: p.PolicyBaseSHA, PRState: p.State, MatchesExpectedHead: p.HeadSHA == expected}
}

func (s followUpService) reply(ctx context.Context, in followUpReplyInput) (followUpResult, error) {
	if err := in.validate(false); err != nil {
		return followUpResult{}, err
	}
	r, p, ref, err := s.prepare(ctx, in.followUpTarget, in.ExpectedSnapshotID)
	if err != nil {
		return followUpResult{}, err
	}
	bot, err := s.bot(ctx)
	if err != nil {
		return followUpResult{}, err
	}
	full, err := reader.Read(ctx, s.src, s.logins)
	if err != nil {
		return followUpResult{}, err
	}
	if full.SnapshotID != r.SnapshotID {
		return followUpResult{}, followUpFailure("PRECONDITION_FAILED", "重複確認中にsnapshotが変化しました")
	}
	marker := followUpMarker(in.followUpTarget)
	for _, c := range full.Snapshot.IssueComments {
		if c.Body != nil && hasFollowUpMarker(*c.Body, marker) && c.AuthorType == "Bot" && trustedauthors.Normalize(c.Author) == trustedauthors.Normalize(bot.Login) {
			previous, e := s.src.ReadReply(ctx, c.ID)
			if e != nil {
				return followUpResult{}, e
			}
			if !sameFollowUpBot(previous.Author, bot) || previous.Body != *c.Body {
				return followUpResult{}, followUpFailure("SNAPSHOT_CHANGED", "既存返信identity/本文を確認できません")
			}
			return followUpResult{}, &followUpError{Code: "ALREADY_REPLIED", Message: "同じ元指摘へのApp返信があります", Candidates: []followUpResource{{ID: c.ID, URL: c.URL}}}
		}
	}
	body := "元指摘: " + ref.URL + "\n\n" + in.Body + "\n\n" + marker
	if err := s.checkRefs(ctx, p); err != nil {
		return followUpResult{}, err
	}
	created, err := s.src.PostReply(ctx, body)
	resource := &followUpResource{ID: created.ID, URL: created.URL}
	if err != nil || created.ID == "" || created.ID == "0" || created.URL == "" {
		return followUpResult{}, postFollowUpError("WRITE_OUTCOME_UNKNOWN", resource)
	}
	verified, err := reader.Read(ctx, s.src, s.logins)
	if err != nil {
		return followUpResult{}, postFollowUpError("WRITE_OUTCOME_UNKNOWN", resource)
	}
	found := false
	for _, c := range verified.Snapshot.IssueComments {
		if c.ID == created.ID && c.Body != nil && *c.Body == body && c.URL == created.URL && c.AuthorType == "Bot" && trustedauthors.Normalize(c.Author) == trustedauthors.Normalize(bot.Login) {
			found = true
		}
	}
	actual, err := s.src.ReadReply(ctx, created.ID)
	if err != nil {
		return followUpResult{}, postFollowUpError("WRITE_OUTCOME_UNKNOWN", resource)
	}
	if !found || actual.ID != created.ID || actual.URL != created.URL || actual.Body != body || !sameFollowUpBot(actual.Author, bot) || !refsMatch(verified.Snapshot, p) || s.checkRefs(ctx, p) != nil {
		return followUpResult{}, postFollowUpError("POST_WRITE_MISMATCH", resource)
	}
	out := followUpOutput(r, p, ref, in.ExpectedHeadSHA)
	out.Comment = &actual
	return out, nil
}

func (s followUpService) issueActors(ctx context.Context, p ghclient.PRContext, i ghclient.FollowUpIssue) error {
	if i.Author == nil || i.Author.NodeID == "" || i.Author.Login == "" || i.Author.Type == "" || (i.Editor == nil && i.EditedAt != "") || (i.Editor != nil && (i.Editor.NodeID == "" || i.Editor.Login == "" || i.Editor.Type == "" || i.EditedAt == "")) {
		return followUpFailure("AUTHOR_CHECK_FAILED", "Issue投稿者・最終編集者を確認できません")
	}
	project, err := s.src.ProjectAllowlist(ctx, p.PolicyBaseSHA)
	if err != nil {
		return err
	}
	allowed := append(append([]string{}, s.logins...), project.Logins...)
	for _, a := range []*ghclient.DescriptionActor{i.Author, i.Editor} {
		if a != nil && !slices.Contains(allowed, trustedauthors.Normalize(a.Login)) {
			return followUpFailure("UNTRUSTED_COMMENT", "Issue投稿者・最終編集者が許可されていません")
		}
	}
	return nil
}
func issueMetadataEqual(a, b ghclient.FollowUpIssue) bool {
	a.Title, a.Body, a.BodySHA256 = "", "", ""
	b.Title, b.Body, b.BodySHA256 = "", "", ""
	a.Labels = slices.Clone(a.Labels)
	b.Labels = slices.Clone(b.Labels)
	slices.Sort(a.Labels)
	slices.Sort(b.Labels)
	return reflect.DeepEqual(a, b)
}
func (s followUpService) readIssue(ctx context.Context, p ghclient.PRContext, number int) (ghclient.FollowUpIssue, error) {
	meta, err := s.src.Issue(ctx, number, false)
	if err != nil {
		return meta, err
	}
	if err = s.issueActors(ctx, p, meta); err != nil {
		return ghclient.FollowUpIssue{}, err
	}
	full, err := s.src.Issue(ctx, number, true)
	if err != nil {
		return ghclient.FollowUpIssue{}, err
	}
	after, err := s.src.Issue(ctx, number, false)
	if err != nil {
		return ghclient.FollowUpIssue{}, err
	}
	if !issueMetadataEqual(meta, full) || !issueMetadataEqual(full, after) {
		return ghclient.FollowUpIssue{}, followUpFailure("SNAPSHOT_CHANGED", "Issue identity・編集時刻・所属が取得中に変化しました")
	}
	return full, nil
}
func (s followUpService) createIssue(ctx context.Context, in followUpIssueInput) (followUpResult, error) {
	if err := in.validate(); err != nil {
		return followUpResult{}, err
	}
	r, p, ref, err := s.prepare(ctx, in.followUpTarget, in.ExpectedSnapshotID)
	if err != nil {
		return followUpResult{}, err
	}
	bot, err := s.bot(ctx)
	if err != nil {
		return followUpResult{}, err
	}
	marker := followUpMarker(in.followUpTarget)
	candidates, err := s.src.FindIssues(ctx, marker)
	if err != nil {
		return followUpResult{}, err
	}
	existing := []followUpResource{}
	for _, number := range candidates {
		i, e := s.readIssue(ctx, p, number)
		if e != nil {
			return followUpResult{}, e
		}
		if hasFollowUpMarker(i.Body, marker) && i.Author != nil && sameFollowUpBot(*i.Author, bot) {
			existing = append(existing, followUpResource{ID: i.ID, Number: i.Number, URL: i.URL})
		}
	}
	if len(existing) > 0 {
		return followUpResult{}, &followUpError{Code: "FOLLOW_UP_EXISTS", Message: "同じ元指摘のApp Issueがあります。内容を確認してください", Candidates: existing}
	}
	if len(candidates) > 1 {
		for _, number := range candidates {
			existing = append(existing, followUpResource{Number: number})
		}
		return followUpResult{}, &followUpError{Code: "FOLLOW_UP_EXISTS", Message: "候補が複数あるため新規作成せず、内容の確認を待ちます", Candidates: existing}
	}
	if err := s.src.Labels(ctx, in.Labels); err != nil {
		return followUpResult{}, err
	}
	// 検索中のPR投稿変化もPOST前に再照合する。検索索引の遅延は排他できない。
	current, err := reader.ReadMetadata(ctx, s.src, s.logins)
	if err != nil {
		return followUpResult{}, err
	}
	if current.SnapshotID != r.SnapshotID {
		return followUpResult{}, followUpFailure("PRECONDITION_FAILED", "Issue確認中にsnapshotが変化しました")
	}
	if err := s.checkRefs(ctx, p); err != nil {
		return followUpResult{}, err
	}
	body := fmt.Sprintf("%s\n\n元PR: %s\n元指摘: %s\n確認HEAD: %s\n理由: %s\n\n%s", in.Body, p.URL, ref.URL, in.ExpectedHeadSHA, in.Reason, marker)
	created, err := s.src.PostIssue(ctx, in.Title, body, in.Labels)
	resource := &followUpResource{ID: created.ID, Number: created.Number, URL: created.URL}
	if err != nil || created.Number <= 0 || created.ID == "" {
		return followUpResult{}, postFollowUpError("WRITE_OUTCOME_UNKNOWN", resource)
	}
	actual, err := s.readIssue(ctx, p, created.Number)
	if err != nil {
		return followUpResult{}, postFollowUpError("WRITE_OUTCOME_UNKNOWN", resource)
	}
	wantLabels := slices.Clone(in.Labels)
	gotLabels := slices.Clone(actual.Labels)
	slices.Sort(wantLabels)
	slices.Sort(gotLabels)
	if actual.ID != created.ID || actual.URL != created.URL || actual.State != "OPEN" || actual.Title != in.Title || actual.Body != body || !slices.Equal(wantLabels, gotLabels) || actual.Author == nil || !sameFollowUpBot(*actual.Author, bot) || s.checkRefs(ctx, p) != nil {
		return followUpResult{}, postFollowUpError("POST_WRITE_MISMATCH", resource)
	}
	out := followUpOutput(r, p, ref, in.ExpectedHeadSHA)
	out.Issue = &actual
	return out, nil
}
func (s followUpService) getIssue(ctx context.Context, in followUpReadInput) (followUpResult, error) {
	if err := in.validate(true); err != nil {
		return followUpResult{}, err
	}
	if in.IssueNumber <= 0 || in.IssueNumber > math.MaxInt32 {
		return followUpResult{}, followUpFailure("INVALID_INPUT", "正のIssue番号を指定してください")
	}
	r, p, ref, err := s.prepare(ctx, in.followUpTarget, "")
	if err != nil {
		return followUpResult{}, err
	}
	i, err := s.readIssue(ctx, p, in.IssueNumber)
	if err != nil {
		return followUpResult{}, err
	}
	if err := s.checkRefs(ctx, p); err != nil {
		return followUpResult{}, followUpFailure("SNAPSHOT_CHANGED", "Issue読取中にPR参照が変化しました")
	}
	out := followUpOutput(r, p, ref, in.ExpectedHeadSHA)
	out.Status = "read"
	out.Issue = &i
	return out, nil
}

func renderFollowUp(out followUpResult, err error) (*mcp.CallToolResult, any, error) {
	if err == nil {
		return nil, out, nil
	}
	var failure *followUpError
	if !errors.As(err, &failure) {
		code := "READ_FAILED"
		var gate *reader.Error
		if errors.As(err, &gate) {
			code = gate.Code
		} else if errors.Is(err, reader.ErrProjectAllowlistInvalid) {
			code = reader.CodeProjectAllowlistInvalid
		} else if errors.Is(err, reader.ErrPolicyRefUnavailable) {
			code = reader.CodePolicyRefUnavailable
		} else if classified, ok := autherr.AsAuthError(err); ok {
			code = string(classified.ErrorType)
		} else if classified := ghclient.ClassifyGitHubError(err); classified != nil {
			code = string(classified.ErrorType)
		}
		failure = &followUpError{Code: code, Message: "投稿前の認証・読取・照合に失敗しました。書込みは行っていません"}
	}
	return &mcp.CallToolResult{IsError: true}, followUpErrorEnvelope{Error: failure}, nil
}

// RegisterFollowUpToolsは専用Appだけに登録し、Issue toolは権限要求と同じopt-inを使う。
func RegisterFollowUpTools(server *mcp.Server, provider githubClientProvider, logins []string, appLogin string, issues bool) {
	logins = slices.Clone(logins)
	service := func(ctx context.Context, req *mcp.CallToolRequest, target ghclient.DescriptionTarget) (followUpService, error) {
		gh, err := provider(ctx, req)
		if err != nil {
			return followUpService{}, err
		}
		return followUpService{src: &githubFollowUpSource{&githubCommentSource{gh: gh, owner: target.Owner, repo: target.Repo, pr: target.PR}}, logins: logins, appLogin: appLogin}, nil
	}
	tool := &mcp.Tool{Name: "reply_to_pr_comment", Description: "reviewed側専用Appで、reader v2の元指摘(kind/id)と期待HEAD/snapshotを照合し、PR conversationへ返信を1回投稿する。thread返信は既存APIを使う。全件投稿者検証・App identity・本文読戻しが成功条件。同じ元指摘へのApp返信があれば停止。mayHaveChanged=trueでは自動再投稿・別認証・rollbackを行わずreadで照合する。source.urlは指定不可。"}
	mcp.AddTool(server, withOutputSchema(tool, outputOrErrorOf[followUpResult, followUpErrorEnvelope]()), func(ctx context.Context, req *mcp.CallToolRequest, in followUpReplyInput) (*mcp.CallToolResult, any, error) {
		if err := in.validate(false); err != nil {
			return renderFollowUp(followUpResult{}, err)
		}
		s, err := service(ctx, req, in.DescriptionTarget)
		if err != nil {
			return renderFollowUp(followUpResult{}, err)
		}
		out, err := s.reply(ctx, in)
		return renderFollowUp(out, err)
	})
	if !issues {
		return
	}
	tool = &mcp.Tool{Name: "create_follow_up_issue", Description: "reviewed側専用Appで同repoの元指摘にfollow-up Issueを1件作成する。OPEN・期待HEAD/snapshot・全件投稿者・元指摘所属を照合し、既存App Issue候補があれば停止。Issues write opt-in必須。POST後は本文/labels/App identityを読戻す。検索遅延による重複は排他できない。同じ元指摘は直列実行し、結果不明時は利用者の再投稿許可を待つ。返信・resolveは別操作。"}
	mcp.AddTool(server, withOutputSchema(tool, outputOrErrorOf[followUpResult, followUpErrorEnvelope]()), func(ctx context.Context, req *mcp.CallToolRequest, in followUpIssueInput) (*mcp.CallToolResult, any, error) {
		if err := in.validate(); err != nil {
			return renderFollowUp(followUpResult{}, err)
		}
		s, err := service(ctx, req, in.DescriptionTarget)
		if err != nil {
			return renderFollowUp(followUpResult{}, err)
		}
		out, err := s.createIssue(ctx, in)
		return renderFollowUp(out, err)
	})
	tool = &mcp.Tool{Name: "get_follow_up_issue", Description: "同repoの通常Issueを投稿者・最終編集者のmetadata検証後に読み、取得中の編集・PR参照変化は停止する。既存Issueの意味上の関連性はcallerが判断。PRがclosed/HEAD移動済みでも照合用に返し、matchesExpectedHeadとprStateを必ず確認する。通常cycle再開を許可するAPIではない。"}
	mcp.AddTool(server, withOutputSchema(tool, outputOrErrorOf[followUpResult, followUpErrorEnvelope]()), func(ctx context.Context, req *mcp.CallToolRequest, in followUpReadInput) (*mcp.CallToolResult, any, error) {
		if err := in.validate(true); err != nil {
			return renderFollowUp(followUpResult{}, err)
		}
		if in.IssueNumber <= 0 || in.IssueNumber > math.MaxInt32 {
			return renderFollowUp(followUpResult{}, followUpFailure("INVALID_INPUT", "Issue番号が不正です"))
		}
		s, err := service(ctx, req, in.DescriptionTarget)
		if err != nil {
			return renderFollowUp(followUpResult{}, err)
		}
		out, err := s.getIssue(ctx, in)
		return renderFollowUp(out, err)
	})
}
