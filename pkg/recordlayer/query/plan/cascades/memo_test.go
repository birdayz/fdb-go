package cascades

import (
	"fmt"
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func memoTestScan(t testing.TB, name string) *expressions.FullUnorderedScanExpression {
	t.Helper()
	return mustFullUnorderedScan(t, []string{name}, values.NotNullLong)
}

func memoTestFilter(
	t testing.TB,
	queryPredicates []predicates.QueryPredicate,
	inner expressions.Quantifier,
) *expressions.LogicalFilterExpression {
	t.Helper()
	filter, err := expressions.NewLogicalFilterExpression(queryPredicates, inner)
	return mustConstruct(t, filter, err)
}

type memoHashObservedPredicate struct {
	predicates.QueryPredicate
	hashVisits *int
}

func (p *memoHashObservedPredicate) Explain() string {
	*p.hashVisits++
	return p.QueryPredicate.Explain()
}

type memoCorrelationObservedPredicate struct {
	predicates.QueryPredicate
	visits *int
}

func (p *memoCorrelationObservedPredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	*p.visits++
	return p.QueryPredicate.GetCorrelatedTo()
}

func TestMemoHashMismatchDoesNotReadGroupCorrelations(t *testing.T) {
	t.Parallel()
	for _, leaf := range []bool{false, true} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("leaf=%t/batch=%t", leaf, batch), func(t *testing.T) {
				t.Parallel()
				var quantifiers []expressions.Quantifier
				if !leaf {
					quantifiers = []expressions.Quantifier{expressions.ForEachQuantifier(expressions.InitialOf(memoTestScan(t, "T")))}
				}
				visits := 0
				observed := &memoCorrelationObservedPredicate{
					QueryPredicate: predicates.NewConstantPredicate(predicates.TriTrue), visits: &visits,
				}
				selectWith := func(predicate predicates.QueryPredicate) *expressions.SelectExpression {
					expr, err := expressions.NewSelectExpression(values.NewBooleanValue(true), quantifiers, []predicates.QueryPredicate{predicate})
					return mustConstruct(t, expr, err)
				}
				stored := selectWith(observed)
				probe := selectWith(predicates.NewConstantPredicate(predicates.TriFalse))
				if stored.HashCodeWithoutChildren() == probe.HashCodeWithoutChildren() {
					t.Fatal("fixture must reject the stored expression by hash")
				}
				group := expressions.InitialOf(stored)
				memo := NewMemo(group)
				observed.GetCorrelatedTo()
				if visits == 0 {
					t.Fatal("correlation observation did not run")
				}
				visits = 0
				memoize := func() *expressions.Reference {
					if batch {
						return memo.MemoizeExpressions([]expressions.RelationalExpression{probe, probe})
					}
					return memo.MemoizeExpression(probe)
				}
				fresh := memoize()
				if fresh == group || memoize() != fresh {
					t.Fatal("the distinct proposal must reuse only its own group")
				}
				if visits != 0 {
					t.Fatalf("hash-mismatched group correlations were read %d times", visits)
				}
			})
		}
	}
}

func TestMemo_MemoizeExpressionReusesAdmittedHash(t *testing.T) {
	t.Parallel()
	m := NewMemo(nil)
	scan := m.MemoizeExpression(memoTestScan(t, "T"))
	visits := 0
	observed := &memoHashObservedPredicate{
		QueryPredicate: predicates.NewConstantPredicate(predicates.TriTrue),
		hashVisits:     &visits,
	}
	m.MemoizeExpression(memoTestFilter(t, []predicates.QueryPredicate{observed}, expressions.ForEachQuantifier(scan)))
	probe := memoTestFilter(t, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)}, expressions.ForEachQuantifier(scan))
	want := m.MemoizeExpression(probe)
	if visits == 0 {
		t.Fatal("hash observation did not run during memo admission")
	}
	visits = 0
	for range 10 {
		if got := m.MemoizeExpression(probe); got != want {
			t.Fatal("memo lookup stopped reusing the existing group")
		}
	}
	if visits != 0 {
		t.Fatalf("memo lookup rehashed an immutable admitted member %d times", visits)
	}
}

