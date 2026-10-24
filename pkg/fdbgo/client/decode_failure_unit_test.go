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

// moveTarget points the RelativeOffset in slot of the object at obj past the end of
// the buffer, leaving the vtable, which identical objects share, untouched.
func moveTarget(t *testing.T, d []byte, obj, slot int) {
	t.Helper()
	field := binary.LittleEndian.Uint16(d[vtableAt(d, obj)+4+2*slot:])
	if field < 4 {
		t.Fatalf("object at %d: slot %d absent", obj, slot)
	}
	binary.LittleEndian.PutUint32(d[obj+int(field):], uint32(len(d)))
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
	return slot2EndpointReader(t, d).ObjectPos()
}

func slot2EndpointReader(t *testing.T, d []byte) *wire.Reader {
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
	return ep
}

// grvProxyEndpoint is the position of the first GRV proxy's getConsistentReadVersion
// Endpoint in an ErrorOr<ClientDBInfo>.
func grvProxyEndpoint(t *testing.T, d []byte) int {
	t.Helper()
	r, err := wire.ReadErrorOr(d)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := r.ReadVectorElementReader(types.ClientDBInfoSlotGrvProxies, 0)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := proxy.ReadNestedReader(types.GrvProxyInterfaceSlotGetConsistentReadVersion)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := wrapper.ReadNestedReader(0)
	if err != nil {
		t.Fatal(err)
	}
	return ep.ObjectPos()
}

