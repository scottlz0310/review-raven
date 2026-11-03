package ghclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFollowUpIssueProjectionAndPagination(t *testing.T) {
	for _, mode := range []string{"metadata", "body", "missing-issue", "label-page-change"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request gqlRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				body := mode == "body"
				if request.Variables["withBody"] != body || !strings.Contains(request.Query, "title: title @include(if:$withBody)") || !strings.Contains(request.Query, "body: body @include(if:$withBody)") || !strings.Contains(request.Query, "editor{") {
					t.Fatalf("projectionが不正: %+v", request)
				}
				var issue any
				if mode != "missing-issue" {
					i := map[string]any{"number": 7, "id": "issue-node", "url": "https://github.com/o/r/issues/7", "state": "OPEN", "author": descriptionActorFixture("Bot", "raven", "bot-id"), "editor": nil, "lastEditedAt": nil,
						"labels": map[string]any{"nodes": []map[string]string{{"name": "bug"}}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": "end"}}}
					if mode == "label-page-change" {
						if calls == 1 {
							i["labels"] = map[string]any{"nodes": []map[string]string{{"name": "bug"}}, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": "next"}}
						} else {
							i["state"] = "CLOSED"
						}
					}
					if body {
						i["title"] = "追跡"
						i["body"] = "未加工\r\n本文"
					}
					issue = i
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"issue": issue}}})
			}))
			defer srv.Close()
			c, err := NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			i, err := c.GetFollowUpIssue(context.Background(), DescriptionTarget{Owner: "o", Repo: "r", PR: 1}, 7, mode == "body")
			if mode == "missing-issue" || mode == "label-page-change" {
				if err == nil {
					t.Fatal("不正なIssueを受理しました")
				}
				return
			}
			if err != nil || i.Author.NodeID != "bot-id" || len(i.Labels) != 1 {
				t.Fatalf("issue=%+v err=%v", i, err)
			}
			if mode == "body" && (i.Body != "未加工\r\n本文" || i.BodySHA256 != descriptionHash(i.Body)) {
				t.Fatal("本文hash不一致")
			}
			if mode == "metadata" && (i.Body != "" || i.Title != "") {
				t.Fatal("metadataで本文取得")
			}
		})
	}
}

func TestFollowUpSearchCompleteness(t *testing.T) {
	for _, mode := range []string{"two-pages", "over-limit", "incomplete", "wrong-repo", "count-changed"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request gqlRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(request.Query, "title") || strings.Contains(request.Query, "body") || !strings.Contains(request.Variables["query"].(string), "repo:o/r is:issue in:body") {
					t.Fatalf("本文なし検索の契約違反: %+v", request)
				}
				count := 2
				next := calls == 1
				repo := "o/r"
				number := calls + 6
				switch mode {
				case "over-limit":
					count = 1001
				case "incomplete":
					next = false
				case "wrong-repo":
					repo = "o/other"
				case "count-changed":
					if calls > 1 {
						count = 3
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"search": map[string]any{"issueCount": count, "nodes": []any{map[string]any{"number": number, "repository": map[string]any{"nameWithOwner": repo}}}, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": "cursor"}}}})
			}))
			defer srv.Close()
			c, err := NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			numbers, err := c.FindFollowUpIssues(context.Background(), DescriptionTarget{Owner: "o", Repo: "r", PR: 1}, "Review-Raven-Source: o/r#1 issue_comment:3")
			if mode == "two-pages" {
				if err != nil || len(numbers) != 2 || calls != 2 {
					t.Fatalf("numbers=%v err=%v calls=%d", numbers, err, calls)
				}
			} else if err == nil {
				t.Fatal("不完全な検索を受理しました")
			}
		})
	}
}

func TestFollowUpRESTSingleWrites(t *testing.T) {
	for _, status := range []int{201, 401, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					t.Fatalf("想定外の操作: %s", r.Method)
				}
				posts++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["body"] != "未加工\r\n本文" {
					t.Fatalf("本文が変化しました: %v", body)
				}
				w.WriteHeader(status)
				if status != 201 {
					return
				}
				if strings.HasSuffix(r.URL.Path, "/comments") {
					_, _ = w.Write([]byte(`{"id":4,"html_url":"https://github.com/o/r/pull/1#issuecomment-4","body":"未加工\r\n本文","user":{"node_id":"bot-id","login":"raven[bot]","type":"Bot"}}`))
				} else {
					if len(body) != 3 || body["title"] != "追跡" {
						t.Fatalf("余分なIssue属性: %v", body)
					}
					_, _ = w.Write([]byte(`{"number":7,"node_id":"issue-id","html_url":"https://github.com/o/r/issues/7"}`))
				}
			}))
			defer srv.Close()
			c, err := NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			target := DescriptionTarget{Owner: "o", Repo: "r", PR: 1}
			comment, err := c.CreateFollowUpReply(context.Background(), target, "未加工\r\n本文")
			if (err == nil) != (status == 201) || posts != 1 {
				t.Fatalf("err=%v posts=%d", err, posts)
			}
			if status == 201 && (comment.Author.NodeID != "bot-id" || comment.BodySHA256 != descriptionHash(comment.Body)) {
				t.Fatal("返信identity/hash不一致")
			}
			_, err = c.CreateFollowUpIssue(context.Background(), target, "追跡", "未加工\r\n本文", []string{"bug"})
			if (err == nil) != (status == 201) || posts != 2 {
				t.Fatalf("err=%v posts=%d", err, posts)
			}
		})
	}
}
