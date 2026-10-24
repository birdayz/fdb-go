package client

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// vtableAt is the absolute position of the vtable of the object at obj.
func vtableAt(d []byte, obj int) int { return obj - int(int32(binary.LittleEndian.Uint32(d[obj:]))) }

// moveSlot points slot of the object at obj past the end of the buffer.
func moveSlot(t *testing.T, d []byte, obj, slot int) {
	t.Helper()
	entry := vtableAt(d, obj) + 4 + 2*slot
	if binary.LittleEndian.Uint16(d[entry:]) < 4 {
		t.Fatalf("object at %d: slot %d absent", obj, slot)
	}
	binary.LittleEndian.PutUint16(d[entry:], 0xFFF0)
}

// errorOrReply is the position of the reply inside an ErrorOr reply payload.
func errorOrReply(t *testing.T, d []byte) int {
	t.Helper()
	var r wire.Reader
	if err := wire.ReadErrorOrInto(d, &r); err != nil {
		t.Fatal(err)
	}
	return r.ObjectPos()
}

// slot2Endpoint is the position of the first server's slot-2 Endpoint in a
// GetKeyServerLocationsReply.
func slot2Endpoint(t *testing.T, d []byte) int {
	t.Helper()
	r, err := wire.ReadErrorOr(d)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := r.ReadVectorElementReader(types.GetKeyServerLocationsReplySlotResults, 0)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := pair.ReadVectorElementReader(types.LocationPairSlotServers, 0)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := ss.ReadNestedReader(types.StorageServerInterfaceSlotField_2)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := wrapper.ReadNestedReader(0)
	if err != nil {
		t.Fatal(err)
	}
	return ep.ObjectPos()
}

// Each reply parser fails with a DecodeError when one present field of its reply
// lies past the buffer, instead of reading it as zero, empty or absent.
func TestReplyParsersFailOnFieldPastBuffer(t *testing.T) {
	t.Parallel()
	dbInfo := (&types.ClientDBInfo{Id: [16]byte{1}}).MarshalFDB()
	cases := []struct {
		name  string
		data  []byte
		field func(d []byte) (obj, slot int)
		parse func(d []byte) error
	}{
		{
			"GetValue value", replyVector(t, "GetValueReply_present"),
			func(d []byte) (int, int) { return errorOrReply(t, d), types.GetValueReplySlotValue + 1 },
			func(d []byte) error { _, _, err := parseGetValueReply(d); return err },
		},
		{
			"GetKey penalty", replyVector(t, "GetKeyReply_basic"),
			func(d []byte) (int, int) { return errorOrReply(t, d), types.GetKeyReplySlotPenalty },
			func(d []byte) error { _, _, _, _, err := parseGetKeyReply(d); return err },
		},
		{
			"GetKeyValues rows", replyVector(t, "GetKeyValuesReply_two_rows"),
			func(d []byte) (int, int) { return errorOrReply(t, d), types.GetKeyValuesReplySlotData },
			func(d []byte) error { _, _, _, err := parseGetKeyValuesReply(d); return err },
		},
		{
			"GetMappedKeyValues rows", replyVector(t, "GetMappedKeyValuesReply_getrange"),
			func(d []byte) (int, int) { return errorOrReply(t, d), types.GetMappedKeyValuesReplySlotData },
			func(d []byte) error { _, _, _, err := parseGetMappedKeyValuesReply(d); return err },
		},
		{
			"GRV version", replyVector(t, "GetReadVersionReply_locked"),
			func(d []byte) (int, int) { return errorOrReply(t, d), types.GetReadVersionReplySlotVersion },
			func(d []byte) error { _, _, _, _, _, _, err := parseGetReadVersionReply(d); return err },
		},
		{
			"commit version", replyVector(t, "CommitID_committed"),
			func(d []byte) (int, int) { return errorOrReply(t, d), types.CommitIDSlotVersion },
			func(d []byte) error { _, err := parseCommitOutcome(d); return err },
		},
		{
			"storage metrics bytes", replyVector(t, "StorageMetrics_basic"),
			func(d []byte) (int, int) { return errorOrReply(t, d), types.StorageMetricsSlotBytes },
			func(d []byte) error { _, err := parseWaitMetricsReply(d); return err },
		},
		{
			"client info id", dbInfo,
			func(d []byte) (int, int) {
				r, err := wire.NewReader(d)
				if err != nil {
					t.Fatal(err)
				}
				return r.ObjectPos(), types.ClientDBInfoSlotId
			},
			func(d []byte) error { _, err := parseStandaloneClientDBInfo(d); return err },
		},
		{
			"storage server endpoint address", locationsReplyVector(t),
			func(d []byte) (int, int) { return slot2Endpoint(t, d), types.EndpointSlotAddresses },
			func(d []byte) error { _, err := parseGetKeyServerLocationsReply(d); return err },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := bytes.Clone(tc.data)
			if err := tc.parse(d); err != nil {
				t.Fatalf("well-formed reply: %v", err)
			}
			obj, slot := tc.field(d)
			moveSlot(t, d, obj, slot)
			err := tc.parse(d)
			var decodeErr *wire.DecodeError
			if !errors.As(err, &decodeErr) {
				t.Fatalf("got %v; want a DecodeError", err)
			}
		})
	}
}

