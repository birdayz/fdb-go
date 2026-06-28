package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

func TestWriteTypesListJSON_Shape(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)
	var buf bytes.Buffer
	if err := writeTypesListJSON(&buf, md); err != nil {
		t.Fatalf("writeTypesListJSON: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, buf.String())
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d:\n%s", len(rows), buf.String())
	}
	// Sorted: Customer < Order < TypedRecord.
	want := []string{"Customer", "Order", "TypedRecord"}
	for i, w := range want {
		if rows[i]["name"] != w {
			t.Errorf("row %d name = %v; want %q", i, rows[i]["name"], w)
		}
		if _, ok := rows[i]["primary_key"]; !ok {
			t.Errorf("row %d missing primary_key", i)
		}
	}
	// since_version is 0 by default and should be elided (omitempty).
	if _, present := rows[0]["since_version"]; present {
		t.Errorf("since_version=0 should be omitted; got present:\n%s", buf.String())
	}
}

func TestWriteTypesList_RendersFixture(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t) // shared fixture from index_test.go

	var buf bytes.Buffer
	if err := writeTypesList(&buf, md); err != nil {
		t.Fatalf("writeTypesList: %v", err)
	}
	out := buf.String()

	// Header + one line per type. Demo proto has 3 types.
	for _, want := range []string{
		"NAME",
		"PRIMARY KEY",
		"SINCE VERSION",
		"Order",
		"Customer",
		"TypedRecord",
		"order_id",
		"customer_id",
		"id",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("writeTypesList output missing %q\nfull output:\n%s", want, out)
		}
	}

	// Output rows must be sorted (Customer < Order < TypedRecord).
	customerIdx := strings.Index(out, "Customer")
	orderIdx := strings.Index(out, "Order")
	typedIdx := strings.Index(out, "TypedRecord")
	if !(customerIdx < orderIdx && orderIdx < typedIdx) {
		t.Errorf("rows not alphabetically sorted:\n%s", out)
	}
}

// writeDemoMetaFile exports the shared demo metadata to a temp file via
// WriteRecordMetaData. Used by validate/evolve-check tests so they don't
// all re-hand-roll the dumper.
func writeDemoMetaFile(t *testing.T, version int32) string {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().
		SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	if version > 0 {
		builder.SetVersion(int(version))
	}
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build demo metadata: %v", err)
	}
	path := filepath.Join(t.TempDir(), "meta.pb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := recordlayer.WriteRecordMetaData(md, f); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestMetaTypesLs_MetaFileFlagWorks drives the command end-to-end via
// --meta-file so the command constructor (RunE, flag wiring, meta.Source
// resolution) gets covered without opening FDB. Without this test, a
// regression in `resolveContextAndOverride` / `meta.FromContext` that
// drops the override path goes undetected until someone runs integration.
func TestMetaTypesLs_MetaFileFlagWorks(t *testing.T) {
	path := writeDemoMetaFile(t, 0)
	// Empty config → ResolveContext fails, but --meta-file should carry us.
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	c := newMetaTypesLsCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", path})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\nout:\n%s", err, out.String())
	}
	got := out.String()
	// Header + every demo type.
	for _, want := range []string{"NAME", "Order", "Customer", "TypedRecord"} {
		if !strings.Contains(got, want) {
			t.Errorf("text output missing %q:\n%s", want, got)
		}
	}
}

// TestMetaTypesLs_JSONShape verifies `-o json` produces the documented
// schema. `meta types ls` was shipped with no command-level test —
// this locks in the JSON contract so downstream `jq -r '.[].name'`
// scripts stay stable.
func TestMetaTypesLs_JSONShape(t *testing.T) {
	path := writeDemoMetaFile(t, 0)
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	c := newMetaTypesLsCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", path, "-o", "json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\nout:\n%s", err, out.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, out.String())
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d:\n%s", len(rows), out.String())
	}
	// Every row must have name + primary_key; both emitted in snake_case
	// to match the rest of the CLI's structured output.
	for i, r := range rows {
		if _, ok := r["name"]; !ok {
			t.Errorf("row %d missing 'name':\n%s", i, out.String())
		}
		if _, ok := r["primary_key"]; !ok {
			t.Errorf("row %d missing 'primary_key':\n%s", i, out.String())
		}
	}
}

