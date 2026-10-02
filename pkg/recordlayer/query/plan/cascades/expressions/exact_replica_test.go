package expressions

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// replicaTestExpr is an operator whose node information is its name plus the
// aliases it reads, compared under the alias map like a real predicate.
type replicaTestExpr struct {
	name        string
	quantifiers []Quantifier
	reads       []values.CorrelationIdentifier
	aliasAware  bool
	asSet       bool
}

func (e *replicaTestExpr) GetResultValue() values.Value {
	return values.NewQueriedValue(nil, values.NotNullLong)
}
func (e *replicaTestExpr) GetQuantifiers() []Quantifier { return e.quantifiers }
func (e *replicaTestExpr) CanCorrelate() bool           { return true }
func (e *replicaTestExpr) ChildrenAsSet() bool          { return e.asSet }
func (e *replicaTestExpr) InternsAliasAware() bool      { return e.aliasAware }
func (e *replicaTestExpr) HashCodeWithoutChildren() uint64 {
	return uint64(len(e.name))
}

func (e *replicaTestExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	result := make(map[values.CorrelationIdentifier]struct{}, len(e.reads))
	for _, alias := range e.reads {
		result[alias] = struct{}{}
	}
	return result
}

func (e *replicaTestExpr) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*replicaTestExpr)
	if !ok || o.name != e.name || len(o.reads) != len(e.reads) {
		return false
	}
	for i, alias := range e.reads {
		if aliases.GetTargetOrDefault(alias, alias) != o.reads[i] {
			return false
		}
	}
	return true
}

func (e *replicaTestExpr) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	copied := *e
	copied.quantifiers = quantifiers
	return &copied, nil
}

// correlatedLeaf is a leaf group member reading an outer alias.
type correlatedLeaf struct {
	stubExpr
	outer values.CorrelationIdentifier
}

func (e *correlatedLeaf) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{e.outer: {}}
}

