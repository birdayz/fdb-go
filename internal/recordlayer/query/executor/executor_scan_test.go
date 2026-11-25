package executor

import (
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestScanComparisonsToTupleRange_Empty(t *testing.T) {
	t.Parallel()
	spec, err := bindScanComparisonsToRangeSet(nil, nil, nil, false, "executor-empty-comparisons")
	if err != nil {
		t.Fatal(err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 1 {
		t.Fatalf("opened %d ranges, want 1", len(ranges))
	}
	assertIsAllRange(t, ranges[0])
}

func TestScanComparisonsToTupleRange_EqualityOnly(t *testing.T) {
	t.Parallel()
	eq1 := predicates.EmptyComparisonRange()
	res := eq1.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("alice")})
	if !res.Complete() {
		t.Fatal("merge failed")
	}

	eq2 := predicates.EmptyComparisonRange()
	res2 := eq2.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(42))})
	if !res2.Complete() {
		t.Fatal("merge2 failed")
	}

	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{res.Range, res2.Range},
		[]values.Type{values.NullableString, values.NullableLong},
		nil, false, "executor-equality-only",
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 1 {
		t.Fatalf("opened %d ranges, want 1", len(ranges))
	}
	r := ranges[0]

	wantPrefix := tuple.Tuple{"alice", int64(42)}
	if len(r.Low) != len(wantPrefix) {
		t.Fatalf("low=%v, want prefix %v", r.Low, wantPrefix)
	}
	for i, v := range wantPrefix {
		if r.Low[i] != v {
			t.Fatalf("low[%d]=%v, want %v", i, r.Low[i], v)
		}
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("lowEndpoint=%v, want RangeInclusive", r.LowEndpoint)
	}
}

func TestScanComparisonsToTupleRange_EqualityPlusInequality(t *testing.T) {
	t.Parallel()

	eq := predicates.EmptyComparisonRange()
	res := eq.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("users")})
	if !res.Complete() {
		t.Fatal("merge eq failed")
	}

	ineq := predicates.EmptyComparisonRange()
	gt := &predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(10))}
	res2 := ineq.Merge(gt)
	if !res2.Complete() {
		t.Fatal("merge gt failed")
	}
	lt := &predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: values.LiteralValue(int64(100))}
	res3 := res2.Range.Merge(lt)
	if !res3.Complete() {
		t.Fatal("merge lt failed")
	}

	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{res.Range, res3.Range},
		[]values.Type{values.NullableString, values.NullableLong},
		nil, false, "executor-equality-plus-inequality",
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 1 {
		t.Fatalf("opened %d ranges, want 1", len(ranges))
	}
	r := ranges[0]

	if len(r.Low) != 2 || r.Low[0] != "users" || r.Low[1] != int64(10) {
		t.Fatalf("low=%v, want [users, 10]", r.Low)
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("lowEndpoint=%v, want RangeExclusive", r.LowEndpoint)
	}
	if len(r.High) != 2 || r.High[0] != "users" || r.High[1] != int64(100) {
		t.Fatalf("high=%v, want [users, 100]", r.High)
	}
	if r.HighEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("highEndpoint=%v, want RangeExclusive", r.HighEndpoint)
	}
}

func TestScanComparisonsToTupleRange_InequalityOnly(t *testing.T) {
	t.Parallel()

	ineq := predicates.EmptyComparisonRange()
	gte := &predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(5))}
	res := ineq.Merge(gte)
	if !res.Complete() {
		t.Fatal("merge gte failed")
	}

	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{res.Range},
		[]values.Type{values.NullableLong},
		nil, false, "executor-inequality-only",
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 1 {
		t.Fatalf("opened %d ranges, want 1", len(ranges))
	}
	r := ranges[0]

	if len(r.Low) != 1 || r.Low[0] != int64(5) {
		t.Fatalf("low=%v, want [5]", r.Low)
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("lowEndpoint=%v, want RangeInclusive (>=)", r.LowEndpoint)
	}
	if r.High != nil {
		t.Fatalf("high=%v, want nil (no upper bound)", r.High)
	}
	if r.HighEndpoint != recordlayer.EndpointTypeTreeEnd {
		t.Fatalf("highEndpoint=%v, want TreeEnd", r.HighEndpoint)
	}
}

