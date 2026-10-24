package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func locEntry(tenant int64, begin, end string, server string) locationEntry {
	e := locationEntry{tenantId: tenant, begin: []byte(begin), servers: []ServerInfo{{Address: server}}}
	if end != "∞" {
		e.end = []byte(end)
	}
	return e
}

func lookupAt(t *testing.T, lc *locationCache, tenant int64, key string) (server, begin, end string) {
	t.Helper()
	loc, ok := lc.lookupLocked(tenant, []byte(key), false)
	if !ok {
		return "", "", ""
	}
	end = string(loc.ShardEnd)
	if loc.ShardEnd == nil {
		end = "∞"
	}
	return loc.Servers[0].Address, string(loc.ShardBegin), end
}

func wantLookup(t *testing.T, lc *locationCache, tenant int64, key, server, begin, end string) {
	t.Helper()
	gs, gb, ge := lookupAt(t, lc, tenant, key)
	if gs != server || gb != begin || ge != end {
		t.Fatalf("lookup %q = %s [%q, %q), want %s [%q, %q)", key, gs, gb, ge, server, begin, end)
	}
}

// requireDisjoint checks the invariant lookups rely on: per tenant, entries are
// sorted, non-empty and do not overlap.
func requireDisjoint(t *testing.T, lc *locationCache) {
	t.Helper()
	for i, e := range lc.entries {
		if e.end != nil && bytes.Compare(e.begin, e.end) >= 0 {
			t.Fatalf("entry %d [%q, %q) is empty or inverted", i, e.begin, e.end)
		}
		if i == 0 || lc.entries[i-1].tenantId != e.tenantId {
			continue
		}
		prev := lc.entries[i-1]
		if prev.end == nil || bytes.Compare(prev.end, e.begin) > 0 {
			t.Fatalf("entries %d [%q, %q) and %d [%q, %q) overlap", i-1, prev.begin, prev.end, i, e.begin, e.end)
		}
	}
}

// A merge replaces every shard it covers, as CoalescedKeyRangeMap::insert does
// (KeyRangeMap.h:206-253); a stale half must not answer for its keys.
func TestLocationCacheMerge(t *testing.T) {
	t.Parallel()
	lc := &locationCache{maxSize: 100}
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "a", "b", "s1"), locEntry(NoTenantID, "b", "c", "s2")})
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "a", "c", "s3")})
	requireDisjoint(t, lc)
	wantLookup(t, lc, NoTenantID, "a5", "s3", "a", "c")
	wantLookup(t, lc, NoTenantID, "bb", "s3", "a", "c")

	// The merged shard begins below every cached shard it absorbs.
	lc = &locationCache{maxSize: 100}
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "b", "c", "s2"), locEntry(NoTenantID, "c", "d", "s4")})
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "a", "d", "s3")})
	requireDisjoint(t, lc)
	for _, k := range []string{"a", "bb", "cc"} {
		wantLookup(t, lc, NoTenantID, k, "s3", "a", "d")
	}
	if len(lc.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(lc.entries))
	}
}

// A split learned from one half keeps the rest of the old shard under its old
// servers, with the bounds trimmed to what is still known.
func TestLocationCacheSplit(t *testing.T) {
	t.Parallel()
	lc := &locationCache{maxSize: 100}
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "a", "c", "s1")})
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "b", "c", "s2")})
	requireDisjoint(t, lc)
	wantLookup(t, lc, NoTenantID, "a5", "s1", "a", "b")
	wantLookup(t, lc, NoTenantID, "bb", "s2", "b", "c")
	got, ok := lc.cachedLocationsLocked(NoTenantID, []byte("a"), []byte("c"), 100, false)
	if !ok || len(got) != 2 || got[0].Servers[0].Address != "s1" || got[1].Servers[0].Address != "s2" {
		t.Fatalf("range walk over the split = %v, %v; want s1 then s2", got, ok)
	}

	// A shard learned inside a cached one splits it in three.
	lc = &locationCache{maxSize: 100}
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "a", "z", "s1")})
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "m", "n", "s2")})
	requireDisjoint(t, lc)
	wantLookup(t, lc, NoTenantID, "b", "s1", "a", "m")
	wantLookup(t, lc, NoTenantID, "mm", "s2", "m", "n")
	wantLookup(t, lc, NoTenantID, "p", "s1", "n", "z")

	// The same with an unbounded last shard.
	lc = &locationCache{maxSize: 100}
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "", "∞", "s1")})
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "m", "n", "s2")})
	requireDisjoint(t, lc)
	wantLookup(t, lc, NoTenantID, "b", "s1", "", "m")
	wantLookup(t, lc, NoTenantID, "p", "s1", "n", "∞")
}

