package types

import (
	"bytes"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

func TestPresenceOnlyOptionalValueSlotOutsideBuffer(t *testing.T) {
	t.Parallel()
	root := func(t *testing.T, data []byte) *wire.Reader {
		t.Helper()
		r, err := wire.NewReader(data)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	cases := []struct {
		name, typ string
		data      []byte
		tagSlot   int
		reader    func(*testing.T, []byte) *wire.Reader
		decode    func(*wire.Reader) (bool, []byte, error)
	}{
		{
			name: "commit cost", typ: "CommitTransactionRequest",
			data:    vectorBytes(t, findVector(t, "CommitTransactionRequest_commit_cost_estimation")),
			tagSlot: CommitTransactionRequestSlotCommitCostEstimation, reader: root,
			decode: func(r *wire.Reader) (bool, []byte, error) {
				var m CommitTransactionRequest
				err := m.UnmarshalFromReader(r)
				return m.HasCommitCostEstimation, m.CommitCostEstimation, err
			},
		},
		{
			name: "encrypt key proxy", typ: "ClientDBInfo",
			data:    (&ClientDBInfo{HasEncryptKeyProxy: true, EncryptKeyProxy: []byte("opaque")}).MarshalFDB(),
			tagSlot: ClientDBInfoSlotEncryptKeyProxy, reader: root,
			decode: func(r *wire.Reader) (bool, []byte, error) {
				var m ClientDBInfo
				err := m.UnmarshalFromReader(r)
				return m.HasEncryptKeyProxy, m.EncryptKeyProxy, err
			},
		},
		{
			name: "tenant token", typ: "TenantInfo",
			data:    (&CommitTransactionRequest{TenantInfo: TenantInfo{HasToken: true, Token: []byte("opaque")}}).MarshalFDB(),
			tagSlot: TenantInfoSlotToken,
			reader: func(t *testing.T, data []byte) *wire.Reader {
				t.Helper()
				r, err := root(t, data).ReadNestedReader(CommitTransactionRequestSlotTenantInfo)
				if err != nil {
					t.Fatal(err)
				}
				return r
			},
			decode: func(r *wire.Reader) (bool, []byte, error) {
				var m TenantInfo
				err := m.UnmarshalFromReader(r)
				return m.HasToken, m.Token, err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// These bodies are deliberately opaque; only the pointer slot is validated.
			if present, body, err := tc.decode(tc.reader(t, tc.data)); err != nil || !present || body != nil {
				t.Fatalf("valid optional: present=%v body=%x err=%v", present, body, err)
			}
			data := bytes.Clone(tc.data)
			r := tc.reader(t, data)
			valueSlot := tc.tagSlot + 1
			if r.ReadUint8(tc.tagSlot) != 1 || fieldPos(t, r, valueSlot)+4 > len(data) {
				t.Fatal("fixture must have tag 1 and an in-bounds value slot")
			}
			le.PutUint16(data[vtablePos(data, r.ObjectPos())+4+2*valueSlot:], 0xfff0)
			r = tc.reader(t, data)
			if !r.FieldPresent(valueSlot) || r.ObjectPos()+r.FieldOffset(valueSlot) < len(data) || r.Err() != nil {
				t.Fatal("mutation must leave a present, out-of-buffer value slot without a prior reader fault")
			}
			_, _, err := tc.decode(r)
			var decodeErr *wire.DecodeError
			var bounds *wire.FieldBoundsError
			if !errors.As(err, &decodeErr) || decodeErr.Type != tc.typ ||
				!errors.As(err, &bounds) || bounds.Slot != valueSlot {
				t.Fatalf("got %v; want %s DecodeError wrapping FieldBoundsError for slot %d", err, tc.typ, valueSlot)
			}
		})
	}
}

func TestVariantTagBoundsWithAbsentValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, typ string
		data      []byte
		tagSlot   int
		knownTag  uint8
		reader    func(*testing.T, []byte) *wire.Reader
		decode    func(*wire.Reader) (uint8, error)
	}{
		{
			name: "IP address", typ: "IPAddress", tagSlot: IPAddressSlotAddr, knownTag: 1,
			data: vectorBytes(t, findVector(t, "Endpoint_secondary_address")),
			reader: func(t *testing.T, data []byte) *wire.Reader {
				t.Helper()
				r, err := wire.NewReader(data)
				if err != nil {
					t.Fatal(err)
				}
				for _, slot := range []int{EndpointSlotAddresses, NetworkAddressListSlotAddress, NetworkAddressSlotIp} {
					r, err = r.ReadNestedReader(slot)
					if err != nil {
						t.Fatal(err)
					}
				}
				return r
			},
			decode: func(r *wire.Reader) (uint8, error) {
				var m IPAddress
				err := m.UnmarshalFromReader(r)
				return m.AddrTag, err
			},
		},
		{
			name: "mapped row", typ: "MappedKeyValueRef", tagSlot: MappedKeyValueRefSlotReqAndResult, knownTag: 2,
			data: vectorBytes(t, findVector(t, "GetMappedKeyValuesReply_getrange")),
			reader: func(t *testing.T, data []byte) *wire.Reader {
				t.Helper()
				r, err := replyReader(t, data).ReadVectorElementReader(GetMappedKeyValuesReplySlotData, 0)
				if err != nil {
					t.Fatal(err)
				}
				return r
			},
			decode: func(r *wire.Reader) (uint8, error) {
				var m MappedKeyValueRef
				err := m.UnmarshalFromReader(r)
				return m.ReqAndResultTag, err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tag, err := tc.decode(tc.reader(t, tc.data)); err != nil || tag != tc.knownTag {
				t.Fatalf("valid variant: tag=%d err=%v; want tag %d", tag, err, tc.knownTag)
			}
			for _, mode := range []string{"known tag", "zero tag", "unknown tag", "absent tag", "out-of-buffer tag"} {
				t.Run(mode, func(t *testing.T) {
					t.Parallel()
					data := bytes.Clone(tc.data)
					r := tc.reader(t, data)
					vt := vtablePos(data, r.ObjectPos())
					tagPos := fieldPos(t, r, tc.tagSlot)
					le.PutUint16(data[vt+4+2*(tc.tagSlot+1):], 0)
					switch mode {
					case "zero tag":
						data[tagPos] = 0
					case "unknown tag":
						data[tagPos] = 255
					case "absent tag":
						le.PutUint16(data[vt+4+2*tc.tagSlot:], 0)
					case "out-of-buffer tag":
						le.PutUint16(data[vt+4+2*tc.tagSlot:], 0xfff0)
					}
					r = tc.reader(t, data)
					if r.FieldPresent(tc.tagSlot+1) || r.Err() != nil {
						t.Fatal("value must be absent without a prior reader fault")
					}
					if mode == "out-of-buffer tag" && (!r.FieldPresent(tc.tagSlot) || r.ObjectPos()+r.FieldOffset(tc.tagSlot) < len(data)) {
						t.Fatal("tag mutation must describe a present field outside the buffer")
					}
					tag, err := tc.decode(r)
					if mode != "out-of-buffer tag" {
						if err != nil || tag != 0 {
							t.Fatalf("absent value: tag=%d err=%v; want empty variant", tag, err)
						}
						return
					}
					var decodeErr *wire.DecodeError
					var bounds *wire.FieldBoundsError
					if !errors.As(err, &decodeErr) || decodeErr.Type != tc.typ ||
						!errors.As(err, &bounds) || bounds.Slot != tc.tagSlot {
						t.Fatalf("got %v; want %s DecodeError wrapping tag-slot FieldBoundsError", err, tc.typ)
					}
				})
			}
		})
	}
}

