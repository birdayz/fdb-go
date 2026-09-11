package cascades

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestLimitMergeRule_Fires(t *testing.T) {
	t.Parallel()

	scan := smallRewriteScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	inner := smallRewriteLimit(100, 0, scanQ)
	innerRef := expressions.InitialOf(inner)
	innerQ := expressions.ForEachQuantifier(innerRef)

	outer := smallRewriteLimit(10, 0, innerQ)
	ref := expressions.InitialOf(outer)

	rule := NewLimitMergeRule()
	results := fireSmallRewriteRule(t, rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	merged, ok := results[0].(*expressions.LogicalLimitExpression)
	if !ok {
		t.Fatalf("expected LogicalLimitExpression, got %T", results[0])
	}
	if merged.GetLimit() != 10 {
		t.Fatalf("limit = %d, want 10", merged.GetLimit())
	}
	if merged.GetOffset() != 0 {
		t.Fatalf("offset = %d, want 0", merged.GetOffset())
	}
}

func TestLimitMergeRule_WithOffsets(t *testing.T) {
	t.Parallel()

	scan := smallRewriteScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Inner: skip 10, take 50 → rows 10..59 from source
	inner := smallRewriteLimit(50, 10, scanQ)
	innerRef := expressions.InitialOf(inner)
	innerQ := expressions.ForEachQuantifier(innerRef)

	// Outer: skip 5, take 20 from inner result → rows 15..34 from source
	// Combined offset = 10 + 5 = 15
	// Available from inner after outer's skip = 50 - 5 = 45
	// Combined limit = min(20, 45) = 20
	outer := smallRewriteLimit(20, 5, innerQ)
	ref := expressions.InitialOf(outer)

	rule := NewLimitMergeRule()
	results := fireSmallRewriteRule(t, rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	merged := results[0].(*expressions.LogicalLimitExpression)
	if merged.GetLimit() != 20 {
		t.Fatalf("limit = %d, want 20", merged.GetLimit())
	}
	if merged.GetOffset() != 15 {
		t.Fatalf("offset = %d, want 15", merged.GetOffset())
	}
}

func TestLimitMergeRule_OuterSkipsAll(t *testing.T) {
	t.Parallel()

	scan := smallRewriteScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Inner takes 5 rows
	inner := smallRewriteLimit(5, 0, scanQ)
	innerRef := expressions.InitialOf(inner)
	innerQ := expressions.ForEachQuantifier(innerRef)

	// Outer skips 10 (more than inner produces) → 0 rows
	outer := smallRewriteLimit(100, 10, innerQ)
	ref := expressions.InitialOf(outer)

	rule := NewLimitMergeRule()
	results := fireSmallRewriteRule(t, rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	merged := results[0].(*expressions.LogicalLimitExpression)
	if merged.GetLimit() != 0 {
		t.Fatalf("limit = %d, want 0 (outer skips all of inner)", merged.GetLimit())
	}
}

func TestLimitMergeRule_InnerUnlimited(t *testing.T) {
	t.Parallel()

	scan := smallRewriteScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Inner: no limit (pure offset)
	inner := smallRewriteLimit(-1, 20, scanQ)
	innerRef := expressions.InitialOf(inner)
	innerQ := expressions.ForEachQuantifier(innerRef)

	// Outer: take 10, skip 5
	outer := smallRewriteLimit(10, 5, innerQ)
	ref := expressions.InitialOf(outer)

	rule := NewLimitMergeRule()
	results := fireSmallRewriteRule(t, rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	merged := results[0].(*expressions.LogicalLimitExpression)
	if merged.GetLimit() != 10 {
		t.Fatalf("limit = %d, want 10", merged.GetLimit())
	}
	if merged.GetOffset() != 25 {
		t.Fatalf("offset = %d, want 25 (20+5)", merged.GetOffset())
	}
}

// TestLimitMergeRule_OffsetOverflow keeps nested skips representable: wrapping
// their sum negative would turn a query that skips every row into a prefix read.
func TestLimitMergeRule_OffsetOverflow(t *testing.T) {
	t.Parallel()
	for _, outerOffset := range []int64{1, math.MaxInt64} {
		t.Run(fmt.Sprint(outerOffset), func(t *testing.T) {
			t.Parallel()
			scanQ := expressions.ForEachQuantifier(expressions.InitialOf(smallRewriteScan("T")))
			inner := smallRewriteLimit(math.MaxInt64, math.MaxInt64, scanQ)
			innerQ := expressions.ForEachQuantifier(expressions.InitialOf(inner))
			outer := smallRewriteLimit(1, outerOffset, innerQ)
			results := fireSmallRewriteRule(t, NewLimitMergeRule(), expressions.InitialOf(outer))
			if len(results) != 0 {
				t.Fatalf("overflowing offsets must keep the nested limits, got %v", results)
			}
		})
	}
}

func TestLimitMergeRule_OffsetBoundary(t *testing.T) {
	t.Parallel()
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(smallRewriteScan("T")))
	inner := smallRewriteLimit(-1, math.MaxInt64-1, scanQ)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	outer := smallRewriteLimit(3, 1, innerQ)
	results := fireSmallRewriteRule(t, NewLimitMergeRule(), expressions.InitialOf(outer))
	if len(results) != 1 {
		t.Fatalf("representable offset sum must still merge, got %d rewrites", len(results))
	}
	merged := results[0].(*expressions.LogicalLimitExpression)
	if merged.GetOffset() != math.MaxInt64 || merged.GetLimit() != 3 {
		t.Fatalf("merged window = %d/%d, want 3/MaxInt64", merged.GetLimit(), merged.GetOffset())
	}
}

// FuzzLimitMerge_Window checks every yielded rewrite against sequential windows
// on an arbitrarily long ordinal stream. Big integers keep the oracle independent
// of the int64 arithmetic whose overflow caused the bug.
func FuzzLimitMerge_Window(f *testing.F) {
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), int64(1), int64(1))
	f.Add(int64(-1), int64(math.MaxInt64-1), int64(3), int64(1))
	f.Add(int64(50), int64(10), int64(20), int64(5))
	f.Add(int64(0), int64(0), int64(-1), int64(1))
	f.Fuzz(func(t *testing.T, iLimit, iOffset, oLimit, oOffset int64) {
		t.Parallel()
		iOffset &= math.MaxInt64
		oOffset &= math.MaxInt64
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(smallRewriteScan("T")))
		inner := smallRewriteLimit(iLimit, iOffset, scanQ)
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		outer := smallRewriteLimit(oLimit, oOffset, innerQ)
		results := fireSmallRewriteRule(t, NewLimitMergeRule(), expressions.InitialOf(outer))
		start := new(big.Int).Add(big.NewInt(iOffset), big.NewInt(oOffset))
		wantRewrites := 0
		if start.IsInt64() {
			wantRewrites = 1
		}
		if len(results) != wantRewrites {
			t.Fatalf("offset sum %s: got %d rewrites, want %d", start, len(results), wantRewrites)
		}
		var end *big.Int
		if iLimit >= 0 {
			end = new(big.Int).Add(big.NewInt(iOffset), big.NewInt(iLimit))
		}
		if oLimit >= 0 {
			outerEnd := new(big.Int).Add(start, big.NewInt(oLimit))
			if end == nil || outerEnd.Cmp(end) < 0 {
				end = outerEnd
			}
		}
		for _, result := range results {
			merged := result.(*expressions.LogicalLimitExpression)
			if end != nil && end.Cmp(start) <= 0 {
				if merged.GetLimit() != 0 {
					t.Fatalf("empty window became LIMIT %d", merged.GetLimit())
				}
				continue
			}
			if big.NewInt(merged.GetOffset()).Cmp(start) != 0 {
				t.Fatalf("merged offset = %d, want %s", merged.GetOffset(), start)
			}
			if end == nil {
				if merged.GetLimit() >= 0 {
					t.Fatalf("unbounded window became LIMIT %d", merged.GetLimit())
				}
			} else if want := new(big.Int).Sub(end, start); big.NewInt(merged.GetLimit()).Cmp(want) != 0 {
				t.Fatalf("merged limit = %d, want %s", merged.GetLimit(), want)
			}
		}
	})
}

