package ghclient

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/go-github/v93/github"
	"github.com/shurcooL/githubv4"
)

type DescriptionActor struct {
	NodeID string `json:"nodeId"`
	Login  string `json:"login"`
	Type   string `json:"type"`
}

type PRDescription struct {
	URL            string            `json:"url"`
	State          string            `json:"state"`
	HeadSHA        string            `json:"headSha"`
	Body           string            `json:"body"`
	BodySHA256     string            `json:"bodySha256"`
	OriginalAuthor *DescriptionActor `json:"originalAuthor"`
	LastEditor     *DescriptionActor `json:"lastEditor"`
	LastEditedAt   *time.Time        `json:"lastEditedAt"`
}

type DescriptionTarget struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	PR    int    `json:"pr"`
}

type DescriptionUpdate struct {
	DescriptionTarget
	ExpectedHeadSHA    string `json:"expectedHeadSha"`
	ExpectedBodySHA256 string `json:"expectedBodySha256"`
	Body               string `json:"body"`
}

type DescriptionUpdateResult struct {
	PRDescription
	Status string `json:"status"`
}

// PATCH開始後の失敗を、再試行可能な「書込みなし」の失敗と混同させない。
type DescriptionError struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	MayHaveChanged bool   `json:"mayHaveChanged"`
	cause          error
}

func (e *DescriptionError) Error() string { return e.Code + ": " + e.Message }
func (e *DescriptionError) Unwrap() error { return e.cause }

func descriptionError(code, message string, changed bool, cause error) *DescriptionError {
	return &DescriptionError{Code: code, Message: message, MayHaveChanged: changed, cause: cause}
}

var descriptionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var descriptionHeadPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var descriptionHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (in DescriptionTarget) Validate() error {
	for _, name := range []string{in.Owner, in.Repo} {
		if !descriptionNamePattern.MatchString(name) || name == "." || name == ".." {
			return descriptionError("INVALID_INPUT", "ownerとrepoにはGitHubの名前を指定してください", false, nil)
		}
	}
	if in.PR <= 0 {
		return descriptionError("INVALID_INPUT", "prには正の番号を指定してください", false, nil)
	}
	return nil
}

func (in DescriptionUpdate) Validate() error {
	if err := in.DescriptionTarget.Validate(); err != nil {
		return err
	}
	if !descriptionHeadPattern.MatchString(in.ExpectedHeadSHA) || !descriptionHashPattern.MatchString(in.ExpectedBodySHA256) || !utf8.ValidString(in.Body) {
		return descriptionError("INVALID_INPUT", "完全な小文字HEAD SHA・本文SHA-256・UTF-8本文を指定してください", false, nil)
	}
	return nil
}

func descriptionHash(body string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
}

type descriptionQueryActor struct {
	Login githubv4.String
	Type  githubv4.String `graphql:"__typename"`
	Node  struct {
		ID githubv4.String
	} `graphql:"... on Node"`
}

func descriptionActor(actor *descriptionQueryActor) (*DescriptionActor, error) {
	if actor == nil {
		return nil, nil
	}
	if actor.Node.ID == "" || actor.Login == "" || actor.Type == "" {
		return nil, fmt.Errorf("GitHubのactor識別情報が不完全です")
	}
	return &DescriptionActor{NodeID: string(actor.Node.ID), Login: string(actor.Login), Type: string(actor.Type)}, nil
}

func (c *Client) GetPRDescription(ctx context.Context, in DescriptionTarget) (PRDescription, error) {
	if err := in.Validate(); err != nil {
		return PRDescription{}, err
	}
	var query struct {
		Repository struct {
			PullRequest *struct {
				URL          githubv4.String
				State        githubv4.String
				HeadRefOID   githubv4.String
				Body         githubv4.String
				Author       *descriptionQueryActor
				Editor       *descriptionQueryActor
				LastEditedAt *githubv4.DateTime
			} `graphql:"pullRequest(number:$number)"`
		} `graphql:"repository(owner:$owner,name:$repo)"`
	}
	if err := c.v4.Query(ctx, &query, map[string]any{
		"owner": githubv4.String(in.Owner), "repo": githubv4.String(in.Repo), "number": githubv4.Int(in.PR),
	}); err != nil {
		code := "READ_FAILED"
		if classified := ClassifyGitHubError(err); classified != nil {
			code = string(classified.ErrorType)
		}
		return PRDescription{}, descriptionError(code, "PR説明の取得に失敗しました。書込みは行っていません", false, err)
	}
	pr := query.Repository.PullRequest
	if pr == nil || pr.URL == "" || !descriptionHeadPattern.MatchString(string(pr.HeadRefOID)) ||
		(pr.State != "OPEN" && pr.State != "CLOSED" && pr.State != "MERGED") {
		return PRDescription{}, descriptionError("READ_FAILED", "PR説明の識別情報が不完全です", false, nil)
	}
	author, authorErr := descriptionActor(pr.Author)
	editor, editorErr := descriptionActor(pr.Editor)
	if authorErr != nil || editorErr != nil {
		return PRDescription{}, descriptionError("READ_FAILED", "PRの作成者・編集者の識別情報が不完全です", false, nil)
	}
	out := PRDescription{URL: string(pr.URL), State: string(pr.State), HeadSHA: string(pr.HeadRefOID),
		Body: string(pr.Body), BodySHA256: descriptionHash(string(pr.Body)), OriginalAuthor: author, LastEditor: editor}
	if pr.LastEditedAt != nil {
		out.LastEditedAt = &pr.LastEditedAt.Time
	}
	return out, nil
}

