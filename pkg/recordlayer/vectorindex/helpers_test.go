package vectorindex

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// Test helpers recordlayer's own tests define for themselves.

func baseBuilder() *recordlayer.RecordMetaDataBuilder {
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	return b
}

// fakeRangeIterator is a deterministic rangeIterator: it returns
// `advancesLeft` trues from Advance(), then false, and surfaces `getErr` from
// Get() once exhausted, placing an FDB error at an exact scan position.
type fakeRangeIterator struct {
	advancesLeft int
	getErr       error
}

func (f *fakeRangeIterator) Advance() bool {
	if f.advancesLeft > 0 {
		f.advancesLeft--
		return true
	}
	return false
}

func (f *fakeRangeIterator) Get() (fdb.KeyValue, error) {
	if f.advancesLeft <= 0 {
		return fdb.KeyValue{}, f.getErr
	}
	return fdb.KeyValue{}, nil
}

func contPos(p int) []byte { return []byte{0x00, 0x00, 0x00, byte(p)} }

// requireContinuationParseError asserts err is a *ContinuationParseError with
// the given Java wording and raw bytes, and that it wraps an unmarshal cause.
func requireContinuationParseError(t *testing.T, err error, wantMessage string, wantRaw []byte) {
	t.Helper()
	var parseErr *recordlayer.ContinuationParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("want *ContinuationParseError, got %T: %v", err, err)
	}
	if !strings.HasPrefix(parseErr.Error(), wantMessage+" (raw_bytes=") {
		t.Errorf("Error() = %q, want prefix %q (Java's RecordCoreException wording)", parseErr.Error(), wantMessage)
	}
	if string(parseErr.RawBytes) != string(wantRaw) {
		t.Errorf("RawBytes = %x, want %x", parseErr.RawBytes, wantRaw)
	}
	if parseErr.Unwrap() == nil {
		t.Error("Unwrap() = nil, want wrapped unmarshal error")
	}
}

func specSubspaceFuzz() subspace.Subspace {
	return subspace.FromBytes([]byte("fuzz-hnsw-node"))
}

// sameTuple reports element-wise equality.
func sameTuple(a, b tuple.Tuple) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func tuplesEqual(a, b tuple.Tuple) bool {
	return string(a.Pack()) == string(b.Pack())
}
