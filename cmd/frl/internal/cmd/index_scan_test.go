package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// buildDescribeMeta pulls the Order$price index out of an already-
// built metadata. Reusing describeBuilder keeps this test decoupled
// from the Index struct's internal wiring (RootExpression, etc.).
func buildIndexForScanTest(t *testing.T) *recordlayer.Index {
	t.Helper()
	md := describeBuilder(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	idx := md.GetIndex("Order$price")
	if idx == nil {
		t.Fatal("fixture missing Order$price")
	}
	return idx
}

// TestWriteIndexEntryAsJSON_HappyPath locks in the NDJSON envelope
// shape emitted by `index scan -o json` (which is the only output
// mode). Any reshape breaks downstream jq scripts — this is the
// contract.
func TestWriteIndexEntryAsJSON_HappyPath(t *testing.T) {
	t.Parallel()
	idx := buildIndexForScanTest(t)
	entry := &recordlayer.IndexEntry{
		Index: idx,
		Key:   tuple.Tuple{int64(100), int64(42)}, // price=100, pk=42
		Value: tuple.Tuple{},
	}
	var buf bytes.Buffer
	if err := writeIndexEntryAsJSON(&buf, entry); err != nil {
		t.Fatalf("writeIndexEntryAsJSON: %v", err)
	}
	// Single-line NDJSON with trailing newline.
	line := buf.String()
	if !strings.HasSuffix(line, "\n") {
		t.Errorf("output missing trailing newline (breaks wc -l): %q", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Errorf("expected single-line NDJSON, got %d newlines: %q",
			strings.Count(line, "\n"), line)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimRight(line, "\n")), &obj); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %q", err, line)
	}
	for _, k := range []string{"index", "index_values", "primary_key", "value"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("missing key %q in envelope: %v", k, obj)
		}
	}
	if obj["index"] != "Order$price" {
		t.Errorf("index = %v; want Order$price", obj["index"])
	}
	if obj["index_values"] != "100" {
		t.Errorf("index_values = %v; want 100", obj["index_values"])
	}
	if obj["primary_key"] != "42" {
		t.Errorf("primary_key = %v; want 42", obj["primary_key"])
	}
}

// TestWriteIndexEntryAsJSON_BinaryPKIsValidJSON is the index-side
// counterpart to the fix in record.go: %q on a byte-containing tuple
// would emit \x00 which is invalid JSON. The fix went into index_scan
// at the same time — this test guards it.
func TestWriteIndexEntryAsJSON_BinaryPKIsValidJSON(t *testing.T) {
	t.Parallel()
	idx := buildIndexForScanTest(t)
	entry := &recordlayer.IndexEntry{
		Index: idx,
		// NUL-containing bytes where the indexed value would be.
		Key:   tuple.Tuple{[]byte{0x00, 0x42}, int64(1)},
		Value: tuple.Tuple{},
	}
	var buf bytes.Buffer
	if err := writeIndexEntryAsJSON(&buf, entry); err != nil {
		t.Fatalf("writeIndexEntryAsJSON: %v", err)
	}
	// Any jq consumer needs this to parse.
	var obj map[string]any
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &obj); err != nil {
		t.Fatalf("invalid JSON produced for binary PK: %v\nraw: %q", err, buf.String())
	}
	// []byte tuple elements render as hex (per formatTupleElement).
	if values, _ := obj["index_values"].(string); values != "0042" {
		t.Errorf("index_values for binary key = %q; want 0042", values)
	}
}
