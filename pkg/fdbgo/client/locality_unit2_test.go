package client

import (
	"encoding/binary"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// ============================================================================
// stripTenantPrefix — turns proxy-absolute boundaries into tenant-relative.
// ============================================================================

func TestStripTenantPrefix_Tenant42(t *testing.T) {
	t.Parallel()
	const tenantID int64 = 42
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], uint64(tenantID))

	entries := []locationEntry{
		{begin: append(append([]byte{}, prefix[:]...), 'a'), end: append(append([]byte{}, prefix[:]...), 'z')},
		{begin: append(append([]byte{}, prefix[:]...), 0x00), end: append(append([]byte{}, prefix[:]...), 0xFF)},
	}
	stripTenantPrefix(entries, tenantID)

	if string(entries[0].begin) != "a" {
		t.Errorf("entry[0].begin: got %q, want \"a\"", entries[0].begin)
	}
	if string(entries[0].end) != "z" {
		t.Errorf("entry[0].end: got %q, want \"z\"", entries[0].end)
	}
	if len(entries[1].begin) != 1 || entries[1].begin[0] != 0x00 {
		t.Errorf("entry[1].begin: got %x, want [0]", entries[1].begin)
	}
	if len(entries[1].end) != 1 || entries[1].end[0] != 0xFF {
		t.Errorf("entry[1].end: got %x, want [0xFF]", entries[1].end)
	}
}

func TestStripTenantPrefix_NegativeTenantNoop(t *testing.T) {
	t.Parallel()
	// NoTenantID is -1. Function must early-return without mutation.
	entries := []locationEntry{
		{begin: []byte("aa"), end: []byte("zz")},
	}
	originalBegin := entries[0].begin
	originalEnd := entries[0].end

	stripTenantPrefix(entries, NoTenantID)
	if &entries[0].begin[0] != &originalBegin[0] || string(entries[0].begin) != "aa" {
		t.Errorf("begin must not be touched: got %q", entries[0].begin)
	}
	if &entries[0].end[0] != &originalEnd[0] || string(entries[0].end) != "zz" {
		t.Errorf("end must not be touched: got %q", entries[0].end)
	}
}

func TestStripTenantPrefix_TooShortToContainPrefix(t *testing.T) {
	t.Parallel()
	const tenantID int64 = 7
	// begin is shorter than the 8-byte tenant prefix → bytes.HasPrefix returns
	// false → leave it alone.
	entries := []locationEntry{
		{begin: []byte("abc"), end: []byte("xyz")},
	}
	stripTenantPrefix(entries, tenantID)
	if string(entries[0].begin) != "abc" {
		t.Errorf("begin should not be modified when prefix absent: got %q", entries[0].begin)
	}
	if string(entries[0].end) != "xyz" {
		t.Errorf("end should not be modified when prefix absent: got %q", entries[0].end)
	}
}

func TestStripTenantPrefix_DifferentTenantsPrefixNotStripped(t *testing.T) {
	t.Parallel()
	// Function is asked to strip prefix for tenant 7, but the entries carry
	// tenant 99's prefix. Even though the bytes are LONG enough (>=8) and
	// LOOK like a tenant prefix, they must be left alone — the function's
	// contract is strip-only-if-matches, NOT strip-always-the-first-8-bytes.
	const targetTenant int64 = 7
	const wrongTenant int64 = 99
	var wrongPrefix [8]byte
	binary.BigEndian.PutUint64(wrongPrefix[:], uint64(wrongTenant))

	entries := []locationEntry{{
		begin: append(append([]byte{}, wrongPrefix[:]...), 'a'),
		end:   append(append([]byte{}, wrongPrefix[:]...), 'z'),
	}}
	originalBegin := append([]byte{}, entries[0].begin...)
	originalEnd := append([]byte{}, entries[0].end...)

	stripTenantPrefix(entries, targetTenant)

	if !bytesEqual(entries[0].begin, originalBegin) {
		t.Errorf("different-tenant prefix was wrongly stripped: got %x, want %x",
			entries[0].begin, originalBegin)
	}
	if !bytesEqual(entries[0].end, originalEnd) {
		t.Errorf("different-tenant prefix was wrongly stripped: got %x, want %x",
			entries[0].end, originalEnd)
	}
}

func TestStripTenantPrefix_NilEndUntouched(t *testing.T) {
	t.Parallel()
	// Final shard's end can be nil (end-of-keyspace marker). Must not panic.
	const tenantID int64 = 99
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], uint64(tenantID))
	entries := []locationEntry{
		{begin: append(append([]byte{}, prefix[:]...), 'k'), end: nil},
	}
	stripTenantPrefix(entries, tenantID)
	if string(entries[0].begin) != "k" {
		t.Errorf("begin: got %q, want \"k\"", entries[0].begin)
	}
	if entries[0].end != nil {
		t.Errorf("end: got %v, want nil", entries[0].end)
	}
}

// ============================================================================
// buildGetKeyServerLocationsRequest — wire-construction round-trip.
// ============================================================================

