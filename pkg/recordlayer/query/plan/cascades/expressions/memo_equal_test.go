package expressions

import (
	"maps"
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type memoObservedExpression struct {
	RelationalExpression
	hashVisits, correlationVisits, equalityVisits int
}

func (e *memoObservedExpression) HashCodeWithoutChildren() uint64 {
	e.hashVisits++
	return 42 // Deliberate collisions must not turn repeated comparisons into repeated graph walks.
}

func (e *memoObservedExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	e.correlationVisits++
	return e.RelationalExpression.GetCorrelatedToWithoutChildren()
}

func (e *memoObservedExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	e.equalityVisits++
	if observed, ok := other.(*memoObservedExpression); ok {
		other = observed.RelationalExpression
	}
	return e.RelationalExpression.EqualsWithoutChildren(other, aliases)
}

func (e *memoObservedExpression) InternsAliasAware() bool { return true }

func TestReferenceMembersWithHash(t *testing.T) {
	t.Parallel()
	first := &memoObservedExpression{RelationalExpression: mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))}
	second := &memoObservedExpression{RelationalExpression: mustExpression(NewFullUnorderedScanExpression([]string{"U"}, testRecordType()))}
	final := &memoObservedExpression{RelationalExpression: mustExpression(NewFullUnorderedScanExpression([]string{"V"}, testRecordType()))}
	ref := InitialOf(first)
	if !ref.Insert(second) || !ref.InsertFinal(final) {
		t.Fatal("fixture did not retain distinct colliding members")
	}
	forwarder := InitialOf(final)
	forwarder.forwardedTo = ref
	for _, source := range []*Reference{nil, {}, ref, forwarder} {
		for _, hash := range []uint64{42, 43} {
			var want []RelationalExpression
			if (source == ref || source == forwarder) && hash == 42 {
				want = []RelationalExpression{first, second}
			}
			members := source.MembersWithHash(hash)
			for range 2 {
				if got := slices.Collect(members); !slices.Equal(got, want) {
					t.Fatalf("hash %d: members=%v, want %v", hash, got, want)
				}
			}
			for member := range members {
				if len(want) == 0 || member != want[0] {
					t.Fatalf("hash %d: first member=%v, want first of %v", hash, member, want)
				}
				break
			}
		}
	}
	if got := ref.Members(); !slices.Equal(got, []RelationalExpression{first, second}) {
		t.Fatal("hash iteration changed the exploratory lane")
	}
	if got := ref.FinalMembers(); !slices.Equal(got, []RelationalExpression{final}) {
		t.Fatal("hash iteration changed the final lane")
	}
}

func TestQuantifierDependenciesUseNilForNoLocalEdges(t *testing.T) {
	t.Parallel()
	scan := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	first := ForEachQuantifier(scan)
	independent := ForEachQuantifier(scan)
	dependent := ForEachQuantifier(InitialOf(mustExpression(NewSelectExpression(mustQOV(first.GetAlias()), nil, nil))))
	external := ForEachQuantifier(InitialOf(mustExpression(NewSelectExpression(mustQOV(values.UniqueCorrelationIdentifier()), nil, nil))))
	correlations := func(ref *Reference) map[values.CorrelationIdentifier]struct{} { return ref.GetCorrelatedTo() }
	for _, tc := range []struct {
		name         string
		quantifiers  []Quantifier
		canCorrelate bool
	}{
		{"empty", nil, true},
		{"uncorrelated operator", []Quantifier{first, dependent}, false},
		{"independent children", []Quantifier{first, independent}, true},
		{"external correlation", []Quantifier{first, external}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := quantifierDependencies(tc.quantifiers, tc.canCorrelate, correlations); got != nil {
				t.Fatalf("empty local dependency graph=%v, want nil", got)
			}
		})
	}
	dependencies := quantifierDependencies([]Quantifier{first, dependent}, true, correlations)
	if len(dependencies) != 2 || len(dependencies[0]) != 0 || !slices.Equal(dependencies[1], []int{0}) {
		t.Fatalf("local dependency graph=%v, want [[], [0]]", dependencies)
	}
}

