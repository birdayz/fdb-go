package cmd

import (
	"bytes"
	"encoding/json"
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
