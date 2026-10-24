package contracts

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// Validate checks a value against a contract schema ("snapshot",
// "request_review.output", ...). The value is round-tripped through JSON, so a
// Go struct is validated exactly as it would appear on the wire.
//
// It reads docs/design/contracts at run time and exists for tests of the
// implementations (internal/reviewrequest and friends); production code must
// not depend on it.
func Validate(schema string, v any) error {
	resolved, err := resolve(schema)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("unmarshal value: %w", err)
	}
	return resolved.Validate(doc)
}

const contractBaseURI = "https://schemas.review-raven.example/contracts/v1/"

var (
	cacheMu sync.Mutex
	cache   = map[string]*jsonschema.Resolved{}
)

func schemaDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "docs", "design", "contracts", "schemas")
}

func loadFile(name string) (*jsonschema.Schema, error) {
	raw, err := os.ReadFile(filepath.Join(schemaDir(), name))
	if err != nil {
		return nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return &s, nil
}

func resolve(name string) (*jsonschema.Resolved, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if r, ok := cache[name]; ok {
		return r, nil
	}
	root, err := loadFile(name + ".schema.json")
	if err != nil {
		return nil, err
	}
	r, err := root.Resolve(&jsonschema.ResolveOptions{
		BaseURI: contractBaseURI + name + ".schema.json",
		Loader: func(u *url.URL) (*jsonschema.Schema, error) {
			if !strings.HasPrefix(u.String(), contractBaseURI) {
				return nil, fmt.Errorf("unexpected external $ref: %s", u)
			}
			return loadFile(path.Base(u.Path))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("resolve schema %s: %w", name, err)
	}
	cache[name] = r
	return r, nil
}