func TestPreparedMemberDuplicate_ReusesGraphDerivations(t *testing.T) {
	t.Parallel()
	child := &memoObservedExpression{RelationalExpression: mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))}
	ref := InitialOf(child)
	member := func(k int64) *memoObservedExpression {
		q := ForEachQuantifier(ref)
		return &memoObservedExpression{RelationalExpression: mustExpression(NewSelectExpression(
			mustExpression(q.RequireFlowedObjectValue()), []Quantifier{q}, []predicates.QueryPredicate{
				predicates.NewComparisonPredicate(mustQOV(q.GetAlias()), predicates.Comparison{
					Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: k},
				}),
			},
		))}
	}
	first, second, incoming := member(1), member(2), member(3)
	child.hashVisits, child.correlationVisits = 0, 0
	if duplicate, _ := PreparedMemberDuplicate([]RelationalExpression{first, second}, incoming); duplicate {
		t.Fatal("hash collisions collapsed different predicates")
	}
	for name, expression := range map[string]*memoObservedExpression{"first": first, "second": second, "incoming": incoming, "child": child} {
		if expression.correlationVisits != 1 {
			t.Errorf("%s correlations derived %d times, want once per preparation", name, expression.correlationVisits)
		}
		if expression != child && expression.hashVisits != 1 {
			t.Errorf("%s hash derived %d times, want once per preparation", name, expression.hashVisits)
		}
	}
	if ref.correlatedToCache.Load() != nil {
		t.Fatal("read-only preparation published a shared correlation cache")
	}
	for _, hashes := range [][]uint64{nil, {42}, {42, 42}} {
		for _, expression := range []*memoObservedExpression{first, second, incoming, child} {
			expression.hashVisits, expression.correlationVisits = 0, 0
		}
		var preparation PreparedMemberEquality
		for range 3 {
			if duplicate, _ := preparation.DuplicateWithHashes([]RelationalExpression{first, second}, hashes, incoming); duplicate {
				t.Fatal("batch equality collapsed different predicates")
			}
		}
		for name, expression := range map[string]*memoObservedExpression{"first": first, "second": second, "incoming": incoming, "child": child} {
			if expression.correlationVisits != 1 || expression.hashVisits > 1 {
				t.Errorf("batch %s: correlation visits=%d hash visits=%d", name, expression.correlationVisits, expression.hashVisits)
			}
		}
		if duplicate, aliasAware := preparation.DuplicateWithHashes([]RelationalExpression{incoming}, nil, member(3)); !duplicate || !aliasAware {
			t.Fatal("batch equality failed to recognize an alias-renamed duplicate")
		}
	}
}

func TestPreparedMemberIndexPrunesDisjointSingletonInputs(t *testing.T) {
	t.Parallel()
	scan := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	parent := func(arity int) *memoObservedExpression {
		qs := make([]Quantifier, arity)
		for i := range qs {
			qs[i] = ForEachQuantifier(scan)
		}
		child := InitialOf(mustExpression(NewLogicalUnionExpression(qs)))
		return &memoObservedExpression{RelationalExpression: mustExpression(NewLogicalUniqueExpression(ForEachQuantifier(child)))}
	}
	members := make([]RelationalExpression, 32)
	for i := range members {
		members[i] = parent(i + 2)
	}
	var equality PreparedMemberEquality
	index := equality.NewMemberIndex(members[:4], nil)
	if duplicate, _ := index.Duplicate(parent(34)); duplicate {
		t.Fatal("small lane collapsed different arities")
	}
	for _, member := range members[:4] {
		member.(*memoObservedExpression).equalityVisits = 0
	}
	for i, member := range members[4:] {
		index.Add(member)
		if i == 3 {
			if duplicate, _ := index.Duplicate(parent(34)); duplicate {
				t.Fatal("lane crossing index threshold collapsed different arities")
			}
		}
	}
	if duplicate, _ := index.Duplicate(parent(34)); duplicate {
		t.Fatal("different child arities collapsed")
	}
	for i, member := range members {
		if visits := member.(*memoObservedExpression).equalityVisits; visits != 0 {
			t.Fatalf("member %d: disjoint child signature caused %d equality calls, want zero", i, visits)
		}
	}
	if duplicate, _ := index.Duplicate(parent(33)); !duplicate {
		t.Fatal("independently built equivalent singleton input did not deduplicate")
	}
}

func preparedIndexPadding() []RelationalExpression {
	members := make([]RelationalExpression, 7)
	for i := range members {
		members[i] = mustExpression(NewFullUnorderedScanExpression([]string{"padding"}, testRecordType()))
	}
	return members
}

func TestPreparedMemberIndexPreservesDirectionalLanes(t *testing.T) {
	t.Parallel()
	scan := func(name string) RelationalExpression {
		return mustExpression(NewFullUnorderedScanExpression([]string{name}, testRecordType()))
	}
	parent := func(exploratory, final int) RelationalExpression {
		ref := InitialOf(scan("T"))
		expression := mustExpression(NewLogicalUniqueExpression(ForEachQuantifier(ref)))
		ref.members, ref.finalMembers = nil, nil
		for i, name := range []string{"T", "U"} {
			if exploratory&(1<<i) != 0 {
				ref.members = append(ref.members, scan(name))
			}
			if final&(1<<i) != 0 {
				ref.finalMembers = append(ref.finalMembers, scan(name))
			}
		}
		return expression
	}
	for haveExploratory := range 4 {
		for haveFinal := range 4 {
			members := append(preparedIndexPadding(), parent(haveExploratory, haveFinal))
			var equality PreparedMemberEquality
			index := equality.NewMemberIndex(members, nil)
			for wantExploratory := range 4 {
				for wantFinal := range 4 {
					incoming := parent(wantExploratory, wantFinal)
					want := wantExploratory & ^haveExploratory == 0 && wantFinal & ^haveFinal == 0
					got, aliasAware := index.Duplicate(incoming)
					linear, linearAliasAware := PreparedMemberDuplicate(members, incoming)
					if got != want || got != linear || aliasAware != linearAliasAware {
						t.Fatalf("have=%02b/%02b want=%02b/%02b: indexed=%t/%t linear=%t/%t expected=%t", haveExploratory, haveFinal, wantExploratory, wantFinal, got, aliasAware, linear, linearAliasAware, want)
					}
				}
			}
		}
	}
}

