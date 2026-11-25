package executor

import (
	"bytes"
	"context"
	"sort"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type unsupportedTestPlan struct{ plans.PlanExprBase }

func (p *unsupportedTestPlan) GetResultType() values.Type { return values.UnknownType }

func (p *unsupportedTestPlan) GetChildren() []plans.RecordQueryPlan { return nil }

func (p *unsupportedTestPlan) EqualsPlanWithoutChildren(plans.RecordQueryPlan) bool {
	return false
}

func (p *unsupportedTestPlan) HashCodeWithoutChildren() uint64 { return 0 }

func (p *unsupportedTestPlan) Explain() string { return "unsupported" }

func (p *unsupportedTestPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*unsupportedTestPlan)
	return ok && p.EqualsPlanWithoutChildren(o)
}

func (p *unsupportedTestPlan) WithQuantifiers(_ []expressions.Quantifier) (expressions.RelationalExpression, error) {
	return p, nil
}

// assertIsAllRange pins that a materialized range denotes the WHOLE subspace.
// The live binder spells that as an inclusive/inclusive range over the EMPTY
// tuple prefix rather than TREE_START/TREE_END, so the claim is checked where
// it is observable — the FDB key range — instead of on the endpoint spelling.
func assertIsAllRange(t *testing.T, got recordlayer.TupleRange) {
	t.Helper()
	ss := subspace.FromBytes([]byte{0x42})
	gotRange := got.ToFDBRange(ss)
	wantRange := recordlayer.TupleRangeAll.ToFDBRange(ss)
	if !bytes.Equal(gotRange.Begin.FDBKey(), wantRange.Begin.FDBKey()) ||
		!bytes.Equal(gotRange.End.FDBKey(), wantRange.End.FDBKey()) {
		t.Fatalf("range %v..%v (endpoints %d/%d), want the ALL range %v..%v",
			gotRange.Begin.FDBKey(), gotRange.End.FDBKey(),
			got.LowEndpoint, got.HighEndpoint,
			wantRange.Begin.FDBKey(), wantRange.End.FDBKey())
	}
}

// --- bindScanComparisonsToRangeSet unit tests ---

func eqRange(val any) *predicates.ComparisonRange {
	r := predicates.EmptyComparisonRange()
	c := predicates.NewLiteralComparison(predicates.ComparisonEquals, val)
	res := r.Merge(&c)
	if !res.Complete() {
		panic("merge failed for equality")
	}
	return res.Range
}

func ineqRange(comps ...predicates.Comparison) *predicates.ComparisonRange {
	r := predicates.EmptyComparisonRange()
	for i := range comps {
		res := r.Merge(&comps[i])
		if !res.Complete() {
			panic("merge failed for inequality")
		}
		r = res.Range
	}
	return r
}

// bindOneRange binds the comparisons and asserts the resulting range set opens
// exactly one physical range, returning it. Most of these cases describe a
// single contiguous key interval; a case that genuinely opens more than one
// asserts the leaf count itself instead of using this helper.
func bindOneRange(
	t *testing.T,
	comparisons []*predicates.ComparisonRange,
	keyTypes []values.Type,
	salt string,
) recordlayer.TupleRange {
	t.Helper()
	spec, err := bindScanComparisonsToRangeSet(comparisons, keyTypes, nil, false, salt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 1 {
		t.Fatalf("opened %d ranges, want 1", len(ranges))
	}
	return ranges[0]
}

// --- distinctKey unit tests ---

func mustQueryResultKey(t *testing.T, qr QueryResult) string {
	t.Helper()
	k, err := queryResultKey(qr)
	if err != nil {
		t.Fatalf("queryResultKey: %v", err)
	}
	return k
}

// mustDistinctKey unwraps distinctKey's error channel for the legacy pins
// (their fixtures never carry unencodable slots).
func mustDistinctKey(t *testing.T, qr QueryResult) string {
	t.Helper()
	k, err := distinctKey(qr)
	if err != nil {
		t.Fatalf("distinctKey: %v", err)
	}
	return k
}

// ---------------------------------------------------------------------------
// mergeSortCursor tests
// ---------------------------------------------------------------------------

// qr is a shorthand to build a QueryResult with a positional row.
func qr(kvs ...any) QueryResult {
	m := make(map[string]any, len(kvs)/2)
	for i := 0; i < len(kvs)-1; i += 2 {
		m[kvs[i].(string)] = kvs[i+1]
	}
	return dmap(m)
}

// collectMergeSortCursor drains a mergeSortCursor and returns all results.
func collectMergeSortCursor(t *testing.T, c *mergeSortCursor) []QueryResult {
	t.Helper()
	ctx := context.Background()
	var out []QueryResult
	for {
		r, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext error: %v", err)
		}
		if !r.HasNext() {
			break
		}
		out = append(out, r.GetValue())
	}
	return out
}

