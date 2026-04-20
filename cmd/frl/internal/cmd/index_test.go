package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// buildDemoMetaData builds a RecordMetaData from the record-layer demo
// proto with two indexes for exercising the list renderers offline. No
// FDB required.
func buildDemoMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().
		SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	builder.AddIndex("Customer", recordlayer.NewIndex("Customer$name", recordlayer.Field("name")))
	meta, err := builder.Build()
	if err != nil {
		t.Fatalf("build demo metadata: %v", err)
	}
	return meta
}

func TestRecordTypeNames_UniversalIndex(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)
	// An index that isn't registered on any type reads as universal.
	idx := &recordlayer.Index{Name: "universal", Type: "VALUE"}
	got := recordTypeNames(md, idx)
	if len(got) != 1 || got[0] != "*" {
		t.Errorf("universal index → %v, want [\"*\"]", got)
	}
}

func TestRecordTypeNames_TypedIndex(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)
	idx := md.GetIndex("Order$price")
	if idx == nil {
		t.Fatal("Order$price not found in demo metadata")
	}
	got := recordTypeNames(md, idx)
	if len(got) != 1 || got[0] != "Order" {
		t.Errorf("record_type names for Order$price = %v, want [Order]", got)
	}
}

// TestDemoMetaDataIndexCount sanity-checks the test fixture — if the
// demo proto changes upstream, this test catches it before downstream
// assertions get confusing.
func TestDemoMetaDataIndexCount(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)
	if got := len(md.GetAllIndexes()); got != 2 {
		t.Errorf("demo metadata indexes = %d, want 2", got)
	}
}

func TestWriteIndexListJSON_RendersArray(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)

	var buf bytes.Buffer
	if err := writeIndexListJSON(&buf, md, func(name string) string { return "readable" }); err != nil {
		t.Fatalf("writeIndexListJSON: %v", err)
	}

	// Parse output back and assert structural invariants.
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("decode JSON output: %v\nraw:\n%s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2:\n%s", len(rows), buf.String())
	}

	// Rows must be sorted by name alphabetically.
	if rows[0]["name"] != "Customer$name" || rows[1]["name"] != "Order$price" {
		t.Errorf("rows not alphabetically sorted by name:\n%s", buf.String())
	}

	// Every row must carry the fixed schema fields.
	for i, row := range rows {
		for _, key := range []string{"name", "type", "state", "record_types", "last_modified_version"} {
			if _, ok := row[key]; !ok {
				t.Errorf("row %d missing %q key:\n%s", i, key, buf.String())
			}
		}
		if row["state"] != "readable" {
			t.Errorf("row %d state = %v; want readable", i, row["state"])
		}
	}
}

// TestIndexLs_NoFDB_WorksWithBogusClusterFile proves the --no-fdb
// contract: the command must render indexes from a meta-file without
// opening any FDB connection. The config points at a bogus cluster
// path that would fail to dial — if --no-fdb weren't respected, this
// test would hang on the FDB connection attempt.
func TestIndexLs_NoFDB_WorksWithBogusClusterFile(t *testing.T) {
	// Not parallel: mutates FRL_CONFIG via t.Setenv.
	metaPath := writeMetaFileWithIndexes(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	raw := fmt.Sprintf(`current_context: local
contexts:
  - name: local
    cluster_file: /definitely/not/a/real/cluster.file
    keyspace_path: /test
    metadata:
      meta_file: %s
`, metaPath)
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", cfgPath)

	c := NewRoot()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"index", "ls", "--no-fdb"})
	if err := c.Execute(); err != nil {
		t.Fatalf("index ls --no-fdb: %v\nout:\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"Order$price", "Customer$name"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// STATE column shows "—" (em dash) when no FDB was contacted — part
	// of the contract documented in --no-fdb help text.
	if !strings.Contains(got, "—") {
		t.Errorf("expected '—' placeholder for STATE; got:\n%s", got)
	}
}

// TestIndexLs_NoFDB_RequiresFileSource — --no-fdb only works with a
// meta_file source; if the context is wired for FDBMetaDataStore (no
// local file), the command must refuse clearly rather than falling
// through to an FDB connection attempt.
func TestIndexLs_NoFDB_RequiresFileSource(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	raw := `current_context: local
contexts:
  - name: local
    cluster_file: /definitely/not/a/real/cluster.file
    keyspace_path: /test
    metadata:
      meta_store_keyspace: /some/other/path
`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", cfgPath)

	c := NewRoot()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"index", "ls", "--no-fdb"})
	err := c.Execute()
	if err == nil {
		t.Fatal("expected error for --no-fdb against FDBMetaDataStore source")
	}
	if !strings.Contains(err.Error(), "--no-fdb") || !strings.Contains(err.Error(), "file") {
		t.Errorf("error = %v; should mention both --no-fdb and the file requirement", err)
	}
}

// TestRenderIndexList_DispatchesByFormat smokes the tiny format-dispatch
// wrapper. One test per branch proves the json vs text switch is wired
// right — without this, the two helpers could be tested individually
// but still miscalled by the dispatcher (e.g. swapped, both pointing
// at json).
func TestRenderIndexList_DispatchesByFormat(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)

	var textBuf bytes.Buffer
	if err := renderIndexList(&textBuf, md, nil, "text"); err != nil {
		t.Fatalf("text: %v", err)
	}
	// tabwriter output — must contain the header words, not JSON braces.
	if !strings.Contains(textBuf.String(), "NAME") || !strings.Contains(textBuf.String(), "TYPE") {
		t.Errorf("text output missing header columns:\n%s", textBuf.String())
	}
	if strings.Contains(textBuf.String(), `"name"`) {
		t.Errorf("text branch emitted JSON keys — dispatcher wired wrong:\n%s", textBuf.String())
	}

	var jsonBuf bytes.Buffer
	if err := renderIndexList(&jsonBuf, md, nil, "json"); err != nil {
		t.Fatalf("json: %v", err)
	}
	// JSON output — starts with '[' and contains "name" keys.
	if !strings.HasPrefix(strings.TrimSpace(jsonBuf.String()), "[") {
		t.Errorf("json branch didn't emit array:\n%s", jsonBuf.String())
	}
	if !strings.Contains(jsonBuf.String(), `"name"`) {
		t.Errorf("json branch missing field keys:\n%s", jsonBuf.String())
	}
}

func TestWriteIndexListJSON_EmptyMetadata(t *testing.T) {
	t.Parallel()
	// Metadata with no indexes renders an empty array, not a text fallback.
	md := describeBuilder(t, func(b *recordlayer.RecordMetaDataBuilder) {})
	var buf bytes.Buffer
	if err := writeIndexListJSON(&buf, md, nil); err != nil {
		t.Fatalf("writeIndexListJSON: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, buf.String())
	}
	if len(rows) != 0 {
		t.Errorf("expected empty array, got %d rows:\n%s", len(rows), buf.String())
	}
}
