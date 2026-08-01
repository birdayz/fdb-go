package executor

// Unit coverage for the non-terminal signed-zero fork (zeroFork /
// expandZeroForks / multiRangeScanCursor): a zero-valued FLOAT/DOUBLE
// equality whose comparand is only known at execution time (correlated /
// parameterised) and does NOT terminate the scan prefix must split the
// probe into one range per signed zero — the two-key set
// {prefix+(-0.0)+rest, prefix+(+0.0)+rest} is not a contiguous interval,
// so neither a single probe (missing rows) nor a widened interval (wrong
// rows) can serve it. End-to-end SQL coverage lives in
// pkg/relational/sqldriver/correlated_zero_composite_sentinel_test.go.

import (
	"context"
	"math"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func mustRanges(t *testing.T, comps []*predicates.ComparisonRange) []recordlayer.TupleRange {
	t.Helper()
	ranges, err := scanComparisonsToTupleRanges(comps, nil)
	if err != nil {
		t.Fatalf("scanComparisonsToTupleRanges: %v", err)
	}
	return ranges
}

// signbitOf fails the test unless v is a float of the expected zero sign.
func requireZeroSign(t *testing.T, v any, negative bool, what string) {
	t.Helper()
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	default:
		t.Fatalf("%s: expected float zero, got %T (%v)", what, v, v)
	}
	if f != 0 {
		t.Fatalf("%s: expected zero, got %v", what, f)
	}
	if math.Signbit(f) != negative {
		t.Fatalf("%s: expected signbit=%v, got %v", what, negative, math.Signbit(f))
	}
}

// A non-terminal zero equality (trailing equality constrains the key) forks
// into TWO disjoint single-key ranges, negative zero first (ascending index
// key order: -0.0's key sorts immediately below +0.0's).
func TestScanComparisonsToTupleRanges_NonTerminalZeroForks(t *testing.T) {
	t.Parallel()
	for _, comparand := range []float64{0.0, math.Copysign(0, -1)} {
		ranges := mustRanges(t, []*predicates.ComparisonRange{
			eqRange(comparand),
			eqRange(int64(5)),
		})
		if len(ranges) != 2 {
			t.Fatalf("comparand %v: expected 2 forked ranges, got %d: %v", comparand, len(ranges), ranges)
		}
		for i, wantNeg := range []bool{true, false} {
			r := ranges[i]
			if len(r.Low) != 2 || len(r.High) != 2 {
				t.Fatalf("range %d: expected 2-element bounds, got low=%v high=%v", i, r.Low, r.High)
			}
			requireZeroSign(t, r.Low[0], wantNeg, "low[0]")
			requireZeroSign(t, r.High[0], wantNeg, "high[0]")
			if r.Low[1] != int64(5) || r.High[1] != int64(5) {
				t.Fatalf("range %d: trailing equality must survive in BOTH probes, got low=%v high=%v", i, r.Low, r.High)
			}
			if r.LowEndpoint != recordlayer.EndpointTypeRangeInclusive || r.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
				t.Fatalf("range %d: expected inclusive/inclusive, got %d/%d", i, r.LowEndpoint, r.HighEndpoint)
			}
		}
	}
}

// A TERMINAL zero (trailing column unconstrained — empty ComparisonRange)
// keeps the single widened [-0.0 .. +0.0] range: the two keys are adjacent,
// so one contiguous interval is exact and no fork is needed.
func TestScanComparisonsToTupleRanges_TerminalZeroStaysSingleWidened(t *testing.T) {
	t.Parallel()
	ranges := mustRanges(t, []*predicates.ComparisonRange{
		eqRange(float64(0)),
		predicates.EmptyComparisonRange(),
	})
	if len(ranges) != 1 {
		t.Fatalf("terminal zero must stay a single widened range, got %d ranges", len(ranges))
	}
	r := ranges[0]
	requireZeroSign(t, r.Low[0], true, "widened low")
	requireZeroSign(t, r.High[0], false, "widened high")
}

// A non-zero float equality with a trailing constraint stays one range.
func TestScanComparisonsToTupleRanges_NonZeroFloatSingle(t *testing.T) {
	t.Parallel()
	ranges := mustRanges(t, []*predicates.ComparisonRange{
		eqRange(float64(1.5)),
		eqRange(int64(5)),
	})
	if len(ranges) != 1 {
		t.Fatalf("non-zero comparand must not fork, got %d ranges", len(ranges))
	}
}

// Two non-terminal zeros fork independently: 4 ranges in ascending
// lexicographic key order (earlier position more significant, -0.0 < +0.0).
func TestScanComparisonsToTupleRanges_TwoZeroForksCartesian(t *testing.T) {
	t.Parallel()
	ranges := mustRanges(t, []*predicates.ComparisonRange{
		eqRange(float64(0)),
		eqRange(math.Copysign(0, -1)),
		eqRange(int64(7)),
	})
	if len(ranges) != 4 {
		t.Fatalf("expected 4 ranges (2 forks), got %d", len(ranges))
	}
	wantSigns := [][2]bool{{true, true}, {true, false}, {false, true}, {false, false}}
	for i, signs := range wantSigns {
		requireZeroSign(t, ranges[i].Low[0], signs[0], "low[0]")
		requireZeroSign(t, ranges[i].Low[1], signs[1], "low[1]")
		if ranges[i].Low[2] != int64(7) {
			t.Fatalf("range %d: trailing equality lost: %v", i, ranges[i].Low)
		}
	}
}