func TestLocationCacheInsertEdges(t *testing.T) {
	t.Parallel()
	lc := &locationCache{maxSize: 100}
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "a", "z", "s1"), locEntry(7, "a", "z", "t1")})
	// An empty range changes nothing (KeyRangeMap.h:209-210), nor does an inverted one.
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "m", "m", "s2"), locEntry(NoTenantID, "q", "c", "s2")})
	wantLookup(t, lc, NoTenantID, "m", "s1", "a", "z")
	if len(lc.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(lc.entries))
	}
	// Another tenant's ranges are separate keyspaces.
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "b", "y", "s3")})
	requireDisjoint(t, lc)
	wantLookup(t, lc, 7, "m", "t1", "a", "z")
	wantLookup(t, lc, NoTenantID, "m", "s3", "b", "y")
	// Re-inserting an identical range replaces it.
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "b", "y", "s4")})
	requireDisjoint(t, lc)
	wantLookup(t, lc, NoTenantID, "m", "s4", "b", "y")
	if len(lc.entries) != 4 {
		t.Fatalf("entries = %d, want 4 ([a,b) [b,y) [y,z) and tenant 7)", len(lc.entries))
	}

	// Ranges that only touch the new one keep their bounds.
	lc = &locationCache{maxSize: 100}
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "a", "b", "s1"), locEntry(NoTenantID, "c", "d", "s2")})
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "b", "c", "s3")})
	requireDisjoint(t, lc)
	wantLookup(t, lc, NoTenantID, "a", "s1", "a", "b")
	wantLookup(t, lc, NoTenantID, "bb", "s3", "b", "c")
	wantLookup(t, lc, NoTenantID, "c", "s2", "c", "d")

	// An unbounded new range replaces everything from its begin on.
	lc = &locationCache{maxSize: 100}
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "a", "b", "s1"), locEntry(NoTenantID, "b", "c", "s2"), locEntry(NoTenantID, "c", "∞", "s3")})
	lc.insertSorted([]locationEntry{locEntry(NoTenantID, "bb", "∞", "s4")})
	requireDisjoint(t, lc)
	wantLookup(t, lc, NoTenantID, "a", "s1", "a", "b")
	wantLookup(t, lc, NoTenantID, "b", "s2", "b", "bb")
	wantLookup(t, lc, NoTenantID, "zz", "s4", "bb", "∞")
	if len(lc.entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(lc.entries))
	}
}