// aliasedLocationsReply is a GetKeyServerLocationsReply whose n results all point at
// one pair with m server offsets to the vector's one server. A root, reply, pair and
// vectors go before the vector's bytes, because offsets point forward only.
func aliasedLocationsReply(t *testing.T, vector []byte, n, m int) []byte {
	t.Helper()
	le := binary.LittleEndian
	layout := locateReplyFields(t, vector)
	keyRange := layout.keyRangeRef + int(le.Uint32(vector[layout.keyRangeRef:]))
	server := layout.server + int(le.Uint32(vector[layout.server:]))
	var b []byte
	u16 := func(v uint16) { b = le.AppendUint16(b, v) }
	u32 := func(v int) { b = le.AppendUint32(b, uint32(v)) }
	u32(16) // 0: root → ErrorOr object at 16
	u32(int(types.GetKeyServerLocationsReplyFileID))
	u16(8) // 8: ErrorOr vtable: tag at 8, value at 4
	u16(9)
	u16(8)
	u16(4)
	u32(8)                    // 16: ErrorOr object, vtable 8 bytes back
	u32(16)                   // 20: value → reply at 36
	b = append(b, 2, 0, 0, 0) // 24: tag 2, the value
	u16(6)                    // 28: reply vtable: results at 4
	u16(8)
	u16(4)
	u16(0)
	u32(8) // 36: reply object, vtable 8 bytes back
	u32(4) // 40: results → vector at 44
	u32(n) // 44
	results := len(b)
	b = append(b, make([]byte, 4*n)...)
	pairVTable := len(b)
	u16(8) // pair vtable: key range at 4, servers at 8
	u16(12)
	u16(4)
	u16(8)
	pair := len(b)
	u32(pair - pairVTable)
	u32(0) // key range, patched below
	u32(4) // servers → vector right after
	u32(m)
	servers := len(b)
	b = append(b, make([]byte, 4*m)...)
	for len(b)%8 != 0 {
		b = append(b, 0)
	}
	base := len(b)
	for i := range n {
		le.PutUint32(b[results+4*i:], uint32(pair-(results+4*i)))
	}
	le.PutUint32(b[pair+4:], uint32(base+keyRange-(pair+4)))
	for i := range m {
		le.PutUint32(b[servers+4*i:], uint32(base+server-(servers+4*i)))
	}
	return append(b, vector...)
}

