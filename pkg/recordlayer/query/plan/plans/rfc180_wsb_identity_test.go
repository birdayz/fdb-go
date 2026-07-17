package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// wsbPlanBuilders builds the RFC-180 Workstream B straggler plan types from a
// Value payload — the plans whose EqualsWithoutChildren/HashCodeWithoutChildren
// still compared comparands by COUNT (or hashed Explain() text) after the F21
// migration. Same contract as rfc176PlanBuilders: every non-Value
// discriminator held fixed so identity varies only with the payload.
func wsbPlanBuilders() []rfc176PlanBuilder {
	inner := stub("Inner")
	oAlias := values.NamedCorrelationIdentifier("o")
	iAlias := values.NamedCorrelationIdentifier("i")
	predsOf := func(vs []values.Value) []predicates.QueryPredicate {
		ps := make([]predicates.QueryPredicate, len(vs))
		for i, v := range vs {
			ps[i] = &predicates.ValuePredicate{Value: v}
		}
		return ps
	}
	return []rfc176PlanBuilder{
		{"InUnion", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryInUnionPlan(inner, []string{"b1"}, vs, false)
		}},
		{"Intersection", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryIntersectionPlan([]RecordQueryPlan{inner, inner}, vs)
		}},
		{"Update", func(vs []values.Value) RecordQueryPlan {
			trs := make([]expressions.UpdateTransform, len(vs))
			for i, v := range vs {
				trs[i] = expressions.UpdateTransform{FieldPath: "F", NewValue: v}
			}
			return NewRecordQueryUpdatePlan(inner, "T", trs)
		}},
		{"FlatMap", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryFlatMapPlan(inner, inner, oAlias, iAlias, vs[0], false)
		}},
		{"NestedLoopJoin", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryNestedLoopJoinPlan(inner, inner, predsOf(vs), JoinInner, "O", "I", vs[0])
		}},
		{"Filter", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryFilterPlan(predsOf(vs), inner)
		}},
		{"PredicatesFilter", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryPredicatesFilterPlan(inner, predsOf(vs))
		}},
	}
}

// TestPlanIdentity_EqualImpliesSameHash_WSBStragglers extends the RFC-176
// equal⟹same-hash property over the Workstream B stragglers: for every pair,
// equality implies identical hash, and identical payloads rebuilt compare
// equal (non-vacuousness).
func TestPlanIdentity_EqualImpliesSameHash_WSBStragglers(t *testing.T) {
	t.Parallel()
	pool := rfc176ValuePool()
	for _, b := range wsbPlanBuilders() {
		instances := make([]RecordQueryPlan, len(pool))
		for i, vs := range pool {
			instances[i] = b.build(vs)
		}
		for i := range instances {
			for j := range instances {
				a, c := instances[i], instances[j]
				if a.EqualsWithoutChildren(c) &&
					a.HashCodeWithoutChildren() != c.HashCodeWithoutChildren() {
					t.Errorf("%s: pool[%d] vs pool[%d]: equal but hash apart — equal⟹same-hash violated",
						b.name, i, j)
				}
			}
		}
		first, dup := instances[0], instances[len(instances)-1]
		if !first.EqualsWithoutChildren(dup) {
			t.Errorf("%s: identical payloads rebuilt must compare equal", b.name)
		}
		if first.HashCodeWithoutChildren() != dup.HashCodeWithoutChildren() {
			t.Errorf("%s: identical payloads rebuilt must hash equal", b.name)
		}
	}
}

// TestInUnionPlan_ComparandsJoinIdentity pins RFC-180 B1: sibling in-union
// alternatives differing only in comparison keys (or IN-literals) must NOT
// collapse into one memo group — the survivor would not produce the ordering
// (or rows) the winner claimed. Count-only identity collapsed them; RED
// before the fix.
func TestInUnionPlan_ComparandsJoinIdentity(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	kA := []values.Value{values.NewFlatFieldValue("A", values.UnknownType)}
	kB := []values.Value{values.NewFlatFieldValue("B", values.UnknownType)}

	a := NewRecordQueryInUnionPlan(inner, []string{"b1"}, kA, false)
	b := NewRecordQueryInUnionPlan(inner, []string{"b1"}, kB, false)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("in-union plans with different comparison keys must NOT compare equal")
	}

	// Different IN-literals (Java: inSources.equals) break equality too.
	c := NewRecordQueryInUnionPlan(inner, []string{"b1"}, kA, false)
	a.SetInSources([][]any{{int64(1), int64(2)}})
	c.SetInSources([][]any{{int64(1), int64(3)}})
	if a.EqualsWithoutChildren(c) {
		t.Fatal("in-union plans with different IN-literals must NOT compare equal")
	}

	// Identical everything ⟹ equal + same hash.
	d := NewRecordQueryInUnionPlan(inner, []string{"b1"}, kA, false)
	d.SetInSources([][]any{{int64(1), int64(2)}})
	if !a.EqualsWithoutChildren(d) {
		t.Fatal("identical in-union plans must compare equal")
	}
	if a.HashCodeWithoutChildren() != d.HashCodeWithoutChildren() {
		t.Fatal("identical in-union plans must hash equal")
	}
}

