package executor

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// --- estimateQueryResultBytes sanity + nil-safety (RFC-130 §3.5) ---

// TestEstimateQueryResultBytes_StoredRecord proves a stored-record row is
// estimated as the proto wire size plus the encoded primary-key length, and
// that the estimate scales with the record's payload (a wider record costs
// more) — the property the budget relies on.
func TestEstimateQueryResultBytes_StoredRecord(t *testing.T) {
	t.Parallel()

	small := &wrapperspb.StringValue{Value: "x"}
	big := &wrapperspb.StringValue{Value: string(make([]byte, 500))}

	smallQR := QueryResult{
		Record:     &recordlayer.FDBStoredRecord[proto.Message]{Record: small},
		PrimaryKey: tuple.Tuple{int64(1)},
	}
	bigQR := QueryResult{
		Record:     &recordlayer.FDBStoredRecord[proto.Message]{Record: big},
		PrimaryKey: tuple.Tuple{int64(1)},
	}

	smallBytes := estimateQueryResultBytes(smallQR)
	bigBytes := estimateQueryResultBytes(bigQR)

	if smallBytes <= 0 {
		t.Fatalf("stored-record estimate must be positive, got %d", smallBytes)
	}
	// proto.Size of the big record is ~503 bytes; the estimate must be within a
	// sane factor (it IS the proto size + pk, not 10x off).
	if bigBytes < int64(proto.Size(big)) {
		t.Fatalf("big estimate %d < proto wire size %d; estimate dropped the payload", bigBytes, proto.Size(big))
	}
	if bigBytes <= smallBytes {
		t.Fatalf("a 500-byte record (%d) must estimate larger than a 1-byte record (%d)", bigBytes, smallBytes)
	}
	// Sanity ceiling: not absurdly larger than the wire size + a small pk.
	if bigBytes > int64(proto.Size(big))+64 {
		t.Fatalf("big estimate %d far exceeds proto size %d + pk slack", bigBytes, proto.Size(big))
	}
}

// TestEstimateQueryResultBytes_ComputedRow proves a computed (Record==nil)
// map[string]any row is estimated by summing key lengths and per-value sizes,
// and scales with the string payload.
func TestEstimateQueryResultBytes_ComputedRow(t *testing.T) {
	t.Parallel()

	narrow := QueryResult{Datum: map[string]any{"ID": int64(1), "P": "x"}}
	wide := QueryResult{Datum: map[string]any{"ID": int64(1), "P": string(make([]byte, 1000))}}

	nb := estimateQueryResultBytes(narrow)
	wb := estimateQueryResultBytes(wide)

	if nb <= 0 {
		t.Fatalf("computed-row estimate must be positive, got %d", nb)
	}
	if wb < 1000 {
		t.Fatalf("a 1000-byte string value must dominate the estimate, got %d", wb)
	}
	if wb <= nb {
		t.Fatalf("wide row (%d) must estimate larger than narrow row (%d)", wb, nb)
	}
}

// TestEstimateQueryResultBytes_NilSafety proves no panic and a small positive
// estimate for the degenerate shapes: Record==nil & Datum==nil, an empty map,
// an empty []any, a nil-valued map entry, and a typed-nil proto Record.
func TestEstimateQueryResultBytes_NilSafety(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		qr   QueryResult
	}{
		{"both-nil", QueryResult{}},
		{"datum-nil", QueryResult{Datum: nil}},
		{"empty-map", QueryResult{Datum: map[string]any{}}},
		{"empty-slice", QueryResult{Datum: []any{}}},
		{"nil-map-value", QueryResult{Datum: map[string]any{"X": nil}}},
		{"nil-proto-record", QueryResult{
			Record:     &recordlayer.FDBStoredRecord[proto.Message]{},
			PrimaryKey: nil,
		}},
		{"scalar-int", QueryResult{Datum: int64(42)}},
		{"scalar-string", QueryResult{Datum: "hello"}},
		{"nested-map", QueryResult{Datum: map[string]any{"M": map[string]any{"K": "v"}}}},
		{"nested-slice", QueryResult{Datum: []any{"a", []byte("bc"), int64(3)}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := estimateQueryResultBytes(tc.qr) // must not panic
			if got <= 0 {
				t.Fatalf("estimate for %s must be a small positive constant, got %d", tc.name, got)
			}
		})
	}
}

// --- ExecuteState semantics (RFC-130 §2.1) ---

// TestExecuteState_Unlimited proves memLimit<=0 (and a nil receiver) is a
// no-op: any charge succeeds and (for the active-counter <=0 case) memUsed is
// never even touched.
func TestExecuteState_Unlimited(t *testing.T) {
	t.Parallel()

	var nilState *recordlayer.ExecuteState
	if err := nilState.ChargeMemory(1 << 40); err != nil {
		t.Fatalf("nil ExecuteState must no-op, got %v", err)
	}

	unl := recordlayer.NewExecuteState(0)
	if err := unl.ChargeMemory(1 << 40); err != nil {
		t.Fatalf("memLimit=0 must be unlimited, got %v", err)
	}
	if unl.MemUsed() != 0 {
		t.Fatalf("unlimited state must not accumulate memUsed, got %d", unl.MemUsed())
	}

	neg := recordlayer.NewExecuteState(-1)
	if err := neg.ChargeMemory(1 << 40); err != nil {
		t.Fatalf("memLimit<0 must be unlimited, got %v", err)
	}
}

