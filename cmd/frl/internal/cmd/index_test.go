package cmd

import (
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