func TestScanComparisonsToTupleRange_EmptyRangeStops(t *testing.T) {
	t.Parallel()

	eq := predicates.EmptyComparisonRange()
	res := eq.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("x")})
	if !res.Complete() {
		t.Fatal("merge failed")
	}

	empty := predicates.EmptyComparisonRange()

	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{res.Range, empty},
		[]values.Type{values.NullableString, values.NullableLong},
		nil, false, "executor-empty-range-stops",
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 1 {
		t.Fatalf("opened %d ranges, want 1", len(ranges))
	}
	r := ranges[0]

	wantPrefix := tuple.Tuple{"x"}
	if len(r.Low) != 1 || r.Low[0] != "x" {
		t.Fatalf("low=%v, want prefix %v", r.Low, wantPrefix)
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("lowEndpoint=%v, want RangeInclusive (prefix scan)", r.LowEndpoint)
	}
}

func TestScanComparisonsToTupleRange_LessThanOnly(t *testing.T) {
	t.Parallel()

	ineq := predicates.EmptyComparisonRange()
	lt := &predicates.Comparison{Type: predicates.ComparisonLessThanOrEq, Operand: values.LiteralValue(int64(50))}
	res := ineq.Merge(lt)
	if !res.Complete() {
		t.Fatal("merge lte failed")
	}

	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{res.Range},
		[]values.Type{values.NullableLong},
		nil, false, "executor-less-than-only",
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 1 {
		t.Fatalf("opened %d ranges, want 1", len(ranges))
	}
	r := ranges[0]

	// Upper-only `<= 50` excludes nulls: low is the NULL boundary (one nil
	// tuple element) RANGE_EXCLUSIVE, not TreeStart. Mirrors Java ScanComparisons.
	if len(r.Low) != 1 || r.Low[0] != nil {
		t.Fatalf("low=%v, want [null] (null boundary)", r.Low)
	}
	if r.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("lowEndpoint=%v, want RangeExclusive (null boundary)", r.LowEndpoint)
	}
	if len(r.High) != 1 || r.High[0] != int64(50) {
		t.Fatalf("high=%v, want [50]", r.High)
	}
	if r.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("highEndpoint=%v, want RangeInclusive (<=)", r.HighEndpoint)
	}
}

// TestScanComparisonsToTupleRange_StartsWith pins the STARTS_WITH → PREFIX_STRING
// range. STARTS_WITH is planner-sargable (it binds into a ComparisonRange and the
// residual filter is dropped), so the scan MUST bound itself to the prefix. Mirrors
// Java ScanComparisons.toTupleRange building
// new TupleRange(startTuple, startTuple, PREFIX_STRING, PREFIX_STRING).
//
// Revert-proof: without the STARTS_WITH arm in the range binder the comparison
// matches neither the low nor the high switch, sets no bound, and the range
// degenerates to TREE_START..TREE_END — silently returning every row instead of
// only the prefix-matching ones.
func TestScanComparisonsToTupleRange_StartsWith(t *testing.T) {
	t.Parallel()

	ineq := predicates.EmptyComparisonRange()
	res := ineq.Merge(&predicates.Comparison{Type: predicates.ComparisonStartsWith, Operand: values.LiteralValue("abc")})
	if !res.Complete() {
		t.Fatal("merge STARTS_WITH failed")
	}

	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{res.Range},
		[]values.Type{values.NullableString},
		nil, false, "executor-starts-with",
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 1 {
		t.Fatalf("opened %d ranges, want 1", len(ranges))
	}
	r := ranges[0]

	if len(r.Low) != 1 || r.Low[0] != "abc" {
		t.Fatalf("low=%v, want [abc]", r.Low)
	}
	if len(r.High) != 1 || r.High[0] != "abc" {
		t.Fatalf("high=%v, want [abc]", r.High)
	}
	if r.LowEndpoint != recordlayer.EndpointTypePrefixString {
		t.Fatalf("lowEndpoint=%v, want PrefixString", r.LowEndpoint)
	}
	if r.HighEndpoint != recordlayer.EndpointTypePrefixString {
		t.Fatalf("highEndpoint=%v, want PrefixString", r.HighEndpoint)
	}
}