// TestExecuteState_TripsOnSum proves a budget trips when the CUMULATIVE charge
// crosses the limit — not on a single charge — and the error carries the
// would-be total and the limit.
func TestExecuteState_TripsOnSum(t *testing.T) {
	t.Parallel()

	st := recordlayer.NewExecuteState(100)
	if err := st.ChargeMemory(60); err != nil {
		t.Fatalf("60 <= 100 must succeed, got %v", err)
	}
	if st.MemUsed() != 60 {
		t.Fatalf("memUsed = %d, want 60", st.MemUsed())
	}
	err := st.ChargeMemory(50) // 110 > 100
	if err == nil {
		t.Fatal("60+50=110 > 100 must trip the budget")
	}
	var memErr *recordlayer.MemoryLimitExceededError
	if !errors.As(err, &memErr) {
		t.Fatalf("want *MemoryLimitExceededError, got %T: %v", err, err)
	}
	if memErr.Used != 110 || memErr.Limit != 100 {
		t.Fatalf("error = {Used:%d Limit:%d}, want {110 100}", memErr.Used, memErr.Limit)
	}
}

// TestExecuteState_ExactBoundary proves the budget trips strictly ABOVE the
// limit (== is allowed), matching ChargeMemory's `> memLimit` check.
func TestExecuteState_ExactBoundary(t *testing.T) {
	t.Parallel()

	st := recordlayer.NewExecuteState(100)
	if err := st.ChargeMemory(100); err != nil {
		t.Fatalf("exactly at the limit (100==100) must succeed, got %v", err)
	}
	if err := st.ChargeMemory(1); err == nil {
		t.Fatal("one byte over (101>100) must trip")
	}
}

// --- boundedBuffer / boundedSet (RFC-130 §2.3) ---

// TestBoundedBuffer_ChargesAndRowCap proves Append charges the statement state
// and still enforces the row cap with the pre-RFC-130 boundary (errors on the
// item that would reach rowLimit rows).
func TestBoundedBuffer_ChargesAndRowCap(t *testing.T) {
	t.Parallel()

	st := recordlayer.NewExecuteState(0) // unlimited bytes; test the row cap
	buf := newBoundedBuffer[int](st, 3, "test", func(int) int64 { return 10 })
	for i := 0; i < 2; i++ {
		if err := buf.Append(i); err != nil {
			t.Fatalf("append %d under cap must succeed, got %v", i, err)
		}
	}
	// rowLimit=3 → the 3rd append (reaching 3 rows) errors, holding at most 2.
	err := buf.Append(99)
	if err == nil {
		t.Fatal("append reaching rowLimit must error")
	}
	var mlErr *MaterializationLimitExceededError
	if !errors.As(err, &mlErr) {
		t.Fatalf("want *MaterializationLimitExceededError, got %T", err)
	}
	if buf.Len() != 2 {
		t.Fatalf("buffer must hold rowLimit-1=2 items, got %d", buf.Len())
	}
}

// TestBoundedBuffer_ByteBudgetTrips proves the byte budget can trip BELOW the
// row cap — the whole point of RFC-130.
func TestBoundedBuffer_ByteBudgetTrips(t *testing.T) {
	t.Parallel()

	st := recordlayer.NewExecuteState(100)                                             // tiny byte budget
	buf := newBoundedBuffer[int](st, 1_000_000, "test", func(int) int64 { return 60 }) // huge row cap
	// Each item is "60 bytes"; the 2nd append crosses 100.
	if err := buf.Append(1); err != nil {
		t.Fatalf("first append (60<=100) must succeed, got %v", err)
	}
	err := buf.Append(2) // 120 > 100
	if err == nil {
		t.Fatal("byte budget must trip before the row cap")
	}
	var memErr *recordlayer.MemoryLimitExceededError
	if !errors.As(err, &memErr) {
		t.Fatalf("want *MemoryLimitExceededError, got %T", err)
	}
}

// TestBoundedSet_ChargesNewKeysOnly proves a set charges only on a NEW key; a
// duplicate Add is free (returns added=false, no charge, no error).
func TestBoundedSet_ChargesNewKeysOnly(t *testing.T) {
	t.Parallel()

	st := recordlayer.NewExecuteState(1000)
	set := newBoundedSet[string](st)

	added, err := set.Add("alpha", 5)
	if err != nil || !added {
		t.Fatalf("first Add(alpha) = (%v,%v), want (true,nil)", added, err)
	}
	if st.MemUsed() != 5 {
		t.Fatalf("memUsed = %d, want 5 after one new key", st.MemUsed())
	}
	added, err = set.Add("alpha", 5) // duplicate
	if err != nil || added {
		t.Fatalf("duplicate Add(alpha) = (%v,%v), want (false,nil)", added, err)
	}
	if st.MemUsed() != 5 {
		t.Fatalf("duplicate Add must not charge; memUsed = %d, want 5", st.MemUsed())
	}
	if _, err := set.Add("beta", 7); err != nil {
		t.Fatalf("Add(beta) = %v, want nil", err)
	}
	if st.MemUsed() != 12 {
		t.Fatalf("memUsed = %d, want 12 after two distinct keys", st.MemUsed())
	}
}
