package client

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func decodeCommit(t *testing.T, body []byte) types.CommitTransactionRequest {
	t.Helper()
	r, err := wire.NewReader(body)
	if err != nil {
		t.Fatal(err)
	}
	var req types.CommitTransactionRequest
	if err := req.UnmarshalFromReader(r); err != nil {
		t.Fatal(err)
	}
	return req
}

// A single-key clear goes out as (k\x00, empty), as MutationRef::serialize
// sends it (CommitTransaction.h:318-334); other clears and the captured
// mutations are unchanged. With a tenant, the rule applies to the prefixed keys.
func TestCommitSingleKeyClearEncoding(t *testing.T) {
	for _, tenant := range []int64{NoTenantID, 7} {
		muts := []Mutation{
			{Type: MutClearRange, Key: []byte("k"), Value: []byte("k\x00")},
			{Type: MutClearRange, Key: []byte("a"), Value: []byte("c")},
			{Type: MutSetValue, Key: []byte("s"), Value: []byte("s\x00")},
		}
		input := &commitInput{muts: muts, tenantID: tenant, readVersion: 5}
		body, buf := buildCommitInputRequest(input, transport.UID{})
		req := decodeCommit(t, body)
		marshalBufPool.Put(buf)
		var p []byte
		if tenant >= 0 {
			p = []byte{0, 0, 0, 0, 0, 0, 0, 7}
		}
		pre := func(s string) []byte { return append(bytes.Clone(p), s...) }
		got := req.Transaction.Mutations
		if len(got) != 3 || !bytes.Equal(got[0].Param1, pre("k\x00")) || len(got[0].Param2) != 0 {
			t.Fatalf("WIRE_SINGLE_CLEAR tenant %d: %+v, want (%q, empty)", tenant, got, pre("k\x00"))
		}
		if !bytes.Equal(got[1].Param1, pre("a")) || !bytes.Equal(got[1].Param2, pre("c")) {
			t.Fatalf("a range clear changed: %+v", got[1])
		}
		if !bytes.Equal(got[2].Param1, pre("s")) || !bytes.Equal(got[2].Param2, []byte("s\x00")) {
			t.Fatalf("a set changed: %+v", got[2])
		}
		if string(muts[0].Key) != "k" || string(muts[0].Value) != "k\x00" {
			t.Fatalf("the captured mutation was rewritten: %+v", muts[0])
		}
	}
}

// A cleared range with operations inside is shipped as the clears between
// them, then the operations, as writeRangeToNativeTransaction does
// (ReadYourWrites.actor.cpp:2001-2018).
func TestCommitClearSplitByOperations(t *testing.T) {
	m := func(typ MutationType, k, v string) Mutation {
		return Mutation{Type: typ, Key: []byte(k), Value: []byte(v)}
	}
	for _, tc := range []struct {
		name string
		ops  []Mutation
		want []Mutation
	}{
		{
			"set inside",
			[]Mutation{m(MutClearRange, "m", "n"), m(MutSetValue, "m1", "5")},
			[]Mutation{m(MutClearRange, "m", "m1"), m(MutClearRange, "m1\x00", "n"), m(MutSetValue, "m1", "5")},
		},
		{
			"set at begin",
			[]Mutation{m(MutClearRange, "m", "n"), m(MutSetValue, "m", "5")},
			[]Mutation{m(MutClearRange, "m\x00", "n"), m(MutSetValue, "m", "5")},
		},
		{
			"set at last key",
			[]Mutation{m(MutClearRange, "m", "m\x00\x00"), m(MutSetValue, "m\x00", "5")},
			[]Mutation{m(MutClearRange, "m", "m\x00"), m(MutSetValue, "m\x00", "5")},
		},
		{
			"two operations",
			[]Mutation{m(MutClearRange, "a", "z"), m(MutSetValue, "c", "1"), m(MutAddValue, "q", "\x01")},
			[]Mutation{m(MutClearRange, "a", "c"), m(MutClearRange, "c\x00", "q"), m(MutClearRange, "q\x00", "z"), m(MutSetValue, "c", "1"), m(MutSetValue, "q", "\x01")}, // an add on a cleared key folds to a set
		},
		{
			"set outside",
			[]Mutation{m(MutClearRange, "m", "n"), m(MutSetValue, "x", "1")},
			[]Mutation{m(MutClearRange, "m", "n"), m(MutSetValue, "x", "1")},
		},
		{
			"clear over set",
			[]Mutation{m(MutSetValue, "m1", "5"), m(MutClearRange, "m", "n")},
			[]Mutation{m(MutClearRange, "m", "n")},
		},
	} {
		got := coalesceCommitMutations(tc.ops)
		if len(got) != len(tc.want) {
			t.Fatalf("WIRE_CLEAR_SPLIT %s: %q, want %q", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i].Type != tc.want[i].Type || !bytes.Equal(got[i].Key, tc.want[i].Key) || !bytes.Equal(got[i].Value, tc.want[i].Value) {
				t.Fatalf("WIRE_CLEAR_SPLIT %s: %q, want %q", tc.name, got, tc.want)
			}
		}
	}
}