func TestPreparedMemberIndexRejectsDifferentChildKeys(t *testing.T) {
	t.Parallel()
	scan := func(name string) RelationalExpression {
		return mustExpression(NewFullUnorderedScanExpression([]string{name}, values.NotNullLong))
	}
	leftScan, rightScan := scan("T"), scan("U")
	if leftScan.HashCodeWithoutChildren() == rightScan.HashCodeWithoutChildren() {
		t.Fatal("scan fixture must have different hashes")
	}
	selectExpression := mustExpression(NewSelectExpression(&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}, nil, nil))
	for _, tc := range []struct {
		name        string
		left, right RelationalExpression
	}{
		{"hash", leftScan, rightScan},
		{"correlation capability", &memoObservedExpression{RelationalExpression: leftScan}, &memoObservedExpression{RelationalExpression: selectExpression}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			left := &memoObservedExpression{RelationalExpression: mustExpression(NewLogicalUniqueExpression(ForEachQuantifier(InitialOf(tc.left))))}
			right := &memoObservedExpression{RelationalExpression: mustExpression(NewLogicalUniqueExpression(ForEachQuantifier(InitialOf(tc.right))))}
			var equality PreparedMemberEquality
			index := equality.NewMemberIndex(append(preparedIndexPadding(), left), nil)
			if duplicate, _ := index.Duplicate(right); duplicate {
				t.Fatal("distinct child keys collapsed")
			}
			if left.equalityVisits != 0 {
				t.Fatalf("distinct child key caused %d equality calls", left.equalityVisits)
			}
		})
	}
}

func TestPreparedMemberIndexPreservesCandidateOrder(t *testing.T) {
	t.Parallel()
	scan := func() RelationalExpression {
		return mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	}
	child := InitialOf(scan())
	alias := values.NamedCorrelationIdentifier("q")
	filter := func(alias values.CorrelationIdentifier, ref *Reference) RelationalExpression {
		return mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{predicates.NewComparisonPredicate(
			mustQOV(alias), predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
		)}, NamedForEachQuantifier(alias, ref)))
	}
	incoming := filter(alias, child)
	for _, keyedFirst := range []bool{false, true} {
		// The unkeyed group contains two distinct scan nodes with equal contents.
		multi := InitialOf(scan())
		multi.members = append(multi.members, scan())
		members := []RelationalExpression{filter(values.UniqueCorrelationIdentifier(), multi), filter(alias, child)}
		if keyedFirst {
			slices.Reverse(members)
		}
		members = append(preparedIndexPadding(), members...)
		var equality PreparedMemberEquality
		index := equality.NewMemberIndex(members, nil)
		got, aliasAware := index.Duplicate(incoming)
		if !got || aliasAware == keyedFirst {
			t.Fatalf("keyedFirst=%t: duplicate=%t aliasAware=%t, want true/%t", keyedFirst, got, aliasAware, !keyedFirst)
		}
	}
}

func TestPreparedMemberIndexCollisionsAndForwarding(t *testing.T) {
	t.Parallel()
	scan := func(name string) RelationalExpression {
		return &memoObservedExpression{RelationalExpression: mustExpression(NewFullUnorderedScanExpression([]string{name}, testRecordType()))}
	}
	child := InitialOf(scan("T"))
	parent := func(ref *Reference) RelationalExpression {
		return mustExpression(NewLogicalUniqueExpression(ForEachQuantifier(ref)))
	}
	forwarded, middle := InitialOf(scan("U")), InitialOf(scan("V"))
	forwarded.forwardedTo, middle.forwardedTo = middle, child
	members := append(preparedIndexPadding(), parent(forwarded))
	var equality PreparedMemberEquality
	index := equality.NewMemberIndex(members, nil)
	for _, name := range []string{"T", "U"} {
		if duplicate, _ := index.Duplicate(parent(InitialOf(scan(name)))); duplicate != (name == "T") {
			t.Fatalf("child %s: duplicate=%t, want %t", name, duplicate, name == "T")
		}
	}
	if forwarded.forwardedTo != middle || middle.forwardedTo != child {
		t.Fatal("prepared index path-compressed forwarding")
	}
	if child.correlatedToCache.Load() != nil {
		t.Fatal("prepared index published a correlation cache")
	}
}