func TestMemo_MemoizeExpressionReusesAdmittedHashOnCollision(t *testing.T) {
	t.Parallel()
	m := NewMemo(nil)
	scan := m.MemoizeExpression(memoTestScan(t, "T"))
	storedVisits, probeVisits := 0, 0
	observedFilter := func(visits *int) *expressions.LogicalFilterExpression {
		return memoTestFilter(t, []predicates.QueryPredicate{&memoHashObservedPredicate{
			QueryPredicate: predicates.NewConstantPredicate(predicates.TriTrue), hashVisits: visits,
		}}, expressions.ForEachQuantifier(scan))
	}
	stored, probe := observedFilter(&storedVisits), observedFilter(&probeVisits)
	if stored.HashCodeWithoutChildren() != probe.HashCodeWithoutChildren() {
		t.Fatal("fixture must drive equality after the cached hash prefilter")
	}
	m.MemoizeExpression(stored)
	want := m.MemoizeExpression(probe)
	if storedVisits == 0 || probeVisits == 0 {
		t.Fatal("hash observations never ran")
	}
	storedVisits = 0
	for range 10 {
		if got := m.MemoizeExpression(probe); got != want {
			t.Fatal("same-hash lookup stopped reusing the existing group")
		}
	}
	if storedVisits != 0 {
		t.Fatalf("same-hash equality rehashed the stored member %d times", storedVisits)
	}
}

func TestMemoAdmissionPreservesAdditionalFinalAlternatives(t *testing.T) {
	t.Parallel()
	forward, err := plans.NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	forward = mustConstruct(t, forward, err)
	reverse, err := plans.NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, true)
	reverse = mustConstruct(t, reverse, err)
	one := expressions.FinalOfAtStage(forward, expressions.StagePlanned)
	both := expressions.FinalOfAtStage(forward, expressions.StagePlanned)
	if !both.InsertFinal(reverse) || len(both.FinalMembers()) != 2 {
		t.Fatal("fixture failed to retain both scan directions")
	}
	wrap := func(ref *expressions.Reference) expressions.RelationalExpression {
		unique, err := expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(ref))
		return mustConstruct(t, unique, err)
	}
	if duplicate, _ := expressions.PreparedMemberDuplicate([]expressions.RelationalExpression{wrap(one)}, wrap(both)); duplicate {
		t.Fatal("admission drops the wrapper that exposes the additional reverse-scan alternative")
	}
}

func TestMemoReuseRejectsExtraGroupCorrelations(t *testing.T) {
	t.Parallel()
	for _, leaf := range []bool{false, true} {
		for _, batch := range []bool{false, true} {
			for _, final := range []bool{false, true} {
				t.Run(fmt.Sprintf("leaf=%t/batch=%t/final=%t", leaf, batch, final), func(t *testing.T) {
					t.Parallel()
					var quantifiers []expressions.Quantifier
					if !leaf {
						quantifiers = []expressions.Quantifier{expressions.ForEachQuantifier(expressions.InitialOf(memoTestScan(t, "T")))}
					}
					constant := &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}
					plain, err := expressions.NewSelectExpression(constant, quantifiers, nil)
					plain = mustConstruct(t, plain, err)
					outer := values.NamedCorrelationIdentifier("outer")
					outerValue, err := values.NewQuantifiedObjectValue(outer, values.NotNullLong)
					outerValue = mustConstruct(t, outerValue, err)
					correlated, err := expressions.NewSelectExpression(constant, quantifiers, []predicates.QueryPredicate{
						predicates.NewComparisonPredicate(outerValue, predicates.Comparison{Type: predicates.ComparisonIsNotNull}),
					})
					correlated = mustConstruct(t, correlated, err)
					group := expressions.InitialOf(plain)
					insert := group.Insert
					if final {
						insert = group.InsertFinal
					}
					if !insert(correlated) {
						t.Fatal("fixture failed to add the correlated alternative")
					}
					if _, ok := group.GetCorrelatedTo()[outer]; !ok {
						t.Fatal("fixture has no extra group correlation")
					}
					m := NewMemo(group)
					var got *expressions.Reference
					if batch {
						got = m.MemoizeExpressions([]expressions.RelationalExpression{plain, plain})
					} else {
						got = m.MemoizeExpression(plain)
					}
					if got == group || len(got.GetCorrelatedTo()) != 0 {
						t.Fatal("uncorrelated memoization reused a group requiring an outer binding")
					}
				})
			}
		}
	}
}