// The compact form applies exactly when the end is the begin plus \x00: empty
// and binary keys included, near misses excluded.
func TestIsSingleKeyClear(t *testing.T) {
	for _, tc := range []struct {
		begin, end string
		want       bool
	}{
		{"", "\x00", true},
		{"\xff\x00", "\xff\x00\x00", true},
		{"k", "k\x00", true},
		{"k", "k\x01", false},
		{"k", "k\x00\x00", false},
		{"k", "l", false},
		{"k\x00", "k", false},
	} {
		m := types.MutationRef{MutType: uint8(MutClearRange), Param1: []byte(tc.begin), Param2: []byte(tc.end)}
		if got := isSingleKeyClear(&m); got != tc.want {
			t.Errorf("WIRE_SINGLE_CLEAR_PREDICATE (%q, %q) = %v, want %v", tc.begin, tc.end, got, tc.want)
		}
	}
	set := types.MutationRef{MutType: uint8(MutSetValue), Param1: []byte("k"), Param2: []byte("k\x00")}
	if isSingleKeyClear(&set) {
		t.Error("a set is not a clear")
	}
}

// Every kind of operation key splits a clear, including an absent value (a
// matched CompareAndClear), adjacent keys and the empty key; the RYW-disabled
// path ships its op-log as is.
func TestCommitClearSplitEdges(t *testing.T) {
	m := func(typ MutationType, k, v string) Mutation {
		return Mutation{Type: typ, Key: []byte(k), Value: []byte(v)}
	}
	check := func(name string, got, want []Mutation) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("WIRE_CLEAR_SPLIT %s: %q, want %q", name, got, want)
		}
		for i := range got {
			if got[i].Type != want[i].Type || !bytes.Equal(got[i].Key, want[i].Key) || !bytes.Equal(got[i].Value, want[i].Value) {
				t.Fatalf("WIRE_CLEAR_SPLIT %s: %q, want %q", name, got, want)
			}
		}
	}
	check("absent value",
		coalesceCommitMutations([]Mutation{m(MutClearRange, "a", "z"), m(MutSetValue, "m", "v"), m(MutCompareAndClear, "m", "v")}),
		[]Mutation{m(MutClearRange, "a", "m"), m(MutClearRange, "m\x00", "z"), m(MutClearRange, "m", "m\x00")})
	check("adjacent keys",
		coalesceCommitMutations([]Mutation{m(MutClearRange, "a", "z"), m(MutSetValue, "m", "1"), m(MutSetValue, "m\x00", "2")}),
		[]Mutation{m(MutClearRange, "a", "m"), m(MutClearRange, "m\x00\x00", "z"), m(MutSetValue, "m", "1"), m(MutSetValue, "m\x00", "2")})
	check("empty key",
		coalesceCommitMutations([]Mutation{m(MutClearRange, "", "b"), m(MutSetValue, "", "1")}),
		[]Mutation{m(MutClearRange, "\x00", "b"), m(MutSetValue, "", "1")})
	raw := []Mutation{m(MutClearRange, "a", "z"), m(MutSetValue, "m", "1")}
	ship, _, coalesced := coalesceCommitVectors(raw, nil, true)
	if coalesced {
		t.Fatal("an RYW-disabled transaction was coalesced")
	}
	check("ryw disabled", ship, raw)
}

