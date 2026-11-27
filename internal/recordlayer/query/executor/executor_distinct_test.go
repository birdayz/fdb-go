package executor

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"

	continuationspb "fdb.dev/internal/pb/continuations"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestExecuteDistinct_DedupsValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan(cols))
	distinctPlan := mustExecutorConstruct(plans.NewRecordQueryDistinctPlan(inner))

	cursor, err := ExecutePlan(ctx, distinctPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestExecuteUnorderedPrimaryKeyDistinct_PrimaryKeySemantics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	alias := values.NamedCorrelationIdentifier("pk_distinct_semantics")
	evalCtx := EmptyEvaluationContext()
	table := evalCtx.GetOrCreateTempTable(alias, nil)
	pk1 := tuple.Tuple{"type", int64(1)}
	pk2 := tuple.Tuple{"type", int64(2)}
	rowType := exactTestRowType(values.Field{Name: "V", FieldType: values.NullableString})
	mkRow := func(pk tuple.Tuple, value string) QueryResult {
		return QueryResult{
			Positional: &PositionalRow{Type: rowType, Slots: []any{value}},
			PrimaryKey: pk,
		}
	}
	for _, row := range []QueryResult{
		mkRow(pk1, "same projected row"),
		mkRow(pk2, "same projected row"),
		mkRow(pk1, "different fanout value"),
	} {
		if err := table.Add(row); err != nil {
			t.Fatalf("seed temp table: %v", err)
		}
	}
	plan := mustExecutorConstruct(plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(
		mustTempTableScan(t, evalCtx, alias),
	))
	cursor, err := ExecutePlan(
		ctx,
		plan,
		nil,
		evalCtx,
		nil,
		recordlayer.DefaultExecuteProperties(),
	)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()
	rows, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want one row per primary key", len(rows))
	}
	if !bytes.Equal(rows[0].PrimaryKey.Pack(), pk1.Pack()) ||
		!bytes.Equal(rows[1].PrimaryKey.Pack(), pk2.Pack()) {
		t.Fatalf(
			"surviving primary keys = [%v, %v], want [%v, %v]",
			rows[0].PrimaryKey,
			rows[1].PrimaryKey,
			pk1,
			pk2,
		)
	}
	for i, row := range rows {
		valuesByName, ok := rowMapOK(row)
		if !ok || valuesByName["V"] != "same projected row" {
			t.Fatalf("row %d = %#v, want first value seen for its PK", i, row)
		}
	}
}