func TestMemoLeafLookupRejectsNonLeafMember(t *testing.T) {
	t.Parallel()
	for _, planning := range []bool{false, true} {
		for _, leafFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("planning=%t/leafFirst=%t", planning, leafFirst), func(t *testing.T) {
				t.Parallel()
				stage := expressions.StageCanonical
				if planning {
					stage = expressions.StagePlanned
				}
				explode := func(n int64) *expressions.ExplodeExpression {
					v := &values.ConstantValue{Value: n, Typ: values.NotNullLong}
					e, err := expressions.NewExplodeExpression(values.NewArrayConstructorValue(values.NotNullLong, []values.Value{v, v}))
					return mustConstruct(t, e, err)
				}
				one := &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}
				child := expressions.ExploratoryOfAtStage(explode(0), stage)
				project, err := expressions.NewSelectExpression(one, []expressions.Quantifier{expressions.ForEachQuantifier(child)}, nil)
				project = mustConstruct(t, project, err)
				alternative := explode(1)
				first, second := expressions.RelationalExpression(project), expressions.RelationalExpression(alternative)
				if leafFirst {
					first, second = second, first
				}
				group := expressions.ExploratoryOfAtStage(first, stage)
				if !group.Insert(second) {
					t.Fatal("fixture did not retain the two-row alternatives")
				}
				memo := NewMemo(group)
				if planning {
					memo.MarkPlanningActive()
				}
				if err := memo.AdmissionErr(); err != nil {
					t.Fatal(err)
				}
				leaf, err := expressions.NewSelectExpression(one, nil, nil)
				leaf = mustConstruct(t, leaf, err)
				if project.HashCodeWithoutChildren() != leaf.HashCodeWithoutChildren() || !project.EqualsWithoutChildren(leaf, expressions.EmptyAliasMap()) {
					t.Fatal("fixture must collide in child-independent identity")
				}
				if expressions.MemoEqual(project, leaf) || memo.refContains(group, leaf) {
					t.Fatal("full containment must reject different quantifier counts")
				}
				if memo.MemoizeExpression(explode(1)) != group {
					t.Fatal("genuine leaf alternative must reuse the mixed group")
				}
				fresh := memo.MemoizeExpression(leaf)
				if fresh == group {
					t.Fatal("leaf shortcut reused a two-row group for a one-row Select")
				}
				if fresh.Stage() != stage || memo.MemoizeExpression(leaf) != fresh {
					t.Fatal("one-row Select must reuse its own same-stage group")
				}
			})
		}
	}
}

func TestMemoReuseRequiresTargetStage(t *testing.T) {
	t.Parallel()
	for _, planning := range []bool{false, true} {
		for _, leaf := range []bool{false, true} {
			for _, batch := range []bool{false, true} {
				t.Run(fmt.Sprintf("planning=%t/leaf=%t/batch=%t", planning, leaf, batch), func(t *testing.T) {
					t.Parallel()
					first := expressions.RelationalExpression(memoTestScan(t, "T"))
					second := expressions.RelationalExpression(memoTestScan(t, "U"))
					if !leaf {
						q := expressions.ForEachQuantifier(expressions.InitialOf(first))
						first = memoTestFilter(t, nil, q)
						second = memoTestFilter(t, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}, q)
					}
					group := expressions.InitialOf(first)
					if batch && !group.Insert(second) {
						t.Fatal("second fixture member not inserted")
					}
					wantStage := expressions.StageCanonical
					if planning {
						wantStage = expressions.StagePlanned
					} else {
						group.AdvanceStagePreservingMembers(expressions.StagePlanned)
					}
					memo := NewMemo(group)
					if planning {
						memo.MarkPlanningActive()
					}
					lookup := func() *expressions.Reference {
						if batch {
							return memo.MemoizeExpressions([]expressions.RelationalExpression{first, second})
						}
						return memo.MemoizeExpression(first)
					}
					fresh := lookup()
					if fresh == group {
						t.Error("reused a group from the wrong planner stage")
					}
					if fresh.Stage() != wantStage {
						t.Errorf("fresh stage=%v, want %v", fresh.Stage(), wantStage)
					}
					if lookup() != fresh {
						t.Error("failed to reuse a matching-stage group")
					}
				})
			}
		}
	}
}

