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

func TestDiffJSON_IdenticalProducesEmptySections(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t)
	var buf bytes.Buffer
	if err := writeMetaDiffJSON(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiffJSON: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(buf.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, buf.String())
	}
	// version key omitted when unchanged (omitempty).
	if _, hasVersion := d["version"]; hasVersion {
		t.Errorf("version key should be omitted when unchanged; got %v", d["version"])
	}
	// Both sections present with empty arrays — scripts can do `.added | length`
	// without null-guards. Crucially NOT nil — the encoder should emit `[]`.
	for _, section := range []string{"record_types", "indexes"} {
		bucket, ok := d[section].(map[string]any)
		if !ok {
			t.Fatalf("missing %s section:\n%s", section, buf.String())
		}
		for _, k := range []string{"added", "removed", "changed"} {
			arr, ok := bucket[k].([]any)
			if !ok {
				t.Errorf("%s.%s not an array (nil would be a scripting footgun):\n%s",
					section, k, buf.String())
			}
			if len(arr) != 0 {
				t.Errorf("%s.%s = %v; want empty", section, k, arr)
			}
		}
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

// TestDiffSection_EntryShape verifies the structured diffEntry output
// replaces the text-parsing that splitDiffLine used to do. Each bucket
// must contain entries with Name populated; Detail is optional (empty
// for bare removals).
func TestDiffSection_EntryShape(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$quantity", recordlayer.Field("quantity")))
	})

	idx := diffIndexes(m1, m2)
	// Expect 1 added (Order$quantity), 1 removed (Order$price), 0 changed.
	if len(idx.Added) != 1 || idx.Added[0].Name != "Order$quantity" {
		t.Errorf("Added = %v; want [Order$quantity]", idx.Added)
	}
	if idx.Added[0].Detail == "" {
		t.Errorf("Added entry missing Detail — was expected to carry type/fields summary")
	}
	if len(idx.Removed) != 1 || idx.Removed[0].Name != "Order$price" {
		t.Errorf("Removed = %v; want [Order$price]", idx.Removed)
	}
	// Removed entries have empty Detail by contract.
	if idx.Removed[0].Detail != "" {
		t.Errorf("Removed entry Detail = %q; want empty", idx.Removed[0].Detail)
	}
	if len(idx.Changed) != 0 {
		t.Errorf("Changed = %v; want empty", idx.Changed)
	}
}