// TestExecuteUnorderedPrimaryKeyDistinct_ContinuationCarriesSeenSet pins the
// SCRATCH-LESS encoding: an execution that supplies no ExecutionScratch writes
// the seen-set into the continuation by value, so the bytes alone resume
// dedup-cleanly. That is the self-contained fallback, and it is quadratic
// across pages — the by-reference carry an ExecutionScratch enables is pinned
// separately in distinct_continuation_growth_test.go. The evalCtx here
// deliberately has no scratch; adding one would move this test onto the other
// encoding and stop covering this one.
func TestExecuteUnorderedPrimaryKeyDistinct_ContinuationCarriesSeenSet(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	alias := values.NamedCorrelationIdentifier("pk_distinct_resume")
	evalCtx := EmptyEvaluationContext()
	table := evalCtx.GetOrCreateTempTable(alias, nil)
	pk1 := tuple.Tuple{int64(1)}
	pk2 := tuple.Tuple{int64(2)}
	rowType := exactTestRowType(values.Field{Name: "V", FieldType: values.NullableString})
	mkRow := func(pk tuple.Tuple, value string) QueryResult {
		return QueryResult{
			Positional: &PositionalRow{Type: rowType, Slots: []any{value}},
			PrimaryKey: pk,
		}
	}
	for _, row := range []QueryResult{
		mkRow(pk1, "first"),
		mkRow(pk1, "duplicate after boundary"),
		mkRow(pk2, "second"),
	} {
		if err := table.Add(row); err != nil {
			t.Fatalf("seed temp table: %v", err)
		}
	}
	plan := mustExecutorConstruct(plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(
		mustTempTableScan(t, evalCtx, alias),
	))
	state := recordlayer.NewExecuteState(1 << 20)
	props := recordlayer.DefaultExecuteProperties()
	props.State = state

	firstPage, err := ExecutePlan(ctx, plan, nil, evalCtx, nil, props)
	if err != nil {
		t.Fatalf("first ExecutePlan: %v", err)
	}
	first, err := firstPage.OnNext(ctx)
	if err != nil || !first.HasNext() {
		t.Fatalf("first row = %#v, err = %v", first, err)
	}
	if state.MemUsed() == 0 {
		t.Fatal("seen primary-key set did not charge statement memory")
	}
	continuation, err := first.GetContinuation().ToBytes()
	if err != nil {
		t.Fatalf("encode continuation: %v", err)
	}
	var hashContinuation continuationspb.DistinctHashContinuation
	if err := hashContinuation.UnmarshalVT(continuation); err != nil {
		t.Fatalf("decode hash continuation: %v", err)
	}
	seenKeys := hashContinuation.GetSeenKeys()
	if len(seenKeys) != 1 || !bytes.Equal(seenKeys[0], pk1.Pack()) {
		t.Fatalf(
			"continuation seen keys = %q, want exact packed PK %q",
			seenKeys,
			pk1.Pack(),
		)
	}
	if err := firstPage.Close(); err != nil {
		t.Fatalf("close first page: %v", err)
	}
	if used := state.MemUsed(); used != 0 {
		t.Fatalf("first-page teardown retained %d seen-set bytes", used)
	}

	resumed, err := ExecutePlan(
		ctx,
		plan,
		nil,
		evalCtx,
		continuation,
		props,
	)
	if err != nil {
		t.Fatalf("resumed ExecutePlan: %v", err)
	}
	if state.MemUsed() == 0 {
		t.Fatal("resumed seen set was not rebuilt and re-charged")
	}
	rows, err := CollectAll(ctx, resumed)
	if err != nil {
		t.Fatalf("resumed CollectAll: %v", err)
	}
	if len(rows) != 1 || !bytes.Equal(rows[0].PrimaryKey.Pack(), pk2.Pack()) {
		t.Fatalf(
			"resumed rows = %#v, want only PK %v (duplicate PK1 must stay suppressed)",
			rows,
			pk2,
		)
	}
	if err := resumed.Close(); err != nil {
		t.Fatalf("close resumed page: %v", err)
	}
	if used := state.MemUsed(); used != 0 {
		t.Fatalf("resumed-page teardown retained %d seen-set bytes", used)
	}
}

func TestExecuteUnorderedPrimaryKeyDistinct_NilPrimaryKeyFailsLoudly(
	t *testing.T,
) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("pk_distinct_nil")
	evalCtx := EmptyEvaluationContext()
	if err := evalCtx.GetOrCreateTempTable(alias, nil).Add(
		QueryResult{Positional: &PositionalRow{
			Type:  exactTestRowType(values.Field{Name: "V", FieldType: values.NullableString}),
			Slots: []any{"computed row"},
		}},
	); err != nil {
		t.Fatalf("seed temp table: %v", err)
	}
	plan := mustExecutorConstruct(plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(
		mustTempTableScan(t, evalCtx, alias),
	))
	cursor, err := ExecutePlan(
		context.Background(),
		plan,
		nil,
		evalCtx,
		nil,
		recordlayer.DefaultExecuteProperties(),
	)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()
	_, err = cursor.OnNext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no primary key") {
		t.Fatalf("nil primary key error = %v, want loud missing-PK error", err)
	}
}

func TestDistinctKey_WithDatum(t *testing.T) {
	t.Parallel()
	pk := tuple.Tuple{int64(42)}
	qr := dmapPK(pk, map[string]any{"A": 1})
	key := mustDistinctKey(t, qr)
	if key == "" {
		t.Fatal("expected non-empty key from datum map")
	}
	qr2 := dmapPK(tuple.Tuple{int64(99)}, map[string]any{"A": 1})
	if mustDistinctKey(t, qr) != mustDistinctKey(t, qr2) {
		t.Fatal("same datum values should produce same distinct key regardless of PK")
	}
}