// The shipped read conflicts are the CoalescedKeyRefRangeMap's ranges
// (KeyRangeMap.h:304-350): the sorted union, overlapping and adjacent ranges
// merged, empty ranges dropped.
func TestReadConflictMap(t *testing.T) {
	r := func(b, e string) KeyRange { return KeyRange{Begin: []byte(b), End: []byte(e)} }
	for _, tc := range []struct {
		name     string
		in, want []KeyRange
	}{
		{"unsorted disjoint", []KeyRange{r("c", "d"), r("a", "b")}, []KeyRange{r("a", "b"), r("c", "d")}},
		{"adjacent merged", []KeyRange{r("b", "c"), r("a", "b")}, []KeyRange{r("a", "c")}},
		{"duplicate", []KeyRange{r("a", "a\x00"), r("a", "a\x00")}, []KeyRange{r("a", "a\x00")}},
		{"overlap", []KeyRange{r("a", "c"), r("b", "d")}, []KeyRange{r("a", "d")}},
		{"contained", []KeyRange{r("a", "d"), r("b", "c")}, []KeyRange{r("a", "d")}},
		{"containing", []KeyRange{r("b", "c"), r("a", "d")}, []KeyRange{r("a", "d")}},
		{"empty key and empty range", []KeyRange{r("", "a"), r("q", "q"), r("", "b")}, []KeyRange{r("", "b")}},
	} {
		got := readConflictMap(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("WIRE_READ_CONFLICTS %s: %q, want %q", tc.name, got, tc.want)
		}
		for i := range got {
			if !bytes.Equal(got[i].Begin, tc.want[i].Begin) || !bytes.Equal(got[i].End, tc.want[i].End) {
				t.Fatalf("WIRE_READ_CONFLICTS %s: %q, want %q", tc.name, got, tc.want)
			}
		}
	}
	reads := []KeyRange{r("c", "d"), r("a", "b"), r("b", "c")}
	if got := shipReadConflicts(reads, false); len(got) != 1 || string(got[0].Begin) != "a" || string(got[0].End) != "d" {
		t.Fatalf("WIRE_READ_CONFLICTS ryw: %q, want [a,d)", got)
	}
	if string(reads[0].Begin) != "c" {
		t.Fatalf("shipReadConflicts mutated its input: %q", reads)
	}
	if got := shipReadConflicts(reads, true); len(got) != 3 || string(got[0].Begin) != "c" {
		t.Fatalf("WIRE_READ_CONFLICTS ryw disabled: %q, want the ranges as added", got)
	}
}

// A WaitMetricsRequest asks for latestVersion without a tenant, with no GRV,
// and for the transaction's read version with one (ReadYourWrites.actor.cpp:
// 1860-1863; NativeAPI.actor.cpp:7820-7824).
func TestMetricsMinVersion(t *testing.T) {
	ctx := context.Background()
	tx := newUnitTx()
	tx.tenantId = NoTenantID
	if v, err := tx.metricsMinVersion(ctx); err != nil || v != LatestVersion {
		t.Fatalf("WIRE_METRICS_VERSION no tenant: %d, %v; want latestVersion", v, err)
	}
	if !tx.readVersionState.metricStart.IsZero() {
		t.Fatal("WIRE_METRICS_VERSION no tenant: the transaction was started")
	}
	tx = newUnitTx()
	tx.tenantId = 7
	tx.readVersionState.hasReadVersion, tx.readVersionState.readVersion = true, 1234
	if v, err := tx.metricsMinVersion(ctx); err != nil || v != 1234 {
		t.Fatalf("WIRE_METRICS_VERSION tenant: %d, %v; want the read version 1234", v, err)
	}
}

func newUnitTx() *Transaction {
	tx := &Transaction{readVersionState: &readVersionState{}}
	tx.state.Store(int32(txStateActive))
	return tx
}

// Read conflicts added after Commit froze its read vector neither suppress the
// self-conflicting range nor ship: the captured reads are the frozen ones, then
// the self-conflicting range (NativeAPI.actor.cpp:5952-5958, 6858-6860).
func TestSelfConflictShipReads_FrozenReads(t *testing.T) {
	tx := newUnitTx()
	tx.tenantId = NoTenantID
	tx.readConflicts = []KeyRange{{Begin: []byte("b"), End: []byte("b\x00")}}
	readSnap := tx.readConflicts[:1:1]
	writeSnap := []KeyRange{{Begin: []byte("a"), End: []byte("a\x00")}}
	// A late read of the written key: a decision on live reads would skip SC.
	tx.readConflicts = append(tx.readConflicts, KeyRange{Begin: []byte("a"), End: []byte("a\x00")})
	reads, sc, added := tx.selfConflictShipReads(writeSnap, shipReadConflicts(readSnap, false))
	if !added {
		t.Fatal("WIRE_SELF_CONFLICT: a read conflict added after the freeze suppressed the self-conflicting range")
	}
	// Another read after the self-conflicting range is created.
	tx.readConflicts = append(tx.readConflicts, KeyRange{Begin: []byte("c"), End: []byte("c\x00")})
	input := tx.captureCommitVectors(nil, true, writeSnap, false, reads, true)
	defer input.release()
	got := input.readConflicts
	if len(got) != 2 || string(got[0].Begin) != "b" || string(got[0].End) != "b\x00" ||
		!bytes.HasPrefix(sc.Begin, selfConflictPrefix) ||
		!bytes.Equal(got[1].Begin, sc.Begin) || !bytes.Equal(got[1].End, keyAfterBytes(sc.Begin)) {
		t.Fatalf("WIRE_SELF_CONFLICT: captured reads %q, want [b, b\\x00) then the self-conflicting range %q", got, sc)
	}
}