func TestStandaloneMappedVectorSharesNestedBudget(t *testing.T) {
	t.Parallel()
	small := aliasedMappedReply(t, 3, 2, false)
	r, err := wire.NewReader(small)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseMappedKeyValueRefVectorFromReader(r, GetMappedKeyValuesReplySlotData)
	if err != nil || len(got) != 3 {
		t.Fatalf("small aliased vector: %d rows, %v; want 3", len(got), err)
	}
	for i, row := range got {
		if row.ReqAndResultTag != 2 || len(row.ReqAndResultAlt1.Result.Data) != 2 {
			t.Fatalf("row %d: tag=%d nested rows=%d; want tag 2 and 2 rows", i, row.ReqAndResultTag, len(row.ReqAndResultAlt1.Result.Data))
		}
	}
	const outer, inner = 64, 64
	data := aliasedMappedReply(t, outer, inner, false)
	if outer+inner > len(data)/4 || outer+outer*inner <= len(data)/4 {
		t.Fatalf("%d bytes must cover individual rows but not %d expanded elements", len(data), outer+outer*inner)
	}
	r, err = wire.NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err = ParseMappedKeyValueRefVectorFromReader(r, GetMappedKeyValuesReplySlotData)
	var decodeErr *wire.DecodeError
	var budget *wire.DecodeBudgetError
	if got != nil || !errors.As(err, &decodeErr) || decodeErr.Type != "RangeResultRef" || decodeErr.Field != "Data" ||
		!errors.As(err, &budget) || budget.Elements != inner || budget.Left >= inner {
		t.Fatalf("got %d rows, %v; want nil and nested RangeResultRef.Data budget failure", len(got), err)
	}
}

