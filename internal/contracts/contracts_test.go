// Package contracts validates the review-request contract documents under
// docs/design/contracts (JSON Schemas, golden fixtures and shared scenarios).
//
// There is no production code here: the schemas are a design artifact that the
// R1 (review-raven) and O1 (thread-owl) implementations must both satisfy.
package contracts

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	contractsDir = "../../docs/design/contracts"
	baseURI      = "https://schemas.review-raven.example/contracts/v1/"
)

func readJSON(t *testing.T, file string) any {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return v
}

func loadSchemaFile(file string) (*jsonschema.Schema, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	return &s, nil
}

func resolveSchema(name string) (*jsonschema.Resolved, error) {
	root, err := loadSchemaFile(filepath.Join(contractsDir, "schemas", name+".schema.json"))
	if err != nil {
		return nil, err
	}
	return root.Resolve(&jsonschema.ResolveOptions{
		BaseURI: baseURI + name + ".schema.json",
		Loader: func(u *url.URL) (*jsonschema.Schema, error) {
			if !strings.HasPrefix(u.String(), baseURI) {
				return nil, fmt.Errorf("unexpected external $ref: %s", u)
			}
			return loadSchemaFile(filepath.Join(contractsDir, "schemas", path.Base(u.Path)))
		},
	})
}

func mustResolve(t *testing.T, name string) *jsonschema.Resolved {
	t.Helper()
	r, err := resolveSchema(name)
	if err != nil {
		t.Fatalf("resolve schema %s: %v", name, err)
	}
	return r
}

func schemaNames(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(contractsDir, "schemas", "*.schema.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no schemas found: %v", err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, strings.TrimSuffix(filepath.Base(f), ".schema.json"))
	}
	sort.Strings(names)
	return names
}

func TestSchemasResolve(t *testing.T) {
	for _, name := range schemaNames(t) {
		t.Run(name, func(t *testing.T) {
			mustResolve(t, name)
		})
	}
}

// patchOp is the subset of RFC 6902 used by invalid fixtures: each invalid
// fixture is a valid base plus a patch, so the single change that breaks the
// contract is explicit and the fixture cannot be invalid for an unrelated reason.
type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

type invalidFixture struct {
	Base  string    `json:"base"`
	Patch []patchOp `json:"patch"`
}

func unescapePointer(tok string) string {
	return strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
}

func applyPatch(doc any, ops []patchOp) (any, error) {
	for _, op := range ops {
		if !strings.HasPrefix(op.Path, "/") {
			return nil, fmt.Errorf("patch path must start with '/': %q", op.Path)
		}
		toks := strings.Split(op.Path[1:], "/")
		for i := range toks {
			toks[i] = unescapePointer(toks[i])
		}
		parent := doc
		for _, tok := range toks[:len(toks)-1] {
			next, err := child(parent, tok)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", op.Path, err)
			}
			parent = next
		}
		last := toks[len(toks)-1]
		switch p := parent.(type) {
		case map[string]any:
			_, exists := p[last]
			switch op.Op {
			case "remove":
				if !exists {
					return nil, fmt.Errorf("remove: %s does not exist", op.Path)
				}
				delete(p, last)
			case "replace":
				if !exists {
					return nil, fmt.Errorf("replace: %s does not exist", op.Path)
				}
				p[last] = op.Value
			case "add":
				p[last] = op.Value
			default:
				return nil, fmt.Errorf("unsupported op %q", op.Op)
			}
		case []any:
			idx, err := strconv.Atoi(last)
			if err != nil || idx < 0 || idx >= len(p) || op.Op != "replace" {
				return nil, fmt.Errorf("array patch supports replace of an existing index only: %s", op.Path)
			}
			p[idx] = op.Value
		default:
			return nil, fmt.Errorf("%s: parent is not a container", op.Path)
		}
	}
	return doc, nil
}

func child(node any, tok string) (any, error) {
	switch n := node.(type) {
	case map[string]any:
		v, ok := n[tok]
		if !ok {
			return nil, fmt.Errorf("missing key %q", tok)
		}
		return v, nil
	case []any:
		idx, err := strconv.Atoi(tok)
		if err != nil || idx < 0 || idx >= len(n) {
			return nil, fmt.Errorf("bad index %q", tok)
		}
		return n[idx], nil
	default:
		return nil, fmt.Errorf("cannot descend into %T at %q", node, tok)
	}
}

func jsonFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func TestFixtures(t *testing.T) {
	for _, name := range schemaNames(t) {
		if name == "common" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			resolved := mustResolve(t, name)
			validDir := filepath.Join(contractsDir, "fixtures", name, "valid")
			invalidDir := filepath.Join(contractsDir, "fixtures", name, "invalid")

			valid := jsonFiles(t, validDir)
			invalid := jsonFiles(t, invalidDir)
			if len(valid) == 0 || len(invalid) == 0 {
				t.Fatalf("%s needs at least one valid and one invalid fixture (valid=%d invalid=%d)", name, len(valid), len(invalid))
			}

			for _, f := range valid {
				t.Run("valid/"+strings.TrimSuffix(filepath.Base(f), ".json"), func(t *testing.T) {
					if err := resolved.Validate(readJSON(t, f)); err != nil {
						t.Fatalf("valid fixture rejected: %v", err)
					}
				})
			}

			for _, f := range invalid {
				t.Run("invalid/"+strings.TrimSuffix(filepath.Base(f), ".json"), func(t *testing.T) {
					var fx invalidFixture
					raw, err := os.ReadFile(f)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(raw, &fx); err != nil || fx.Base == "" || len(fx.Patch) == 0 {
						t.Fatalf("invalid fixture must be {base, patch}: err=%v base=%q patch=%d", err, fx.Base, len(fx.Patch))
					}
					// base は valid/ の実在ファイル。パッチ前の base が受理されることで、不適合の原因をパッチに限定する。
					doc := readJSON(t, filepath.Join(validDir, fx.Base+".json"))
					if err := resolved.Validate(doc); err != nil {
						t.Fatalf("base %q must be valid: %v", fx.Base, err)
					}
					patched, err := applyPatch(readJSON(t, filepath.Join(validDir, fx.Base+".json")), fx.Patch)
					if err != nil {
						t.Fatalf("apply patch: %v", err)
					}
					if err := resolved.Validate(patched); err == nil {
						t.Fatal("patched document must be rejected, but was accepted")
					}
				})
			}
		})
	}
}

// --- scenarios ---

type scenarioStep struct {
	Actor  string          `json:"actor"`
	Op     string          `json:"op"`
	Event  string          `json:"event"`
	Input  json.RawMessage `json:"input"`
	Expect struct {
		Error    string          `json:"error"`
		Output   json.RawMessage `json:"output"`
		Snapshot map[string]any  `json:"snapshot"`
	} `json:"expect"`
}

type scenario struct {
	ID          string         `json:"id"`
	Description string         `json:"description"`
	Covers      string         `json:"covers"`
	Steps       []scenarioStep `json:"steps"`
}

var operations = map[string]string{
	"request_review":             "request_review.output",
	"get_review_request":         "snapshot",
	"cancel_review_request":      "cancel_review_request.output",
	"accept_review_candidate":    "accept_review_candidate.output",
	"cancel_review_candidate":    "cancel_review_candidate.output",
	"ingest_review_candidate":    "ingest_review_candidate.output",
	"publish_review_result":      "publish_review_result.output",
	"get_review_result":          "get_review_result.output",
	"publish_review_observation": "publish_review_observation.output",
}

var actors = map[string]bool{"agent": true, "raven": true, "owl": true, "squirrel": true, "github": true}

func loadScenarios(t *testing.T) []scenario {
	t.Helper()
	var all []scenario
	for _, f := range jsonFiles(t, filepath.Join(contractsDir, "scenarios")) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var list []scenario
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		all = append(all, list...)
	}
	if len(all) == 0 {
		t.Fatal("no scenarios found")
	}
	return all
}

// schemaHasPath reports whether a dotted path (e.g. "dispatch.state") names a
// property reachable in the snapshot schema, following $ref into common.
func schemaHasPath(t *testing.T, node map[string]any, dotted string) bool {
	t.Helper()
	cur := node
	for seg := range strings.SplitSeq(dotted, ".") {
		next, ok := property(t, cur, seg)
		if !ok {
			return false
		}
		cur = next
	}
	return true
}