// A zero equality followed by an INEQUALITY also forks, and each fork
// carries the inequality bound — the fix is not equality-suffix-specific.
func TestScanComparisonsToTupleRanges_ZeroForkWithTrailingInequality(t *testing.T) {
	t.Parallel()
	ranges := mustRanges(t, []*predicates.ComparisonRange{
		eqRange(float64(0)),
		ineqRange(predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10))),
	})
	if len(ranges) != 2 {
		t.Fatalf("expected 2 forked ranges, got %d", len(ranges))
	}
	for i, wantNeg := range []bool{true, false} {
		r := ranges[i]
		requireZeroSign(t, r.Low[0], wantNeg, "low[0]")
		requireZeroSign(t, r.High[0], wantNeg, "high[0]")
		if len(r.Low) != 2 || r.Low[1] != int64(10) || r.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
			t.Fatalf("range %d: expected exclusive low [zero 10], got %v (%d)", i, r.Low, r.LowEndpoint)
		}
		if len(r.High) != 1 {
			t.Fatalf("range %d: expected prefix-only high, got %v", i, r.High)
		}
	}
}

// FLOAT columns fork with float32-typed variants — the tuple packer
// dispatches on the Go runtime type, so a float64 zero would pack under the
// wrong type code and probe the wrong keys.
func TestScanComparisonsToTupleRanges_Float32ForkKeepsType(t *testing.T) {
	t.Parallel()
	ranges := mustRanges(t, []*predicates.ComparisonRange{
		eqRange(float32(0)),
		eqRange(int64(5)),
	})
	if len(ranges) != 2 {
		t.Fatalf("expected 2 forked ranges, got %d", len(ranges))
	}
	for i := range ranges {
		if _, ok := ranges[i].Low[0].(float32); !ok {
			t.Fatalf("range %d: expected float32 zero variant, got %T", i, ranges[i].Low[0])
		}
	}
}

// The single-range projection must fail LOUDLY on forked comparisons —
// silently dropping a fork would reintroduce the missing-row bug.
func TestScanComparisonsToTupleRange_SingleErrorsOnFork(t *testing.T) {
	t.Parallel()
	_, err := scanComparisonsToTupleRange([]*predicates.ComparisonRange{
		eqRange(float64(0)),
		eqRange(int64(5)),
	}, nil)
	if err == nil {
		t.Fatal("expected loud error for forked comparisons on the single-range projection")
	}
	if !strings.Contains(err.Error(), "disjoint ranges") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// multiRangeScanCursor concatenates the per-range cursors in slice order
// forward and in REVERSED order for a reverse scan, and its continuations
// resume without loss or duplication at every stop point.
func TestMultiRangeScanCursor_OrderReverseAndResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Two disjoint "ranges", distinguished by their low bound; the factory
	// serves each range its own item list, so range-order mistakes and
	// factory/range mispairings both surface as wrong output.
	ranges := []recordlayer.TupleRange{
		recordlayer.TupleRangeAllOf(tuple.Tuple{"a"}),
		recordlayer.TupleRangeAllOf(tuple.Tuple{"b"}),
	}
	byKey := map[string][]string{"a": {"a1", "a2"}, "b": {"b1"}}
	scan := func(r recordlayer.TupleRange, cont []byte) recordlayer.RecordCursor[string] {
		return recordlayer.FromListWithContinuation(byKey[r.Low[0].(string)], cont)
	}

	drain := func(reverse bool) []string {
		var out []string
		cur := multiRangeScanCursor(ranges, reverse, nil, scan)
		defer cur.Close()
		for {
			res, err := cur.OnNext(ctx)
			if err != nil {
				t.Fatalf("OnNext: %v", err)
			}
			if !res.HasNext() {
				return out
			}
			out = append(out, res.GetValue())
		}
	}

	if got := drain(false); len(got) != 3 || got[0] != "a1" || got[1] != "a2" || got[2] != "b1" {
		t.Fatalf("forward order wrong: %v", got)
	}
	// Reverse consumes the HIGHEST range first (each real reverse scan yields
	// descending keys within its range; the list stands in for that stream).
	if got := drain(true); len(got) != 3 || got[0] != "b1" || got[1] != "a1" || got[2] != "a2" {
		t.Fatalf("reverse range order wrong: %v", got)
	}

	// Resume from EVERY stop point: stop after k rows, capture the
	// continuation, rebuild the cursor from it, and check the tail follows
	// with no loss and no replay.
	for k := 1; k <= 2; k++ {
		cur := multiRangeScanCursor(ranges, false, nil, scan)
		var contBytes []byte
		for i := 0; i < k; i++ {
			res, err := cur.OnNext(ctx)
			if err != nil || !res.HasNext() {
				t.Fatalf("priming row %d: err=%v hasNext=%v", i, err, res.HasNext())
			}
			contBytes, err = res.GetContinuation().ToBytes()
			if err != nil {
				t.Fatalf("continuation: %v", err)
			}
		}
		_ = cur.Close()

		resumed := multiRangeScanCursor(ranges, false, contBytes, scan)
		var tail []string
		for {
			res, err := resumed.OnNext(ctx)
			if err != nil {
				t.Fatalf("resumed OnNext: %v", err)
			}
			if !res.HasNext() {
				break
			}
			tail = append(tail, res.GetValue())
		}
		_ = resumed.Close()
		want := []string{"a1", "a2", "b1"}[k:]
		if len(tail) != len(want) {
			t.Fatalf("resume after %d rows: got %v, want %v", k, tail, want)
		}
		for i := range want {
			if tail[i] != want[i] {
				t.Fatalf("resume after %d rows: got %v, want %v", k, tail, want)
			}
		}
	}
}