func BenchmarkMemoCandidateParentsFirst(b *testing.B) {
	child := expressions.InitialOf(memoTestScan(b, "T"))
	memo := NewMemo(nil)
	var first *expressions.Reference
	for range 512 {
		member, err := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(child))
		if err != nil {
			b.Fatal(err)
		}
		parent := expressions.InitialOf(member)
		memo.RegisterReference(parent)
		if first == nil {
			first = parent
		}
	}
	qs := []expressions.Quantifier{expressions.ForEachQuantifier(child)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		found := false
		for parent := range memo.findCandidateParents(qs, nil) {
			if parent != first {
				b.Fatal("candidate order changed")
			}
			found = true
			break
		}
		if !found {
			b.Fatal("no candidate yielded")
		}
	}
}

func BenchmarkMemoizeNonLeafSharedChild(b *testing.B) {
	memo := NewMemo(nil)
	child := memo.MemoizeExpression(memoTestScan(b, "T"))
	makeMember := func(k int64) expressions.RelationalExpression {
		q := expressions.ForEachQuantifier(child)
		value, err := q.RequireFlowedObjectValue()
		if err != nil {
			b.Fatal(err)
		}
		return memoTestFilter(b, []predicates.QueryPredicate{predicates.NewComparisonPredicate(value,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, k))}, q)
	}
	var want *expressions.Reference
	for k := range int64(512) {
		want = expressions.ExploratoryOfAtStage(makeMember(k), expressions.StageCanonical)
		memo.RegisterReference(want)
	}
	incoming := makeMember(511)
	if got := memo.MemoizeExpression(incoming); got != want {
		b.Fatal("lookup did not reuse the final matching parent")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := memo.MemoizeExpression(incoming); got != want {
			b.Fatal("lookup did not reuse the final matching parent")
		}
	}
}

func TestMemoCandidatesIntersectExpressions(t *testing.T) {
	t.Parallel()
	a, b := expressions.InitialOf(memoTestScan(t, "A")), expressions.InitialOf(memoTestScan(t, "B"))
	group := expressions.InitialOf(memoTestFilter(t, nil, expressions.ForEachQuantifier(a)))
	if !group.Insert(memoTestFilter(t, nil, expressions.ForEachQuantifier(b))) {
		t.Fatal("fixture failed to retain both alternatives")
	}
	memo := NewMemo(group)
	qs := []expressions.Quantifier{expressions.ForEachQuantifier(a), expressions.ForEachQuantifier(b)}
	if got := slices.Collect(memo.findCandidateParents(qs, nil)); len(got) != 0 {
		t.Fatalf("candidate has no member referencing both children: %v", got)
	}
	union, err := expressions.NewLogicalUnionExpression(qs)
	union = mustConstruct(t, union, err)
	if !memo.InsertReExploring(group, union) {
		t.Fatal("union not inserted")
	}
	if got := slices.Collect(memo.findCandidateParents(qs, nil)); len(got) != 1 || got[0] != group {
		t.Fatalf("candidate with a common referencing expression was lost: %v", got)
	}
}

