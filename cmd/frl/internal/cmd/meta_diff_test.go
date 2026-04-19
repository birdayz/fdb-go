package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// buildMetaWith builds a demo metadata with customizable PK / index /
// version so diff tests can target one axis at a time.
func buildMetaWith(t *testing.T, opts ...func(*recordlayer.RecordMetaDataBuilder)) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	for _, o := range opts {
		o(b)
	}
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return md
}

func TestDiff_Identical(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t)
	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	if !strings.Contains(buf.String(), "identical") {
		t.Errorf("expected 'identical' in output, got:\n%s", buf.String())
	}
}

func TestDiff_IndexAdded(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "INDEXES:") {
		t.Errorf("missing INDEXES section:\n%s", got)
	}
	if !strings.Contains(got, "+ Order$price") {
		t.Errorf("missing '+ Order$price':\n%s", got)
	}
}

func TestDiff_IndexRemoved(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	m2 := buildMetaWith(t)
	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	if !strings.Contains(buf.String(), "- Order$price") {
		t.Errorf("missing '- Order$price':\n%s", buf.String())
	}
}

func TestDiff_IndexTypeChanged(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		// count index requires grouping — Ungrouped() wraps EmptyKeyExpression
		// into a GroupingKeyExpression so the builder accepts it.
		idx := recordlayer.NewCountIndex("Order$price", recordlayer.Ungrouped(&recordlayer.EmptyKeyExpression{}))
		b.AddIndex("Order", idx)
	})
	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "~ Order$price") || !strings.Contains(got, "type value -> count") {
		t.Errorf("expected ~ line with type change, got:\n%s", got)
	}
}

func TestDiff_VersionChange(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.SetVersion(42)
	})
	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "VERSION:") || !strings.Contains(got, "-> 42") {
		t.Errorf("expected VERSION change line, got:\n%s", got)
	}
}

func TestDiffJSON_IndexAddedAndRemoved(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$quantity", recordlayer.Field("quantity")))
	})

	var buf bytes.Buffer
	if err := writeMetaDiffJSON(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiffJSON: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(buf.Bytes(), &d); err != nil {
		t.Fatalf("decode JSON: %v\nraw:\n%s", err, buf.String())
	}
	idx, ok := d["indexes"].(map[string]any)
	if !ok {
		t.Fatalf("missing indexes key:\n%s", buf.String())
	}
	// added
	added, _ := idx["added"].([]any)
	if len(added) != 1 || added[0] != "Order$quantity" {
		t.Errorf("indexes.added = %v; want [Order$quantity]", added)
	}
	// removed
	removed, _ := idx["removed"].([]any)
	if len(removed) != 1 || removed[0] != "Order$price" {
		t.Errorf("indexes.removed = %v; want [Order$price]", removed)
	}
}

func TestDiffJSON_VersionBumpEmitsVersion(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.SetVersion(7)
	})
	var buf bytes.Buffer
	if err := writeMetaDiffJSON(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiffJSON: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(buf.Bytes(), &d); err != nil {
		t.Fatalf("decode JSON: %v\nraw:\n%s", err, buf.String())
	}
	v, ok := d["version"].(map[string]any)
	if !ok {
		t.Fatalf("missing version key:\n%s", buf.String())
	}
	if v["new"].(float64) != 7 {
		t.Errorf("version.new = %v; want 7", v["new"])
	}
}

func TestSplitDiffLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in           string
		wantCategory string
		wantName     string
	}{
		{"+ Order$new_idx (value on price)", "+", "Order$new_idx"},
		{"- Order$old_idx", "-", "Order$old_idx"},
		{"~ Order: pk changed (x -> y)", "~", "Order"},
		{"~ Order$price: type value -> count", "~", "Order$price"},
		{"invalid", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			cat, name := splitDiffLine(tc.in)
			if cat != tc.wantCategory || name != tc.wantName {
				t.Errorf("splitDiffLine(%q) = (%q, %q); want (%q, %q)",
					tc.in, cat, name, tc.wantCategory, tc.wantName)
			}
		})
	}
}