func TestDistinctKey_NilPrimaryKey(t *testing.T) {
	t.Parallel()
	qr := dmap(map[string]any{"A": 1})
	key := mustDistinctKey(t, qr)
	// The key is the tuple-packed slot values (Java's Key.Evaluated), not a
	// NAME=type:value string.
	expected := string(tuple.Tuple{1}.Pack())
	if key != expected {
		t.Fatalf("expected tuple-packed %q, got %q", expected, key)
	}
}

func TestDistinctKey_Deterministic(t *testing.T) {
	t.Parallel()
	// With multiple slots the packed key must be stable regardless of map
	// iteration order (dmap sorts columns by name → A,B,C).
	qr := dmap(map[string]any{"B": 2, "A": 1, "C": 3})
	key1 := mustDistinctKey(t, qr)
	key2 := mustDistinctKey(t, qr)
	if key1 != key2 {
		t.Fatalf("non-deterministic: %q vs %q", key1, key2)
	}
	expected := string(tuple.Tuple{1, 2, 3}.Pack())
	if key1 != expected {
		t.Fatalf("expected tuple-packed %q, got %q", expected, key1)
	}
}

// TestDistinctKey_DelimiterInjection pins the fix for a value forging an
// inter-column boundary. Under the old '|'-joined NAME=type:value scheme, a
// string containing the literal separator reproduced the column boundary, so
// two DIFFERENT rows keyed identically and DISTINCT dropped the second. Tuple
// packing is length-prefixed, so the keys must differ.
func TestDistinctKey_DelimiterInjection(t *testing.T) {
	t.Parallel()
	rowA := dorder([]string{"A", "B"}, []any{"x", "y|B=string:z"})
	rowB := dorder([]string{"A", "B"}, []any{"x|B=string:y", "z"})
	if mustDistinctKey(t, rowA) == mustDistinctKey(t, rowB) {
		t.Fatalf("delimiter injection: distinct rows keyed identically (%q)", mustDistinctKey(t, rowA))
	}
}

// TestQueryResultKey_DelimiterInjection pins F31: the recursive-CTE UNION-DISTINCT
// keyers (queryResultKey / cteDedupKeyer.key) had the same '|'-joined %v flaw as
// distinctKey, and — unlike distinctKey — no per-slot type prefix, so the
// "\x00NULL\x00" NULL sentinel also collided with the literal string. All three
// now route through packedDedupKey (tuple encoding). Revert-proven: on the old
// '|'-join both injection pairs key identically.
func TestQueryResultKey_DelimiterInjection(t *testing.T) {
	t.Parallel()
	// Delimiter injection: ["x","y|z"] and ["x|y","z"] both rendered "x|y|z".
	r1 := dorder([]string{"A", "B"}, []any{"x", "y|z"})
	r2 := dorder([]string{"A", "B"}, []any{"x|y", "z"})
	if mustQueryResultKey(t, r1) == mustQueryResultKey(t, r2) {
		t.Fatalf("queryResultKey delimiter injection: distinct rows keyed identically (%q)", mustQueryResultKey(t, r1))
	}
	// NULL-sentinel: the literal string "\x00NULL\x00" must not key equal to SQL NULL.
	rNull := dorder([]string{"A"}, []any{nil})
	rSent := dorder([]string{"A"}, []any{"\x00NULL\x00"})
	if mustQueryResultKey(t, rNull) == mustQueryResultKey(t, rSent) {
		t.Fatal("queryResultKey: NULL sentinel collides with the literal string \\x00NULL\\x00")
	}
	// Preserved: same values under differently-named columns still dedup (the
	// recursive-CTE seed {SRC:1} vs recursive {DST:1} case, values-only + name-sorted).
	rSrc := dorder([]string{"SRC"}, []any{int64(1)})
	rDst := dorder([]string{"DST"}, []any{int64(1)})
	if mustQueryResultKey(t, rSrc) != mustQueryResultKey(t, rDst) {
		t.Fatal("queryResultKey: {SRC:1} and {DST:1} must still dedup as duplicates")
	}
}