func TestPreparedMemberDuplicateReusesPublishedCorrelations(t *testing.T) {
	t.Parallel()
	outer := values.NamedCorrelationIdentifier("outer")
	child := &memoObservedExpression{RelationalExpression: mustExpression(NewSelectExpression(mustQOV(outer), nil, nil))}
	childRef := InitialOf(child)
	parent := InitialOf(mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(childRef))))
	left := mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(parent)))
	right := mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(parent)))
	want := map[values.CorrelationIdentifier]struct{}{outer: {}}
	if got := parent.GetCorrelatedTo(); !maps.Equal(got, want) {
		t.Fatalf("warm correlations=%v, want %v", got, want)
	}
	parentSnapshot, childSnapshot := parent.correlatedToCache.Load(), childRef.correlatedToCache.Load()
	child.correlationVisits = 0
	for range 3 {
		if duplicate, _ := PreparedMemberDuplicate([]RelationalExpression{left}, right); !duplicate {
			t.Fatal("alias-renamed duplicate was not recognized")
		}
	}
	if child.correlationVisits != 0 {
		t.Fatalf("unchanged cached descendant correlations rederived %d times", child.correlationVisits)
	}
	if parent.correlatedToCache.Load() != parentSnapshot || childRef.correlatedToCache.Load() != childSnapshot {
		t.Fatal("read-only equality replaced a published correlation snapshot")
	}
}

func TestPreparedCorrelationsValidatePublishedDependencies(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"cold", "unchanged", "exploratory", "final", "forwarded", "pruned"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			outer := values.NamedCorrelationIdentifier("outer")
			added := values.NamedCorrelationIdentifier("added")
			member := mustExpression(NewSelectExpression(mustQOV(outer), nil, nil))
			child := InitialOf(member)
			parent := InitialOf(mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(child))))
			root := mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(parent)))
			want := map[values.CorrelationIdentifier]struct{}{outer: {}}
			extra := mustExpression(NewSelectExpression(mustQOV(added), nil, nil))
			if change == "pruned" {
				if !child.InsertFinal(extra) {
					t.Fatal("prunable final correlation did not insert")
				}
				want[added] = struct{}{}
			}
			warmWant := maps.Clone(want)
			var borrowed map[values.CorrelationIdentifier]struct{}
			if change != "cold" {
				borrowed = parent.GetCorrelatedTo()
				if !maps.Equal(borrowed, want) {
					t.Fatalf("warm correlations=%v, want %v", borrowed, want)
				}
			}
			switch change {
			case "exploratory":
				if !child.Insert(extra) {
					t.Fatal("new exploratory correlation did not insert")
				}
				want[added] = struct{}{}
			case "final":
				if !child.InsertFinal(extra) {
					t.Fatal("new final correlation did not insert")
				}
				want[added] = struct{}{}
			case "forwarded":
				child.forwardedTo = InitialOf(extra)
				want = map[values.CorrelationIdentifier]struct{}{added: {}}
			case "pruned":
				child.PruneWith(member)
				delete(want, added)
			}
			parentSnapshot, childSnapshot := parent.correlatedToCache.Load(), child.correlatedToCache.Load()
			for range 2 {
				equality := newMemoEquality()
				if got := equality.correlations.expression(root); !maps.Equal(got, want) {
					t.Fatalf("prepared correlations=%v, want %v", got, want)
				}
			}
			if parent.correlatedToCache.Load() != parentSnapshot || child.correlatedToCache.Load() != childSnapshot {
				t.Fatal("read-only equality published refreshed correlations")
			}
			if borrowed != nil && !maps.Equal(borrowed, warmWant) {
				t.Fatalf("read-only equality changed borrowed correlations: %v", borrowed)
			}
		})
	}
}

func TestPreparedCorrelationsPublicationAfterGraphChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"exploratory", "final", "forwarded", "pruned"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			outer := values.NamedCorrelationIdentifier("outer")
			added := values.NamedCorrelationIdentifier("added")
			member := mustExpression(NewSelectExpression(mustQOV(outer), nil, nil))
			extra := mustExpression(NewSelectExpression(mustQOV(added), nil, nil))
			child := InitialOf(member)
			if change == "pruned" && !child.InsertFinal(extra) {
				t.Fatal("prunable final correlation did not insert")
			}
			parent := InitialOf(mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(child))))
			left := mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(parent)))
			right := mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(parent)))
			var preparation PreparedMemberEquality
			if duplicate, _ := preparation.DuplicateWithHashes([]RelationalExpression{left}, nil, right); !duplicate {
				t.Fatal("alias-renamed duplicate was not recognized")
			}
			if parent.correlatedToCache.Load() != nil || child.correlatedToCache.Load() != nil {
				t.Fatal("preparation published before commit")
			}
			want := map[values.CorrelationIdentifier]struct{}{outer: {}, added: {}}
			switch change {
			case "exploratory":
				if !child.Insert(extra) {
					t.Fatal("new exploratory correlation did not insert")
				}
			case "final":
				if !child.InsertFinal(extra) {
					t.Fatal("new final correlation did not insert")
				}
			case "forwarded":
				child.forwardedTo = InitialOf(extra)
				delete(want, outer)
			case "pruned":
				child.PruneWith(member)
				delete(want, added)
			}
			preparation.PublishCorrelations()
			if parent.correlatedToCache.Load() == nil {
				t.Fatal("unchanged parent version did not exercise stale-dependency publication")
			}
			if child.correlatedToCache.Load() != nil {
				t.Fatal("changed or forwarded child received an old snapshot")
			}
			for range 2 {
				if got := parent.GetCorrelatedTo(); !maps.Equal(got, want) {
					t.Fatalf("published correlations=%v, want %v after %s", got, want, change)
				}
			}
		})
	}
}

