package tools

import (
	"encoding/json"
	"testing"
)

func TestWebPostSchemaMarshals(t *testing.T) {
	tool := NewWebPostTool(5, 0, "", nil)
	b, err := json.Marshal(tool.Parameters())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	props, ok := m["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("no properties")
	}
	for _, k := range []string{"url", "method", "headers", "body", "json", "files", "fields"} {
		if _, ok := props[k]; !ok {
			t.Errorf("missing property %q", k)
		}
	}
	if _, ok := m["required"]; !ok {
		t.Error("missing required")
	}
}
