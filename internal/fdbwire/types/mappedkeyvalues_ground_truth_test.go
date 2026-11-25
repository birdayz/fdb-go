package types

import (
	"bytes"
	"testing"
)

// GetMappedKeyValuesReply is the only reply in the schema whose rows carry a
// flatbuffers UNION whose alternatives are themselves TABLES
// (MappedReqAndResultRef = std::variant<GetValueReqAndResultRef,
// GetRangeReqAndResultRef>, FDBTypes.h:893). Every other union in the schema has
// scalar or byte-vector alternatives, which the generator reaches through a
// different emit arm. So these vectors are the only thing standing between the
// struct-alternative codegen and a silent mis-parse: a union arm read as a
// count-prefixed byte vector still decodes without error, it just yields garbage.
//
// The rows deliberately cover the shapes a happy-path probe never reaches — a
// point lookup whose mapped key is ABSENT, and a range lookup that matched
// NOTHING. Both are legal on the wire and both look identical to a populated row
// unless the union tag and the nested table are actually decoded.
//
// Bytes come from the real C++ ObjectWriter via cmd/fdb-schema-extract; they are
// not hand-rolled.

func findVector(t *testing.T, name string) testVectorEntry {
	t.Helper()
	for _, v := range loadTestVectors(t) {
		if v.Name == name {
			return v
		}
	}
	// A missing vector is a broken net, never a skip: it would otherwise render
	// every assertion below vacuously true.
	t.Fatalf("vector %s missing from testdata.json", name)
	return testVectorEntry{}
}

func decodeMappedReply(t *testing.T, name string) *GetMappedKeyValuesReply {
	t.Helper()
	raw := vectorBytes(t, findVector(t, name))
	var r GetMappedKeyValuesReply
	if err := r.UnmarshalFDB(raw); err != nil {
		t.Fatalf("%s: UnmarshalFDB on C++ bytes: %v", name, err)
	}
	return &r
}

// TestMappedReplyGetValueArm pins the tag-1 (GetValueReqAndResultRef) arm,
// including the absent-result row.
func TestMappedReplyGetValueArm(t *testing.T) {
	t.Parallel()
	r := decodeMappedReply(t, "GetMappedKeyValuesReply_getvalue")

	if r.Version != 111222 {
		t.Errorf("version = %d, want 111222", r.Version)
	}
	if r.More {
		t.Error("more = true, want false")
	}
	if len(r.Data) != 2 {
		t.Fatalf("decoded %d rows, want 2 — the row vector itself did not parse", len(r.Data))
	}

	// Row 0: index entry preserved AND the mapped value present.
	row := r.Data[0]
	if !bytes.Equal(row.KeyValue.Key, []byte("idx_a")) || !bytes.Equal(row.KeyValue.Value, []byte("pk_a")) {
		t.Errorf("row0 base KeyValueRef = (%q,%q), want (idx_a,pk_a) — the "+
			"KeyValueRef base of MappedKeyValueRef (FDBTypes.h:924-926) did not decode",
			row.KeyValue.Key, row.KeyValue.Value)
	}
	if row.ReqAndResultTag != 1 {
		t.Fatalf("row0 union tag = %d, want 1 (GetValueReqAndResultRef)", row.ReqAndResultTag)
	}
	if !bytes.Equal(row.ReqAndResultAlt0.Key, []byte("rec_a")) {
		t.Errorf("row0 mapped key = %q, want rec_a", row.ReqAndResultAlt0.Key)
	}
	if !row.ReqAndResultAlt0.HasResult {
		t.Error("row0 HasResult = false, want true")
	}
	if !bytes.Equal(row.ReqAndResultAlt0.Result, []byte("record_a")) {
		t.Errorf("row0 mapped value = %q, want record_a", row.ReqAndResultAlt0.Result)
	}

	// Row 1: the mapper resolved to a key that does not exist. The row is still
	// present with its index entry intact; only the Optional<ValueRef> is absent.
	// A decoder that ignored the presence tag would report an empty value here and
	// be indistinguishable from a real empty record.
	row = r.Data[1]
	if !bytes.Equal(row.KeyValue.Key, []byte("idx_b")) {
		t.Errorf("row1 base key = %q, want idx_b", row.KeyValue.Key)
	}
	if row.ReqAndResultTag != 1 {
		t.Fatalf("row1 union tag = %d, want 1", row.ReqAndResultTag)
	}
	if !bytes.Equal(row.ReqAndResultAlt0.Key, []byte("rec_missing")) {
		t.Errorf("row1 mapped key = %q, want rec_missing", row.ReqAndResultAlt0.Key)
	}
	if row.ReqAndResultAlt0.HasResult {
		t.Errorf("row1 HasResult = true (value %q), want false — an ABSENT mapped "+
			"value must stay distinguishable from an empty one",
			row.ReqAndResultAlt0.Result)
	}
}

