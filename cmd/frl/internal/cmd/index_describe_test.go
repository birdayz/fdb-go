package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// describeBuilder wraps buildDemoMetaData so each test can tweak the
// index under scrutiny without duplicating PK setup.
func describeBuilder(t *testing.T, mut func(*recordlayer.RecordMetaDataBuilder)) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	mut(b)
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return md
}

func TestWriteIndexDescription_SimpleValueIndex(t *testing.T) {
	t.Parallel()
	md := describeBuilder(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	idx := md.GetIndex("Order$price")
	if idx == nil {
		t.Fatal("index not found in fixture")
	}

	var buf bytes.Buffer
	if err := writeIndexDescription(&buf, md, idx); err != nil {
		t.Fatalf("writeIndexDescription: %v", err)
	}
	got := buf.String()
	wants := []string{
		"Name:                   Order$price",
		"Type:                   value",
		"Expression fields:      price",
		"Column size:            1",
		"Record types:           Order",
		"Unique:                 false",
		"Clear-when-zero:        false",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, got)
		}
	}
	// Options block must be absent when no extra options set.
	if strings.Contains(got, "Options:") {
		t.Errorf("output unexpectedly contains Options: section:\n%s", got)
	}
}

func TestWriteIndexDescription_UniqueIndex(t *testing.T) {
	t.Parallel()
	md := describeBuilder(t, func(b *recordlayer.RecordMetaDataBuilder) {
		idx := recordlayer.NewIndex("Order$price_unique", recordlayer.Field("price")).SetUnique()
		b.AddIndex("Order", idx)
	})
	var buf bytes.Buffer
	writeIndexDescription(&buf, md, md.GetIndex("Order$price_unique"))
	if !strings.Contains(buf.String(), "Unique:                 true") {
		t.Errorf("expected Unique: true in output:\n%s", buf.String())
	}
}

// TestWriteIndexDescriptionJSON_HappyPath locks in the JSON contract —
// jq consumers will key off these field names. Options must be an empty
// object (not null) when no options are set, so `.options.foo` doesn't
// null-ref mid-pipeline.
func TestWriteIndexDescriptionJSON_HappyPath(t *testing.T) {
	t.Parallel()
	md := describeBuilder(t, func(b *recordlayer.RecordMetaDataBuilder) {
		idx := recordlayer.NewIndex("Order$price", recordlayer.Field("price")).SetUnique()
		b.AddIndex("Order", idx)
	})
	idx := md.GetIndex("Order$price")

	var buf bytes.Buffer
	if err := writeIndexDescriptionJSON(&buf, md, idx); err != nil {
		t.Fatalf("writeIndexDescriptionJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, buf.String())
	}

	if got["name"] != "Order$price" {
		t.Errorf("name = %v; want Order$price", got["name"])
	}
	if got["type"] != "value" {
		t.Errorf("type = %v; want value", got["type"])
	}
	if got["unique"] != true {
		t.Errorf("unique = %v; want true", got["unique"])
	}
	fields, _ := got["expression_fields"].([]any)
	if len(fields) != 1 || fields[0] != "price" {
		t.Errorf("expression_fields = %v; want [price]", fields)
	}
	rts, _ := got["record_types"].([]any)
	if len(rts) != 1 || rts[0] != "Order" {
		t.Errorf("record_types = %v; want [Order]", rts)
	}
	// Options MUST be a map, never null — jq scripts rely on it.
	if _, ok := got["options"].(map[string]any); !ok {
		t.Errorf("options is %T; want object (possibly empty)", got["options"])
	}
}

// TestIndexDescribeCmd_MetaFile exercises the cobra command directly
// via --meta-file (no FDB, no config). Covers RunE + flag wiring +
// resolveContextAndOverride + meta.FromContext fallback. Without
// this, newIndexDescribeCmd stayed at 22% (only helpers were
// exercised).
func TestIndexDescribeCmd_MetaFile(t *testing.T) {
	metaPath := writeMetaFileWithIndexes(t) // Order$price + Customer$name
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	c := newIndexDescribeCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", metaPath, "Order$price"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\nout:\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"Name:", "Order$price", "Type:", "value", "Expression fields:", "price"} {
		if !strings.Contains(got, want) {
			t.Errorf("text output missing %q:\n%s", want, got)
		}
	}
}

// TestIndexDescribeCmd_UnknownName lists available names when the
// requested index is absent — guardrail for the operator-facing typo
// experience.
func TestIndexDescribeCmd_UnknownName(t *testing.T) {
	metaPath := writeMetaFileWithIndexes(t)
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	c := newIndexDescribeCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", metaPath, "not-an-index"})
	err := c.Execute()
	if err == nil {
		t.Fatal("expected error for unknown index")
	}
	// Available names must both appear in the error suggestion list
	// (alphabetical order → Customer$name first, Order$price second).
	for _, want := range []string{"Customer$name", "Order$price"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing suggestion for %q", err, want)
		}
	}
}

// TestIndexDescribeCmd_JSON end-to-end coverage of the -o json path
// via the cobra command (not just the helper).
func TestIndexDescribeCmd_JSON(t *testing.T) {
	metaPath := writeMetaFileWithIndexes(t)
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	c := newIndexDescribeCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", metaPath, "-o", "json", "Order$price"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\nout:\n%s", err, out.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, out.String())
	}
	if obj["name"] != "Order$price" {
		t.Errorf("name = %v; want Order$price", obj["name"])
	}
	// Options is always an object per the contract, even when empty.
	if _, ok := obj["options"].(map[string]any); !ok {
		t.Errorf("options is %T; want object", obj["options"])
	}
}

func TestWriteIndexDescription_NotFoundLists_AvailableNames(t *testing.T) {
	t.Parallel()
	md := describeBuilder(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$alpha", recordlayer.Field("price")))
		b.AddIndex("Customer", recordlayer.NewIndex("Customer$beta", recordlayer.Field("name")))
	})
	names := sortedIndexNames(md)
	// Alphabetical order: Customer$beta, Order$alpha
	if len(names) != 2 || names[0] != "Customer$beta" || names[1] != "Order$alpha" {
		t.Errorf("sortedIndexNames = %v; want [Customer$beta Order$alpha]", names)
	}
}