// Random inserts agree with a per-key model of the C++ range map: every key maps
// to the last range inserted over it, and a lookup reports the run of keys that
// range still owns.
func TestLocationCacheInsertModel(t *testing.T) {
	t.Parallel()
	const keys = 16 // single-byte keys 0..15; an end of 16 is unbounded
	rng := rand.New(rand.NewPCG(1, 2))
	for round := 0; round < 200; round++ {
		lc := &locationCache{maxSize: 1 << 20}
		model := make([]int, keys) // 0 = uncached, else insert id
		for id := 1; id <= 12; id++ {
			b := rng.IntN(keys)
			e := b + 1 + rng.IntN(keys-b)
			ne := locationEntry{tenantId: NoTenantID, begin: []byte{byte(b)}, servers: []ServerInfo{{Address: fmt.Sprint(id)}}}
			if e < keys {
				ne.end = []byte{byte(e)}
			}
			lc.insertSorted([]locationEntry{ne})
			for k := b; k < e; k++ {
				model[k] = id
			}
			requireDisjoint(t, lc)
			for k := 0; k < keys; k++ {
				loc, ok := lc.lookupLocked(NoTenantID, []byte{byte(k)}, false)
				if model[k] == 0 {
					if ok {
						t.Fatalf("round %d: key %d cached as %v, want a miss", round, k, loc)
					}
					continue
				}
				lo, hi := k, k+1
				for lo > 0 && model[lo-1] == model[k] {
					lo--
				}
				for hi < keys && model[hi] == model[k] {
					hi++
				}
				var wantEnd []byte
				if hi < keys {
					wantEnd = []byte{byte(hi)}
				}
				if !ok || loc.Servers[0].Address != fmt.Sprint(model[k]) ||
					!bytes.Equal(loc.ShardBegin, []byte{byte(lo)}) || !bytes.Equal(loc.ShardEnd, wantEnd) || (wantEnd == nil) != (loc.ShardEnd == nil) {
					t.Fatalf("round %d: key %d = %v (ok=%v), want insert %d over [%d, %d)", round, k, loc, ok, model[k], lo, hi)
				}
			}
		}
	}
}

