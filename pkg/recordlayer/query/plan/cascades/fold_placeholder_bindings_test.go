package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// foldTestBinding wraps a literal comparison as a placeholderBinding whose
// query predicate is a fresh ComparisonPredicate carrying that comparison, so
// membership can be asserted by identity.
func foldTestBinding(typ predicates.ComparisonType, lit any) placeholderBinding {
	cmp := predicates.NewLiteralComparison(typ, lit)
	cp := predicates.NewComparisonPredicate(values.LiteralValue(int64(0)), cmp)
	return placeholderBinding{pred: cp, cp: cp, comparison: &cp.Comparison}
}

func foldTestMembers(members []placeholderBinding) []predicates.QueryPredicate {
	out := make([]predicates.QueryPredicate, len(members))
	for i, m := range members {
		out[i] = m.pred
	}
	return out
}

func foldTestSameMembers(got []placeholderBinding, want ...placeholderBinding) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].pred != want[i].pred {
			return false
		}
	}
	return true
}

// TestFoldPlaceholderBindings pins the fold's algebra arm by arm: it is
// Java's ComparisonRange.mergeAll over the comparisons bound to one
// placeholder — the equality wins, distinct inequalities accumulate, and the
// members returned are exactly the bindings the range carries. Each arm names
// the comparisons the range must hold and the bindings that must be members,
// so an arm that residualised the wrong binding, or dropped one, cannot pass.
func TestFoldPlaceholderBindings(t *testing.T) {
	t.Parallel()

	eq1 := foldTestBinding(predicates.ComparisonEquals, int64(1))
	eq1Again := foldTestBinding(predicates.ComparisonEquals, int64(1))
	eq2 := foldTestBinding(predicates.ComparisonEquals, int64(2))
	gt0 := foldTestBinding(predicates.ComparisonGreaterThan, int64(0))
	gt0Again := foldTestBinding(predicates.ComparisonGreaterThan, int64(0))
	lt9 := foldTestBinding(predicates.ComparisonLessThan, int64(9))
	ne5 := foldTestBinding(predicates.ComparisonNotEquals, int64(5))
	starts := foldTestBinding(predicates.ComparisonStartsWith, "x")

	for _, tc := range []struct {
		name        string
		bound       []placeholderBinding
		wantType    predicates.ComparisonRangeType
		wantRange   []predicates.ComparisonType // the comparisons the range carries, in order
		wantMembers []placeholderBinding
	}{
		{"nothing bound", nil, predicates.ComparisonRangeEmpty, nil, nil},
		{"one equality", []placeholderBinding{eq1}, predicates.ComparisonRangeEquality, []predicates.ComparisonType{predicates.ComparisonEquals}, []placeholderBinding{eq1}},
		{"two inequalities both carried", []placeholderBinding{gt0, lt9}, predicates.ComparisonRangeInequality, []predicates.ComparisonType{predicates.ComparisonGreaterThan, predicates.ComparisonLessThan}, []placeholderBinding{gt0, lt9}},
		{"duplicate inequality is a member, not appended", []placeholderBinding{gt0, gt0Again}, predicates.ComparisonRangeInequality, []predicates.ComparisonType{predicates.ComparisonGreaterThan}, []placeholderBinding{gt0, gt0Again}},
		{"equality then inequality: the inequality is a residual", []placeholderBinding{eq1, gt0}, predicates.ComparisonRangeEquality, []predicates.ComparisonType{predicates.ComparisonEquals}, []placeholderBinding{eq1}},
		{"inequality then equality: the equality wins, the inequality is a residual", []placeholderBinding{gt0, eq1}, predicates.ComparisonRangeEquality, []predicates.ComparisonType{predicates.ComparisonEquals}, []placeholderBinding{eq1}},
		{"two inequalities then an equality: both inequalities are residuals", []placeholderBinding{gt0, lt9, eq1}, predicates.ComparisonRangeEquality, []predicates.ComparisonType{predicates.ComparisonEquals}, []placeholderBinding{eq1}},
		{"first equality wins over a second, by list order", []placeholderBinding{eq2, eq1}, predicates.ComparisonRangeEquality, []predicates.ComparisonType{predicates.ComparisonEquals}, []placeholderBinding{eq2}},
		{"duplicate equality is a member", []placeholderBinding{eq1, eq1Again}, predicates.ComparisonRangeEquality, []predicates.ComparisonType{predicates.ComparisonEquals}, []placeholderBinding{eq1, eq1Again}},
		{"a none-type is never a member", []placeholderBinding{gt0, ne5}, predicates.ComparisonRangeInequality, []predicates.ComparisonType{predicates.ComparisonGreaterThan}, []placeholderBinding{gt0}},
		{"a none-type alone binds nothing", []placeholderBinding{ne5}, predicates.ComparisonRangeEmpty, nil, nil},
		// The fold never consults the candidate: STARTS_WITH beside an
		// inequality is folded like any two inequalities, and it is the
		// candidate's prefix map that declines the range as a whole
		// (TestFoldPlaceholderBindings_CandidateDecidesTheWholeRange).
		{"starts_with beside an inequality folds unconditionally", []placeholderBinding{starts, gt0}, predicates.ComparisonRangeInequality, []predicates.ComparisonType{predicates.ComparisonStartsWith, predicates.ComparisonGreaterThan}, []placeholderBinding{starts, gt0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			merged, members := foldPlaceholderBindings(tc.bound)
			if merged == nil {
				t.Fatal("fold returned a nil range")
			}
			if got := merged.GetRangeType(); got != tc.wantType {
				t.Fatalf("range type = %v, want %v", got, tc.wantType)
			}
			got := merged.GetComparisons()
			if len(got) != len(tc.wantRange) {
				t.Fatalf("range carries %d comparisons %v, want %d %v", len(got), got, len(tc.wantRange), tc.wantRange)
			}
			for i := range got {
				if got[i].Type != tc.wantRange[i] {
					t.Fatalf("range[%d] = %v, want %v", i, got[i].Type, tc.wantRange[i])
				}
			}
			if !foldTestSameMembers(members, tc.wantMembers...) {
				t.Fatalf("members = %v, want %v", foldTestMembers(members), foldTestMembers(tc.wantMembers))
			}
			// Every member's comparison is one the range carries, and every
			// non-member's is not — the residual set is exactly the complement.
			carried := map[*predicates.Comparison]bool{}
			for _, c := range got {
				carried[c] = true
			}
			for _, b := range tc.bound {
				isMember := false
				for _, m := range members {
					if m.pred == b.pred {
						isMember = true
					}
				}
				if isMember && !carried[b.comparison] {
					// A duplicate is a member whose own comparison object is not
					// in the range (the first copy is); it must equal one that is.
					found := false
					for _, c := range got {
						if comparisonsEqual(c, b.comparison) {
							found = true
						}
					}
					if !found {
						t.Fatalf("member %v carries no comparison of the range", b.comparison.Type)
					}
				}
				if !isMember && carried[b.comparison] {
					t.Fatalf("non-member %v is carried by the range", b.comparison.Type)
				}
			}
		})
	}
}