// TestIntersectionPlan_KeysJoinIdentity pins RFC-180 B2: intersections with
// different comparison keys must not compare equal (length-only equality
// collapsed them), and the count-only hash violated equal⟹same-hash the
// other way for the pre-fix comparator family.
func TestIntersectionPlan_KeysJoinIdentity(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	kA := []values.Value{values.NewFlatFieldValue("A", values.UnknownType)}
	kB := []values.Value{values.NewFlatFieldValue("B", values.UnknownType)}

	a := NewRecordQueryIntersectionPlan([]RecordQueryPlan{inner, inner}, kA)
	b := NewRecordQueryIntersectionPlan([]RecordQueryPlan{inner, inner}, kB)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("intersection plans with different comparison keys must NOT compare equal")
	}
	c := NewRecordQueryIntersectionPlan([]RecordQueryPlan{inner, inner}, kA)
	if !a.EqualsWithoutChildren(c) {
		t.Fatal("identical intersection plans must compare equal")
	}
	if a.HashCodeWithoutChildren() != c.HashCodeWithoutChildren() {
		t.Fatal("identical intersection plans must hash equal")
	}
}

// TestUpdatePlan_TransformsJoinIdentity pins RFC-180 B3: `SET a=1` and
// `SET a=2` compared EQUAL under count-only transform identity — a memo
// collapse on the WRITE path executes the wrong update. RED before the fix.
func TestUpdatePlan_TransformsJoinIdentity(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	set1 := []expressions.UpdateTransform{{FieldPath: "a", NewValue: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}}}
	set2 := []expressions.UpdateTransform{{FieldPath: "a", NewValue: &values.ConstantValue{Value: int64(2), Typ: values.NotNullLong}}}
	setB := []expressions.UpdateTransform{{FieldPath: "b", NewValue: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}}}

	a := NewRecordQueryUpdatePlan(inner, "T", set1)
	if a.EqualsWithoutChildren(NewRecordQueryUpdatePlan(inner, "T", set2)) {
		t.Fatal("SET a=1 and SET a=2 must NOT compare equal")
	}
	if a.EqualsWithoutChildren(NewRecordQueryUpdatePlan(inner, "T", setB)) {
		t.Fatal("SET a=1 and SET b=1 must NOT compare equal")
	}
	c := NewRecordQueryUpdatePlan(inner, "T", set1)
	if !a.EqualsWithoutChildren(c) {
		t.Fatal("identical update plans must compare equal")
	}
	if a.HashCodeWithoutChildren() != c.HashCodeWithoutChildren() {
		t.Fatal("identical update plans must hash equal")
	}
}

// TestFlatMapAndNLJPlan_ResultValueJoinsIdentity pins RFC-180 B4: the
// resultValue (Java semanticEqualsForResults) joins identity on both
// join-shaped plans; FlatMap's inheritOuterRecordProperties flag joins in
// BOTH equals and hash (Java folds it into the hash only — an
// equal⟹same-hash violation Go does not import).
func TestFlatMapAndNLJPlan_ResultValueJoinsIdentity(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	oAlias := values.NamedCorrelationIdentifier("o")
	iAlias := values.NamedCorrelationIdentifier("i")
	rvA := values.NewFlatFieldValue("A", values.UnknownType)
	rvB := values.NewFlatFieldValue("B", values.UnknownType)

	fm := NewRecordQueryFlatMapPlan(inner, inner, oAlias, iAlias, rvA, false)
	if fm.EqualsWithoutChildren(NewRecordQueryFlatMapPlan(inner, inner, oAlias, iAlias, rvB, false)) {
		t.Fatal("flat-map plans with different result values must NOT compare equal")
	}
	if fm.EqualsWithoutChildren(NewRecordQueryFlatMapPlan(inner, inner, oAlias, iAlias, rvA, true)) {
		t.Fatal("flat-map plans with different inheritOuterRecordProperties must NOT compare equal")
	}
	fmDup := NewRecordQueryFlatMapPlan(inner, inner, oAlias, iAlias, values.NewFlatFieldValue("A", values.UnknownType), false)
	if !fm.EqualsWithoutChildren(fmDup) || fm.HashCodeWithoutChildren() != fmDup.HashCodeWithoutChildren() {
		t.Fatal("identical flat-map plans must compare equal and hash equal")
	}

	nlj := NewRecordQueryNestedLoopJoinPlan(inner, inner, nil, JoinInner, "O", "I", rvA)
	if nlj.EqualsWithoutChildren(NewRecordQueryNestedLoopJoinPlan(inner, inner, nil, JoinInner, "O", "I", rvB)) {
		t.Fatal("NLJ plans with different result values must NOT compare equal")
	}
	nljDup := NewRecordQueryNestedLoopJoinPlan(inner, inner, nil, JoinInner, "O", "I", values.NewFlatFieldValue("A", values.UnknownType))
	if !nlj.EqualsWithoutChildren(nljDup) || nlj.HashCodeWithoutChildren() != nljDup.HashCodeWithoutChildren() {
		t.Fatal("identical NLJ plans must compare equal and hash equal")
	}
}