// replyVector returns the C++ ObjectWriter bytes of a ground-truth reply
// (cmd/fdb-schema-extract generateTestVectors).
func replyVector(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../wire/types/testdata.json")
	if err != nil {
		t.Fatalf("testdata.json not found (missing bazel data dep?): %v", err)
	}
	var vecs []struct {
		Name string `json:"name"`
		Hex  string `json:"hex"`
	}
	if err := json.Unmarshal(data, &vecs); err != nil {
		t.Fatalf("parse testdata.json: %v", err)
	}
	for _, v := range vecs {
		if v.Name == name {
			b, err := hex.DecodeString(v.Hex)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Fatalf("%s not in testdata.json", name)
	return nil
}

// The proxy-list parser returns the oversized-count error.
func TestParseClientDBInfoOversizedCount(t *testing.T) {
	t.Parallel()
	data := (&types.ClientDBInfo{GrvProxies: make([]byte, 8)}).MarshalFDB()
	r, err := wire.NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	field := r.ObjectPos() + r.FieldOffset(types.ClientDBInfoSlotGrvProxies)
	count := field + int(binary.LittleEndian.Uint32(data[field:]))
	binary.LittleEndian.PutUint32(data[count:], 1000)
	info, err := parseStandaloneClientDBInfo(data)
	var countErr *wire.VectorCountError
	if !errors.As(err, &countErr) || countErr.Count != 1000 {
		t.Fatalf("parse = %+v, %v; want a wire.VectorCountError", info, err)
	}
}

// locationsReplyVector is a one-shard GetKeyServerLocationsReply.
func locationsReplyVector(t *testing.T) []byte {
	return replyVector(t, "GetKeyServerLocationsReply_one_range")
}

// The split-point parser returns the oversized-count error rather than reading
// the bytes after the vector as split points.
func TestParseSplitRangeReplyOversizedCount(t *testing.T) {
	t.Parallel()
	data := replyVector(t, "SplitRangeReply_two_points")
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		t.Fatal(err)
	}
	field := r.ObjectPos() + r.FieldOffset(types.SplitRangeReplySlotSplitPoints)
	count := field + int(binary.LittleEndian.Uint32(data[field:]))
	if binary.LittleEndian.Uint32(data[count:]) != 2 {
		t.Fatal("did not locate the split-point vector's count")
	}
	binary.LittleEndian.PutUint32(data[count:], 1000)
	points, err := parseSplitRangeReply(data)
	var countErr *wire.VectorCountError
	if !errors.As(err, &countErr) {
		t.Fatalf("parse = %q, %v; want a wire.VectorCountError", points, err)
	}
}

// locationsReplyLayout locates the fields the malformed-reply cases rewrite.
type locationsReplyLayout struct {
	resultsCount int // uint32 element count of the results vector
	result       int // RelativeOffset to the first result
	keyRangeSlot int // the result vtable's entry for its key range
	keyRangeRef  int // RelativeOffset from the result to its KeyRangeRef
	beginField   int // RelativeOffset from the KeyRangeRef to its begin
	endSlot      int // the KeyRangeRef vtable's entry for its end
	beginString  int // uint32 length prefix of KeyRangeRef.begin
	endString    int // uint32 length prefix of KeyRangeRef.end
	serversCount int // uint32 element count of the result's server vector
	server       int // RelativeOffset to the first server
	token        int // the server's endpoint token
}

func locateReplyFields(t *testing.T, d []byte) locationsReplyLayout {
	t.Helper()
	u32 := func(p int) int { return int(binary.LittleEndian.Uint32(d[p:])) }
	vtable := func(obj int) int { return obj - int(int32(binary.LittleEndian.Uint32(d[obj:]))) }
	field := func(obj, slot int) int {
		vt := vtable(obj)
		if 4+2*slot+2 > int(binary.LittleEndian.Uint16(d[vt:])) {
			t.Fatalf("object at %d has no slot %d", obj, slot)
		}
		off := int(binary.LittleEndian.Uint16(d[vt+4+2*slot:]))
		if off == 0 {
			t.Fatalf("object at %d: slot %d absent", obj, slot)
		}
		return obj + off
	}
	deref := func(p int) int { return p + u32(p) }
	root := u32(0)                                                              // ErrorOr union root
	reply := deref(field(root, 1))                                              // GetKeyServerLocationsReply
	results := deref(field(reply, types.GetKeyServerLocationsReplySlotResults)) // [count][offsets]
	pair := deref(results + 4)
	krField := field(pair, types.LocationPairSlotKeyRange)
	kr := deref(krField)
	servers := deref(field(pair, types.LocationPairSlotServers))
	l := locationsReplyLayout{
		resultsCount: results,
		result:       results + 4,
		keyRangeSlot: vtable(pair) + 4 + 2*types.LocationPairSlotKeyRange,
		keyRangeRef:  krField,
		beginField:   field(kr, types.KeyRangeRefSlotBegin),
		endSlot:      vtable(kr) + 4 + 2*types.KeyRangeRefSlotEnd,
		beginString:  deref(field(kr, types.KeyRangeRefSlotBegin)),
		endString:    deref(field(kr, types.KeyRangeRefSlotEnd)),
		serversCount: servers,
		server:       servers + 4,
		token:        bytes.Index(d, bytes.Repeat([]byte{0x73}, 8)),
	}
	if u32(l.resultsCount) != 1 || u32(l.serversCount) != 1 || l.token < 0 ||
		string(d[l.beginString+4:l.beginString+4+u32(l.beginString)]) != "range_begin" ||
		string(d[l.endString+4:l.endString+4+u32(l.endString)]) != "range_end" {
		t.Fatal("ground-truth vector layout changed; update locateReplyFields")
	}
	return l
}

// Each malformed reply is refused for its own defect and caches nothing; C++
// fails such a reply in deserialization and never delivers it
// (FlowTransport.actor.cpp:1195-1207). Results C++ would cache nothing for are
// skipped instead.
func TestParseLocationsReplyMalformed(t *testing.T) {
	t.Parallel()
	vector := locationsReplyVector(t)
	layout := locateReplyFields(t, vector)
	if entries, err := parseGetKeyServerLocationsReply(vector); err != nil || len(entries) != 1 ||
		string(entries[0].begin) != "range_begin" || string(entries[0].end) != "range_end" {
		t.Fatalf("well-formed reply: %+v, %v", entries, err)
	}
	put := func(d []byte, p int, v uint32) { binary.LittleEndian.PutUint32(d[p:], v) }
	cases := []struct {
		name      string
		mutate    func(d []byte)
		defect    locationsDefect
		wantCount bool // the cause is a *wire.VectorCountError
	}{
		{name: "absent key range", mutate: func(d []byte) { binary.LittleEndian.PutUint16(d[layout.keyRangeSlot:], 0) }, defect: locationsNoShards},
		{name: "empty key range", mutate: func(d []byte) { put(d, layout.beginString, 0); put(d, layout.endString, 0) }, defect: locationsNoShards},
		{name: "server without an endpoint", mutate: func(d []byte) { clear(d[layout.token : layout.token+8]) }, defect: locationsNoShards},
		{name: "key range offset zero", mutate: func(d []byte) { put(d, layout.keyRangeRef, 0) }, defect: locationsUnreadable},
		{name: "result past the buffer", mutate: func(d []byte) { put(d, layout.result, 1<<20) }, defect: locationsUnreadable},
		{name: "server past the buffer", mutate: func(d []byte) { put(d, layout.server, 1<<20) }, defect: locationsUnreadable},
		{name: "results count past the buffer", mutate: func(d []byte) { put(d, layout.resultsCount, 1000) }, defect: locationsUnreadable, wantCount: true},
		{name: "server count past the buffer", mutate: func(d []byte) { put(d, layout.serversCount, 1000) }, defect: locationsUnreadable, wantCount: true},
		{name: "range key past the buffer", mutate: func(d []byte) { put(d, layout.beginField, 1<<20) }, defect: locationsUnreadable},
		{name: "inverted range", mutate: func(d []byte) { d[layout.beginString+4] = 'z' }, defect: locationsInvertedRange},
		// The result and its KeyRangeRef share one vtable here, so the result's
		// servers vanish too; the range is checked first.
		{name: "absent range end", mutate: func(d []byte) { binary.LittleEndian.PutUint16(d[layout.endSlot:], 0) }, defect: locationsInvertedRange},
		{name: "single-key form without a trailing NUL", mutate: func(d []byte) { put(d, layout.endString, 0) }, defect: locationsInvertedRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := bytes.Clone(vector)
			tc.mutate(d)
			entries, err := parseGetKeyServerLocationsReply(d)
			var malformed *locationsReplyError
			if !errors.As(err, &malformed) || malformed.defect != tc.defect {
				t.Fatalf("parse = %+v, %v; want defect %d", entries, err, tc.defect)
			}
			var countErr *wire.VectorCountError
			if got := errors.As(err, &countErr); got != tc.wantCount {
				t.Fatalf("VectorCountError in %v: %v, want %v", err, got, tc.wantCount)
			}
		})
	}

	// The single-key form C++ writes (begin + \x00 == end) still decodes.
	d := bytes.Clone(vector)
	d[layout.beginString+4+len("range_begin")-1] = 0
	put(d, layout.endString, 0)
	entries, err := parseGetKeyServerLocationsReply(d)
	if err != nil || len(entries) != 1 || string(entries[0].begin) != "range_begi" || string(entries[0].end) != "range_begi\x00" {
		t.Fatalf("single-key range: %+v, %v", entries, err)
	}
}