func TestMemoCandidatesPreserveOrderAndIndex(t *testing.T) {
	t.Parallel()
	a := expressions.InitialOf(memoTestScan(t, "A"))
	b := expressions.InitialOf(memoTestScan(t, "B"))
	c := expressions.InitialOf(memoTestScan(t, "C"))
	memo := NewMemo(nil)
	var parents []*expressions.Reference
	for _, children := range [][]*expressions.Reference{{a}, {a, b, b}, {a, b}, {a, b, b}} {
		qs := make([]expressions.Quantifier, len(children))
		for i, child := range children {
			qs[i] = expressions.ForEachQuantifier(child)
		}
		union, err := expressions.NewLogicalUnionExpression(qs)
		parent := expressions.InitialOf(mustConstruct(t, union, err))
		memo.RegisterReference(parent)
		parents = append(parents, parent)
	}
	for _, tc := range []struct {
		children []*expressions.Reference
		want     []*expressions.Reference
	}{
		{nil, nil},
		{[]*expressions.Reference{nil}, nil},
		{[]*expressions.Reference{a, b}, parents[1:]},
		{[]*expressions.Reference{a}, parents},
		{[]*expressions.Reference{b, a}, parents[1:]},
		{[]*expressions.Reference{a, a}, parents},
		{[]*expressions.Reference{a, c}, nil},
		{[]*expressions.Reference{a, nil}, nil},
	} {
		qs := make([]expressions.Quantifier, len(tc.children))
		for i, child := range tc.children {
			qs[i] = expressions.ForEachQuantifier(child)
		}
		candidates := memo.findCandidateParents(qs, nil)
		for range 2 {
			if got := slices.Collect(candidates); !slices.Equal(got, tc.want) {
				t.Fatalf("candidates=%v, want ordered %v", got, tc.want)
			}
		}
		for first := range candidates {
			if len(tc.want) == 0 || first != tc.want[0] {
				t.Fatalf("first candidate=%v, want first of %v", first, tc.want)
			}
			break
		}
		if len(tc.want) > 1 {
			last := tc.want[len(tc.want)-1]
			got := slices.Collect(memo.findCandidateParents(qs, func(ref *expressions.Reference) bool { return ref == last }))
			if !slices.Equal(got, []*expressions.Reference{last}) {
				t.Fatalf("filtered candidates=%v, want [%v]", got, last)
			}
		}
		if got := slices.Collect(memo.findCandidateParents(qs, func(*expressions.Reference) bool { return false })); len(got) != 0 {
			t.Fatalf("ineligible candidates yielded: %v", got)
		}
	}
}

func TestMemo_NewMemo_NilRoot(t *testing.T) {
	t.Parallel()
	m := NewMemo(nil)
	if m.Root() != nil {
		t.Fatal("expected nil root")
	}
	if len(m.References()) != 0 {
		t.Fatal("expected empty references")
	}
}

func TestMemo_IndexesBothMemberLanes(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"root", "register", "yield"} {
		for _, lane := range []string{"exploratory", "final", "mixed"} {
			t.Run(entry+"/"+lane, func(t *testing.T) {
				t.Parallel()
				leaf := expressions.FinalOf(memoTestScan(t, "T"))
				inner, err := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(leaf))
				inner = mustConstruct(t, inner, err)
				var middle *expressions.Reference
				switch lane {
				case "exploratory":
					middle = expressions.InitialOf(inner)
				case "final":
					middle = expressions.FinalOf(inner)
				case "mixed":
					middle = expressions.InitialOf(memoTestScan(t, "T"))
					middle.InsertFinal(inner)
				}
				outer, err := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(middle))
				outer = mustConstruct(t, outer, err)
				root := expressions.FinalOf(outer)
				var memo *Memo
				switch entry {
				case "root":
					memo = NewMemo(root)
				case "register":
					memo = NewMemo(nil)
					memo.RegisterReference(root)
				case "yield":
					root = expressions.InitialOf(memoTestScan(t, "T"))
					memo = NewMemo(root)
					root.InsertFinal(outer)
					memo.AddExpression(root, outer)
				}
				if memo.admissionErr != nil {
					t.Fatal(memo.admissionErr)
				}
				for _, group := range []*expressions.Reference{root, middle, leaf} {
					if !memo.ContainsReference(group) {
						t.Errorf("reachable reference missing from %s/%s topology", entry, lane)
					}
				}
				for _, edge := range []struct {
					parent, child *expressions.Reference
					expr          expressions.RelationalExpression
				}{{root, middle, outer}, {middle, leaf, inner}} {
					indexed := 0
					for _, got := range memo.childToParents[edge.child] {
						if got.parent == edge.parent && got.expr == edge.expr {
							indexed++
						}
					}
					if indexed != 1 {
						t.Errorf("member edge indexed %d times, want exactly once", indexed)
					}
				}
				for _, group := range []*expressions.Reference{root, middle, leaf} {
					_, isLeaf := memo.leafRefsSet[group]
					wantLeaf := group == leaf || group == middle && lane == "mixed" || group == root && entry == "yield"
					if isLeaf != wantLeaf {
						t.Errorf("leaf classification=%v, want %v (any leaf member in either lane)", isLeaf, wantLeaf)
					}
				}
			})
		}
	}
}