func (c *Client) UpdatePRDescription(ctx context.Context, in DescriptionUpdate, appBotLogin string) (DescriptionUpdateResult, error) {
	if err := in.Validate(); err != nil {
		return DescriptionUpdateResult{}, err
	}
	before, err := c.GetPRDescription(ctx, in.DescriptionTarget)
	if err != nil {
		return DescriptionUpdateResult{}, err
	}
	if before.State != "OPEN" || before.HeadSHA != in.ExpectedHeadSHA || before.BodySHA256 != in.ExpectedBodySHA256 {
		return DescriptionUpdateResult{}, descriptionError("PRECONDITION_FAILED", "OPEN・期待HEAD・期待本文hashが一致しません。書込みは行っていません", false, nil)
	}
	if before.Body == in.Body {
		return DescriptionUpdateResult{PRDescription: before, Status: "unchanged"}, nil
	}
	if appBotLogin == "" {
		return DescriptionUpdateResult{}, descriptionError("APP_IDENTITY_FAILED", "認証済みAppのidentityがありません。書込みは行っていません", false, nil)
	}
	bot, _, err := c.gh.Users.Get(ctx, appBotLogin)
	if err != nil {
		code := "APP_IDENTITY_FAILED"
		if classified := ClassifyGitHubError(err); classified != nil {
			code = string(classified.ErrorType)
		}
		return DescriptionUpdateResult{}, descriptionError(code, "認証済みAppのBot取得に失敗しました。書込みは行っていません", false, err)
	}
	if bot.GetType() != "Bot" || !strings.EqualFold(bot.GetLogin(), appBotLogin) || bot.GetNodeID() == "" {
		return DescriptionUpdateResult{}, descriptionError("APP_IDENTITY_FAILED", "認証済みAppのBot Node IDを確認できません。書込みは行っていません", false, err)
	}
	// go-githubに再試行は指定しない。照合からPATCHまでの他者編集は排他できない。
	if _, _, err := c.gh.PullRequests.Edit(ctx, in.Owner, in.Repo, in.PR, &github.PullRequest{Body: &in.Body}); err != nil {
		return DescriptionUpdateResult{}, descriptionError("UPDATE_OUTCOME_UNKNOWN", "PATCHが完了したか不明です。現在値を取得し、再試行前に利用者へ確認してください", true, err)
	}
	after, err := c.GetPRDescription(ctx, in.DescriptionTarget)
	if err != nil {
		return DescriptionUpdateResult{}, descriptionError("UPDATE_OUTCOME_UNKNOWN", "PATCH後の読戻しに失敗しました。再試行前に現在値と利用者の判断を確認してください", true, err)
	}
	sameAuthor := before.OriginalAuthor == nil && after.OriginalAuthor == nil ||
		before.OriginalAuthor != nil && after.OriginalAuthor != nil && before.OriginalAuthor.NodeID == after.OriginalAuthor.NodeID
	if after.State != "OPEN" || after.HeadSHA != in.ExpectedHeadSHA || after.Body != in.Body || !sameAuthor ||
		after.LastEditor == nil || after.LastEditor.Type != "Bot" || after.LastEditor.NodeID != bot.GetNodeID() ||
		!strings.EqualFold(after.LastEditor.Login, bot.GetLogin()) {
		return DescriptionUpdateResult{}, descriptionError("POST_UPDATE_MISMATCH", "PATCH後の本文・HEAD・state・作成者・App編集者が一致しません。変更された可能性があるため現在値を確認してください", true, nil)
	}
	return DescriptionUpdateResult{PRDescription: after, Status: "updated"}, nil
}
