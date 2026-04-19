package cmd

import (
	"bytes"
	"encoding/json"
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