// TestDistinctKey_NullSentinelCollision guards that SQL NULL never keys equal
// to a string that spells the "\x00NULL\x00" sentinel. Tuple packing gives nil
// its own type code, so the two can never collide. (This is a guard, not a
// revert-proof: the old distinctKey happened to disambiguate these two via its
// per-slot "type:" prefix; the sibling recursive-CTE keyers that omit that
// prefix are the ones with a genuine sentinel collision.)
func TestDistinctKey_NullSentinelCollision(t *testing.T) {
	t.Parallel()
	nullRow := dorder([]string{"A"}, []any{nil})
	sentinelStr := dorder([]string{"A"}, []any{"\x00NULL\x00"})
	if mustDistinctKey(t, nullRow) == mustDistinctKey(t, sentinelStr) {
		t.Fatal("SQL NULL keyed equal to a string holding the NULL sentinel")
	}
}

// --- compareValues unit tests ---

// TestCompareValues_UnsignedIntegerDomain pins the unsigned half of the
// integer row domain:
// tuple decoding preserves positive integers above math.MaxInt64 as uint64
// (tuple.go's only unsigned return), so uint64 reaches the sort/merge
// comparator on VALID same-domain data. The loud cross-type arm rejecting
// it failed real ORDER BY / merge-union execution. Integer pairs compare
// EXACTLY — float promotion would tie the adjacent boundary pair
// (float64(2^63) == float64(2^63-1)).
func TestCompareValues_UnsignedIntegerDomain(t *testing.T) {
	t.Parallel()
	big := uint64(math.MaxInt64) + 1
	if mustCompareValues(t, big, uint64(math.MaxUint64)) >= 0 {
		t.Fatal("2^63 < MaxUint64")
	}
	if mustCompareValues(t, big, big) != 0 {
		t.Fatal("uint64 self-compare must be 0")
	}
	// Adjacent across the signed/unsigned boundary: exact, not float-tied.
	if mustCompareValues(t, big, int64(math.MaxInt64)) <= 0 {
		t.Fatal("2^63 > MaxInt64 (float promotion would tie this pair)")
	}
	if mustCompareValues(t, int64(math.MaxInt64), big) >= 0 {
		t.Fatal("MaxInt64 < 2^63")
	}
	// Any negative sorts below any unsigned value.
	if mustCompareValues(t, uint64(0), int64(-1)) <= 0 {
		t.Fatal("uint64(0) > int64(-1)")
	}
	if mustCompareValues(t, int64(math.MinInt64), uint64(0)) >= 0 {
		t.Fatal("MinInt64 < uint64(0) (MinInt64 negation must not overflow)")
	}
	// Plain int is an admitted integer form on BOTH sides (the a-side
	// previously fell through to the loud arm asymmetrically).
	if mustCompareValues(t, int(5), int64(7)) >= 0 {
		t.Fatal("int(5) < int64(7)")
	}
	if mustCompareValues(t, int64(7), int(5)) <= 0 {
		t.Fatal("int64(7) > int(5)")
	}
	// Integer vs float still uses the float total order.
	if mustCompareValues(t, uint64(3), 3.5) >= 0 {
		t.Fatal("uint64(3) < 3.5")
	}
	// Cross-type stays loud: the unsigned arm must not have widened the
	// comparator into accepting non-numeric pairs.
	if _, err := compareValues(uint64(1), "x"); err == nil {
		t.Fatal("uint64 vs string must stay a loud cross-type error")
	}
}

func TestCompareValues_NullHandling(t *testing.T) {
	t.Parallel()
	if mustCompareValues(t, nil, nil) != 0 {
		t.Fatal("nil == nil should be 0")
	}
	if mustCompareValues(t, nil, int64(1)) >= 0 {
		t.Fatal("nil < non-nil")
	}
	if mustCompareValues(t, int64(1), nil) <= 0 {
		t.Fatal("non-nil > nil")
	}
}