func TestCheckedLimitSum(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		a, b, want int64
		ok         bool
	}{
		{name: "zero", ok: true},
		{name: "finite", a: 5, b: 2, want: 7, ok: true},
		{name: "maximum", a: math.MaxInt64 - 1, b: 1, want: math.MaxInt64, ok: true},
		{name: "overflow", a: math.MaxInt64, b: 1},
		{name: "negative_limit", a: -1, b: 2},
		{name: "negative_offset", a: 2, b: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := checkedLimitSum(tc.a, tc.b)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("sum(%d,%d) = %d/%v, want %d/%v", tc.a, tc.b, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestLogicalLimit_RejectsNegativeOffset(t *testing.T) {
	t.Parallel()
	for _, offset := range []int64{-1, -4, math.MinInt64} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			t.Parallel()
			q := expressions.ForEachQuantifier(expressions.InitialOf(smallRewriteScan("T")))
			var offsetErr *expressions.InvalidLimitOffsetError
			if _, err := expressions.NewLogicalLimitExpression(5, offset, q); !errors.As(err, &offsetErr) || offsetErr.Offset != offset {
				t.Fatalf("static LIMIT offset %d: got %v, want structured offset error", offset, err)
			}
			cap := &values.ConstantValue{Value: int64(5), Typ: values.NotNullLong}
			if _, err := expressions.NewRuntimeLogicalLimitExpression(cap, offset, q); !errors.As(err, &offsetErr) || offsetErr.Offset != offset {
				t.Fatalf("runtime LIMIT offset %d: got %v, want structured offset error", offset, err)
			}
		})
	}
}

func TestLimitMergeRule_DoesNotFireOnNonLimit(t *testing.T) {
	t.Parallel()

	scan := smallRewriteScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Limit over scan (not over another limit)
	lim := smallRewriteLimit(10, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewLimitMergeRule()
	results := fireSmallRewriteRule(t, rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire when inner is not a limit, got %d results", len(results))
	}
}