// TestFoldPlaceholderBindings_CandidateDecidesTheWholeRange pins where the
// executable-range decision lives after the fold: the value-index and
// primary-scan prefix maps decline a folded STARTS_WITH-plus-inequality
// range as a whole (Go's bindRangeTail executes a lone STARTS_WITH; Java's
// InequalityRangeCombiner throws on the same range at execution), and the
// self-limiting vector candidate declines the CANDIDATE on the same range
// over a partition column, by its raw-bindings eligibility. Neither shape is
// reachable from SQL — ComparisonStartsWith has no producer under
// pkg/relational — so this is the pin a future LIKE→STARTS_WITH conversion
// would have to re-decide deliberately.
func TestFoldPlaceholderBindings_CandidateDecidesTheWholeRange(t *testing.T) {
	t.Parallel()

	starts := foldTestBinding(predicates.ComparisonStartsWith, "x")
	gt := foldTestBinding(predicates.ComparisonGreaterThan, "a")
	folded, members := foldPlaceholderBindings([]placeholderBinding{starts, gt})
	if len(members) != 2 || !folded.IsInequality() || len(folded.GetInequalityComparisons()) != 2 {
		t.Fatalf("precondition: the fold must carry both, got %v / %d members", folded, len(members))
	}
	lone, _ := foldPlaceholderBindings([]placeholderBinding{starts})

	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	knownDistinct := false
	valueCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"idx_s", []string{"T"}, []string{"S"}, nil, aliases,
		physicalKeyRowType(), false, nil, &knownDistinct,
	).WithKeyComponentTypes([]values.Type{values.NullableString})
	if prefix := valueCandidate.ComputeBoundParameterPrefixMap(map[values.CorrelationIdentifier]*predicates.ComparisonRange{aliases[0]: folded}); len(prefix) != 0 {
		t.Fatalf("value index accepted a STARTS_WITH beside an inequality: prefix %v", prefix)
	}
	if prefix := valueCandidate.ComputeBoundParameterPrefixMap(map[values.CorrelationIdentifier]*predicates.ComparisonRange{aliases[0]: lone}); len(prefix) != 1 {
		t.Fatalf("control: a lone STARTS_WITH on a STRING key must bind, prefix %v", prefix)
	}

	primaryCandidate := NewPrimaryScanMatchCandidate(
		nil, aliases, []string{"T"}, []string{"T"}, []string{"S"}, true, physicalKeyRowType(),
	).WithKeyComponentTypes([]values.Type{values.NullableString})
	if prefix := primaryCandidate.ComputeBoundParameterPrefixMap(map[values.CorrelationIdentifier]*predicates.ComparisonRange{aliases[0]: folded}); len(prefix) != 0 {
		t.Fatalf("primary scan accepted a STARTS_WITH beside an inequality: prefix %v", prefix)
	}

	vector := NewVectorIndexScanMatchCandidate(
		"vector_s", []string{"T"}, []string{"S", "EMBEDDING"}, 1,
		values.DistanceEuclidean, physicalKeyRowType(), false, nil,
	).WithPartitionKeyComponentTypes([]values.Type{values.NullableString})
	partitionAlias := vector.GetSargableAliases()[0]
	if candidateBindingRangesEligible(vector, map[values.CorrelationIdentifier]*predicates.ComparisonRange{partitionAlias: folded}) {
		t.Fatal("vector candidate stayed eligible with a STARTS_WITH beside an inequality on its partition column")
	}
	if !candidateBindingRangesEligible(vector, map[values.CorrelationIdentifier]*predicates.ComparisonRange{partitionAlias: lone}) {
		t.Fatal("control: a lone STARTS_WITH on a STRING partition column keeps the vector candidate eligible")
	}
}