// Root selects retain externally visible binding identities; wrapping them lets
// prepared admission exercise alias-aware equality of their child populations.
func preparedChildDuplicate(a, b RelationalExpression) bool {
	left := mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(InitialOf(a))))
	right := mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(InitialOf(b))))
	duplicate, _ := PreparedMemberDuplicate([]RelationalExpression{left}, right)
	return duplicate
}

func TestMemoEqual_InternsAliasVariants(t *testing.T) {
	t.Parallel()
	scanRef := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	filter := func(k int64) RelationalExpression {
		q := ForEachQuantifier(scanRef)
		pred := predicates.NewComparisonPredicate(mustQOV(q.GetAlias()),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: k}})
		return mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q))
	}
	a := filter(1) // fresh alias q$N
	b := filter(1) // fresh alias q$M, same shape

	if !MemoEqual(a, b) {
		t.Fatal("alias-variant filters must be MemoEqual (the activation property)")
	}
	// Contrast: plain SemanticEquals (empty map at top) misses them — the gap.
	if SemanticEquals(a, b, EmptyAliasMap()) {
		t.Fatal("precondition: SemanticEquals should NOT see alias-variants equal (top-level empty map) — test vacuous otherwise")
	}
	// Negative: different constant ⇒ not MemoEqual.
	if MemoEqual(a, filter(2)) {
		t.Fatal("filters with different constants must not be MemoEqual")
	}
}

// TestMemoEqual_DistinctOuterCorrelationsDoNotIntern pins the external-
// correlation guard (correlatedToMatches / expressionCorrelatedTo) — the whole
// reason that machinery exists, and the motivating case in Java's own comment
// (Reference.java:755-762: a node correlated to outer `a.x` must NOT be
// memo-equal to one correlated to outer `b.y`). Two filters identical in shape
// and alias-invariant hash, differing ONLY in which OUTER alias their predicate
// references, must stay DISTINCT — otherwise a correlated subquery referencing
// the wrong outer binding would be silently shared.
func TestMemoEqual_DistinctOuterCorrelationsDoNotIntern(t *testing.T) {
	t.Parallel()
	scanRef := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	// filterCorrelatedTo builds Filter(QOV(localQ) = QOV(outer), →scan): the
	// comparison operand QOV(outer) references an alias NOT bound by the
	// filter's own quantifier, so the filter is EXTERNALLY correlated to outer.
	filterCorrelatedTo := func(outer values.CorrelationIdentifier) RelationalExpression {
		q := ForEachQuantifier(scanRef)
		pred := predicates.NewComparisonPredicate(mustQOV(q.GetAlias()),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: mustQOV(outer)})
		return mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q))
	}
	a := filterCorrelatedTo(values.NamedCorrelationIdentifier("a"))
	b := filterCorrelatedTo(values.NamedCorrelationIdentifier("b"))

	// Precondition: the alias-invariant hash is EQUAL, so both reach the
	// correlatedToMatches guard. Without this, an unequal hash would short-
	// circuit MemoEqual and the test would prove nothing about the guard.
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("precondition: alias-invariant hash must be equal so the external-correlation guard is reached — test vacuous otherwise")
	}
	if MemoEqual(a, b) {
		t.Fatal("filters correlated to DIFFERENT outer aliases must NOT be MemoEqual (external-correlation guard)")
	}
	// Sanity: SAME outer alias ⇒ MemoEqual (guard passes; local quantifier is a
	// permitted alias variant). Proves the guard rejects on the correlation
	// difference, not on something incidental to the construction.
	if !MemoEqual(filterCorrelatedTo(values.NamedCorrelationIdentifier("a")),
		filterCorrelatedTo(values.NamedCorrelationIdentifier("a"))) {
		t.Fatal("filters correlated to the SAME outer alias must be MemoEqual")
	}
}

