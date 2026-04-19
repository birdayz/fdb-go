package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
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