func property(t *testing.T, node map[string]any, name string) (map[string]any, bool) {
	t.Helper()
	if props, ok := node["properties"].(map[string]any); ok {
		if p, ok := props[name].(map[string]any); ok {
			return p, true
		}
	}
	if ref, ok := node["$ref"].(string); ok {
		if def, ok := commonDef(t, ref); ok {
			return property(t, def, name)
		}
	}
	if anyOf, ok := node["anyOf"].([]any); ok {
		for _, alt := range anyOf {
			if m, ok := alt.(map[string]any); ok {
				if p, ok := property(t, m, name); ok {
					return p, true
				}
			}
		}
	}
	return nil, false
}

func commonDef(t *testing.T, ref string) (map[string]any, bool) {
	t.Helper()
	const prefix = "common.schema.json#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, false
	}
	common, _ := readJSON(t, filepath.Join(contractsDir, "schemas", "common.schema.json")).(map[string]any)
	defs, _ := common["$defs"].(map[string]any)
	def, ok := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
	return def, ok
}

func errorCodes(t *testing.T) map[string]bool {
	t.Helper()
	def, ok := commonDef(t, "common.schema.json#/$defs/errorCode")
	if !ok {
		t.Fatal("errorCode definition missing")
	}
	codes := map[string]bool{}
	for _, c := range def["enum"].([]any) {
		codes[c.(string)] = true
	}
	return codes
}

func TestScenarios(t *testing.T) {
	codes := errorCodes(t)
	seenIDs := map[string]bool{}
	usedCodes := map[string]bool{}
	snapshotSchema, _ := readJSON(t, filepath.Join(contractsDir, "schemas", "snapshot.schema.json")).(map[string]any)

	for _, sc := range loadScenarios(t) {
		t.Run(sc.ID, func(t *testing.T) {
			if sc.ID == "" || seenIDs[sc.ID] {
				t.Fatalf("scenario id must be unique and non-empty: %q", sc.ID)
			}
			seenIDs[sc.ID] = true
			if sc.Description == "" || sc.Covers == "" || len(sc.Steps) == 0 {
				t.Fatal("scenario needs description, covers and steps")
			}

			for i, step := range sc.Steps {
				where := fmt.Sprintf("step %d", i+1)
				if step.Event != "" {
					if step.Op != "" {
						t.Fatalf("%s: event step must not carry op", where)
					}
					continue
				}
				outName, known := operations[step.Op]
				if !known {
					t.Fatalf("%s: unknown op %q", where, step.Op)
				}
				if !actors[step.Actor] {
					t.Fatalf("%s: unknown actor %q", where, step.Actor)
				}

				var input any
				if err := json.Unmarshal(step.Input, &input); err != nil {
					t.Fatalf("%s: input: %v", where, err)
				}
				// INVALID_ARGUMENT の step だけは、入力が schema に違反していることを検証する。
				inErr := mustResolve(t, step.Op+".input").Validate(input)
				if step.Expect.Error == "INVALID_ARGUMENT" {
					if inErr == nil {
						t.Fatalf("%s: INVALID_ARGUMENT step must violate the input schema", where)
					}
				} else if inErr != nil {
					t.Fatalf("%s: input violates %s.input: %v", where, step.Op, inErr)
				}

				switch {
				case step.Expect.Error != "":
					if !codes[step.Expect.Error] {
						t.Fatalf("%s: unknown error code %q", where, step.Expect.Error)
					}
					usedCodes[step.Expect.Error] = true
				case len(step.Expect.Output) > 0:
					if step.Op == "get_review_request" {
						t.Fatalf("%s: get_review_request returns a snapshot; use expect.snapshot", where)
					}
					var out any
					if err := json.Unmarshal(step.Expect.Output, &out); err != nil {
						t.Fatalf("%s: output: %v", where, err)
					}
					if err := mustResolve(t, outName).Validate(out); err != nil {
						t.Fatalf("%s: output violates %s: %v", where, outName, err)
					}
				case len(step.Expect.Snapshot) > 0:
					for p := range step.Expect.Snapshot {
						if !schemaHasPath(t, snapshotSchema, p) {
							t.Fatalf("%s: snapshot path %q is not in the snapshot schema", where, p)
						}
					}
				default:
					t.Fatalf("%s: expect needs error, output or snapshot", where)
				}
			}
		})
	}

	t.Run("every error code is exercised by a scenario", func(t *testing.T) {
		for code := range codes {
			if !usedCodes[code] {
				t.Errorf("error code %s is not covered by any scenario", code)
			}
		}
	})
}