// TestMemoEqual_ChildrenAsSet_PermutationBranch exercises the ChildrenAsSet
// permutation path of matchChildrenInMemo — load-bearing for join-order
// enumeration (PR-C) and otherwise unexercised (LogicalFilter et al. report
// ChildrenAsSet=false → positional path only). LogicalUnion is ChildrenAsSet
// (UNION is commutative), so two unions over the same child SET in swapped
// order must be MemoEqual via the permutation match. Distinct scans (T, U)
// make the positional permutation [0,1] FAIL — only [1,0] matches — so the
// test genuinely drives the permute fallback, not just the first attempt.
func TestMemoEqual_ChildrenAsSet_PermutationBranch(t *testing.T) {
	t.Parallel()
	scanT := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	scanU := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"U"}, testRecordType())))
	union := func(refs ...*Reference) RelationalExpression {
		qs := make([]Quantifier, len(refs))
		for i, r := range refs {
			qs[i] = ForEachQuantifier(r)
		}
		return mustExpression(NewLogicalUnionExpression(qs))
	}
	a := union(scanT, scanU)
	b := union(scanU, scanT) // swapped child order ⇒ only the permutation branch can match

	if !MemoEqual(a, b) {
		t.Fatal("ChildrenAsSet union with swapped child order must be MemoEqual (permutation branch)")
	}
	// Negative: a different child SET (two T's, no U) ⇒ no permutation matches.
	if MemoEqual(union(scanT, scanT), a) {
		t.Fatal("union over a different child set must NOT be MemoEqual")
	}
}

func TestMemoEqualityAuditBindings(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier
	scan := func(name string) *Reference {
		return InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{name}, values.NotNullLong)))
	}
	free := func(name string) *Reference {
		return InitialOf(mustExpression(NewSelectExpression(mustQOV(alias(name)), nil, nil)))
	}
	union := func(names []string, children ...*Reference) RelationalExpression {
		qs := make([]Quantifier, len(children))
		for i, child := range children {
			qs[i] = NamedForEachQuantifier(alias(names[i]), child)
		}
		return mustExpression(NewLogicalUnionExpression(qs))
	}
	a, b := scan("A"), scan("B")
	project := func(first, second string) RelationalExpression {
		return mustExpression(NewSelectExpression(mustQOV(alias("x")), []Quantifier{
			NamedForEachQuantifier(alias(first), a), NamedForEachQuantifier(alias(second), b),
		}, nil))
	}
	sharedLeft := project("x", "y")
	sharedRight := mustExpression(NewSelectExpression(sharedLeft.GetResultValue(), project("y", "x").GetQuantifiers(), nil))
	for _, tc := range []struct {
		name        string
		left, right RelationalExpression
	}{
		{"different_bindings", project("x", "y"), project("y", "x")},
		{"shared_projection", sharedLeft, sharedRight},
		{"free_correlation", union([]string{"a", "b"}, a, free("a")), union([]string{"c", "d"}, a, free("c"))},
		{"prefix_captures_free_alias", union([]string{"a", "b"}, free("b"), free("a")), union([]string{"c", "d"}, free("b"), free("c"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if MemoEqual(tc.left, tc.right) {
				t.Error("MemoEqual identifies different bindings")
			}
			if preparedChildDuplicate(tc.left, tc.right) {
				t.Error("prepared admission identifies different child bindings")
			}
			if SemanticEquals(tc.left, tc.right, EmptyAliasMap()) {
				t.Error("SemanticEquals identifies different bindings")
			}
			if duplicate, _ := PreparedMemberDuplicate([]RelationalExpression{tc.left}, tc.right); duplicate {
				t.Error("prepared admission discards an expression with different bindings")
			}
		})
	}
}

func TestMemoEqual_ReferenceLanesAreDirectional(t *testing.T) {
	t.Parallel()
	population := func(mask int) []RelationalExpression {
		var members []RelationalExpression
		for i, name := range []string{"T", "U"} {
			if mask&(1<<i) != 0 {
				members = append(members, mustExpression(NewFullUnorderedScanExpression([]string{name}, testRecordType())))
			}
		}
		return members
	}
	for haveExploratory := range 4 {
		for haveFinal := range 4 {
			for wantExploratory := range 4 {
				for wantFinal := range 4 {
					have := &Reference{members: population(haveExploratory), finalMembers: population(haveFinal)}
					want := &Reference{members: population(wantExploratory), finalMembers: population(wantFinal)}
					expected := wantExploratory & ^haveExploratory == 0 && wantFinal & ^haveFinal == 0
					if got := newMemoEquality().references(have, want, EmptyAliasMap()); got != expected {
						t.Fatalf("have exploratory/final=%02b/%02b want=%02b/%02b: contains=%t want %t", haveExploratory, haveFinal, wantExploratory, wantFinal, got, expected)
					}
				}
			}
		}
	}
}