// slot2EndpointIP is the position of that endpoint's primary IPAddress.
func slot2EndpointIP(t *testing.T, d []byte) int {
	t.Helper()
	r := slot2EndpointReader(t, d)
	for _, slot := range []int{types.EndpointSlotAddresses, types.NetworkAddressListSlotAddress, types.NetworkAddressSlotIp} {
		next, err := r.ReadNestedReader(slot)
		if err != nil {
			t.Fatal(err)
		}
		r = next
	}
	return r.ObjectPos()
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
			"GetKey selector key", replyVector(t, "GetKeyReply_basic"),
			func(d []byte) (int, int) {
				var r wire.Reader
				if err := wire.ReadErrorOrInto(d, &r); err != nil {
					t.Fatal(err)
				}
				sel, err := r.ReadNestedReader(types.GetKeyReplySlotSel)
				if err != nil {
					t.Fatal(err)
				}
				return sel.ObjectPos(), types.KeySelectorRefSlotKey
			},
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
		{
			"GRV proxy endpoint address", replyVector(t, "ClientDBInfo_secondary_address"),
			func(d []byte) (int, int) { return grvProxyEndpoint(t, d), types.EndpointSlotAddresses },
			func(d []byte) error { _, err := parseErrorOrClientDBInfo(d); return err },
		},
		{
			"GRV proxy endpoint token", replyVector(t, "ClientDBInfo_secondary_address"),
			func(d []byte) (int, int) { return grvProxyEndpoint(t, d), types.EndpointSlotToken },
			func(d []byte) error { _, err := parseErrorOrClientDBInfo(d); return err },
		},
		{
			"storage server endpoint token", locationsReplyVector(t),
			func(d []byte) (int, int) { return slot2Endpoint(t, d), types.EndpointSlotToken },
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
	// An IPAddress shares its vtable with the ErrorOr root, so its value moves by target.
	t.Run("storage server IPv4 address", func(t *testing.T) {
		t.Parallel()
		d := locationsReplyVector(t)
		moveTarget(t, d, slot2EndpointIP(t, d), types.IPAddressSlotAddr+1)
		_, err := parseGetKeyServerLocationsReply(d)
		var decodeErr *wire.DecodeError
		if !errors.As(err, &decodeErr) {
			t.Fatalf("got %v; want a DecodeError", err)
		}
	})
}

// aliasedLocationsReply has n results pointing at one pair of m servers, all the vector's
// one server; the prefix goes before the vector because offsets point forward only.
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

// A server without a slot-2 endpoint is scanned over the schema's slots, not the vtable
// length the reply claims. Not parallel: AllocsPerRun counts process-wide allocations.
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

// withVTable points the object at obj at a vtable of entries appended to d.
func withVTable(d []byte, obj int, entries ...uint16) []byte {
	vt := len(d)
	for _, e := range entries {
		d = binary.LittleEndian.AppendUint16(d, e)
	}
	binary.LittleEndian.PutUint32(d[obj:], uint32(int32(obj-vt)))
	return d
}

// firstLocation returns the readers of the first pair and its first server.
func firstLocation(t *testing.T, d []byte) (pair, server *wire.Reader) {
	t.Helper()
	r, err := wire.ReadErrorOr(d)
	if err != nil {
		t.Fatal(err)
	}
	if pair, err = r.ReadVectorElementReader(types.GetKeyServerLocationsReplySlotResults, 0); err != nil {
		t.Fatal(err)
	}
	if server, err = pair.ReadVectorElementReader(types.LocationPairSlotServers, 0); err != nil {
		t.Fatal(err)
	}
	return pair, server
}

// A vtable that declares more entries than the buffer holds fails the hand-written
// walks instead of reading its fields as absent.
func TestLocationAndSplitWalksFailOnVTablePastBuffer(t *testing.T) {
	t.Parallel()
	// Four entries declared, two present: slot 0's entry lies past the buffer.
	const declared, size = 8, 8
	unreadable := func(t *testing.T, err error) {
		t.Helper()
		var lre *locationsReplyError
		var bounds *wire.FieldBoundsError
		if !errors.As(err, &lre) || lre.defect != locationsUnreadable || !errors.As(err, &bounds) {
			t.Fatalf("got %v; want an unreadable locations reply with a FieldBoundsError", err)
		}
	}
	t.Run("location pair", func(t *testing.T) {
		t.Parallel()
		d := locationsReplyVector(t)
		pair, _ := firstLocation(t, d)
		_, err := parseGetKeyServerLocationsReply(withVTable(d, pair.ObjectPos(), declared, size))
		unreadable(t, err)
	})
	t.Run("storage server", func(t *testing.T) {
		t.Parallel()
		d := locationsReplyVector(t)
		_, server := firstLocation(t, d)
		_, err := parseGetKeyServerLocationsReply(withVTable(d, server.ObjectPos(), declared, size))
		unreadable(t, err)
	})
	t.Run("slot-2 RequestStream", func(t *testing.T) {
		t.Parallel()
		d := locationsReplyVector(t)
		_, server := firstLocation(t, d)
		wrapper, err := server.ReadNestedReader(types.StorageServerInterfaceSlotField_2)
		if err != nil {
			t.Fatal(err)
		}
		_, err = parseGetKeyServerLocationsReply(withVTable(d, wrapper.ObjectPos(), declared, size))
		unreadable(t, err)
	})
	t.Run("split points", func(t *testing.T) {
		t.Parallel()
		d := replyVector(t, "SplitRangeReply_two_points")
		points, err := parseSplitRangeReply(withVTable(d, errorOrReply(t, d), declared, size))
		var bounds *wire.FieldBoundsError
		if !errors.As(err, &bounds) {
			t.Fatalf("got %d points, %v; want a FieldBoundsError", len(points), err)
		}
	})
}

// An absent slot-2 endpoint, or an absent RequestStream field, is not an error: the
// scan looks for an endpoint elsewhere. C++ 7.3 writes only slot 2, so the first case
// moves it to slot 0 for the scan to find.
func TestLocationsSlot2EndpointAbsentUsesScan(t *testing.T) {
	t.Parallel()
	want, err := parseGetKeyServerLocationsReply(locationsReplyVector(t))
	if err != nil || len(want[0].servers) != 1 {
		t.Fatalf("well-formed reply: %+v, %v", want, err)
	}
	t.Run("slot 2 absent", func(t *testing.T) {
		t.Parallel()
		d := locationsReplyVector(t)
		_, server := firstLocation(t, d)
		entries := vtableAt(d, server.ObjectPos()) + 4
		slot2 := entries + 2*types.StorageServerInterfaceSlotField_2
		copy(d[entries:], d[slot2:slot2+2])
		binary.LittleEndian.PutUint16(d[slot2:], 0)
		got, err := parseGetKeyServerLocationsReply(d)
		if err != nil || len(got[0].servers) != 1 || got[0].servers[0].Address != want[0].servers[0].Address {
			t.Fatalf("got %+v, %v; want server %s from the scan", got, err, want[0].servers[0].Address)
		}
	})
	t.Run("wrapper field absent", func(t *testing.T) {
		t.Parallel()
		d := locationsReplyVector(t)
		_, server := firstLocation(t, d)
		wrapper, err := server.ReadNestedReader(types.StorageServerInterfaceSlotField_2)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseGetKeyServerLocationsReply(withVTable(d, wrapper.ObjectPos(), 4, 4)) // no fields
		if err != nil || len(got) != 1 || len(got[0].servers) != 0 {
			t.Fatalf("got %+v, %v; want the shard with no server", got, err)
		}
	})
}