// TestPredicateMultiMapBuilder_AdmitsAFoldGroupOnly pins the one shape
// checkConflicts admits beyond Java's identity rule: several query
// predicates mapping ONE placeholder are a fold group iff every mapping
// carries the same parameter alias and the same merged range object.
// Different ranges, a different alias, or a non-sargable mapping of the same
// candidate remain conflicts.
func TestPredicateMultiMapBuilder_AdmitsAFoldGroupOnly(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("p")
	other := values.NamedCorrelationIdentifier("q")
	placeholder := predicates.NewPlaceholder(alias, values.LiteralValue(int64(0)))
	gt := foldTestBinding(predicates.ComparisonGreaterThan, int64(0))
	lt := foldTestBinding(predicates.ComparisonLessThan, int64(9))
	merged, _ := foldPlaceholderBindings([]placeholderBinding{gt, lt})
	otherRange, _ := foldPlaceholderBindings([]placeholderBinding{gt})

	sargable := func(b placeholderBinding, a values.CorrelationIdentifier, cr *predicates.ComparisonRange) *PredicateMapping {
		return RegularMappingBuilder(b.pred, b.pred, placeholder).SetSargable(a, cr).Build()
	}
	residual := func(b placeholderBinding) *PredicateMapping {
		return RegularMappingBuilder(b.pred, b.pred, placeholder).Build()
	}

	for _, tc := range []struct {
		name   string
		first  *PredicateMapping
		second *PredicateMapping
		want   bool
	}{
		{"fold group: same alias, same range object", sargable(gt, alias, merged), sargable(lt, alias, merged), true},
		{"different range objects conflict", sargable(gt, alias, merged), sargable(lt, alias, otherRange), false},
		{"different aliases conflict", sargable(gt, alias, merged), sargable(lt, other, merged), false},
		{"a non-sargable mapping of the same candidate conflicts", sargable(gt, alias, merged), residual(lt), false},
		{"two non-sargable mappings of the same candidate conflict", residual(gt), residual(lt), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			builder := NewPredicateMultiMapBuilder()
			builder.Put(tc.first.GetOriginalQueryPredicate(), tc.first)
			builder.Put(tc.second.GetOriginalQueryPredicate(), tc.second)
			if got := builder.BuildMaybe() != nil; got != tc.want {
				t.Fatalf("BuildMaybe admitted = %v, want %v", got, tc.want)
			}
		})
	}
}
