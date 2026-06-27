package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

func TestSortedRecordTypeNames(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)
	got := sortedRecordTypeNames(md)
	want := []string{"Customer", "Order", "TypedRecord"}
	if len(got) != len(want) {
		t.Fatalf("got %d names, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWriteRecordTypeDescription_Shape(t *testing.T) {
	t.Parallel()
	md := describeBuilder(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	rt := md.GetRecordType("Order")
	if rt == nil {
		t.Fatal("Order type not found in fixture")
	}

	var buf bytes.Buffer
	if err := writeRecordTypeDescription(&buf, md, rt); err != nil {
		t.Fatalf("writeRecordTypeDescription: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"Name:                   Order",
		"Primary key:            order_id",
		"Proto message:",
		"Order$price (value on price)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestWriteRecordTypeDescriptionJSON_Shape(t *testing.T) {
	t.Parallel()
	md := describeBuilder(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	rt := md.GetRecordType("Order")

	var buf bytes.Buffer
	if err := writeRecordTypeDescriptionJSON(&buf, md, rt); err != nil {
		t.Fatalf("writeRecordTypeDescriptionJSON: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(buf.Bytes(), &obj); err != nil {
		t.Fatalf("decode JSON: %v\nraw:\n%s", err, buf.String())
	}

	// Required top-level fields.
	for _, key := range []string{"name", "primary_key", "record_type_key", "proto_message", "proto_field_count"} {
		if _, ok := obj[key]; !ok {
			t.Errorf("missing key %q:\n%s", key, buf.String())
		}
	}
	if obj["name"] != "Order" {
		t.Errorf("name = %v; want Order", obj["name"])
	}
	if obj["primary_key"] != "order_id" {
		t.Errorf("primary_key = %v; want order_id", obj["primary_key"])
	}

	// Indexes array has the one we added.
	ix, ok := obj["indexes"].([]any)
	if !ok || len(ix) != 1 {
		t.Fatalf("expected 1 index in output:\n%s", buf.String())
	}
	firstIdx, _ := ix[0].(map[string]any)
	if firstIdx["name"] != "Order$price" {
		t.Errorf("index name = %v; want Order$price", firstIdx["name"])
	}
}

func TestWriteRecordTypeDescriptionJSON_NoIndexesOmitsKeys(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)
	rt := md.GetRecordType("TypedRecord")

	var buf bytes.Buffer
	if err := writeRecordTypeDescriptionJSON(&buf, md, rt); err != nil {
		t.Fatalf("writeRecordTypeDescriptionJSON: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(buf.Bytes(), &obj); err != nil {
		t.Fatalf("decode JSON: %v\nraw:\n%s", err, buf.String())
	}
	// omitempty on nil slices — indexes key shouldn't appear at all.
	if _, present := obj["indexes"]; present {
		t.Errorf("indexes should be omitted when empty; got %v", obj["indexes"])
	}
}

// TestMetaTypesDescribeCmd_MetaFile drives the full cobra command via
// --meta-file so RunE + arg handling + meta.Source resolution get
// covered without opening FDB. Before this, newMetaTypesDescribeCmd
// was at 22% (only the underlying writeRecordTypeDescription was
// exercised).
func TestMetaTypesDescribeCmd_MetaFile(t *testing.T) {
	path := writeDemoMetaFile(t, 0)
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	c := newMetaTypesDescribeCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", path, "Order"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\nout:\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"Name:", "Order", "Primary key:", "order_id"} {
		if !strings.Contains(got, want) {
			t.Errorf("text output missing %q:\n%s", want, got)
		}
	}
}

// TestMetaTypesDescribeCmd_UnknownType verifies the error path lists
// available type names instead of regressing to a stack trace.
func TestMetaTypesDescribeCmd_UnknownType(t *testing.T) {
	path := writeDemoMetaFile(t, 0)
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	c := newMetaTypesDescribeCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", path, "NotARealType"})
	err := c.Execute()
	if err == nil {
		t.Fatal("expected error for unknown record type")
	}
	// Must suggest the fixture's actual types so the operator can fix the
	// typo immediately.
	for _, want := range []string{"Order", "Customer", "TypedRecord"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing suggestion for %q", err, want)
		}
	}
}

// TestMetaTypesDescribeCmd_JSON end-to-end for the -o json path so
// scripts keying off documented field names stay stable.
func TestMetaTypesDescribeCmd_JSON(t *testing.T) {
	path := writeDemoMetaFile(t, 0)
	t.Setenv("FRL_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	c := newMetaTypesDescribeCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--meta-file", path, "-o", "json", "Order"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\nout:\n%s", err, out.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, out.String())
	}
	if obj["name"] != "Order" {
		t.Errorf("name = %v; want Order", obj["name"])
	}
	if obj["primary_key"] != "order_id" {
		t.Errorf("primary_key = %v; want order_id", obj["primary_key"])
	}
}

func TestWriteRecordTypeDescription_NoIndexes(t *testing.T) {
	t.Parallel()
	md := buildDemoMetaData(t)
	rt := md.GetRecordType("TypedRecord")
	if rt == nil {
		t.Fatal("TypedRecord not found in fixture")
	}
	var buf bytes.Buffer
	if err := writeRecordTypeDescription(&buf, md, rt); err != nil {
		t.Fatalf("writeRecordTypeDescription: %v", err)
	}
	if !strings.Contains(buf.String(), "Indexes:                (none)") {
		t.Errorf("expected '(none)' marker for index-less type, got:\n%s", buf.String())
	}
}
