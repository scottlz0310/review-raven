package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottlz0310/review-raven/internal/reviewrequest"
)

const contractFixtureDir = "../../docs/design/contracts/fixtures"

// contractIDs yields the identifiers used by the contract fixtures, so that
// fixture inputs that name a cycle or a cancellation refer to what the ledger
// actually created.
func contractIDs(prefix string) string {
	switch prefix {
	case "cyc":
		return "cyc_01jbq3x8n5r7t9v2w4y6z8a0c1"
	case "cxl":
		return "cxl_01jbq3x8n5r7t9v2w4y6z8a0e1"
	}
	return prefix + "_01jbq3x8n5r7t9v2w4y6z8a0z9"
}

func contractFixture(t *testing.T, schema, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractFixtureDir, schema, "valid", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	return args
}

// 契約 v1 の入力 schema に適合する引数は、そのまま受理されなければならない。
// （schemaVersion は契約で必須。過去に tool の入力型がこれを持たず、契約どおりの
// クライアントが全 tool で拒否された。）
func TestReviewRequestAcceptsContractInputFixtures(t *testing.T) {
	f := newRequestFixtureWith(t, true, func(d *reviewrequest.Deps) { d.NewID = contractIDs })
	alice := f.connect("alice")

	created := ok(t, call(t, alice, "request_review", contractFixture(t, "request_review.input", "opened")), "request_review.output")
	if created["cycleId"] != contractIDs("cyc") || created["round"] != float64(1) {
		t.Fatalf("unexpected output for the contract fixture: %+v", created)
	}

	snap := ok(t, call(t, alice, "get_review_request", contractFixture(t, "get_review_request.input", "cycle")), "snapshot")
	if snap["cycleId"] != contractIDs("cyc") {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	// 契約 fixture の expectedSnapshotRevision(3) は実際の revision(1) と違うため、引数は受理された上で
	// 意味的な競合として契約のエラー envelope で返る（プロトコル層の拒否ではない）。
	failed(t, call(t, alice, "cancel_review_request", contractFixture(t, "cancel_review_request.input", "basic")), "REVISION_CONFLICT")

	t.Run("every valid input fixture reaches the handler", func(t *testing.T) {
		for _, schema := range []string{"request_review.input", "get_review_request.input", "cancel_review_request.input"} {
			tool := schema[:len(schema)-len(".input")]
			files, err := filepath.Glob(filepath.Join(contractFixtureDir, schema, "valid", "*.json"))
			if err != nil || len(files) == 0 {
				t.Fatalf("no valid fixtures for %s: %v", schema, err)
			}
			for _, file := range files {
				name := filepath.Base(file)
				name = name[:len(name)-len(".json")]
				t.Run(tool+"/"+name, func(t *testing.T) {
					result, err := alice.CallTool(context.Background(), &mcp.CallToolParams{
						Name: tool, Arguments: contractFixture(t, schema, name),
					})
					if err != nil {
						t.Fatalf("a contract-valid input was rejected at the protocol layer: %v", err)
					}
					if result.IsError {
						// 意味上の失敗（HEAD の競合など）は、契約のエラー envelope でなければならない。
						envelope, isMap := result.StructuredContent.(map[string]any)
						if !isMap {
							t.Fatalf("an error without a structured envelope: %+v", result)
						}
						mustContract(t, "error", envelope)
					}
				})
			}
		}
	})
}

func TestReviewRequestEnforcesSchemaVersion(t *testing.T) {
	f := newRequestFixture(t, true)
	alice := f.connect("alice")
	before := f.cycleCount()

	t.Run("an unsupported version is an explicit INVALID_ARGUMENT", func(t *testing.T) {
		for tool, args := range map[string]map[string]any{
			"request_review":        requestArgs("agent-req-0001", rrHeadA),
			"get_review_request":    getArgs("cyc_01jbq3x8n5r7t9v2w4y6z8a0c1"),
			"cancel_review_request": {"cycleId": "cyc_01jbq3x8n5r7t9v2w4y6z8a0c1", "expectedHeadSha": rrHeadA, "expectedSnapshotRevision": 1, "idempotencyKey": "agent-cancel-0001"},
		} {
			args["schemaVersion"] = 2
			failed(t, call(t, alice, tool, args), "INVALID_ARGUMENT")
		}
	})

	t.Run("a missing version is rejected before the handler", func(t *testing.T) {
		args := requestArgs("agent-req-0002", rrHeadA)
		delete(args, "schemaVersion")
		result, err := alice.CallTool(context.Background(), &mcp.CallToolParams{Name: "request_review", Arguments: args})
		if err == nil && (result == nil || !result.IsError) {
			t.Fatalf("an input without schemaVersion must be rejected: %+v", result)
		}
	})

	if after := f.cycleCount(); after != before {
		t.Fatalf("a rejected call changed the ledger: %d -> %d", before, after)
	}
}
