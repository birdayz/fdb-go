package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteCatalogDatabases_Text(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCatalogDatabases(&buf, []string{"/myapp", "/other"}, "text"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"/myapp\n", "/other\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestWriteCatalogDatabases_TextEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCatalogDatabases(&buf, nil, "text"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(buf.String(), "no databases") {
		t.Errorf("expected empty-marker for zero databases, got %q", buf.String())
	}
}

func TestWriteCatalogDatabases_JSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCatalogDatabases(&buf, []string{"/myapp", "/other"}, "json"); err != nil {
		t.Fatalf("write: %v", err)
	}
	var rows []map[string]string
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, buf.String())
	}
	if len(rows) != 2 || rows[0]["id"] != "/myapp" || rows[1]["id"] != "/other" {
		t.Errorf("rows = %v", rows)
	}
}

func TestWriteCatalogSchemas_Text(t *testing.T) {
	t.Parallel()
	rows := []schemaRow{
		{Database: "/myapp", Name: "main", Template: "orders", TemplateVersion: 2},
		{Database: "/other", Name: "users", Template: "users_tpl", TemplateVersion: 1},
	}
	var buf bytes.Buffer
	if err := writeCatalogSchemas(&buf, rows, "text"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"DATABASE", "SCHEMA", "TEMPLATE", "VERSION", "/myapp", "orders", "2"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestWriteCatalogSchemas_JSONEmptyIsArray(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCatalogSchemas(&buf, nil, "json"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Contract: empty JSON output is `[]`, not `null`. jq scripts doing
	// `length` / `.[0]` on `null` break.
	trimmed := strings.TrimSpace(buf.String())
	if trimmed != "[]" {
		t.Errorf("empty output = %q; want []", trimmed)
	}
}

func TestWriteCatalogSchemas_JSONShape(t *testing.T) {
	t.Parallel()
	rows := []schemaRow{{Database: "/d", Name: "s", Template: "t", TemplateVersion: 7}}
	var buf bytes.Buffer
	if err := writeCatalogSchemas(&buf, rows, "json"); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, buf.String())
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d; want 1", len(got))
	}
	r := got[0]
	if r["database"] != "/d" || r["name"] != "s" || r["template"] != "t" ||
		r["template_version"].(float64) != 7 {
		t.Errorf("row shape wrong: %v", r)
	}
}

func TestWriteCatalogTemplates_TextAndJSON(t *testing.T) {
	t.Parallel()
	rows := []templateRow{
		{Name: "orders", Version: 1},
		{Name: "orders", Version: 2},
		{Name: "users", Version: 1},
	}

	var textBuf bytes.Buffer
	if err := writeCatalogTemplates(&textBuf, rows, "text"); err != nil {
		t.Fatalf("text: %v", err)
	}
	text := textBuf.String()
	for _, want := range []string{"NAME", "VERSION", "orders", "users", "1", "2"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}

	var jsonBuf bytes.Buffer
	if err := writeCatalogTemplates(&jsonBuf, rows, "json"); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal(jsonBuf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, jsonBuf.String())
	}
	if len(got) != 3 {
		t.Errorf("rows = %d; want 3", len(got))
	}
}

// TestWriteMetaDataRendered_JSONAndYAML smokes the shared renderer for
// meta get / meta catalog get: both call writeMetaDataRendered and the
// contract is "protojson by default, protoyaml on yaml".
func TestWriteMetaDataRendered_JSONAndYAML(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)

	var jsonBuf bytes.Buffer
	if err := writeMetaDataRendered(&jsonBuf, md, "json"); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !strings.Contains(jsonBuf.String(), `"records"`) {
		t.Errorf("json missing records key:\n%s", jsonBuf.String())
	}

	var yamlBuf bytes.Buffer
	if err := writeMetaDataRendered(&yamlBuf, md, "yaml"); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	// protoyaml emits un-quoted keys; the JSON-style `"records":` shape
	// would indicate we accidentally fell through to protojson.
	if strings.Contains(yamlBuf.String(), `"records"`) {
		t.Errorf("yaml output looks like JSON:\n%s", yamlBuf.String())
	}
	if !strings.Contains(yamlBuf.String(), "records:") {
		t.Errorf("yaml output missing records: key:\n%s", yamlBuf.String())
	}
}