func TestMemoEqual_RespectsSuppliedAliases(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier
	scan := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong)))
	build := func(local, outer string) RelationalExpression {
		q := NamedForEachQuantifier(alias(local), scan)
		return mustExpression(NewSelectExpression(mustQOV(alias(outer)), []Quantifier{q}, nil))
	}
	left, right := build("a", "x"), build("b", "y")
	for _, tc := range []struct {
		name    string
		aliases *AliasMap
		want    bool
	}{
		{"unbound_external", EmptyAliasMap(), false},
		{"renamed_external", AliasMapOf(alias("x"), alias("y")), true},
		{"complete", AliasMapOf(alias("x"), alias("y"), alias("a"), alias("b")), true},
		{"wrong_external", AliasMapOf(alias("x"), alias("z")), false},
		{"wrong_local", AliasMapOf(alias("x"), alias("y"), alias("a"), alias("z")), false},
		{"occupied_target", AliasMapOf(alias("x"), alias("y"), alias("z"), alias("b")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for name, equal := range map[string]func(RelationalExpression, RelationalExpression, *AliasMap) bool{
				"kernel": newMemoEquality().equal, "semantic": SemanticEquals,
			} {
				if got := equal(left, right, tc.aliases); got != tc.want {
					t.Errorf("%s equality=%t, want %t", name, got, tc.want)
				}
			}
		})
	}
}

func TestMemoEqual_CorrelatedChildrenDependencyOrder(t *testing.T) {
	t.Parallel()
	scan := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong)))
	makeSelect := func(reverse bool) RelationalExpression {
		base := ForEachQuantifier(scan)
		inner := ForEachQuantifier(scan)
		p := predicates.NewComparisonPredicate(mustExpression(inner.RequireFlowedObjectValue()), predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: mustExpression(base.RequireFlowedObjectValue()),
		})
		dependent := ForEachQuantifier(InitialOf(mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{p}, inner))))
		qs := []Quantifier{base, dependent}
		if reverse {
			qs[0], qs[1] = qs[1], qs[0]
		}
		return mustExpression(NewSelectExpression(mustExpression(base.RequireFlowedObjectValue()), qs, nil))
	}
	left, right := makeSelect(true), makeSelect(false)
	if !MemoEqual(left, right) || !preparedChildDuplicate(left, right) {
		t.Fatal("correlated children must be compared after their owning dependencies, regardless of slice order")
	}
}

func TestMemoEqual_ChildrenAsSetChecksNodeForEachMatch(t *testing.T) {
	t.Parallel()
	scan := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	leftA, leftB := ForEachQuantifier(scan), ForEachQuantifier(scan)
	rightA, rightB := ForEachQuantifier(scan), ForEachQuantifier(scan)
	left := mustExpression(NewSelectExpression(mustExpression(leftA.RequireFlowedObjectValue()), []Quantifier{leftA, leftB}, nil))
	right := mustExpression(NewSelectExpression(mustExpression(rightB.RequireFlowedObjectValue()), []Quantifier{rightA, rightB}, nil))
	for name, equal := range map[string]func(RelationalExpression, RelationalExpression) bool{
		"memo":     MemoEqual,
		"prepared": preparedChildDuplicate,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !equal(left, right) {
				t.Fatal("first child match disagrees on projection; the swapped match must still be considered")
			}
		})
	}
}

func BenchmarkMemoEqualRejectedUnionPairings(b *testing.B) {
	leftQs, rightQs := make([]Quantifier, 9), make([]Quantifier, 9)
	for i := range leftQs {
		leftQs[i] = ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))))
		rightQs[i] = ForEachQuantifier(InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"U"}, testRecordType()))))
		leftQs[i].GetRangesOver().GetCorrelatedTo()
		rightQs[i].GetRangesOver().GetCorrelatedTo()
	}
	left := mustExpression(NewLogicalUnionExpression(leftQs))
	right := mustExpression(NewLogicalUnionExpression(rightQs))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if MemoEqual(left, right) {
			b.Fatal("unions over distinct scans compared equal")
		}
	}
}