// queryTwoProxies runs queryLocations for tenant over two proxies that answer
// first and second, and returns what it returned.
func queryTwoProxies(t *testing.T, ctx context.Context, tenant int64, first, second []byte) (*database, []locationEntry, error) {
	t.Helper()
	p1, p2 := newIdentityPeer(t), newIdentityPeer(t)
	db, _ := identityDatabase(ctx, p1, p2)
	t.Cleanup(func() { closeIdentityDatabase(db) })
	db.locCache.maxSize = 16
	db.dbInfo.Store(&DBInfo{CommitProxies: []ProxyInfo{{Address: "p1:4500", Token: transport.UID{First: 1}}, {Address: "p2:4500", Token: transport.UID{First: 2}}}})

	type result struct {
		entries []locationEntry
		err     error
	}
	done := make(chan result, 1)
	go func() {
		entries, err := db.locCache.queryLocations(db, ctx, tenant, func(tok transport.UID) []byte {
			return buildGetKeyServerLocationsRequest([]byte("range_c"), false, tenant, types.SpanContext{}, tok)
		}, nil)
		done <- result{entries, err}
	}()
	for _, step := range []struct {
		peer  *identityPeer
		reply []byte
	}{{p1, first}, {p2, second}} {
		f, ok := step.peer.recv(ctx)
		if !ok {
			t.Fatal("timed out waiting for a location request")
		}
		var req types.GetKeyServerLocationsRequest
		if err := req.UnmarshalFDB(f.body); err != nil {
			t.Fatal(err)
		}
		if err := transport.WriteFrame(step.peer.server, replyToken(req.Reply.Token), step.reply, false); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-done:
		return db, got.entries, got.err
	case <-ctx.Done():
		t.Fatal("queryLocations did not return")
		return nil, nil, nil
	}
}

