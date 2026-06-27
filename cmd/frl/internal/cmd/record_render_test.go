package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// TestWriteRecordAsJSON_ValidJSONWithNUL is a regression test: the renderer
// must always produce valid JSON, even when the primary key contains bytes
// (like NUL) that Go's fmt %q would emit as non-JSON escapes like `\x00`.
// Without the json.Marshal fix, jq breaks mid-stream with "invalid escape".
func TestWriteRecordAsJSON_ValidJSONWithNUL(t *testing.T) {
	t.Parallel()
	rec := &recordlayer.FDBStoredRecord[proto.Message]{
		PrimaryKey: tuple.Tuple{"has\x00nul"},
		RecordType: &recordlayer.RecordType{Name: "Order"},
		Record:     &gen.Order{OrderId: proto.Int64(42)},
	}
	var buf bytes.Buffer
	if err := writeRecordAsJSON(&buf, rec); err != nil {
		t.Fatalf("writeRecordAsJSON: %v", err)
	}
	line := strings.TrimRight(buf.String(), "\n")
	// json.Unmarshal rejects invalid escapes — this is the contract we care
	// about. Go's fmt %q would have emitted \x00 here, which fails parsing.
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("output is not valid JSON: %v\nraw: %q", err, line)
	}
	if pk, _ := obj["primary_key"].(string); pk != "has\x00nul" {
		t.Errorf("primary_key = %q; want %q", pk, "has\x00nul")
	}
	if rt, _ := obj["record_type"].(string); rt != "Order" {
		t.Errorf("record_type = %q; want Order", rt)
	}
}

// TestWriteRecordAsJSON_HappyPath locks in the one-line envelope shape so
// streaming consumers (jq, wc -l, awk) stay stable.
func TestWriteRecordAsJSON_HappyPath(t *testing.T) {
	t.Parallel()
	rec := &recordlayer.FDBStoredRecord[proto.Message]{
		PrimaryKey: tuple.Tuple{int64(42)},
		RecordType: &recordlayer.RecordType{Name: "Order"},
		Record:     &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(100)},
	}
	var buf bytes.Buffer
	if err := writeRecordAsJSON(&buf, rec); err != nil {
		t.Fatalf("writeRecordAsJSON: %v", err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output missing trailing newline (breaks `wc -l`): %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("output must be single-line NDJSON; got %d newlines: %q",
			strings.Count(out, "\n"), out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimRight(out, "\n")), &obj); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %q", err, out)
	}
	for _, k := range []string{"primary_key", "record_type", "record"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("missing key %q in %v", k, obj)
		}
	}
}

// TestWriteRecordAsJSON_NilRecordType handles the "universal index" /
// orphaned-record case where RecordType is nil. Should still produce valid
// JSON with an empty record_type string.
func TestWriteRecordAsJSON_NilRecordType(t *testing.T) {
	t.Parallel()
	rec := &recordlayer.FDBStoredRecord[proto.Message]{
		PrimaryKey: tuple.Tuple{int64(1)},
		RecordType: nil,
		Record:     &gen.Order{OrderId: proto.Int64(1)},
	}
	var buf bytes.Buffer
	if err := writeRecordAsJSON(&buf, rec); err != nil {
		t.Fatalf("writeRecordAsJSON: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &obj); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if rt, _ := obj["record_type"].(string); rt != "" {
		t.Errorf("record_type = %q; want empty", rt)
	}
}