// Aliased results and servers would cost the parser n×m server decodes from
// O(n+m) bytes; one budget covers both vector levels.
func TestParseLocationsReplyAliasedBudget(t *testing.T) {
	t.Parallel()
	vector := locationsReplyVector(t)
	if entries, err := parseGetKeyServerLocationsReply(aliasedLocationsReply(t, vector, 2, 3)); err != nil ||
		len(entries) != 2 || len(entries[1].servers) != 3 || string(entries[1].begin) != "range_begin" {
		t.Fatalf("small aliased reply: %+v, %v; want 2 shards of 3 servers", entries, err)
	}
	entries, err := parseGetKeyServerLocationsReply(aliasedLocationsReply(t, vector, 300, 300))
	var malformed *locationsReplyError
	var budgetErr *wire.DecodeBudgetError
	if !errors.As(err, &malformed) || malformed.defect != locationsUnreadable || !errors.As(err, &budgetErr) {
		t.Fatalf("parse = %d shards, %v; want a budget error", len(entries), err)
	}
}

// A server with no usable slot-2 endpoint is scanned only over the slots the
// StorageServerInterface schema has, not over the vtable length the reply claims.
// Not parallel: AllocsPerRun counts process-wide allocations.
func TestParseLocationsReplyEndpointScanBounded(t *testing.T) {
	d := locationsReplyVector(t)
	layout := locateReplyFields(t, d)
	clear(d[layout.token : layout.token+8])
	server := layout.server + int(binary.LittleEndian.Uint32(d[layout.server:]))
	binary.LittleEndian.PutUint16(d[vtableAt(d, server):], 0xFFFE)
	entries, err := parseGetKeyServerLocationsReply(d)
	if err != nil || len(entries) != 1 || len(entries[0].servers) != 0 {
		t.Fatalf("parse = %+v, %v; want the shard without servers", entries, err)
	}
	if allocs := testing.AllocsPerRun(5, func() { _, _ = parseGetKeyServerLocationsReply(d) }); allocs > 500 {
		t.Fatalf("parse allocates %.0f times for a 32K-slot vtable, want at most 500", allocs)
	}
}

// The range-read parser fails on a row blob the rows do not fill exactly.
func TestParseGetKeyValuesReplyRejectsRowBlobOverrun(t *testing.T) {
	t.Parallel()
	rows := []byte{1, 0, 0, 0, 1, 0, 0, 0, 'k', 1, 0, 0, 0, 'v'}
	for _, tc := range []struct {
		name string
		data []byte
		ok   bool
	}{
		{"exact", rows, true},
		{"trailing byte", append(bytes.Clone(rows), 0), false},
		{"count above the rows", append([]byte{2, 0, 0, 0}, rows[4:]...), false},
	} {
		kvs, _, _, err := parseGetKeyValuesReply(types.MarshalErrorOrValueGetKeyValuesReply(&types.GetKeyValuesReply{Data: tc.data}))
		var vecErr *wire.StringVectorError
		if tc.ok != (err == nil) || (!tc.ok && !errors.As(err, &vecErr)) {
			t.Errorf("%s: %d rows, %v", tc.name, len(kvs), err)
		}
	}
}

// ClientDBInfo.forward is present only with tag 1 and its value slot, as in C++.
func TestParseClientDBInfoForwardTag(t *testing.T) {
	t.Parallel()
	d := (&types.ClientDBInfo{HasForward: true, Forward: []byte("desc:id@10.0.0.1:4500")}).MarshalFDB()
	if info, err := parseStandaloneClientDBInfo(d); err != nil || info.Forward != "desc:id@10.0.0.1:4500" {
		t.Fatalf("forward tag 1: %+v, %v", info, err)
	}
	r, err := wire.NewReader(d)
	if err != nil {
		t.Fatal(err)
	}
	d[r.ObjectPos()+r.FieldOffset(types.ClientDBInfoSlotForward)] = 2
	if info, err := parseStandaloneClientDBInfo(d); err != nil || info.Forward != "" {
		t.Fatalf("forward tag 2: %+v, %v; want no forward", info, err)
	}
}