func TestStandaloneMappedVectorPropagatesElementDecodeError(t *testing.T) {
	t.Parallel()
	data := (&GetMappedKeyValuesReply{Data: []MappedKeyValueRef{
		{KeyValue: KeyValueRef{Key: []byte("a"), Value: []byte("first")}},
		{KeyValue: KeyValueRef{Key: []byte("b"), Value: []byte("second")}},
	}}).MarshalFDB()
	r, err := wire.NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseMappedKeyValueRefVectorFromReader(r, GetMappedKeyValuesReplySlotData)
	if err != nil || len(got) != 2 || string(got[0].KeyValue.Value) != "first" || string(got[1].KeyValue.Value) != "second" {
		t.Fatalf("valid standalone vector: %+v, %v", got, err)
	}
	row, err := r.ReadVectorElementReader(GetMappedKeyValuesReplySlotData, 1)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := row.ReadNestedReader(MappedKeyValueRefSlotKeyValue)
	if err != nil {
		t.Fatal(err)
	}
	le.PutUint32(data[fieldPos(t, kv, KeyValueRefSlotValue):], 1<<20)
	r, err = wire.NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadVectorElementReader(GetMappedKeyValuesReplySlotData, 1); err != nil {
		t.Fatalf("mutation must leave the second element offset valid: %v", err)
	}
	got, err = ParseMappedKeyValueRefVectorFromReader(r, GetMappedKeyValuesReplySlotData)
	var decodeErr *wire.DecodeError
	var bounds *wire.FieldBoundsError
	if got != nil || !errors.As(err, &decodeErr) || decodeErr.Type != "KeyValueRef" ||
		!errors.As(err, &bounds) || bounds.Slot != KeyValueRefSlotValue {
		t.Fatalf("got %d rows, %v; want nil and KeyValueRef DecodeError wrapping value-slot FieldBoundsError", len(got), err)
	}
}

func requireStandaloneStringVectorError(t *testing.T, err error, typ string, data []byte) {
	t.Helper()
	var count uint32
	if len(data) >= 4 {
		count = le.Uint32(data)
	}
	var decodeErr *wire.DecodeError
	var blobErr *wire.StringVectorError
	if !errors.As(err, &decodeErr) || decodeErr.Type != typ || decodeErr.Field != "" ||
		!errors.As(err, &blobErr) || blobErr.Count != count || blobErr.Len != len(data) {
		t.Fatalf("got %v; want %s DecodeError wrapping StringVectorError{Count:%d, Len:%d}", err, typ, count, len(data))
	}
}

