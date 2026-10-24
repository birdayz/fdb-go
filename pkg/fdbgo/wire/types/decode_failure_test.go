package types

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

var le = binary.LittleEndian

// fieldPos is the absolute position of a present field of r's object.
func fieldPos(t testing.TB, r *wire.Reader, slot int) int {
	t.Helper()
	off := r.FieldOffset(slot)
	if off < 4 {
		t.Fatalf("slot %d absent", slot)
	}
	return r.ObjectPos() + off
}

// vtablePos is the absolute position of the vtable of the object at obj.
func vtablePos(d []byte, obj int) int { return obj - int(int32(le.Uint32(d[obj:]))) }

// aliasedMappedReply has n Data offsets to one row of m empty rows; the prefix goes before
// a marshaled one-row reply because offsets point forward only.
func aliasedMappedReply(t testing.TB, n, m int) []byte {
	t.Helper()
	row := MappedKeyValueRef{ReqAndResultTag: 2, ReqAndResultAlt1: GetRangeReqAndResult{
		Result: RangeResultRef{Data: make([]KeyValueRef, m)},
	}}
	tail := (&GetMappedKeyValuesReply{Data: []MappedKeyValueRef{row}}).MarshalFDB()
	r, err := wire.NewReader(tail)
	if err != nil {
		t.Fatal(err)
	}
	elem, err := r.ReadVectorElementReader(GetMappedKeyValuesReplySlotData, 0)
	if err != nil {
		t.Fatal(err)
	}

	var b []byte
	u16 := func(v int) { b = le.AppendUint16(b, uint16(v)) }
	u32 := func(v int) { b = le.AppendUint32(b, uint32(v)) }
	u32(8) // 0: root → FakeRoot at 8
	u32(int(GetMappedKeyValuesReplyFileID))
	u32(0)  // 8: FakeRoot soffset (NewReader does not read it)
	u32(16) // 12: FakeRoot field 0 → message at 28

	// 16: message vtable, Data at offset 4
	u16(4 + 2*(GetMappedKeyValuesReplySlotData+1))
	u16(8)
	for range GetMappedKeyValuesReplySlotData {
		u16(0)
	}
	u16(4)
	u32(12) // 28: message, vtable 12 bytes back
	u32(4)  // 32: Data → vector at 36
	vec := len(b)
	u32(n)
	b = append(b, make([]byte, 4*n)...)
	for len(b)%8 != 0 {
		b = append(b, 0)
	}
	target := len(b) + elem.ObjectPos()
	for i := range n {
		slot := vec + 4 + 4*i
		le.PutUint32(b[slot:], uint32(target-slot))
	}
	return append(b, tail...)
}

func decodeMapped(data []byte) (GetMappedKeyValuesReply, error) {
	var m GetMappedKeyValuesReply
	err := m.UnmarshalFDB(data)
	return m, err
}

// Aliased offsets would decode n×m rows from O(n+m) bytes; the budget stops them.
func TestDecodeAliasedVectorsExhaustBudget(t *testing.T) {
	t.Parallel()
	if got, err := decodeMapped(aliasedMappedReply(t, 3, 2)); err != nil || len(got.Data) != 3 ||
		len(got.Data[2].ReqAndResultAlt1.Result.Data) != 2 {
		t.Fatalf("small aliased reply: %d rows, %v; want 3 rows of 2", len(got.Data), err)
	}
	data := aliasedMappedReply(t, 2000, 1000)
	_, err := decodeMapped(data)
	var budgetErr *wire.DecodeBudgetError
	var decodeErr *wire.DecodeError
	if !errors.As(err, &budgetErr) || !errors.As(err, &decodeErr) || decodeErr.Type != "RangeResultRef" {
		t.Fatalf("%d-byte reply aliasing 2000×1000 rows: %v; want a budget error", len(data), err)
	}
}