func TestBuildGetKeyServerLocationsRequest_RoundTrip(t *testing.T) {
	t.Parallel()
	key := []byte("user/42")
	const tenantID int64 = 5
	replyToken := transport.UID{First: 0xCAFE, Second: 0xBABE}
	// refresh derives this child span once and passes it verbatim; the builder must
	// stamp it (C++ GetKeyServerLocationsRequest(span.context,…), NativeAPI.actor.cpp:3037).
	span := types.SpanContext{
		TraceID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:  0x99,
		Flags:   traceFlagSampled,
	}

	body := buildGetKeyServerLocationsRequest(key, false, tenantID, span, replyToken)

	var req types.GetKeyServerLocationsRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if string(req.Begin) != string(key) {
		t.Errorf("Begin: got %q, want %q", req.Begin, key)
	}
	// Revert-prove: drop the SpanContext field in buildGetKeyServerLocationsRequest → zero → red.
	if req.SpanContext != span {
		t.Errorf("SpanContext: got %+v, want %+v", req.SpanContext, span)
	}
	if req.HasEnd {
		t.Error("HasEnd: got true for single-key request, want false")
	}
	if req.Limit != 100 {
		t.Errorf("Limit: got %d, want 100", req.Limit)
	}
	if req.Tenant.TenantId != tenantID {
		t.Errorf("TenantId: got %d, want %d", req.Tenant.TenantId, tenantID)
	}
	if req.MinTenantVersion != LatestVersion {
		t.Errorf("MinTenantVersion: got %d, want %d (LatestVersion)", req.MinTenantVersion, LatestVersion)
	}
	first := binary.LittleEndian.Uint64(req.Reply.Token[0:8])
	second := binary.LittleEndian.Uint64(req.Reply.Token[8:16])
	if first != replyToken.First || second != replyToken.Second {
		t.Errorf("reply token: got {%x,%x}, want {%x,%x}",
			first, second, replyToken.First, replyToken.Second)
	}
}

// ============================================================================
// buildGetKeyServerLocationsRangeRequest — range variant with reverse flag.
// ============================================================================

func TestBuildGetKeyServerLocationsRangeRequest_Forward(t *testing.T) {
	t.Parallel()
	begin, end := []byte("a"), []byte("z")
	const (
		limit    = 50
		tenantID = 3
	)
	span := types.SpanContext{TraceID: [16]byte{0xAB, 0xCD}, SpanID: 0x77, Flags: traceFlagSampled}
	body := buildGetKeyServerLocationsRangeRequest(begin, end, limit, false /*reverse*/, tenantID,
		span, transport.UID{First: 1, Second: 2})

	var req types.GetKeyServerLocationsRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if string(req.Begin) != "a" || string(req.End) != "z" {
		t.Errorf("range: got [%q,%q), want [a,z)", req.Begin, req.End)
	}
	if req.SpanContext != span {
		t.Errorf("SpanContext: got %+v, want %+v", req.SpanContext, span)
	}
	if !req.HasEnd {
		t.Error("HasEnd must be true for the range variant")
	}
	if req.Limit != int32(limit) {
		t.Errorf("Limit: got %d, want %d", req.Limit, limit)
	}
	if req.Reverse {
		t.Error("Reverse: got true, want false")
	}
}

func TestBuildGetKeyServerLocationsRangeRequest_Reverse(t *testing.T) {
	t.Parallel()
	body := buildGetKeyServerLocationsRangeRequest(
		[]byte("a"), []byte("z"), 10, true /*reverse*/, 0,
		types.SpanContext{}, transport.UID{First: 1, Second: 2})

	var req types.GetKeyServerLocationsRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if !req.Reverse {
		t.Error("Reverse: got false, want true")
	}
}

// ============================================================================
// collectOverlapping — overlap query under tenant isolation.
// ============================================================================

func TestCollectOverlapping_NoEntriesReturnsNil(t *testing.T) {
	t.Parallel()
	lc := &locationCache{}
	got := lc.collectOverlapping(0, []byte("a"), []byte("z"))
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestCollectOverlapping_OnlyMatchingTenant(t *testing.T) {
	t.Parallel()
	lc := &locationCache{}
	// Cache invariant: entries sorted by (tenantId, begin). Tenant 2's entry
	// MUST come after all tenant-1 entries.
	lc.entries = []locationEntry{
		{tenantId: 1, begin: []byte("a"), end: []byte("c")},
		{tenantId: 1, begin: []byte("c"), end: []byte("e")},
		{tenantId: 1, begin: []byte("e"), end: []byte("g")},
		{tenantId: 2, begin: []byte("a"), end: []byte("z")}, // different tenant — must be excluded
	}
	got := lc.collectOverlapping(1, []byte("b"), []byte("f"))
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (a-c, c-e, e-g for tenant 1)", len(got))
	}
	// Sanity: each result must come from tenant 1, NOT the tenant-2 a-z shard.
	for i, r := range got {
		if string(r.ShardEnd) == "z" {
			t.Errorf("entry[%d] is the tenant-2 shard; cross-tenant leak", i)
		}
	}
}

func TestCollectOverlapping_StopsAfterRange(t *testing.T) {
	t.Parallel()
	lc := &locationCache{}
	lc.entries = []locationEntry{
		{tenantId: 1, begin: []byte("a"), end: []byte("c")},
		{tenantId: 1, begin: []byte("c"), end: []byte("e")},
		{tenantId: 1, begin: []byte("e"), end: []byte("g")},
		{tenantId: 1, begin: []byte("g"), end: []byte("z")}, // past range
	}
	got := lc.collectOverlapping(1, []byte("b"), []byte("d"))
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (a-c, c-e)", len(got))
	}
}

func TestCollectOverlapping_FinalShardWithNilEnd(t *testing.T) {
	t.Parallel()
	lc := &locationCache{}
	lc.entries = []locationEntry{
		{tenantId: 1, begin: []byte("a"), end: nil}, // tail shard
	}
	got := lc.collectOverlapping(1, []byte("b"), []byte("z"))
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1 (open-ended tail shard)", len(got))
	}
	if got[0].ShardEnd != nil {
		t.Errorf("ShardEnd: got %v, want nil", got[0].ShardEnd)
	}
}
