package cmd

import (
	"bytes"
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
