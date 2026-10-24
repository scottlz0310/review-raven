package contracts

import (
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	validRequest := readJSON(t, filepath.Join(contractsDir, "fixtures", "request_review.input", "valid", "opened.json"))
	badRequest := map[string]any{"schemaVersion": 1}

	cases := []struct {
		name    string
		schema  string
		value   any
		wantErr bool
	}{
		{"valid fixture", "request_review.input", validRequest, false},
		{"missing required fields", "request_review.input", badRequest, true},
		{"unknown schema", "no_such_schema", validRequest, true},
		{"unmarshalable value", "request_review.input", make(chan int), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.schema, tc.value); (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%s) error = %v, wantErr %v", tc.schema, err, tc.wantErr)
			}
		})
	}
}
