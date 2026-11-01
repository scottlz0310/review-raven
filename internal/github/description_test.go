package ghclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func descriptionFixture(body any) map[string]any {
	return map[string]any{
		"url": "https://github.com/owner/repo/pull/1", "state": "OPEN", "headRefOid": headA,
		"body": body, "author": descriptionActorFixture("User", "creator", "author-id"),
		"editor": nil, "lastEditedAt": nil,
	}
}

func descriptionActorFixture(kind, login, id string) map[string]any {
	return map[string]any{"__typename": kind, "login": login, "id": id}
}

type descriptionAPIFixture struct {
	before, after        map[string]any
	bot                  map[string]any
	readStatus           int
	botStatus            int
	patchStatus          int
	afterStatus          int
	reads, bots, patches int
}

func (f *descriptionAPIFixture) client(t *testing.T, body string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/" && r.Method == http.MethodPost:
			f.reads++
			var request gqlRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if !strings.Contains(request.Query, "editor{") || !strings.Contains(request.Query, "... on Node") ||
				request.Variables["owner"] != "owner" || request.Variables["repo"] != "repo" || request.Variables["number"] != float64(1) {
				t.Errorf("PRメタデータの取得クエリが不正です: %+v", request)
			}
			pr, status := f.before, f.readStatus
			if f.reads > 1 {
				pr, status = f.after, f.afterStatus
			}
			if status != 0 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"機密本文を含む失敗"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pr}}})
		case r.URL.Path == "/users/verified-app[bot]" && r.Method == http.MethodGet:
			f.bots++
			if f.botStatus != 0 {
				w.WriteHeader(f.botStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(f.bot)
		case r.URL.Path == "/repos/owner/repo/pulls/1" && r.Method == http.MethodPatch:
			f.patches++
			var patch map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Error(err)
			}
			if !reflect.DeepEqual(patch, map[string]any{"body": body}) {
				t.Errorf("本文以外の更新・正規化が発生しました: %+v", patch)
			}
			if f.patchStatus == -1 {
				_, _ = w.Write([]byte("{"))
				return
			}
			if f.patchStatus != 0 {
				w.WriteHeader(f.patchStatus)
				_, _ = w.Write([]byte(`{"message":"機密本文を含む失敗"}`))
				return
			}
			_, _ = w.Write([]byte("{}"))
		default:
			t.Errorf("想定外のGitHub操作: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewWithHTTPClientAndURLFull(server.Client(), server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestGetPRDescription(t *testing.T) {
	for _, tc := range []struct {
		name string
		body any
		want string
	}{
		{"null", nil, ""},
		{"空本文", "", ""},
		{"未加工", "日本語\r\n😀 e\u0301  \n", "日本語\r\n😀 e\u0301  \n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := descriptionAPIFixture{before: descriptionFixture(tc.body)}
			f.before["author"] = nil
			out, err := f.client(t, "").GetPRDescription(context.Background(), DescriptionTarget{"owner", "repo", 1})
			if err != nil || out.Body != tc.want || out.BodySHA256 != descriptionHash(tc.want) ||
				out.OriginalAuthor != nil || out.LastEditor != nil || out.LastEditedAt != nil {
				t.Fatalf("取得結果=%+v err=%v", out, err)
			}
			if f.patches != 0 || f.bots != 0 {
				t.Fatal("読取で更新処理を実行しました")
			}
		})
	}
	if descriptionHash("a\r\n") == descriptionHash("a\n") || descriptionHash("é") == descriptionHash("e\u0301") ||
		descriptionHash("") != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal("未加工UTF-8のSHA-256契約に違反しています")
	}
}