// TestScanComparisonsToTupleRange_EqualityPlusStartsWith pins that the STARTS_WITH
// prefix element is appended AFTER the equality prefix — startTuple = prefix +
// comparand, both endpoints PREFIX_STRING (Java ScanComparisons.toTupleRange
// baseTuple.addObject(comparand)).
func TestScanComparisonsToTupleRange_EqualityPlusStartsWith(t *testing.T) {
	t.Parallel()

	eq := predicates.EmptyComparisonRange()
	resEq := eq.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(7))})
	if !resEq.Complete() {
		t.Fatal("merge eq failed")
	}

	ineq := predicates.EmptyComparisonRange()
	resSw := ineq.Merge(&predicates.Comparison{Type: predicates.ComparisonStartsWith, Operand: values.LiteralValue("abc")})
	if !resSw.Complete() {
		t.Fatal("merge STARTS_WITH failed")
	}

	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{resEq.Range, resSw.Range},
		[]values.Type{values.NullableLong, values.NullableString},
		nil, false, "executor-equality-plus-starts-with",
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 1 {
		t.Fatalf("opened %d ranges, want 1", len(ranges))
	}
	r := ranges[0]

	if len(r.Low) != 2 || r.Low[0] != int64(7) || r.Low[1] != "abc" {
		t.Fatalf("low=%v, want [7, abc]", r.Low)
	}
	if len(r.High) != 2 || r.High[0] != int64(7) || r.High[1] != "abc" {
		t.Fatalf("high=%v, want [7, abc]", r.High)
	}
	if r.LowEndpoint != recordlayer.EndpointTypePrefixString || r.HighEndpoint != recordlayer.EndpointTypePrefixString {
		t.Fatalf("endpoints=%v/%v, want PrefixString/PrefixString", r.LowEndpoint, r.HighEndpoint)
	}
}

// TestScanComparisonsToTupleRange_StartsWithPlusInequality_Loud pins F39: a
// STARTS_WITH merged with a SECOND inequality on the same column (so the
// len(ineqs)==1 PREFIX_STRING fast-path is skipped and STARTS_WITH reaches the
// endpoint combiner) must FAIL LOUD, mirroring Java
// ScanComparisons.InequalityRangeCombiner.addComparison's `default: throw`.
// STARTS_WITH ∩ (> v) is not a representable single scan range; a binder with no
// STARTS_WITH case and no default in the endpoint switch silently drops the
// STARTS_WITH bound and returns a superset (every row matching the bare `> v`,
// ignoring the prefix).
//
// Revert-proof: without the shape rejection the binder returns (spec, nil) with
// only the `> v` bound applied — err==nil and the prefix silently lost. The test
// asserts the typed InvalidScanComparisonShapeError, not its wording.
func TestScanComparisonsToTupleRange_StartsWithPlusInequality_Loud(t *testing.T) {
	t.Parallel()

	// One inequality ComparisonRange carrying BOTH comparisons on the same column.
	ineq := predicates.EmptyComparisonRange()
	resSw := ineq.Merge(&predicates.Comparison{Type: predicates.ComparisonStartsWith, Operand: values.LiteralValue("abc")})
	if !resSw.Complete() {
		t.Fatal("merge STARTS_WITH failed")
	}
	resBoth := resSw.Range.Merge(&predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue("abd")})
	if !resBoth.Complete() {
		t.Fatal("merge STARTS_WITH + GREATER_THAN failed")
	}
	if got := len(resBoth.Range.GetInequalityComparisons()); got != 2 {
		t.Fatalf("expected 2 inequalities in the merged range, got %d", got)
	}

	_, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{resBoth.Range},
		[]values.Type{values.NullableString},
		nil, false, "executor-starts-with-plus-inequality",
	)
	if err == nil {
		t.Fatal("STARTS_WITH combined with a second inequality must fail loud (Java default: throw), got nil error")
	}
	var shape *InvalidScanComparisonShapeError
	if !errors.As(err, &shape) || shape.Component != 0 {
		t.Fatalf("error = %T(%v), want InvalidScanComparisonShapeError at component 0", err, err)
	}
}