func TestMetaValidate_Succeeds(t *testing.T) {
	t.Parallel()
	path := writeDemoMetaFile(t, 0)
	c := newMetaValidateCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--file", path})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "ok:") {
		t.Errorf("output missing 'ok:' line:\n%s", out.String())
	}
}

func TestMetaValidate_RequiresFile(t *testing.T) {
	t.Parallel()
	c := newMetaValidateCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{})
	if err := c.Execute(); err == nil {
		t.Fatal("expected error when --file omitted")
	}
}

func TestMetaEvolveCheck_SameVersionRejected(t *testing.T) {
	t.Parallel()
	old := writeDemoMetaFile(t, 0)
	newer := writeDemoMetaFile(t, 0)
	c := newMetaEvolveCheckCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--old", old, "--new", newer})
	// Same version → MetaDataEvolutionValidator rejects (newer must advance).
	err := c.Execute()
	if err == nil {
		t.Fatal("expected evolve-check to reject same-version evolution, got nil")
	}
	if !strings.Contains(err.Error(), "newer version") {
		t.Errorf("error text %q doesn't mention the version-advance requirement", err.Error())
	}
}

// TestMetaEvolveCheck_InvalidJSON_NoValidFalse locks in the CLI's
// shape contract (documented in README): failures exit non-zero with
// the error on stderr — they must NOT emit `{"valid": false}` on
// stdout. CI gates that branch on exit code would otherwise accept a
// rejected evolution because stdout looks successful.
func TestMetaEvolveCheck_InvalidJSON_NoValidFalse(t *testing.T) {
	t.Parallel()
	old := writeDemoMetaFile(t, 0)
	newer := writeDemoMetaFile(t, 0)
	c := newMetaEvolveCheckCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--old", old, "--new", newer, "-o", "json"})
	if err := c.Execute(); err == nil {
		t.Fatal("expected error for rejected evolution, got nil")
	}
	// stdout must stay empty (or at least contain no JSON object with
	// "valid": anything). fang/cobra writes the error banner to the
	// configured writer; we only care that no misleading JSON leaked.
	if strings.Contains(out.String(), `"valid"`) {
		t.Errorf("rejected evolution leaked a JSON envelope onto stdout:\n%s",
			out.String())
	}
}

func TestMetaGet_DefaultJSONContainsRecords(t *testing.T) {
	path := writeDemoMetaFile(t, 0)
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml")) // empty → --meta-file mode

	c := newMetaGetCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", path})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\n%s", err, out.String())
	}
	// Protojson default render — records field and at least one record type
	// should be present.
	got := out.String()
	if !strings.Contains(got, `"records"`) {
		t.Errorf("JSON output missing records field:\n%s", got)
	}
	if !strings.Contains(got, `"Order"`) {
		t.Errorf("JSON output missing Order record type:\n%s", got)
	}
}

func TestMetaGet_YAMLContainsRecords(t *testing.T) {
	path := writeDemoMetaFile(t, 0)
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	c := newMetaGetCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", path, "-o", "yaml"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\n%s", err, out.String())
	}
	got := out.String()
	// protoyaml uses camelCase keys (records: … / messageType: …) without
	// quotes, unlike protojson. Assert on that distinguishing pattern.
	if strings.Contains(got, `"records"`) {
		t.Errorf("yaml output shouldn't quote keys (looks like JSON):\n%s", got)
	}
	if !strings.Contains(got, "records:") {
		t.Errorf("yaml output missing records: key:\n%s", got)
	}
	if !strings.Contains(got, "name: Order") {
		t.Errorf("yaml output missing Order type name:\n%s", got)
	}
}