// replyReader positions a reader at the reply inside an ErrorOr reply payload.
func replyReader(t *testing.T, d []byte) *wire.Reader {
	t.Helper()
	var r wire.Reader
	if err := wire.ReadErrorOrInto(d, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

// A present vector, table, optional or variant that does not resolve fails the
// decode with a DecodeError naming the field.
func TestDecodeFailsOnUnresolvableFields(t *testing.T) {
	t.Parallel()
	mapped := vectorBytes(t, findVector(t, "GetMappedKeyValuesReply_getrange"))
	withError := MarshalErrorOrValueGetKeyValuesReply(&GetKeyValuesReply{HasError: true, Error: Error{ErrorCode: 1001}})
	present := vectorBytes(t, findVector(t, "GetValueReply_present"))
	past := uint32(1 << 20)
	cases := []struct {
		name        string
		data        []byte
		at          func(d []byte) int // RelativeOffset to move past the buffer
		decode      func(r *wire.Reader) error
		typ, field  string
		fieldBounds bool
	}{
		{"vector element", mapped, func(d []byte) int {
			r := replyReader(t, d)
			p := fieldPos(t, r, GetMappedKeyValuesReplySlotData)
			return p + int(le.Uint32(d[p:])) + 4
		}, func(r *wire.Reader) error { var m GetMappedKeyValuesReply; return m.UnmarshalFromReader(r) }, "GetMappedKeyValuesReply", "Data", false},
		{"nested table", mapped, func(d []byte) int {
			e, err := replyReader(t, d).ReadVectorElementReader(GetMappedKeyValuesReplySlotData, 0)
			if err != nil {
				t.Fatal(err)
			}
			return fieldPos(t, e, MappedKeyValueRefSlotKeyValue)
		}, func(r *wire.Reader) error { var m GetMappedKeyValuesReply; return m.UnmarshalFromReader(r) }, "MappedKeyValueRef", "KeyValue", false},
		{"variant alternative", mapped, func(d []byte) int {
			e, err := replyReader(t, d).ReadVectorElementReader(GetMappedKeyValuesReplySlotData, 0)
			if err != nil {
				t.Fatal(err)
			}
			return fieldPos(t, e, MappedKeyValueRefSlotReqAndResult+1)
		}, func(r *wire.Reader) error { var m GetMappedKeyValuesReply; return m.UnmarshalFromReader(r) }, "MappedKeyValueRef", "ReqAndResult", false},
		{"optional table", withError, func(d []byte) int {
			return fieldPos(t, replyReader(t, d), GetKeyValuesReplySlotError+1)
		}, func(r *wire.Reader) error { var m GetKeyValuesReply; return m.UnmarshalFromReader(r) }, "GetKeyValuesReply", "Error", false},
		{"optional bytes", present, func(d []byte) int {
			return fieldPos(t, replyReader(t, d), GetValueReplySlotValue+1)
		}, func(r *wire.Reader) error { var m GetValueReply; return m.UnmarshalFromReader(r) }, "GetValueReply", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := bytes.Clone(tc.data)
			if err := tc.decode(replyReader(t, d)); err != nil {
				t.Fatalf("well-formed reply: %v", err)
			}
			le.PutUint32(d[tc.at(d):], past)
			err := tc.decode(replyReader(t, d))
			var decodeErr *wire.DecodeError
			if !errors.As(err, &decodeErr) || decodeErr.Type != tc.typ || decodeErr.Field != tc.field {
				t.Fatalf("got %v; want a DecodeError for %s.%s", err, tc.typ, tc.field)
			}
			var bounds *wire.FieldBoundsError
			if errors.As(err, &bounds) != tc.fieldBounds {
				t.Fatalf("FieldBoundsError in %v: %v, want %v", err, !tc.fieldBounds, tc.fieldBounds)
			}
		})
	}
}

// A vector parsed on its own fails on an element that does not resolve.
func TestParseVectorFromReaderFailsOnElement(t *testing.T) {
	t.Parallel()
	req := CoordOpenDatabaseRequest{Coordinators: []NetworkAddress{
		{Ip: IPAddress{AddrTag: 1, AddrAlt0: 0x0a000001}, Port: 4500},
		{Ip: IPAddress{AddrTag: 1, AddrAlt0: 0x0a000002}, Port: 4500},
	}}
	d := req.MarshalFDB()
	r, err := wire.NewReader(d)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseNetworkAddressVectorFromReader(r, OpenDatabaseCoordRequestSlotCoordinators); err != nil || len(got) != 2 {
		t.Fatalf("well-formed vector: %d addresses, %v", len(got), err)
	}
	p := fieldPos(t, r, OpenDatabaseCoordRequestSlotCoordinators)
	le.PutUint32(d[p+int(le.Uint32(d[p:]))+8:], 1<<20) // second element
	got, err := ParseNetworkAddressVectorFromReader(r, OpenDatabaseCoordRequestSlotCoordinators)
	var decodeErr *wire.DecodeError
	if !errors.As(err, &decodeErr) || decodeErr.Type != "[]NetworkAddress" || got != nil {
		t.Fatalf("got %d addresses, %v; want a DecodeError", len(got), err)
	}
}

// decoderFor returns the generated decoder for the type a vector is named after.
func decoderFor(t *testing.T, vectorName string) func([]byte) error {
	t.Helper()
	name, _, _ := strings.Cut(vectorName, "_")
	for _, d := range generatedDecoders {
		if d.name == name {
			return d.unmarshal
		}
	}
	t.Fatalf("no generated decoder for %s", vectorName)
	return nil
}

// Every C++-written vector decodes with no error.
func TestGroundTruthVectorsDecode(t *testing.T) {
	t.Parallel()
	for _, v := range loadTestVectors(t) {
		if err := decoderFor(t, v.Name)(vectorBytes(t, v)); err != nil {
			t.Errorf("%s: %v", v.Name, err)
		}
	}
}

// The densest legitimate shape decodes within the budget, and the budget counts
// each decoded element once.
func TestDenseReplyWithinBudget(t *testing.T) {
	t.Parallel()
	const rows, kvs = 64, 64
	reply := GetMappedKeyValuesReply{Data: make([]MappedKeyValueRef, rows)}
	for i := range reply.Data {
		reply.Data[i] = MappedKeyValueRef{ReqAndResultTag: 2, ReqAndResultAlt1: GetRangeReqAndResult{
			Result: RangeResultRef{Data: make([]KeyValueRef, kvs)},
		}}
	}
	d := reply.MarshalFDB()
	r, err := wire.NewReader(d)
	if err != nil {
		t.Fatal(err)
	}
	b := wire.NewDecodeBudget(d)
	before := b.Left()
	var got GetMappedKeyValuesReply
	if err := got.decode(r, &b); err != nil {
		t.Fatalf("%d-byte dense reply: %v", len(d), err)
	}
	const elements = rows + rows*kvs
	if used := before - b.Left(); used != elements || len(got.Data) != rows {
		t.Fatalf("budget used %d for %d decoded elements (%d rows)", used, elements, len(got.Data))
	}
	if 4*elements > len(d) {
		t.Fatalf("%d elements in %d bytes: a legitimate reply would exceed len/4", elements, len(d))
	}
}

// A reply from a schema with fewer fields decodes; the missing fields are zero.
func TestShortVTablesDecode(t *testing.T) {
	t.Parallel()
	vector := vectorBytes(t, findVector(t, "GetMappedKeyValuesReply_getrange"))
	shorten := func(d []byte, obj, slots int) {
		le.PutUint16(d[vtablePos(d, obj):], uint16(4+2*slots))
	}
	for slots := 0; slots <= MappedKeyValueRefSlotReqAndResult+1; slots++ {
		d := bytes.Clone(vector)
		e, err := replyReader(t, d).ReadVectorElementReader(GetMappedKeyValuesReplySlotData, 0)
		if err != nil {
			t.Fatal(err)
		}
		shorten(d, e.ObjectPos(), slots)
		var m GetMappedKeyValuesReply
		if err := m.UnmarshalFromReader(replyReader(t, d)); err != nil {
			t.Fatalf("row vtable cut to %d slots: %v", slots, err)
		}
		if m.Data[0].ReqAndResultTag != 0 {
			t.Fatalf("row vtable cut to %d slots: tag %d, want 0", slots, m.Data[0].ReqAndResultTag)
		}
	}
	d := bytes.Clone(vector)
	shorten(d, replyReader(t, d).ObjectPos(), GetMappedKeyValuesReplySlotData)
	var m GetMappedKeyValuesReply
	if err := m.UnmarshalFromReader(replyReader(t, d)); err != nil || m.Data != nil || m.Version != 0 {
		t.Fatalf("reply vtable cut before Data: %d rows, version %d, %v", len(m.Data), m.Version, err)
	}
}

// FuzzUnmarshalFDB feeds raw bytes to every generated decoder. A decode returns
// nil or a DecodeError, and allocates at most a constant factor of its input.
func FuzzUnmarshalFDB(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 64))
	for _, v := range loadTestVectors(f) {
		f.Add(vectorBytes(f, v))
	}
	f.Add(aliasedMappedReply(f, 2000, 1000))
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, err := wire.NewReader(data); err != nil {
			return // no message to decode
		}
		limit := uint64(1024*len(data) + 1<<20)
		var before, after runtime.MemStats
		for _, d := range generatedDecoders {
			runtime.ReadMemStats(&before)
			err := d.unmarshal(data)
			runtime.ReadMemStats(&after)
			var decodeErr *wire.DecodeError
			if err != nil && !errors.As(err, &decodeErr) {
				t.Fatalf("%s: %v is not a DecodeError", d.name, err)
			}
			if n := after.TotalAlloc - before.TotalAlloc; n > limit {
				t.Fatalf("%s: decoding %d bytes allocated %d (limit %d)", d.name, len(data), n, limit)
			}
		}
	})
}