func TestScanComparisons_Empty(t *testing.T) {
	t.Parallel()
	assertIsAllRange(t, bindOneRange(t, nil, nil, "scan-comparisons-empty"))
}

func TestScanComparisons_EmptySlice(t *testing.T) {
	t.Parallel()
	assertIsAllRange(t, bindOneRange(
		t, []*predicates.ComparisonRange{}, nil, "scan-comparisons-empty-slice"))
}

func TestScanComparisons_SingleEquality(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{eqRange("alice")},
		[]values.Type{values.NullableString},
		"scan-comparisons-single-equality")
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeInclusive || tr.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected inclusive/inclusive, got low=%d high=%d", tr.LowEndpoint, tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != "alice" {
		t.Fatalf("expected low=[alice], got %v", tr.Low)
	}
	if len(tr.High) != 1 || tr.High[0] != "alice" {
		t.Fatalf("expected high=[alice], got %v", tr.High)
	}
}

func TestScanComparisons_MultiEquality(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			eqRange("alice"),
			eqRange(int64(42)),
		},
		[]values.Type{values.NullableString, values.NullableLong},
		"scan-comparisons-multi-equality")
	if len(tr.Low) != 2 || tr.Low[0] != "alice" || tr.Low[1] != int64(42) {
		t.Fatalf("expected low=[alice 42], got %v", tr.Low)
	}
	if len(tr.High) != 2 || tr.High[0] != "alice" || tr.High[1] != int64(42) {
		t.Fatalf("expected high=[alice 42], got %v", tr.High)
	}
}

func TestScanComparisons_EqualityThenEmpty(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			eqRange("alice"),
			predicates.EmptyComparisonRange(),
		},
		[]values.Type{values.NullableString, values.NullableLong},
		"scan-comparisons-equality-then-empty")
	if len(tr.Low) != 1 || tr.Low[0] != "alice" {
		t.Fatalf("expected prefix [alice], got low=%v", tr.Low)
	}
}

func TestScanComparisons_GreaterThanNoPrefix(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			ineqRange(predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10))),
		},
		[]values.Type{values.NullableLong},
		"scan-comparisons-gt-no-prefix")
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low exclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeTreeEnd {
		t.Fatalf("expected high TreeEnd, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != int64(10) {
		t.Fatalf("expected low=[10], got %v", tr.Low)
	}
	if tr.High != nil {
		t.Fatalf("expected nil high, got %v", tr.High)
	}
}

func TestScanComparisons_GreaterThanOrEqNoPrefix(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			ineqRange(predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, int64(10))),
		},
		[]values.Type{values.NullableLong},
		"scan-comparisons-gte-no-prefix")
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected low inclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeTreeEnd {
		t.Fatalf("expected high TreeEnd, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != int64(10) {
		t.Fatalf("expected low=[10], got %v", tr.Low)
	}
}

func TestScanComparisons_LessThanNoPrefix(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			ineqRange(predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(50))),
		},
		[]values.Type{values.NullableLong},
		"scan-comparisons-lt-no-prefix")
	// An upper-only range must EXCLUDE NULL index entries (NULL sorts first;
	// `a < 50` is UNKNOWN, not TRUE, on NULL). The low bound is therefore the
	// NULL boundary — one nil tuple element, RANGE_EXCLUSIVE — not TreeStart,
	// which would sweep nulls in. Mirrors Java ScanComparisons.
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low RangeExclusive (null boundary) for LT-only, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected high exclusive, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != nil {
		t.Fatalf("expected low=[null] (null boundary), got %v", tr.Low)
	}
	if len(tr.High) != 1 || tr.High[0] != int64(50) {
		t.Fatalf("expected high=[50], got %v", tr.High)
	}
}