func TestStandaloneGetValueStringVector(t *testing.T) {
	t.Parallel()
	valid := []byte{2, 0, 0, 0, 1, 0, 0, 0, 'a', 2, 0, 0, 0, 'b', 'c'}
	got, err := ParseGetValueReqAndResultStringVector(valid)
	if err != nil || len(got) != 2 || string(got[0].Key) != "a" || string(got[1].Key) != "bc" ||
		got[0].HasResult || got[0].Result != nil || got[1].HasResult || got[1].Result != nil {
		t.Fatalf("literal key vector: %+v, %v; want keys a, bc with no results", got, err)
	}
	for _, empty := range [][]byte{nil, {}, {0, 0, 0, 0}} {
		if got, err := ParseGetValueReqAndResultStringVector(empty); err != nil || got != nil {
			t.Fatalf("empty vector %x: %+v, %v", empty, got, err)
		}
	}
	for name, data := range map[string][]byte{
		"short count":             {1, 0, 0},
		"missing second length":   {2, 0, 0, 0, 5, 0, 0, 0, 'a', 'l', 'p', 'h', 'a', 1, 0, 0},
		"truncated key":           {1, 0, 0, 0, 5, 0, 0, 0, 'a', 'b'},
		"truncated final row":     valid[:len(valid)-1],
		"oversized key length":    {1, 0, 0, 0, 255, 255, 255, 255},
		"count too large":         append([]byte{255, 255, 255, 255}, valid[4:]...),
		"count too small":         append([]byte{1, 0, 0, 0}, valid[4:]...),
		"trailing bytes":          append(bytes.Clone(valid), 0),
		"zero count with payload": {0, 0, 0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseGetValueReqAndResultStringVector(data)
			if got != nil {
				t.Fatalf("malformed vector returned %d partial rows", len(got))
			}
			requireStandaloneStringVectorError(t, err, "[]GetValueReqAndResult", data)
		})
	}
}

func TestStandaloneKeyRangeStringVector(t *testing.T) {
	t.Parallel()
	// C++ stores single-key ranges as (end, empty), including the empty key.
	valid := []byte{
		3, 0, 0, 0,
		1, 0, 0, 0, 'a', 1, 0, 0, 0, 'z',
		4, 0, 0, 0, 'c', 'a', 't', 0, 0, 0, 0, 0,
		1, 0, 0, 0, 0, 0, 0, 0, 0,
	}
	got, err := ParseKeyRangeRefStringVector(valid)
	if err != nil || len(got) != 3 || string(got[0].Begin) != "a" || string(got[0].End) != "z" ||
		string(got[1].Begin) != "cat" || string(got[1].End) != "cat\x00" ||
		len(got[2].Begin) != 0 || string(got[2].End) != "\x00" {
		t.Fatalf("literal range vector: %+v, %v; want [a,z), [cat,cat\\x00), [empty,\\x00)", got, err)
	}
	for _, empty := range [][]byte{nil, {}, {0, 0, 0, 0}} {
		if got, err := ParseKeyRangeRefStringVector(empty); err != nil || got != nil {
			t.Fatalf("empty vector %x: %+v, %v", empty, got, err)
		}
	}
	for name, data := range map[string][]byte{
		"short count":             {1, 0, 0},
		"missing end length":      {1, 0, 0, 0, 5, 0, 0, 0, 'a', 'l', 'p', 'h', 'a', 1, 0, 0},
		"truncated begin":         {1, 0, 0, 0, 8, 0, 0, 0, 'a', 'b', 'c', 'd'},
		"truncated end":           {1, 0, 0, 0, 1, 0, 0, 0, 'a', 3, 0, 0, 0, 'y', 'z'},
		"truncated final row":     valid[:len(valid)-1],
		"oversized begin length":  {1, 0, 0, 0, 255, 255, 255, 255, 0, 0, 0, 0},
		"count too large":         append([]byte{255, 255, 255, 255}, valid[4:]...),
		"count too small":         append([]byte{1, 0, 0, 0}, valid[4:]...),
		"trailing bytes":          append(bytes.Clone(valid), 0),
		"zero count with payload": {0, 0, 0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseKeyRangeRefStringVector(data)
			if got != nil {
				t.Fatalf("malformed vector returned %d partial rows", len(got))
			}
			requireStandaloneStringVectorError(t, err, "[]KeyRangeRef", data)
		})
	}
}
