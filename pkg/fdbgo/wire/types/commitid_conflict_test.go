package types

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// CommitID.conflictingKRIndices is Optional<Standalone<VectorRef<int>>>
// (CommitProxyInterface.h:193): a count followed by that many 4-byte ints
// (flat_buffers.h:1205-1232). Read as a byte string, the count was taken as a byte
// length and the indices were lost.

// TestCommitIDConflictIndices_GroundTruth decodes the C++-serialized conflict reply,
// whose constructor (cmd/fdb-schema-extract/main.cpp) set the indices {0, 2}.
func TestCommitIDConflictIndices_GroundTruth(t *testing.T) {
	t.Parallel()
	var found bool
	for _, vec := range loadTestVectors(t) {
		if vec.Name != "CommitID_conflict" {
			continue
		}
		found = true
		var r wire.Reader
		if err := wire.ReadErrorOrInto(vectorBytes(t, vec), &r); err != nil {
			t.Fatalf("ReadErrorOrInto: %v", err)
		}
		var m CommitID
		if err := m.UnmarshalFromReader(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if m.Version != -1 || !m.HasConflictingKRIndices || !reflect.DeepEqual(m.ConflictingKRIndices, []int32{0, 2}) {
			t.Fatalf("decoded %+v, want version -1 and indices [0 2]", m)
		}
	}
	if !found {
		t.Fatal("no CommitID_conflict vector in testdata.json")
	}
}

// TestCommitIDConflictIndices_RoundTrip pins the encoder against the decoder for the
// shapes a proxy sends: absent, empty and several indices, including negative and wide
// values that a byte-wise decode would truncate.
func TestCommitIDConflictIndices_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, in := range []CommitID{
		{Version: 42, TxnBatchId: 7},
		{Version: -1, HasConflictingKRIndices: true},
		{Version: -1, HasConflictingKRIndices: true, ConflictingKRIndices: []int32{3}},
		{
			Version: -1, TxnBatchId: 2, HasMetadataVersion: true, MetadataVersion: []byte("mv"),
			HasConflictingKRIndices: true, ConflictingKRIndices: []int32{0, 2, 1 << 20, -5, 2},
		},
	} {
		var out CommitID
		if err := out.UnmarshalFDB(in.MarshalFDB()); err != nil {
			t.Fatalf("%+v: %v", in, err)
		}
		if len(out.ConflictingKRIndices) == 0 {
			out.ConflictingKRIndices = nil // an empty vector decodes as an empty slice
		}
		if !reflect.DeepEqual(out, in) {
			t.Fatalf("round trip\n got %+v\nwant %+v", out, in)
		}
		pooled := in.MarshalFDBPooled(make([]byte, 0, 512))
		if !reflect.DeepEqual(pooled, in.MarshalFDB()) {
			t.Fatalf("%+v: pooled and fresh encodings differ", in)
		}
	}
}