func TestMemo_NewMemo_IndexesDAG(t *testing.T) {
	t.Parallel()
	// Build: Filter(P, Scan)
	scan := memoTestScan(t, "MyRecord")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	filter := memoTestFilter(
		t,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		scanQ,
	)
	rootRef := expressions.InitialOf(filter)

	m := NewMemo(rootRef)
	if m.Root() != rootRef {
		t.Fatal("root mismatch")
	}
	if !m.ContainsReference(rootRef) {
		t.Fatal("root not indexed")
	}
	if !m.ContainsReference(scanRef) {
		t.Fatal("scanRef not indexed")
	}
	if len(m.References()) != 2 {
		t.Fatalf("expected 2 references, got %d", len(m.References()))
	}
}

func TestMemo_MemoizeExpression_LeafReuse(t *testing.T) {
	t.Parallel()
	// Two structurally-equal leaf scans should be memoized into the
	// same Reference.
	scan1 := memoTestScan(t, "T")
	scan2 := memoTestScan(t, "T")

	m := NewMemo(nil)
	ref1 := m.MemoizeExpression(scan1)
	ref2 := m.MemoizeExpression(scan2)

	if ref1 != ref2 {
		t.Fatal("expected same Reference for structurally-equal leaves")
	}
	if len(ref1.Members()) != 1 {
		t.Fatalf("expected 1 member, got %d", len(ref1.Members()))
	}
}

func TestMemo_MemoizeExpression_LeafDistinct(t *testing.T) {
	t.Parallel()
	// Two scans with different record types are different.
	scan1 := memoTestScan(t, "A")
	scan2 := memoTestScan(t, "B")

	m := NewMemo(nil)
	ref1 := m.MemoizeExpression(scan1)
	ref2 := m.MemoizeExpression(scan2)

	if ref1 == ref2 {
		t.Fatal("expected different References for different record types")
	}
}

func TestMemo_MemoizeExpression_NonLeafReuse(t *testing.T) {
	t.Parallel()
	// Two structurally-equal filters over the SAME child Reference
	// should be memoized into the same Reference.
	scan := memoTestScan(t, "T")
	scanRef := expressions.InitialOf(scan)
	scanQ1 := expressions.ForEachQuantifier(scanRef)
	scanQ2 := expressions.ForEachQuantifier(scanRef)

	pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	filter1 := memoTestFilter(t, pred, scanQ1)
	filter2 := memoTestFilter(t, pred, scanQ2)

	m := NewMemo(nil)
	// Register the scan Reference first (simulates prior memoization).
	m.RegisterReference(scanRef)

	ref1 := m.MemoizeExpression(filter1)
	ref2 := m.MemoizeExpression(filter2)

	if ref1 != ref2 {
		t.Fatal("expected same Reference for structurally-equal non-leaf expressions over same child")
	}
}

func TestMemo_MemoizeExpression_ProjectionAliasesDoNotCollapse(t *testing.T) {
	t.Parallel()
	scan := memoTestScan(t, "T")
	scanRef := expressions.InitialOf(scan)
	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	projection := func(alias string) *expressions.LogicalProjectionExpression {
		projection, err := expressions.NewLogicalProjectionExpressionWithAliases(
			[]values.Value{values.NewBooleanValue(true)},
			[]string{alias},
			expressions.ForEachQuantifier(scanRef),
		)
		return mustConstruct(t, projection, err)
	}

	refA := m.MemoizeExpression(projection("A"))
	refB := m.MemoizeExpression(projection("B"))
	if refA == refB {
		t.Fatal("memo collapsed projections with different output schemas")
	}
	if refATwin := m.MemoizeExpression(projection("A")); refATwin != refA {
		t.Fatal("memo failed to intern a projection with the same exact output alias")
	}
}