// FuzzParseKeyValueRefStringVector: rows parse only from a blob they fill exactly.
func FuzzParseKeyValueRefStringVector(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0})
	f.Add(packKVVector([]KeyValueRef{{Key: []byte("k"), Value: []byte("v")}, {Key: []byte("key"), Value: nil}}))
	f.Fuzz(func(t *testing.T, data []byte) {
		rows, err := ParseKeyValueRefStringVector(data)
		if err != nil {
			var vecErr *wire.StringVectorError
			if !errors.As(err, &vecErr) || rows != nil {
				t.Fatalf("%v (with %d rows) is not a StringVectorError", err, len(rows))
			}
			return
		}
		if len(data) == 0 {
			if rows != nil {
				t.Fatalf("empty blob gave %d rows", len(rows))
			}
			return
		}
		if again := packKVVector(rows); !bytes.Equal(again, data) {
			t.Fatalf("rows re-pack to %x, not the input %x", again, data)
		}
	})
}

// An Optional is present only with tag 1 and its value slot, and a variant only
// with a known tag and its value slot; anything else loads as absent, as in C++.
func TestOptionalAndVariantTags(t *testing.T) {
	t.Parallel()
	value := vectorBytes(t, findVector(t, "GetValueReply_present"))
	decodeValue := func(d []byte) GetValueReply {
		t.Helper()
		var m GetValueReply
		if err := m.UnmarshalFromReader(replyReader(t, d)); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := decodeValue(value); !m.HasValue || len(m.Value) == 0 {
		t.Fatalf("well-formed reply: HasValue %t, %d value bytes", m.HasValue, len(m.Value))
	}
	d := bytes.Clone(value)
	d[fieldPos(t, replyReader(t, d), GetValueReplySlotValue)] = 2
	if m := decodeValue(d); m.HasValue || m.Value != nil {
		t.Fatalf("value tag 2: HasValue %t, %q; want absent", m.HasValue, m.Value)
	}
	d = bytes.Clone(value)
	le.PutUint16(d[vtablePos(d, replyReader(t, d).ObjectPos())+4+2*(GetValueReplySlotValue+1):], 0)
	if m := decodeValue(d); m.HasValue || m.Value != nil {
		t.Fatalf("value tag 1 without its value slot: HasValue %t, %q; want absent", m.HasValue, m.Value)
	}

	inline := MarshalErrorOrInlineError(1001, 0.5)
	if e := wire.ReadInlineReplyError(replyReader(t, inline), GetValueReplySlotError); e == nil || e.Code != 1001 {
		t.Fatalf("inline error with tag 1: %v, want code 1001", e)
	}
	d = bytes.Clone(inline)
	d[fieldPos(t, replyReader(t, d), GetValueReplySlotError)] = 2
	if e := wire.ReadInlineReplyError(replyReader(t, d), GetValueReplySlotError); e != nil {
		t.Fatalf("inline error with tag 2: %v, want none", e)
	}
	if m := decodeValue(d); m.HasError {
		t.Fatalf("inline error with tag 2 decodes as present: %+v", m.Error)
	}
	// An error code past the buffer: the helper reads no error, the generated decoder fails.
	d = bytes.Clone(inline)
	errTable, err := replyReader(t, d).ReadNestedReader(GetValueReplySlotError + 1)
	if err != nil {
		t.Fatal(err)
	}
	le.PutUint16(d[vtablePos(d, errTable.ObjectPos())+4:], 0xFFF0)
	if e := wire.ReadInlineReplyError(replyReader(t, d), GetValueReplySlotError); e != nil {
		t.Fatalf("inline error code past the buffer: %v, want none", e)
	}
	var badCode GetValueReply
	var decodeErr *wire.DecodeError
	if err := badCode.UnmarshalFromReader(replyReader(t, d)); !errors.As(err, &decodeErr) {
		t.Fatalf("generated decode of an error code past the buffer: %v; want a DecodeError", err)
	}

	d = bytes.Clone(vectorBytes(t, findVector(t, "GetMappedKeyValuesReply_getrange")))
	e, err := replyReader(t, d).ReadVectorElementReader(GetMappedKeyValuesReplySlotData, 0)
	if err != nil {
		t.Fatal(err)
	}
	d[fieldPos(t, e, MappedKeyValueRefSlotReqAndResult)] = 3
	var m GetMappedKeyValuesReply
	if err := m.UnmarshalFromReader(replyReader(t, d)); err != nil || m.Data[0].ReqAndResultTag != 0 {
		t.Fatalf("variant tag 3: %v, tag %d; want no alternative", err, m.Data[0].ReqAndResultTag)
	}
}

// KeyRangeRef's hand-written decoder fails on a key outside the buffer.
func TestKeyRangeRefDecodeFailsOutsideBuffer(t *testing.T) {
	t.Parallel()
	d := vectorBytes(t, findVector(t, "CommitTransactionRequest_single_set"))
	var req CommitTransactionRequest
	if err := req.UnmarshalFDB(d); err != nil || len(req.Transaction.WriteConflictRanges) == 0 {
		t.Fatalf("well-formed request: %v, %d write conflict ranges", err, len(req.Transaction.WriteConflictRanges))
	}
	r, err := wire.NewReader(d)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := r.ReadNestedReader(CommitTransactionRequestSlotTransaction)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := tr.ReadVectorElementReader(CommitTransactionRefSlotWriteConflictRanges, 0)
	if err != nil {
		t.Fatal(err)
	}
	le.PutUint32(d[fieldPos(t, kr, KeyRangeRefSlotBegin):], 1<<20)
	err = req.UnmarshalFDB(d)
	var decodeErr *wire.DecodeError
	var bounds *wire.FieldBoundsError
	if !errors.As(err, &decodeErr) || decodeErr.Type != "KeyRangeRef" || !errors.As(err, &bounds) {
		t.Fatalf("got %v; want a KeyRangeRef DecodeError", err)
	}
}

// An ErrorOr error whose code lies outside the buffer is a decode error, not an
// FDB error with code 0.
func TestErrorOrErrorCodeOutsideBuffer(t *testing.T) {
	t.Parallel()
	d := (&ErrorOrError{ErrorCode: 1020}).MarshalFDB()
	var fdbErr *wire.FDBError
	if err := wire.ReadErrorOrInto(d, &wire.Reader{}); !errors.As(err, &fdbErr) || fdbErr.Code != 1020 {
		t.Fatalf("well-formed error: %v", err)
	}
	root, err := wire.ReaderAtRootObject(d)
	if err != nil {
		t.Fatal(err)
	}
	e, err := root.ReadNestedReader(1)
	if err != nil {
		t.Fatal(err)
	}
	le.PutUint16(d[vtablePos(d, e.ObjectPos())+4:], 0xFFF0)
	err = wire.ReadErrorOrInto(d, &wire.Reader{})
	var decodeErr *wire.DecodeError
	if !errors.As(err, &decodeErr) || errors.As(err, &fdbErr) {
		t.Fatalf("error code past the buffer: %v; want a DecodeError", err)
	}
}

// An Optional of a table with no Go type decodes as presence only.
func TestOptionalTableWithoutGoType(t *testing.T) {
	t.Parallel()
	var m ClientDBInfo
	if err := m.UnmarshalFDB((&ClientDBInfo{HasEncryptKeyProxy: true, EncryptKeyProxy: []byte("ekp")}).MarshalFDB()); err != nil {
		t.Fatal(err)
	}
	if !m.HasEncryptKeyProxy || m.EncryptKeyProxy != nil {
		t.Fatalf("encryptKeyProxy: present %t, %q; want present and not decoded", m.HasEncryptKeyProxy, m.EncryptKeyProxy)
	}
}