func TestScanComparisons_LessThanOrEqNoPrefix(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			ineqRange(predicates.NewLiteralComparison(predicates.ComparisonLessThanOrEq, int64(50))),
		},
		[]values.Type{values.NullableLong},
		"scan-comparisons-lte-no-prefix")
	// Upper-only range excludes nulls via the NULL boundary low (see LT case).
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low RangeExclusive (null boundary) for LTE-only, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected high inclusive, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != nil {
		t.Fatalf("expected low=[null] (null boundary), got %v", tr.Low)
	}
	if len(tr.High) != 1 || tr.High[0] != int64(50) {
		t.Fatalf("expected high=[50], got %v", tr.High)
	}
}

func TestScanComparisons_NullComparand_EmptyRange(t *testing.T) {
	t.Parallel()
	// `a < NULL` (and >, >=, <=) is UNKNOWN for every row (SQL 3VL) →
	// unsatisfiable → empty result. The live range set says so structurally:
	// spec.empty with no materializer at all, so no range is ever opened. It
	// must NOT be the null-boundary low with an unbounded high, which would
	// strinc to an inverted FDB range (begin > end).
	for _, typ := range []predicates.ComparisonType{
		predicates.ComparisonLessThan,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonGreaterThanEq,
	} {
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{
				ineqRange(predicates.Comparison{Type: typ, Operand: &values.NullValue{}}),
			},
			[]values.Type{values.NullableLong},
			nil, false, "scan-comparisons-null-comparand",
		)
		if err != nil {
			t.Fatalf("%v: unexpected error: %v", typ, err)
		}
		if !spec.empty || spec.materialize != nil {
			t.Fatalf("%v: expected an empty range set, got empty=%v materialize!=nil=%v",
				typ, spec.empty, spec.materialize != nil)
		}
		if got := materializeAllRanges(t, spec); len(got) != 0 {
			t.Fatalf("%v: empty range set opened %d ranges: %v", typ, len(got), got)
		}
	}
}

func TestScanComparisons_BetweenGTAndLT(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			ineqRange(
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10)),
				predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(50)),
			),
		},
		[]values.Type{values.NullableLong},
		"scan-comparisons-between-gt-lt")
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low exclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected high exclusive, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 1 || tr.Low[0] != int64(10) {
		t.Fatalf("expected low=[10], got %v", tr.Low)
	}
	if len(tr.High) != 1 || tr.High[0] != int64(50) {
		t.Fatalf("expected high=[50], got %v", tr.High)
	}
}

func TestScanComparisons_BetweenGTEAndLTE(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			ineqRange(
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, int64(10)),
				predicates.NewLiteralComparison(predicates.ComparisonLessThanOrEq, int64(50)),
			),
		},
		[]values.Type{values.NullableLong},
		"scan-comparisons-between-gte-lte")
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected low inclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected high inclusive, got %d", tr.HighEndpoint)
	}
}

func TestScanComparisons_EqualityPrefixThenGT(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			eqRange("alice"),
			ineqRange(predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10))),
		},
		[]values.Type{values.NullableString, values.NullableLong},
		"scan-comparisons-equality-prefix-gt")
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low exclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected high inclusive (prefix default), got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 2 || tr.Low[0] != "alice" || tr.Low[1] != int64(10) {
		t.Fatalf("expected low=[alice 10], got %v", tr.Low)
	}
	if len(tr.High) != 1 || tr.High[0] != "alice" {
		t.Fatalf("expected high=[alice] (prefix only), got %v", tr.High)
	}
}

func TestScanComparisons_EqualityPrefixThenLT(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			eqRange("alice"),
			ineqRange(predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(50))),
		},
		[]values.Type{values.NullableString, values.NullableLong},
		"scan-comparisons-equality-prefix-lt")
	// With an equality prefix [alice] and an upper-only `a < 50`, the low bound
	// is the NULL boundary for the next column: [alice, null] RANGE_EXCLUSIVE,
	// which excludes rows where x=alice and a IS NULL. Mirrors Java
	// ScanComparisons (baseTuple + null element, exclusive).
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low RangeExclusive (null boundary after prefix), got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected high exclusive, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 2 || tr.Low[0] != "alice" || tr.Low[1] != nil {
		t.Fatalf("expected low=[alice null] (prefix + null boundary), got %v", tr.Low)
	}
	if len(tr.High) != 2 || tr.High[0] != "alice" || tr.High[1] != int64(50) {
		t.Fatalf("expected high=[alice 50], got %v", tr.High)
	}
}