func TestCompareValues_NumericTypes(t *testing.T) {
	t.Parallel()
	if mustCompareValues(t, int64(1), int64(2)) >= 0 {
		t.Fatal("1 < 2")
	}
	if mustCompareValues(t, int64(2), int64(1)) <= 0 {
		t.Fatal("2 > 1")
	}
	if mustCompareValues(t, int64(42), float64(42.0)) != 0 {
		t.Fatal("int64(42) == float64(42.0)")
	}
	if mustCompareValues(t, float64(3.14), int64(3)) <= 0 {
		t.Fatal("3.14 > 3")
	}
}

// The sort/merge/dedup comparator must impose the SAME total order the FDB
// tuple encoding gives an indexed FLOAT column, so an in-memory ORDER BY and
// an ordered index scan of the same data agree. That order is Java's
// Double.compare: -0.0 sorts strictly before 0.0, NaN sorts LAST (greatest),
// and NaN compares equal to NaN. A native float `<`/`>`/`==` comparator gets
// both edge values wrong (-0.0 == 0.0, and NaN != NaN with `<`/`>` both false).
func TestCompareValues_FloatTotalOrder(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	cases := []struct {
		name string
		a, b any
		want int // sign: -1 a<b, 0 equal, +1 a>b
	}{
		{"neg_zero_before_pos_zero", math.Copysign(0, -1), 0.0, -1},
		{"pos_zero_after_neg_zero", 0.0, math.Copysign(0, -1), 1},
		{"neg_zero_equals_itself", math.Copysign(0, -1), math.Copysign(0, -1), 0},
		{"pos_zero_equals_itself", 0.0, 0.0, 0},
		{"nan_greater_than_finite", nan, 5.0, 1},
		{"finite_less_than_nan", 5.0, nan, -1},
		{"nan_equals_nan", nan, nan, 0},
		{"nan_greater_than_neg", nan, -5.0, 1},
		{"nan_greater_than_inf", nan, math.Inf(1), 1},
		{"nan_greater_than_int", nan, int64(100), 1},
		{"int_less_than_nan", int64(100), nan, -1},
		// float32 arm mirrors float64 on both edge values.
		{"f32_neg_zero_before_pos_zero", float32(math.Copysign(0, -1)), float32(0.0), -1},
		{"f32_nan_greater_than_finite", float32(math.NaN()), float32(5.0), 1},
		// Normal finite values are unaffected by the edge-value handling.
		{"finite_ordering_unchanged", 2.5, 10.5, -1},
		{"finite_equal_unchanged", 3.25, 3.25, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mustCompareValues(t, c.a, c.b)
			if (c.want < 0 && got >= 0) || (c.want > 0 && got <= 0) || (c.want == 0 && got != 0) {
				t.Errorf("mustCompareValues(t, %v, %v) = %d, want sign %d", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestCompareValues_CrossTypeNotEqual(t *testing.T) {
	t.Parallel()
	// The former contract was "not silently equal" via the fmt fallback's
	// lexical order; the contract is now strictly stronger — cross-type
	// pairs ERROR (correct-or-loud), so they can never compare equal OR
	// misordered.
	if _, err := compareValues(int64(42), "hello"); err == nil {
		t.Fatal("int64 vs string must error loudly")
	}
	if _, err := compareValues(float64(3.14), "world"); err == nil {
		t.Fatal("float64 vs string must error loudly")
	}
}

func TestCompareValues_Strings(t *testing.T) {
	t.Parallel()
	if mustCompareValues(t, "abc", "def") >= 0 {
		t.Fatal("abc < def")
	}
	if mustCompareValues(t, "xyz", "abc") <= 0 {
		t.Fatal("xyz > abc")
	}
	if mustCompareValues(t, "same", "same") != 0 {
		t.Fatal("same == same")
	}
}

// BYTES must compare in unsigned lexicographic byte order — FDB tuple order —
// not by the fmt fallback's decimal-list string ("[0 1]" < "[0]" because
// ' ' < ']'). Regression: an unindexed ORDER BY on a BYTES column disagreed
// with the same query's indexed plan (F9/F13).
func TestCompareValues_Bytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b []byte
		want int // sign
	}{
		{"single_bytes_numeric_not_lexical", []byte{0x02}, []byte{0x0A}, -1}, // fmt: "[2]" > "[10]"
		{"prefix_sorts_first", []byte{0x00}, []byte{0x00, 0x01}, -1},         // fmt: "[0]" > "[0 1]"
		{"high_byte_beats_longer", []byte{0xFF}, []byte{0x00, 0x01}, 1},
		{"empty_before_zero", []byte{}, []byte{0x00}, -1},
		{"equal", []byte{0x00, 0xFF}, []byte{0x00, 0xFF}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mustCompareValues(t, c.a, c.b)
			if (c.want < 0 && got >= 0) || (c.want > 0 && got <= 0) || (c.want == 0 && got != 0) {
				t.Errorf("mustCompareValues(t, %v, %v) = %d, want sign %d", c.a, c.b, got, c.want)
			}
			// Antisymmetry: the reversed call must flip the sign.
			rev := mustCompareValues(t, c.b, c.a)
			if (got < 0 && rev <= 0) || (got > 0 && rev >= 0) || (got == 0 && rev != 0) {
				t.Errorf("compareValues not antisymmetric: (a,b)=%d (b,a)=%d", got, rev)
			}
		})
	}
}

// float32 must compare numerically (defense in depth behind the covering-read
// normalization): the fmt fallback compared "10.5" < "2.5" lexically (F10).
func TestCompareValues_Float32(t *testing.T) {
	t.Parallel()
	if mustCompareValues(t, float32(10.5), float32(2.5)) <= 0 {
		t.Fatal("float32 10.5 > 2.5 (was lexical \"10.5\" < \"2.5\")")
	}
	if mustCompareValues(t, float32(2.5), float32(10.5)) >= 0 {
		t.Fatal("float32 2.5 < 10.5")
	}
	if mustCompareValues(t, float32(1.5), float64(1.5)) != 0 {
		t.Fatal("float32(1.5) == float64(1.5) across widths")
	}
	if mustCompareValues(t, float64(1.5), float32(1.5)) != 0 {
		t.Fatal("float64(1.5) == float32(1.5) across widths")
	}
	if mustCompareValues(t, float32(2.5), int64(3)) >= 0 {
		t.Fatal("float32(2.5) < int64(3)")
	}
	if mustCompareValues(t, int64(3), float32(2.5)) <= 0 {
		t.Fatal("int64(3) > float32(2.5)")
	}
}

// bool: false < true — FDB tuple order (0x26 < 0x27). Previously correct only
// by lexical accident ("false" < "true" in the fmt fallback); now pinned.
func TestCompareValues_Bool(t *testing.T) {
	t.Parallel()
	if mustCompareValues(t, false, true) >= 0 {
		t.Fatal("false < true")
	}
	if mustCompareValues(t, true, false) <= 0 {
		t.Fatal("true > false")
	}
	if mustCompareValues(t, true, true) != 0 || mustCompareValues(t, false, false) != 0 {
		t.Fatal("equal bools == 0")
	}
}

// TestCompareValues_CrossTypeIsLoud pins the correct-or-loud contract on the
// comparison authority: a pair with no typed arm errors instead of the
// retired fmt fallback's silently-wrong lexical order.
func TestCompareValues_CrossTypeIsLoud(t *testing.T) {
	t.Parallel()
	if _, err := compareValues("x", int64(1)); err == nil {
		t.Fatal("string vs int64 has no defined ordering — must error, never compare lexically")
	}
	if _, err := compareValues([]byte{1}, "x"); err == nil {
		t.Fatal("bytes vs string must error")
	}
}