func TestExactReplica(t *testing.T) {
	t.Parallel()
	x, y := values.NamedCorrelationIdentifier("X"), values.NamedCorrelationIdentifier("Y")
	s, u := values.NamedCorrelationIdentifier("S"), values.NamedCorrelationIdentifier("U")
	scan := InitialOf(&stubExpr{name: "scan"})
	sameContent := InitialOf(&stubExpr{name: "scan"})
	forwarded := InitialOf(&stubExpr{name: "scan"})
	forwarded.forwardedTo = scan
	correlatedToX := InitialOf(&correlatedLeaf{stubExpr: stubExpr{name: "probe"}, outer: x})
	correlatedOutside := InitialOf(&correlatedLeaf{stubExpr: stubExpr{name: "probe"}, outer: values.NamedCorrelationIdentifier("OUTER")})
	m1, m2 := values.MergeCorrelationIdentifier(1), values.MergeCorrelationIdentifier(2)
	correlatedToM1 := InitialOf(&correlatedLeaf{stubExpr: stubExpr{name: "probe"}, outer: m1})

	over := func(name string, aware bool, quantifiers ...Quantifier) *replicaTestExpr {
		reads := make([]values.CorrelationIdentifier, 0, len(quantifiers))
		for _, q := range quantifiers {
			reads = append(reads, q.GetAlias())
		}
		return &replicaTestExpr{name: name, quantifiers: quantifiers, reads: reads, aliasAware: aware}
	}
	asSet := func(e *replicaTestExpr) *replicaTestExpr { e.asSet = true; return e }

	for _, tc := range []struct {
		name string
		a, b RelationalExpression
		want bool
	}{
		{"same operator over same group", over("f", false, NamedForEachQuantifier(x, scan)), over("f", false, NamedForEachQuantifier(x, scan)), true},
		{"different operator", over("f", false, NamedForEachQuantifier(x, scan)), over("g", false, NamedForEachQuantifier(x, scan)), false},
		{"equal content in another group", over("f", false, NamedForEachQuantifier(x, scan)), over("f", false, NamedForEachQuantifier(x, sameContent)), false},
		{"input merged into the same group", over("f", false, NamedForEachQuantifier(x, forwarded)), over("f", false, NamedForEachQuantifier(x, scan)), true},
		{"renamed alias without alias-aware interning", over("f", false, NamedForEachQuantifier(x, scan)), over("f", false, NamedForEachQuantifier(y, scan)), false},
		{"renamed alias with alias-aware interning", over("f", true, NamedForEachQuantifier(x, scan)), over("f", true, NamedForEachQuantifier(y, scan)), true},
		{
			"renamed alias a sibling input reads",
			over("j", true, NamedForEachQuantifier(x, scan), NamedForEachQuantifier(s, correlatedToX)),
			over("j", true, NamedForEachQuantifier(y, scan), NamedForEachQuantifier(s, correlatedToX)),
			false,
		},
		{
			"renamed alias no input reads",
			over("j", true, NamedForEachQuantifier(x, correlatedOutside), NamedForEachQuantifier(s, scan)),
			over("j", true, NamedForEachQuantifier(x, correlatedOutside), NamedForEachQuantifier(u, scan)),
			true,
		},
		{
			"renamed alias the other side reads from outside",
			&replicaTestExpr{name: "f", aliasAware: true, quantifiers: []Quantifier{NamedForEachQuantifier(y, scan)}, reads: []values.CorrelationIdentifier{y, x}},
			&replicaTestExpr{name: "f", aliasAware: true, quantifiers: []Quantifier{NamedForEachQuantifier(x, scan)}, reads: []values.CorrelationIdentifier{x, x}},
			false,
		},
		{"renamed merge alias", over("f", false, NamedForEachQuantifier(m1, scan)), over("f", false, NamedForEachQuantifier(m2, scan)), true},
		{
			"renamed merge alias beside an unrenamed table alias",
			over("j", false, NamedForEachQuantifier(m1, scan), NamedForEachQuantifier(x, sameContent)),
			over("j", false, NamedForEachQuantifier(m2, scan), NamedForEachQuantifier(x, sameContent)),
			true,
		},
		{"merge alias against a named alias", over("f", false, NamedForEachQuantifier(m1, scan)), over("f", false, NamedForEachQuantifier(x, scan)), false},
		{
			"renamed merge alias a sibling input reads",
			over("j", false, NamedForEachQuantifier(m1, scan), NamedForEachQuantifier(s, correlatedToM1)),
			over("j", false, NamedForEachQuantifier(m2, scan), NamedForEachQuantifier(s, correlatedToM1)),
			false,
		},
		{"null-on-empty differs", over("f", false, NamedForEachQuantifier(x, scan)), over("f", false, NamedForEachNullOnEmptyQuantifier(x, scan)), false},
		{
			"children as set in another order",
			asSet(over("j", false, NamedForEachQuantifier(x, scan), NamedForEachQuantifier(s, sameContent))),
			asSet(&replicaTestExpr{name: "j", quantifiers: []Quantifier{NamedForEachQuantifier(s, sameContent), NamedForEachQuantifier(x, scan)}, reads: []values.CorrelationIdentifier{x, s}}),
			true,
		},
		{
			"positional children in another order",
			over("j", false, NamedForEachQuantifier(x, scan), NamedForEachQuantifier(s, sameContent)),
			&replicaTestExpr{name: "j", quantifiers: []Quantifier{NamedForEachQuantifier(s, sameContent), NamedForEachQuantifier(x, scan)}, reads: []values.CorrelationIdentifier{x, s}},
			false,
		},
		{"leaf", &stubExpr{name: "scan"}, &stubExpr{name: "scan"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExactReplica(tc.a, tc.b); got != tc.want {
				t.Fatalf("ExactReplica=%v, want %v", got, tc.want)
			}
			if got := ExactReplica(tc.b, tc.a); got != tc.want {
				t.Fatalf("ExactReplica is not symmetric: reversed=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestAbsorbPlanningStateFoldsMatchesAndForwards(t *testing.T) {
	t.Parallel()
	survivor := InitialOf(&stubExpr{name: "a"})
	loser := InitialOf(&stubExpr{name: "b"})
	survivor.AddPartialMatch("index1", "m1")
	loser.AddPartialMatch("index2", "m2")
	loser.AddPartialMatch("index1", "m1")
	loser.AddPartialMatch("index1", "m3")
	loser.aliasAwareDedups = 2

	if added := survivor.AbsorbPlanningState(loser); added != 2 {
		t.Fatalf("new matches=%d, want 2 (m1 was already present)", added)
	}
	if got := survivor.GetPartialMatchCandidates(); !slices.Equal(got, []any{"index1", "index2"}) {
		t.Fatalf("candidates=%v, want survivor order then the loser's new candidate", got)
	}
	if got := survivor.GetAllPartialMatches(); !slices.Equal(got, []any{"m1", "m3", "m2"}) {
		t.Fatalf("matches=%v", got)
	}
	if loser.Canonical() != survivor || survivor.AliasAwareDedups() != 2 {
		t.Fatalf("loser must forward to the survivor and keep its dedup shadow: canonical=%p dedups=%d",
			loser.Canonical(), survivor.AliasAwareDedups())
	}
	if got := loser.GetAllPartialMatches(); !slices.Equal(got, []any{"m1", "m3", "m2"}) {
		t.Fatalf("a task holding the loser must read the survivor's matches, got %v", got)
	}
}

func TestRemoveExploratoryMember(t *testing.T) {
	t.Parallel()
	first, second, final := &stubExpr{name: "a"}, &stubExpr{name: "b"}, &stubExpr{name: "c"}
	ref := InitialOf(first)
	ref.Insert(second)
	ref.InsertFinal(final)
	members := ref.Members()
	version := ref.MemberVersion()
	if ref.RemoveExploratoryMember(final) {
		t.Fatal("a final member is not an exploratory member")
	}
	if !ref.RemoveExploratoryMember(second) || ref.ContainsExactly(second) {
		t.Fatal("exploratory member was not removed")
	}
	if ref.RemoveExploratoryMember(second) {
		t.Fatal("removing an absent member must report false")
	}
	if got := ref.Members(); !slices.Equal(got, []RelationalExpression{first}) || ref.MemberVersion() == version {
		t.Fatalf("members=%v version %d→%d", got, version, ref.MemberVersion())
	}
	if !slices.Equal(members, []RelationalExpression{first, second}) {
		t.Fatalf("an earlier member snapshot changed: %v", members)
	}
}

func TestMergeAliasTwinIsTheSameMember(t *testing.T) {
	t.Parallel()
	scan := InitialOf(&stubExpr{name: "scan"})
	over := func(alias values.CorrelationIdentifier) *replicaTestExpr {
		return &replicaTestExpr{name: "s", quantifiers: []Quantifier{NamedForEachQuantifier(alias, scan)}, reads: []values.CorrelationIdentifier{alias}}
	}
	first := over(values.MergeCorrelationIdentifier(1))
	twin := over(values.MergeCorrelationIdentifier(2))
	named := over(values.NamedCorrelationIdentifier("Y"))

	ref := InitialOf(first)
	if ref.Insert(twin) {
		t.Fatal("a twin differing only in a merge alias was inserted as a new member")
	}
	if !ref.Insert(named) {
		t.Fatal("a twin binding a named alias is a different member")
	}
	final := FinalOfAtStage(first, StagePlanned)
	if final.InsertFinal(twin) {
		t.Fatal("a final twin differing only in a merge alias was inserted")
	}
	if duplicate, aliasAwareOnly := PreparedMemberDuplicate([]RelationalExpression{first}, twin); !duplicate || aliasAwareOnly {
		t.Fatalf("prepared admission: duplicate=%v aliasAwareOnly=%v, want a duplicate outside the alias-aware tier", duplicate, aliasAwareOnly)
	}
	if duplicate, _ := PreparedMemberDuplicate([]RelationalExpression{first}, named); duplicate {
		t.Fatal("prepared admission deduped a named-alias twin")
	}
	if ref.AliasAwareDedups() != 0 || final.AliasAwareDedups() != 0 {
		t.Fatal("merge-alias twins are not alias-aware tier dedups")
	}
}
