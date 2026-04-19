package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
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
