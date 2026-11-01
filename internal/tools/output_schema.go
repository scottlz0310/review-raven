package tools

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func outputOrErrorOf[T, E any]() *jsonschema.Schema {
	success, err := jsonschema.For[T](nil)
	if err != nil {
		// 静的な型からのschema導出失敗は実装の誤りなので起動を止める。
		panic(fmt.Sprintf("derive output schema: %v", err))
	}
	failure, err := jsonschema.For[E](nil)
	if err != nil {
		panic(fmt.Sprintf("derive error envelope schema: %v", err))
	}
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{success, failure}}
}

func withOutputSchema(tool *mcp.Tool, schema *jsonschema.Schema) *mcp.Tool {
	declared := *tool
	declared.OutputSchema = schema
	return &declared
}