func TestMemo_MemoizeExpression_PhysicalProjectionAliasesDoNotCollapse(t *testing.T) {
	t.Parallel()
	scan, scanErr := plans.NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	scan = mustConstruct(t, scan, scanErr)
	scanRef := expressions.InitialOf(scan)
	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	projection := func(alias string) *plans.RecordQueryProjectionPlan {
		projection, err := plans.NewRecordQueryProjectionPlanFromQuantifier(
			[]values.Value{values.NewBooleanValue(true)},
			[]string{alias},
			expressions.ForEachQuantifier(scanRef),
		)
		return mustConstruct(t, projection, err)
	}

	refA := m.MemoizeExpression(projection("A"))
	refB := m.MemoizeExpression(projection("B"))
	if refA == refB {
		t.Fatal("memo collapsed physical projections with different output schemas")
	}
	if refATwin := m.MemoizeExpression(projection("A")); refATwin != refA {
		t.Fatal("memo failed to intern a physical projection with the same exact output alias")
	}
}

func TestMemo_MemoizeExpression_NonLeafDistinctChildren(t *testing.T) {
	t.Parallel()
	// Two filters that are structurally the same node-info but point to
	// DIFFERENT child References are in different equivalence classes.
	scanA := memoTestScan(t, "A")
	scanB := memoTestScan(t, "B")
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)

	pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	filter1 := memoTestFilter(t, pred, expressions.ForEachQuantifier(refA))
	filter2 := memoTestFilter(t, pred, expressions.ForEachQuantifier(refB))

	m := NewMemo(nil)
	m.RegisterReference(refA)
	m.RegisterReference(refB)

	ref1 := m.MemoizeExpression(filter1)
	ref2 := m.MemoizeExpression(filter2)

	if ref1 == ref2 {
		t.Fatal("expected different References for expressions with different children")
	}
}

func TestMemo_MemoizeExpression_IntegrationWithPlanner(t *testing.T) {
	t.Parallel()
	// Build a simple tree and verify the Planner's Memo is populated.
	scan := memoTestScan(t, "T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	filter := memoTestFilter(
		t,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		scanQ,
	)
	rootRef := expressions.InitialOf(filter)

	p := NewPlanner(DefaultExpressionRules(), nil)
	_, converged := exploreRewriting(p, rootRef)
	if !converged {
		t.Fatal("expected convergence")
	}
	if p.Memo() == nil {
		t.Fatal("expected non-nil Memo after exploration")
	}
	if !p.Memo().ContainsReference(rootRef) {
		t.Fatal("root not in Memo")
	}
	if !p.Memo().ContainsReference(scanRef) {
		t.Fatal("scanRef not in Memo")
	}
}

func TestMemo_MemoizeExpressions_Batch(t *testing.T) {
	t.Parallel()
	scan1 := memoTestScan(t, "T")
	scan2 := memoTestScan(t, "T")

	m := NewMemo(nil)
	ref := m.MemoizeExpressions([]expressions.RelationalExpression{scan1, scan2})

	// Both go into the same Reference (they're structurally equal leaves).
	if len(ref.Members()) != 1 {
		t.Fatalf("expected 1 member (dedup'd), got %d", len(ref.Members()))
	}

	// Memoizing again returns the same Reference.
	scan3 := memoTestScan(t, "T")
	ref2 := m.MemoizeExpression(scan3)
	if ref != ref2 {
		t.Fatal("expected same Reference from subsequent memoize")
	}
}