func TestMetaGet_InvalidOutputRejected(t *testing.T) {
	path := writeDemoMetaFile(t, 0)
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	c := newMetaGetCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", path, "-o", "xml"})
	if err := c.Execute(); err == nil {
		t.Fatal("expected error for unsupported --output")
	}
}

func TestMetaValidate_JSON(t *testing.T) {
	t.Parallel()
	path := writeDemoMetaFile(t, 0)
	c := newMetaValidateCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--file", path, "-o", "json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, out.String())
	}
	if obj["valid"] != true {
		t.Errorf("valid = %v, want true", obj["valid"])
	}
	if obj["file"] != path {
		t.Errorf("file = %v, want %q", obj["file"], path)
	}
}

func TestMetaEvolveCheck_ValidJSON(t *testing.T) {
	t.Parallel()
	old := writeDemoMetaFile(t, 0)
	newer := writeDemoMetaFile(t, 1)
	c := newMetaEvolveCheckCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--old", old, "--new", newer, "-o", "json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\nout:\n%s", err, out.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, out.String())
	}
	if obj["valid"] != true {
		t.Errorf("valid = %v, want true", obj["valid"])
	}
	if obj["old"] != old || obj["new"] != newer {
		t.Errorf("paths mismatch: %v / %v", obj["old"], obj["new"])
	}
}

func TestMetaEvolveCheck_DefaultStrict_NoFlagsAllowNothingExtra(t *testing.T) {
	t.Parallel()
	// Smoke the default-strict assertion: the three loosener flags, when
	// NONE are set, still produce the standard strict behavior. Prevents
	// a refactor accidentally flipping a default to true.
	p1 := writeDemoMetaFile(t, 0)
	p2 := writeDemoMetaFile(t, 0)

	c := newMetaEvolveCheckCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--old", p1, "--new", p2}) // no --allow-*
	err := c.Execute()
	if err == nil {
		t.Fatal("strict mode should reject same-version evolution")
	}
	if !strings.Contains(err.Error(), "newer version") {
		t.Errorf("strict error = %v; want 'newer version'", err)
	}
}

func TestMetaEvolveCheck_AllowNoVersionChangeFlag(t *testing.T) {
	t.Parallel()
	// Same-version files that would normally be rejected.
	p1 := writeDemoMetaFile(t, 0)
	p2 := writeDemoMetaFile(t, 0)

	// Without the flag: rejected.
	c1 := newMetaEvolveCheckCmd()
	var out1 bytes.Buffer
	c1.SetOut(&out1)
	c1.SetErr(&out1)
	c1.SetArgs([]string{"--old", p1, "--new", p2})
	if err := c1.Execute(); err == nil {
		t.Fatal("expected rejection without --allow-no-version-change")
	}

	// With the flag: accepted.
	c2 := newMetaEvolveCheckCmd()
	var out2 bytes.Buffer
	c2.SetOut(&out2)
	c2.SetErr(&out2)
	c2.SetArgs([]string{"--allow-no-version-change", "--old", p1, "--new", p2})
	if err := c2.Execute(); err != nil {
		t.Fatalf("expected acceptance with --allow-no-version-change, got: %v\nout:\n%s", err, out2.String())
	}
	if !strings.Contains(out2.String(), "ok:") {
		t.Errorf("expected 'ok:' in output with flag set:\n%s", out2.String())
	}
}

func TestMetaEvolveCheck_ValidEvolution(t *testing.T) {
	t.Parallel()
	// Version bumps cleanly from 0 → 1 with identical schema shape,
	// which MetaDataEvolutionValidator accepts.
	oldPath := writeDemoMetaFile(t, 0)
	newPath := writeDemoMetaFile(t, 1)

	c := newMetaEvolveCheckCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--old", oldPath, "--new", newPath})
	if err := c.Execute(); err != nil {
		t.Fatalf("expected valid evolution to pass, got: %v\nout:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "ok:") {
		t.Errorf("output missing 'ok:' line:\n%s", out.String())
	}
}