// A malformed reply from one proxy is not a transport failure: the query moves
// to the next proxy and caches that proxy's answer.
func TestQueryLocationsSkipsMalformedReply(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	vector := locationsReplyVector(t)
	layout := locateReplyFields(t, vector)
	bad := bytes.Clone(vector)
	bad[layout.beginString+4] = 'z'

	db, entries, err := queryTwoProxies(t, ctx, NoTenantID, bad, vector)
	if err != nil || len(entries) != 1 || string(entries[0].begin) != "range_begin" {
		t.Fatalf("queryLocations = %+v, %v; want the second proxy's shard", entries, err)
	}
	db.locCache.mu.RLock()
	defer db.locCache.mu.RUnlock()
	wantLookup(t, &db.locCache, NoTenantID, "range_c", "10.1.2.3:4500", "range_begin", "range_end")
	if len(db.locCache.entries) != 1 {
		t.Fatalf("cache holds %d entries, want only the well-formed shard", len(db.locCache.entries))
	}
	if db.failMon.isFailed("p1:4500") {
		t.Fatal("a malformed reply marked its proxy failed")
	}
}

// A tenant reply whose shard end lacks the tenant prefix is ordered as sent but
// inverted once the prefix is stripped from its begin; it is neither cached nor
// returned, and the next proxy's answer is used.
func TestQueryLocationsSkipsReplyInvertedByTenantStrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const tenant int64 = 1
	vector := locationsReplyVector(t)
	layout := locateReplyFields(t, vector)
	tenantReply := func(begin, end string) []byte {
		d := bytes.Clone(vector)
		copy(d[layout.beginString+4:], begin) // 11 bytes, as "range_begin"
		copy(d[layout.endString+4:], end)     // 9 bytes, as "range_end"
		return d
	}
	prefix := "\x00\x00\x00\x00\x00\x00\x00\x01"
	next := "\x00\x00\x00\x00\x00\x00\x00\x02"
	bad := tenantReply(prefix+"zzz", next+"x")
	good := tenantReply(prefix+"aaa", prefix+"z")

	db, entries, err := queryTwoProxies(t, ctx, tenant, bad, good)
	if err != nil || len(entries) != 1 || string(entries[0].begin) != "aaa" || string(entries[0].end) != "z" {
		t.Fatalf("queryLocations = %+v, %v; want the second proxy's shard [aaa, z)", entries, err)
	}
	db.locCache.mu.RLock()
	defer db.locCache.mu.RUnlock()
	wantLookup(t, &db.locCache, tenant, "b", "10.1.2.3:4500", "aaa", "z")
	if len(db.locCache.entries) != 1 {
		t.Fatalf("cache holds %d entries, want only the well-formed shard", len(db.locCache.entries))
	}
}