// TestMappedReplyGetRangeArm pins the tag-2 (GetRangeReqAndResultRef) arm,
// including the matched-nothing row. This arm nests two more levels than the
// GetValue arm — a RangeResultRef holding a vector of KeyValueRef tables — so it
// is what proves the struct-alternative emit recurses rather than bottoming out.
func TestMappedReplyGetRangeArm(t *testing.T) {
	t.Parallel()
	r := decodeMappedReply(t, "GetMappedKeyValuesReply_getrange")

	if r.Version != 333444 {
		t.Errorf("version = %d, want 333444", r.Version)
	}
	if !r.More {
		t.Error("more = false, want true")
	}
	if len(r.Data) != 2 {
		t.Fatalf("decoded %d rows, want 2", len(r.Data))
	}

	row := r.Data[0]
	if !bytes.Equal(row.KeyValue.Key, []byte("idx_r")) {
		t.Errorf("row0 base key = %q, want idx_r", row.KeyValue.Key)
	}
	if row.ReqAndResultTag != 2 {
		t.Fatalf("row0 union tag = %d, want 2 (GetRangeReqAndResultRef)", row.ReqAndResultTag)
	}
	gr := row.ReqAndResultAlt1
	if !bytes.Equal(gr.Begin.Key, []byte("pfx_r\x00")) || !gr.Begin.OrEqual || gr.Begin.Offset != 1 {
		t.Errorf("row0 begin selector = (%q,%v,%d), want (pfx_r\\x00,true,1)",
			gr.Begin.Key, gr.Begin.OrEqual, gr.Begin.Offset)
	}
	if !bytes.Equal(gr.End.Key, []byte("pfx_r\xff")) {
		t.Errorf("row0 end selector key = %q, want pfx_r\\xff", gr.End.Key)
	}
	if len(gr.Result.Data) != 2 {
		t.Fatalf("row0 nested range result has %d rows, want 2 — the "+
			"VectorRef<KeyValueRef> inside RangeResultRef did not decode", len(gr.Result.Data))
	}
	if !bytes.Equal(gr.Result.Data[0].Key, []byte("pfx_r_1")) ||
		!bytes.Equal(gr.Result.Data[0].Value, []byte("sub_1")) {
		t.Errorf("row0 sub[0] = (%q,%q), want (pfx_r_1,sub_1)",
			gr.Result.Data[0].Key, gr.Result.Data[0].Value)
	}
	if !bytes.Equal(gr.Result.Data[1].Key, []byte("pfx_r_2")) ||
		!bytes.Equal(gr.Result.Data[1].Value, []byte("sub_2")) {
		t.Errorf("row0 sub[1] = (%q,%q), want (pfx_r_2,sub_2)",
			gr.Result.Data[1].Key, gr.Result.Data[1].Value)
	}
	if gr.Result.More {
		t.Error("row0 nested result.more = true, want false")
	}

	// Row 1: the mapped range matched nothing. The selectors still describe the
	// range that was probed, and the result is empty with more=false — that pair is
	// what tells a caller "this prefix is genuinely empty" rather than "truncated".
	row = r.Data[1]
	if row.ReqAndResultTag != 2 {
		t.Fatalf("row1 union tag = %d, want 2", row.ReqAndResultTag)
	}
	gr = row.ReqAndResultAlt1
	if !bytes.Equal(gr.Begin.Key, []byte("pfx_e\x00")) {
		t.Errorf("row1 begin key = %q, want pfx_e\\x00 — an empty match must still "+
			"report the range it probed", gr.Begin.Key)
	}
	if len(gr.Result.Data) != 0 {
		t.Errorf("row1 nested range result has %d rows, want 0", len(gr.Result.Data))
	}
	if gr.Result.More {
		t.Error("row1 nested result.more = true, want false")
	}
}

// TestMappedReplyEmpty pins the empty index range: a well-formed reply with zero
// rows. It is the shape that makes every other assertion in this file vacuous if
// the row vector silently decodes to nothing, so it is asserted explicitly rather
// than inferred.
func TestMappedReplyEmpty(t *testing.T) {
	t.Parallel()
	r := decodeMappedReply(t, "GetMappedKeyValuesReply_empty")

	if len(r.Data) != 0 {
		t.Errorf("decoded %d rows, want 0", len(r.Data))
	}
	if r.Version != 555666 {
		t.Errorf("version = %d, want 555666 — a zero-row reply must still carry "+
			"its read version", r.Version)
	}
	if r.More {
		t.Error("more = true, want false")
	}
}

// TestMappedReplyArmsDiffer guards the failure mode that all three tests above
// would miss individually: a decoder that hardcodes one arm. The two populated
// vectors must disagree on the union tag. If the tags ever agree, one of the
// vectors stopped exercising the arm it was written for and the arm-specific
// assertions became a test of the other arm.
func TestMappedReplyArmsDiffer(t *testing.T) {
	t.Parallel()
	gv := decodeMappedReply(t, "GetMappedKeyValuesReply_getvalue")
	grr := decodeMappedReply(t, "GetMappedKeyValuesReply_getrange")

	if len(gv.Data) == 0 || len(grr.Data) == 0 {
		t.Fatal("one of the mapped reply vectors decoded to zero rows; the " +
			"per-arm assertions elsewhere in this file are vacuous")
	}
	if gv.Data[0].ReqAndResultTag == grr.Data[0].ReqAndResultTag {
		t.Fatalf("both vectors decode to union tag %d — the two arms are no longer "+
			"being distinguished", gv.Data[0].ReqAndResultTag)
	}
}