// TestMemoEqual_OuterJoinNotChildrenAsSet pins REVIEW.md #215: SelectExpression's
// ChildrenAsSet marker must reflect actual commutability, not be true for every
// join. A LEFT/FULL OUTER join is NOT invariant under quantifier permutation
// (NULL-extension is directional: `A LEFT JOIN B` != `B LEFT JOIN A`), so swapped
// outer joins must NOT be MemoEqual — otherwise matchChildrenInMemo permutes them
// and memoizeNonLeaf interns the two directions into one Reference. INNER/CROSS
// stay commutative. Distinct scans (T1, T2) make the positional permutation [0,1]
// fail, so MemoEqual can only succeed via the ChildrenAsSet permutation branch —
// the exact branch the fix gates on join type.
func TestMemoEqual_OuterJoinNotChildrenAsSet(t *testing.T) {
	t.Parallel()
	scanT1 := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T1"}, testRecordType())))
	scanT2 := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T2"}, testRecordType())))
	mkJoin := func(jt JoinType) *SelectExpression {
		q1 := NamedForEachQuantifier(values.NamedCorrelationIdentifier("T1"), scanT1)
		q2 := NamedForEachQuantifier(values.NamedCorrelationIdentifier("T2"), scanT2)
		// The POSITIONAL merge row (IsPositionalMergeRC) — the ordinal
		// merge-select marker; the join RV just has to reference both legs
		// symmetrically so swapped quantifiers share the node-info hash. (The
		// name-model anchored RC this test originally seeded was deleted along
		// with its producer.)
		rv := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "_0", Value: mustQOV(q1.GetAlias())},
			values.RecordConstructorField{Name: "_1", Value: mustQOV(q2.GetAlias())},
		)
		return mustExpression(NewSelectExpressionWithJoinType(rv, []Quantifier{q1, q2}, nil, []string{"T1", "T2"}, jt))
	}

	// INNER is commutative: swapped order interns (drives the permutation branch,
	// since distinct scans make positional [0,1] fail). This is the positive
	// control — it proves the permutation machinery works and that the negatives
	// below fail because of the join-type narrowing, not an incidental mismatch.
	innerAB := mkJoin(JoinInner)
	innerBA := innerAB.WithSwappedQuantifiers()
	if !MemoEqual(innerAB, innerBA) {
		t.Fatal("INNER join with swapped quantifiers must be MemoEqual (commutative, permutation branch)")
	}

	// LEFT OUTER is directional: swapped order must NOT intern.
	leftAB := mkJoin(JoinLeftOuter)
	leftBA := leftAB.WithSwappedQuantifiers()
	// Precondition: node-info hash is equal (joinType/resultValue/predicates match),
	// so MemoEqual is actually reached and returns false on the child/permutation
	// path — not short-circuited by the hash gate. Test is vacuous otherwise.
	if leftAB.HashCodeWithoutChildren() != leftBA.HashCodeWithoutChildren() {
		t.Fatal("precondition: swapped LEFT OUTER joins must share node-info hash — test vacuous otherwise")
	}
	if MemoEqual(leftAB, leftBA) {
		t.Fatal("swapped LEFT OUTER joins must NOT be MemoEqual (directional — ChildrenAsSet must be false)")
	}

	// FULL OUTER keeps the original left/right column layout (translator: no swap),
	// so it is likewise not permutation-invariant.
	fullAB := mkJoin(JoinFullOuter)
	fullBA := fullAB.WithSwappedQuantifiers()
	if MemoEqual(fullAB, fullBA) {
		t.Fatal("swapped FULL OUTER joins must NOT be MemoEqual (ChildrenAsSet must be false)")
	}
}

// TestMemoEqual_QuantifierAttributeVariantsDoNotIntern pins the RFC-186
// quantifier-attribute identity refinement at the MEMO layer, independent of
// any rule: edge attributes (kind / null-on-empty / strict-single) travel on
// the quantifier, not the child content, so two selects over the SAME child
// reference that differ only in a quantifier flag are DIFFERENT semantics — a
// LEFT box vs an INNER join, a semi-join vs a join, a cardinality gate vs
// none — and must not collapse into one memo member. Before the refinement,
// both MemoEqual and the sameChildReferences interning fast path in
// Reference.Insert conflated them, leaving the first arrival's flags
// authoritative for both (the wrong-rows class the box-join families
// surfaced).
func TestMemoEqual_QuantifierAttributeVariantsDoNotIntern(t *testing.T) {
	t.Parallel()
	scanRef := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	alias := values.NamedCorrelationIdentifier("q")
	selOver := func(q Quantifier) RelationalExpression {
		return mustExpression(NewSelectExpression(mustExpression(q.RequireFlowedObjectValue()), []Quantifier{q}, nil))
	}
	plain := selOver(NamedForEachQuantifier(alias, scanRef))
	noe := selOver(NamedForEachNullOnEmptyQuantifier(alias, scanRef))
	strictSingle := selOver(NamedForEachStrictSingleQuantifier(alias, scanRef))
	existential := selOver(NamedExistentialQuantifier(alias, scanRef))

	if MemoEqual(plain, noe) {
		t.Fatal("selects differing only in a quantifier's null-on-empty flag must NOT be MemoEqual (LEFT box vs INNER join)")
	}
	if MemoEqual(plain, strictSingle) {
		t.Fatal("selects differing only in a quantifier's strict-single flag must NOT be MemoEqual")
	}
	if MemoEqual(plain, existential) {
		t.Fatal("selects differing only in quantifier KIND must NOT be MemoEqual (join vs semi-join)")
	}
	// Sanity: identical attributes over the same child ARE MemoEqual — the
	// refinement narrows identity, it does not break interning.
	if !MemoEqual(plain, selOver(NamedForEachQuantifier(alias, scanRef))) {
		t.Fatal("attribute-identical selects over the same child must remain MemoEqual")
	}

	// The Insert/InsertFinal interning fast path (sameChildReferences) must
	// make the same distinction: the noe variant is a NEW member, an
	// attribute-identical twin dedups.
	ref := InitialOf(plain)
	if !ref.Insert(noe) {
		t.Fatal("noe variant must insert as a DISTINCT member (interning fast path conflated the flags)")
	}
	if got := len(ref.Members()); got != 2 {
		t.Fatalf("expected 2 distinct members after inserting the noe variant, got %d", got)
	}
	if ref.Insert(selOver(NamedForEachQuantifier(alias, scanRef))) {
		t.Fatal("attribute-identical twin must dedup against the existing member")
	}
}
