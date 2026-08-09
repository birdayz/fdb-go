package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/cmd/frl/internal/meta"
	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
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

// runIndexLsNoFDBWithConfig writes cfgYAML as the active frl config and
// runs `index ls --no-fdb` (plus extraArgs) through the real root
// command, returning the error the operator would see. Uses t.Setenv,
// so callers must not be parallel.
func runIndexLsNoFDBWithConfig(t *testing.T, cfgYAML string, extraArgs ...string) error {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", cfgPath)

	c := NewRoot()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(append([]string{"index", "ls", "--no-fdb"}, extraArgs...))
	return c.Execute()
}

// assertNoFDBSourceError pins the three properties of the `index ls
// --no-fdb` metadata-source failure that operators actually depend on.
// Each is a defect this message once had:
//
//   - it names the context, so an operator juggling several knows which
//     one is missing the file source;
//   - it does not lead with the flag, because fang title-cases the first
//     word of the error banner and turns "--no-fdb" into "--No-Fdb";
//   - it states the remediation exactly once — the wrapped sentinel
//     already carries "or pass --meta-file", and the wrapper used to
//     repeat that sentence verbatim.
func assertNoFDBSourceError(t *testing.T, err error, wantContext string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a metadata-source error")
	}
	msg := err.Error()
	if !strings.Contains(msg, wantContext) {
		t.Errorf("error = %q; must name the context %q", msg, wantContext)
	}
	if strings.HasPrefix(msg, "-") {
		t.Errorf("error = %q; must not start with a flag name (fang banner "+
			"capitalization renders it as --No-Fdb)", msg)
	}
	if n := strings.Count(msg, "pass --meta-file"); n > 1 {
		t.Errorf("error = %q; remediation repeated %d times, want once", msg, n)
	}
	if n := strings.Count(msg, "meta_file"); n > 1 {
		t.Errorf("error = %q; `meta_file` remediation repeated %d times, want once", msg, n)
	}
}

// TestIndexLs_NoFDB_MissingSourceNamesContext — a context with no
// metadata source at all must fail with an ErrMissingSource-wrapping
// error that names the context.
func TestIndexLs_NoFDB_MissingSourceNamesContext(t *testing.T) {
	// Not parallel: runIndexLsNoFDBWithConfig uses t.Setenv.
	err := runIndexLsNoFDBWithConfig(t, `current_context: local-dev
contexts:
  - name: local-dev
    cluster_file: /definitely/not/a/real/cluster.file
    keyspace_path: /test
`)
	if !errors.Is(err, meta.ErrMissingSource) {
		t.Errorf("error %v should unwrap to ErrMissingSource", err)
	}
	assertNoFDBSourceError(t, err, "local-dev")
}

// TestIndexLs_NoFDB_FDBStoreSourceNamesContext — same dance for the
// FDB-store-unsupported sentinel, so callers using errors.Is can tell
// "this command is file-only" apart from "context has no metadata at
// all" while the operator still sees which context is at fault.
func TestIndexLs_NoFDB_FDBStoreSourceNamesContext(t *testing.T) {
	// Not parallel: runIndexLsNoFDBWithConfig uses t.Setenv.
	err := runIndexLsNoFDBWithConfig(t, `current_context: prod
contexts:
  - name: prod
    cluster_file: /definitely/not/a/real/cluster.file
    keyspace_path: /test
    metadata:
      meta_store_keyspace: /myapp/_meta
`)
	if !errors.Is(err, meta.ErrFDBStoreNotAvailable) {
		t.Errorf("error %v should unwrap to ErrFDBStoreNotAvailable", err)
	}
	assertNoFDBSourceError(t, err, "prod")
}

// TestIndexLs_NoFDB_MetaFileOverridesContextSource — --meta-file
// short-circuits the context's metadata source, so an fdb_store context
// that `--no-fdb` alone rejects still renders when the flag supplies a
// file.
func TestIndexLs_NoFDB_MetaFileOverridesContextSource(t *testing.T) {
	// Not parallel: runIndexLsNoFDBWithConfig uses t.Setenv.
	metaPath := writeMetaFileWithIndexes(t)
	err := runIndexLsNoFDBWithConfig(t, `current_context: prod
contexts:
  - name: prod
    cluster_file: /definitely/not/a/real/cluster.file
    keyspace_path: /test
    metadata:
      meta_store_keyspace: /myapp/_meta
`, "--meta-file", metaPath)
	if err != nil {
		t.Fatalf("--meta-file should override the context source: %v", err)
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

// formatPartlyBuilt must render both stamps and both escape hatches —
// the raw PartlyBuiltError means nothing to an operator without them
// (FDB C++ dev C2 on RFC-174).
func TestFormatPartlyBuilt_RendersStampsAndRemediation(t *testing.T) {
	t.Parallel()
	err := formatPartlyBuilt(&recordlayer.PartlyBuiltError{
		IndexName:     "Order$price",
		SavedStamp:    "by-records",
		ExpectedStamp: "mutual",
	}, "Order$price")
	for _, want := range []string{"by-records", "mutual", "rebuild", "same settings"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("PartlyBuiltError rendering missing %q:\n%s", want, err)
		}
	}
}