func TestUpdatePRDescription(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*descriptionAPIFixture, *DescriptionUpdate, *string)
		code    string
		changed bool
		patches int
		status  string
	}{
		{name: "更新", patches: 1, status: "updated"},
		{name: "GraphQLのsuffixなしBot", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) {
			f.after["editor"] = descriptionActorFixture("Bot", "verified-app", "bot-id")
		}, patches: 1, status: "updated"},
		{name: "suffixなし同名別Node", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) {
			f.after["editor"] = descriptionActorFixture("Bot", "verified-app", "other-id")
		}, code: "POST_UPDATE_MISMATCH", changed: true, patches: 1},
		{name: "suffix重複は拒否", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) {
			f.after["editor"] = descriptionActorFixture("Bot", "verified-app[bot][bot]", "bot-id")
		}, code: "POST_UPDATE_MISMATCH", changed: true, patches: 1},
		{name: "空本文へ置換", change: func(f *descriptionAPIFixture, in *DescriptionUpdate, _ *string) { in.Body = ""; f.after["body"] = "" }, patches: 1, status: "updated"},
		{name: "同一本文・編集者不明", change: func(_ *descriptionAPIFixture, in *DescriptionUpdate, _ *string) { in.Body = "old" }, status: "unchanged"},
		{name: "CLOSED", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.before["state"] = "CLOSED" }, code: "PRECONDITION_FAILED"},
		{name: "MERGED", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.before["state"] = "MERGED" }, code: "PRECONDITION_FAILED"},
		{name: "HEAD移動", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.before["headRefOid"] = headB }, code: "PRECONDITION_FAILED"},
		{name: "本文競合", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.before["body"] = "other" }, code: "PRECONDITION_FAILED"},
		{name: "取得失敗", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.readStatus = 500 }, code: "TRANSIENT_UPSTREAM_ERROR"},
		{name: "取得の認証拒否", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.readStatus = 401 }, code: "REAUTH_REQUIRED"},
		{name: "取得の認可拒否", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.readStatus = 403 }, code: "PERMISSION_DENIED"},
		{name: "PRなし", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.before = nil }, code: "READ_FAILED"},
		{name: "不完全actor", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) {
			f.before["author"] = descriptionActorFixture("User", "creator", "")
		}, code: "READ_FAILED"},
		{name: "認証App不明", change: func(_ *descriptionAPIFixture, _ *DescriptionUpdate, login *string) { *login = "" }, code: "APP_IDENTITY_FAILED"},
		{name: "Bot取得拒否", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.botStatus = 403 }, code: "PERMISSION_DENIED"},
		{name: "Botなし", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.bot = nil }, code: "APP_IDENTITY_FAILED"},
		{name: "人間identity", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.bot["type"] = "User" }, code: "APP_IDENTITY_FAILED"},
		{name: "異なるBot", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.bot["login"] = "other[bot]" }, code: "APP_IDENTITY_FAILED"},
		{name: "NodeIDなし", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { delete(f.bot, "node_id") }, code: "APP_IDENTITY_FAILED"},
		{name: "PATCH応答不明", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.patchStatus = 500 }, code: "UPDATE_OUTCOME_UNKNOWN", changed: true, patches: 1},
		{name: "PATCH成功後の応答欠損", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.patchStatus = -1 }, code: "UPDATE_OUTCOME_UNKNOWN", changed: true, patches: 1},
		{name: "PATCH認可拒否も再試行しない", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.patchStatus = 403 }, code: "UPDATE_OUTCOME_UNKNOWN", changed: true, patches: 1},
		{name: "読戻し失敗", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.afterStatus = 500 }, code: "UPDATE_OUTCOME_UNKNOWN", changed: true, patches: 1},
		{name: "読戻し認証拒否も結果不明", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.afterStatus = 401 }, code: "UPDATE_OUTCOME_UNKNOWN", changed: true, patches: 1},
		{name: "読戻し本文違い", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.after["body"] = "other" }, code: "POST_UPDATE_MISMATCH", changed: true, patches: 1},
		{name: "読戻しHEAD違い", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.after["headRefOid"] = headB }, code: "POST_UPDATE_MISMATCH", changed: true, patches: 1},
		{name: "更新後close", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.after["state"] = "CLOSED" }, code: "POST_UPDATE_MISMATCH", changed: true, patches: 1},
		{name: "編集者不明", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.after["editor"] = nil }, code: "POST_UPDATE_MISMATCH", changed: true, patches: 1},
		{name: "同名別Node", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) {
			f.after["editor"] = descriptionActorFixture("Bot", "verified-app[bot]", "other-id")
		}, code: "POST_UPDATE_MISMATCH", changed: true, patches: 1},
		{name: "同Node別login", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) {
			f.after["editor"] = descriptionActorFixture("Bot", "other[bot]", "bot-id")
		}, code: "POST_UPDATE_MISMATCH", changed: true, patches: 1},
		{name: "同Node人間", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) {
			f.after["editor"] = descriptionActorFixture("User", "verified-app[bot]", "bot-id")
		}, code: "POST_UPDATE_MISMATCH", changed: true, patches: 1},
		{name: "作成者変更", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) { f.after["author"] = nil }, code: "POST_UPDATE_MISMATCH", changed: true, patches: 1},
		{name: "両作成者null", change: func(f *descriptionAPIFixture, _ *DescriptionUpdate, _ *string) {
			f.before["author"] = nil
			f.after["author"] = nil
		}, patches: 1, status: "updated"},
		{name: "不正HEAD入力", change: func(_ *descriptionAPIFixture, in *DescriptionUpdate, _ *string) { in.ExpectedHeadSHA = "short" }, code: "INVALID_INPUT"},
		{name: "大文字hash", change: func(_ *descriptionAPIFixture, in *DescriptionUpdate, _ *string) {
			in.ExpectedBodySHA256 = strings.Repeat("A", 64)
		}, code: "INVALID_INPUT"},
		{name: "パス入力", change: func(_ *descriptionAPIFixture, in *DescriptionUpdate, _ *string) { in.Repo = "../repo" }, code: "INVALID_INPUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := DescriptionUpdate{DescriptionTarget: DescriptionTarget{"owner", "repo", 1}, ExpectedHeadSHA: headA, ExpectedBodySHA256: descriptionHash("old"), Body: "新本文\r\n😀  "}
			login := "verified-app[bot]"
			f := descriptionAPIFixture{before: descriptionFixture("old"), after: descriptionFixture(in.Body),
				bot: map[string]any{"login": login, "type": "Bot", "node_id": "bot-id"}}
			f.after["editor"] = descriptionActorFixture("Bot", login, "bot-id")
			f.after["lastEditedAt"] = "2026-11-02T00:00:00Z"
			if tc.change != nil {
				tc.change(&f, &in, &login)
			}
			out, err := f.client(t, in.Body).UpdatePRDescription(context.Background(), in, login)
			if tc.code == "" {
				if err != nil || out.Status != tc.status || out.Body != in.Body || out.BodySHA256 != descriptionHash(in.Body) {
					t.Fatalf("更新結果=%+v err=%v", out, err)
				}
				if out.Status == "updated" && (out.LastEditor == nil || out.LastEditedAt == nil) {
					t.Fatal("更新identity・時刻がありません")
				}
				if out.Status == "unchanged" && out.LastEditor != nil {
					t.Fatal("unchangedをApp編集に偽装しました")
				}
			} else {
				var failure *DescriptionError
				if !errors.As(err, &failure) || failure.Code != tc.code || failure.MayHaveChanged != tc.changed {
					t.Fatalf("失敗分類=%+v err=%v", failure, err)
				}
				if strings.Contains(err.Error(), "機密本文") {
					t.Fatal("失敗に応答本文を漏洩しました")
				}
			}
			if f.patches != tc.patches {
				t.Fatalf("PATCH回数=%d、期待=%d", f.patches, tc.patches)
			}
			if tc.code == "INVALID_INPUT" && f.reads != 0 {
				t.Fatal("入力検証前にAPIを呼びました")
			}
			if tc.status == "unchanged" && f.bots != 0 {
				t.Fatal("同一本文で編集identityを要求しました")
			}
			if f.patchStatus != 0 && f.reads != 1 {
				t.Fatal("結果不明のPATCH後に処理を続行しました")
			}
		})
	}
}