func TestScanComparisons_EqualityPrefixThenBetween(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			eqRange("alice"),
			ineqRange(
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, int64(10)),
				predicates.NewLiteralComparison(predicates.ComparisonLessThanOrEq, int64(50)),
			),
		},
		[]values.Type{values.NullableString, values.NullableLong},
		"scan-comparisons-equality-prefix-between")
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected low inclusive, got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("expected high inclusive, got %d", tr.HighEndpoint)
	}
	if len(tr.Low) != 2 || tr.Low[0] != "alice" || tr.Low[1] != int64(10) {
		t.Fatalf("expected low=[alice 10], got %v", tr.Low)
	}
	if len(tr.High) != 2 || tr.High[0] != "alice" || tr.High[1] != int64(50) {
		t.Fatalf("expected high=[alice 50], got %v", tr.High)
	}
}

func TestScanComparisons_IsNotNullNoPrefix(t *testing.T) {
	t.Parallel()
	tr := bindOneRange(t,
		[]*predicates.ComparisonRange{
			ineqRange(predicates.Comparison{Type: predicates.ComparisonIsNotNull}),
		},
		[]values.Type{values.NullableLong},
		"scan-comparisons-is-not-null")
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low exclusive (IS NOT NULL sets low exclusive), got %d", tr.LowEndpoint)
	}
	if tr.HighEndpoint != recordlayer.EndpointTypeTreeEnd {
		t.Fatalf("expected high TreeEnd, got %d", tr.HighEndpoint)
	}
	// IS NOT NULL is the pure NULL boundary: low is one nil tuple element,
	// RANGE_EXCLUSIVE — the scan starts strictly after all null entries
	// (Java: lowItem null, RANGE_EXCLUSIVE). A nil low tuple would scan from
	// the index start and wrongly include nulls.
	if len(tr.Low) != 1 || tr.Low[0] != nil {
		t.Fatalf("expected low=[null] (null boundary), got %v", tr.Low)
	}
}

func TestScanComparisons_MultiEqualityThenInequality(t *testing.T) {
	t.Parallel()
	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{
			eqRange("alice"),
			eqRange(int64(1)),
			ineqRange(predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, float64(3.14))),
		},
		[]values.Type{values.NullableString, values.NullableLong, values.NullableDouble},
		nil, false, "scan-comparisons-multi-equality-then-inequality",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// `> 3.14` on a DOUBLE key coordinate is TWO physical ranges, not one: the
	// finite/+Inf/positive-NaN tail above the comparand, plus the negative-NaN
	// block, which is logically greatest (all NaNs are) but sorts BELOW -Inf in
	// FDB tuple order. Ascending, the finite tail comes first.
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 2 {
		t.Fatalf("opened %d ranges, want 2 (finite tail + negative-NaN block): %v", len(ranges), ranges)
	}
	tr := ranges[0]
	if len(tr.Low) != 3 || tr.Low[0] != "alice" || tr.Low[1] != int64(1) || tr.Low[2] != float64(3.14) {
		t.Fatalf("expected low=[alice 1 3.14], got %v", tr.Low)
	}
	if tr.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
		t.Fatalf("expected low exclusive, got %d", tr.LowEndpoint)
	}
	if len(tr.High) != 2 || tr.High[0] != "alice" || tr.High[1] != int64(1) {
		t.Fatalf("expected high=[alice 1] (prefix only), got %v", tr.High)
	}
	nan := ranges[1]
	if len(nan.Low) != 3 || nan.Low[0] != "alice" || nan.Low[1] != int64(1) ||
		!math.IsNaN(nan.Low[2].(float64)) || !math.Signbit(nan.Low[2].(float64)) {
		t.Fatalf("expected negative-NaN block low=[alice 1 -NaN], got %v", nan.Low)
	}
	if len(nan.High) != 3 || nan.High[0] != "alice" || nan.High[1] != int64(1) ||
		nan.High[2] != math.Inf(-1) {
		t.Fatalf("expected negative-NaN block high=[alice 1 -Inf], got %v", nan.High)
	}
}