func TestMemo_AddExpression_UpdatesIndex(t *testing.T) {
	t.Parallel()
	scan := memoTestScan(t, "T")
	scanRef := expressions.InitialOf(scan)

	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	// Now create a filter over scanRef and add it to a new Reference.
	pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	q := expressions.ForEachQuantifier(scanRef)
	filter := memoTestFilter(t, pred, q)
	filterRef := expressions.InitialOf(filter)
	m.RegisterReference(filterRef)

	// The childToParents index should show scanRef → filterRef.
	edges := m.childToParents[scanRef]
	found := false
	for _, e := range edges {
		if e.parent == filterRef {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected scanRef to have filterRef as parent in index")
	}

	// Now memoize a second filter with same pred over the same scanRef —
	// should find filterRef.
	q2 := expressions.ForEachQuantifier(scanRef)
	filter2 := memoTestFilter(t, pred, q2)
	ref := m.MemoizeExpression(filter2)
	if ref != filterRef {
		t.Fatal("expected memoization to reuse filterRef")
	}
}

func TestMemo_CrossReferenceSharing_ThroughRules(t *testing.T) {
	t.Parallel()
	// Scenario: two different paths in the DAG independently produce
	// the same sub-expression. The Memo should route them to the same
	// Reference.
	//
	// Build: Union(Filter(P, Scan("T")), Sort(Filter(P, Scan("T"))))
	// Both branches have "Filter(P, Scan("T"))" — with memoization,
	// they should share the same Reference for the Filter node.

	scan := memoTestScan(t, "T")

	m := NewMemo(nil)
	scanRef := m.MemoizeExpression(scan)

	pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}

	// First branch creates Filter(P, scanRef).
	q1 := expressions.ForEachQuantifier(scanRef)
	filter1 := memoTestFilter(t, pred, q1)
	filterRef1 := m.MemoizeExpression(filter1)

	// Second branch independently creates Filter(P, scanRef).
	q2 := expressions.ForEachQuantifier(scanRef)
	filter2 := memoTestFilter(t, pred, q2)
	filterRef2 := m.MemoizeExpression(filter2)

	// They should be the same Reference.
	if filterRef1 != filterRef2 {
		t.Fatal("expected cross-Reference sharing for identical Filter sub-expressions")
	}
}

func TestMemo_ExpressionRuleCall_MemoizeExpression_WithMemo(t *testing.T) {
	t.Parallel()
	scan := memoTestScan(t, "T")
	scanRef := expressions.InitialOf(scan)

	// Create a parent Reference (the one the rule fires on) that is
	// DIFFERENT from scanRef — otherwise the self-reference guard triggers.
	filter := memoTestFilter(
		t,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		expressions.ForEachQuantifier(scanRef),
	)
	parentRef := expressions.InitialOf(filter)

	m := NewMemo(nil)
	m.RegisterReference(parentRef)

	// Simulate a rule call on the parent Reference.
	call := NewExpressionRuleCallWithMemo(parentRef, nil, nil, m)

	// MemoizeExpression should find scanRef via the Memo.
	scan2 := memoTestScan(t, "T")
	ref := call.MemoizeExpression(scan2)
	if ref != scanRef {
		t.Fatal("expected MemoizeExpression via rule call to find existing scanRef")
	}
}

func TestMemo_ExpressionRuleCall_SelfRefGuard(t *testing.T) {
	t.Parallel()
	// When the Memo would return the SAME Reference the rule is
	// yielding into, the self-reference guard creates a fresh Reference
	// to prevent cycles.
	scan := memoTestScan(t, "T")
	scanRef := expressions.InitialOf(scan)

	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	call := NewExpressionRuleCallWithMemo(scanRef, nil, nil, m)
	scan2 := memoTestScan(t, "T")
	ref := call.MemoizeExpression(scan2)
	// Guard prevents returning scanRef (would be a cycle).
	if ref == scanRef {
		t.Fatal("self-reference guard should prevent returning call.Reference")
	}
	if len(ref.Members()) != 1 {
		t.Fatalf("expected fresh single-member ref, got %d members", len(ref.Members()))
	}
}

func TestMemo_ExpressionRuleCall_MemoizeExpression_WithoutMemo(t *testing.T) {
	t.Parallel()
	scan := memoTestScan(t, "T")
	scanRef := expressions.InitialOf(scan)

	// No Memo — should fall back to InitialOf.
	call := NewExpressionRuleCall(scanRef, nil, nil)
	scan2 := memoTestScan(t, "T")
	ref := call.MemoizeExpression(scan2)
	// Without Memo, a fresh Reference is created.
	if ref == scanRef {
		t.Fatal("without Memo, MemoizeExpression should create a fresh Reference")
	}
	if len(ref.Members()) != 1 {
		t.Fatalf("expected 1 member in fresh ref, got %d", len(ref.Members()))
	}
}

func TestMemo_PanicOnNilExpression(t *testing.T) {
	t.Parallel()
	m := NewMemo(nil)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil expression")
		}
	}()
	m.MemoizeExpression(nil)
}