// fieldVal returns the int64 at key k from a QueryResult datum.
func fieldVal(t *testing.T, r QueryResult, k string) int64 {
	t.Helper()
	m, ok := rowMapOK(r)
	if !ok {
		t.Fatalf("datum type %T, want map[string]any", r.Positional)
	}
	v, ok := m[k]
	if !ok {
		t.Fatalf("key %q missing from datum %v", k, m)
	}
	n, ok := v.(int64)
	if !ok {
		t.Fatalf("key %q = %T, want int64", k, v)
	}
	return n
}

func newMergeSortCursor(
	cursors []recordlayer.RecordCursor[QueryResult],
	compKeys []values.Value,
	reverse bool,
	dedup bool,
) *mergeSortCursor {
	states := make([]*mergeSortChildState, len(cursors))
	for i, c := range cursors {
		states[i] = &mergeSortChildState{cursor: c, cont: &recordlayer.StartContinuation{}}
	}
	return &mergeSortCursor{states: states, compKeys: compKeys, reverse: reverse, dedup: dedup}
}

// ---------------------------------------------------------------------------
// collectCursor drains any RecordCursor[QueryResult] and returns all results.
// ---------------------------------------------------------------------------

func collectCursor(t *testing.T, c recordlayer.RecordCursor[QueryResult]) []QueryResult {
	t.Helper()
	ctx := context.Background()
	var out []QueryResult
	for {
		r, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext error: %v", err)
		}
		if !r.HasNext() {
			break
		}
		out = append(out, r.GetValue())
	}
	return out
}

// ===========================================================================
// concatCursor
// ===========================================================================

// newUnorderedUnionForTest builds the cursor over pre-built children — the
// executor recipe minus plan execution (pinned separately via the SQL e2e).
func newUnorderedUnionForTest(children ...recordlayer.RecordCursor[QueryResult]) *unorderedUnionCursor {
	return &unorderedUnionCursor{
		children:  children,
		states:    make([]recordlayer.RecordCursorContinuation, len(children)),
		reasons:   make([]recordlayer.NoNextReason, len(children)),
		stopped:   make([]bool, len(children)),
		exhausted: make([]bool, len(children)),
	}
}

// ===========================================================================
// ORDER BY via the live newCustomSortCursor (executeInMemorySort's cursor)
// ===========================================================================

// expressionSortFn is a test fixture: the ordinal-positional sort comparator
// used to drive the live newCustomSortCursor (buffer + yield + error
// propagation) in isolation. Sort keys are VALUES evaluated against the
// authoritative ordinal positional row (baked at plan time; loud on a miss) —
// never a name lookup. Ties break by primary key, directed by the last
// explicit key. The production sort path (executeInMemorySort) carries its own
// equivalent comparator; that end-to-end path is covered by the sqldriver FDB
// sort suites.
func expressionSortFn(keys []expressions.SortKey) func([]QueryResult) error {
	return func(results []QueryResult) error {
		pkDesc := false
		if len(keys) > 0 {
			pkDesc = keys[len(keys)-1].Reverse
		}
		var sortErr error
		sort.SliceStable(results, func(i, j int) bool {
			if sortErr != nil {
				return false
			}
			for _, k := range keys {
				vi, err := k.Value.Evaluate(results[i].Positional)
				if err != nil {
					sortErr = err
					return false
				}
				vj, err := k.Value.Evaluate(results[j].Positional)
				if err != nil {
					sortErr = err
					return false
				}
				iNil, jNil := vi == nil, vj == nil
				if iNil && jNil {
					continue
				}
				if iNil || jNil {
					nf := !k.Reverse
					if k.NullsFirst != nil {
						nf = *k.NullsFirst
					}
					if nf {
						return iNil
					}
					return jNil
				}
				// Mirror the production sort comparator (executeInMemorySort uses
				// compareValues, the S2 total-order authority) rather than a separate
				// scalar comparator, so this fixture exercises the real ordering.
				cmp, cmpErr := compareValues(vi, vj)
				if cmpErr != nil {
					if sortErr == nil {
						sortErr = cmpErr
					}
					return false
				}
				if cmp == 0 {
					continue
				}
				if k.Reverse {
					return cmp > 0
				}
				return cmp < 0
			}
			if results[i].PrimaryKey != nil && results[j].PrimaryKey != nil {
				cmp := comparePKTuples(results[i].PrimaryKey, results[j].PrimaryKey)
				if cmp != 0 {
					if pkDesc {
						return cmp > 0
					}
					return cmp < 0
				}
			}
			return false
		})
		return sortErr
	}
}

// mustCompareValues asserts the pair has a defined ordering — every fixture
// in these tests stays inside the typed comparison domain.
func mustCompareValues(t *testing.T, a, b any) int {
	t.Helper()
	cmp, err := compareValues(a, b)
	if err != nil {
		t.Fatalf("compareValues(%#v, %#v): %v", a, b, err)
	}
	return cmp
}
