package plans

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// rfc176ValuePool returns a set of Value payloads with distinct semantic
// identities, plus one deliberate duplicate of the first entry (the last
// element) so the equal⟹same-hash property test below is never vacuously
// true. The pool spans the discriminator axes the RFC-176 migration must
// keep: field text, resolved ordinal vs literal-'#' identifier, constant
// literals, and correlation aliases.
func rfc176ValuePool() [][]values.Value {
	return [][]values.Value{
		{values.NewFlatFieldValue("A", values.UnknownType)},
		{values.NewFlatFieldValue("B", values.UnknownType)},
		{values.NewFlatFieldValue("A", values.UnknownType), values.NewFlatFieldValue("B", values.UnknownType)},
		{values.NewFieldValueWithResolvedOrdinal("X", 0, values.UnknownType)},
		{values.NewFieldValueWithResolvedOrdinal("X", 1, values.UnknownType)},
		{values.NewFlatFieldValue("X#0", values.UnknownType)},
		{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
		{&values.ConstantValue{Value: int64(2), Typ: values.NotNullLong}},
		{values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q1"))},
		{values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q2"))},
		// Duplicate of entry 0 — guarantees at least one genuinely equal,
		// non-identical pair per plan type.
		{values.NewFlatFieldValue("A", values.UnknownType)},
	}
}

// rfc176PlanBuilder builds one of the ten RFC-176 P2 plan types from a Value
// payload, holding every non-Value discriminator (flags, counts, orderings)
// fixed so identity varies only with the Values.
type rfc176PlanBuilder struct {
	name  string
	build func(vs []values.Value) RecordQueryPlan
}

func rfc176PlanBuilders() []rfc176PlanBuilder {
	inner := stub("Inner")
	return []rfc176PlanBuilder{
		{"MultiIntersection", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{inner, inner}, vs, vs[0])
		}},
		{"Projection", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryProjectionPlan(vs, inner)
		}},
		// NOTE: the sort operator is intentionally absent here. The RFC-176 P2
		// semantic-Value-identity plan was RecordQuerySortPlan, which is now
		// removed as producer-less dead code. The live sort plan,
		// RecordQueryInMemorySortPlan, deliberately does NOT participate in
		// RFC-176 P2: its EqualsWithoutChildren compares SortKey structs by
		// identity (ValueExpr pointer) and its hash folds Field+Desc only — so
		// it cannot satisfy this suite's "identical payloads rebuilt compare
		// equal" guard (distinct-but-semantically-equal Value instances).
		{"StreamingAggregation", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryStreamingAggregationPlan(inner, vs,
				[]expressions.AggregateSpec{{Function: expressions.AggCount, Operand: vs[0]}})
		}},
		{"Values", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryValuesPlan(vs)
		}},
		{"Map", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryMapPlan(inner, vs[0])
		}},
		{"DefaultOnEmpty", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryDefaultOnEmptyPlan(inner, vs[0])
		}},
		{"FirstOrDefault", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryFirstOrDefaultPlan(inner, vs[0])
		}},
		{"Comparator", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryComparatorPlan([]RecordQueryPlan{inner, inner}, vs, 0, false, false)
		}},
		{"MergeSortUnion", func(vs []values.Value) RecordQueryPlan {
			return NewRecordQueryMergeSortUnionPlan([]RecordQueryPlan{inner, inner}, vs, false, true)
		}},
	}
}

// TestPlanIdentity_EqualImpliesSameHash_AllNine_RFC176 is the plan-level
// equal⟹same-hash property test over all nine plan types migrated to semantic
// Value identity (RFC-176 P2): for every pair of instances of each type,
// EqualsWithoutChildren(a, b) implies identical HashCodeWithoutChildren. A
// violation means a hash-first memo lookup can miss an "equal" existing
// member and insert a duplicate — the defect class RFC-176 §1 documents,
// live on pre-P2 comparator/merge-sort-union (equality on key COUNT only,
// hash on the full key renderings).
func TestPlanIdentity_EqualImpliesSameHash_AllNine_RFC176(t *testing.T) {
	t.Parallel()
	pool := rfc176ValuePool()
	for _, b := range rfc176PlanBuilders() {
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
		// Non-vacuousness guard: pool[0] and the trailing duplicate are the
		// same payload rebuilt — they MUST compare equal and hash equal.
		first, dup := instances[0], instances[len(instances)-1]
		if !first.EqualsWithoutChildren(dup) {
			t.Errorf("%s: identical payloads rebuilt must compare equal", b.name)
		}
		if first.HashCodeWithoutChildren() != dup.HashCodeWithoutChildren() {
			t.Errorf("%s: identical payloads rebuilt must hash equal", b.name)
		}
	}
}

// TestComparatorPlan_KeysJoinIdentity_RFC176 pins the RFC-176 §1 comparator
// defect: equality compared only the comparison-key COUNT while the hash
// folded the full key renderings, so two plans with equal key counts but
// different keys compared EQUAL yet hashed APART — a live plan-level
// equal⟹same-hash violation. The comparison keys join equality (semantic
// Value identity); this test is RED on pre-P2 code.
func TestComparatorPlan_KeysJoinIdentity_RFC176(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	kA := []values.Value{values.NewFlatFieldValue("A", values.UnknownType)}
	kB := []values.Value{values.NewFlatFieldValue("B", values.UnknownType)}

	a := NewRecordQueryComparatorPlan([]RecordQueryPlan{inner, inner}, kA, 0, false, false)
	b := NewRecordQueryComparatorPlan([]RecordQueryPlan{inner, inner}, kB, 0, false, false)
	if a.EqualsWithoutChildren(b) {
		t.Fatalf("comparator plans with different comparison keys must NOT compare equal "+
			"(key-count-only equality + rendering-keyed hash = equal⟹same-hash violation): hashes %#x vs %#x",
			a.HashCodeWithoutChildren(), b.HashCodeWithoutChildren())
	}

	// Genuinely equal plans (same keys, ref index, reverse) stay equal and
	// hash equal; the non-Value discriminators still break equality.
	c := NewRecordQueryComparatorPlan([]RecordQueryPlan{inner, inner}, kA, 0, false, false)
	if !a.EqualsWithoutChildren(c) {
		t.Fatal("identical comparator plans must compare equal")
	}
	if a.HashCodeWithoutChildren() != c.HashCodeWithoutChildren() {
		t.Fatal("identical comparator plans must hash equal")
	}
	if a.EqualsWithoutChildren(NewRecordQueryComparatorPlan([]RecordQueryPlan{inner, inner}, kA, 1, false, false)) {
		t.Fatal("different reference plan index must break equality")
	}
	if a.EqualsWithoutChildren(NewRecordQueryComparatorPlan([]RecordQueryPlan{inner, inner}, kA, 0, true, false)) {
		t.Fatal("different reverse flag must break equality")
	}
}

// TestMergeSortUnionPlan_KeysJoinIdentity_RFC176 pins the same RFC-176 §1
// defect for merge-sort-union: equality was reverse + removeDuplicates + key
// COUNT while the hash folded the full key renderings — different-key plans
// compared equal yet hashed apart. RED on pre-P2 code.
func TestMergeSortUnionPlan_KeysJoinIdentity_RFC176(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	kA := []values.Value{values.NewFlatFieldValue("A", values.UnknownType)}
	kB := []values.Value{values.NewFlatFieldValue("B", values.UnknownType)}

	a := NewRecordQueryMergeSortUnionPlan([]RecordQueryPlan{inner, inner}, kA, false, true)
	b := NewRecordQueryMergeSortUnionPlan([]RecordQueryPlan{inner, inner}, kB, false, true)
	if a.EqualsWithoutChildren(b) {
		t.Fatalf("merge-sort-union plans with different comparison keys must NOT compare equal "+
			"(key-count-only equality + rendering-keyed hash = equal⟹same-hash violation): hashes %#x vs %#x",
			a.HashCodeWithoutChildren(), b.HashCodeWithoutChildren())
	}

	c := NewRecordQueryMergeSortUnionPlan([]RecordQueryPlan{inner, inner}, kA, false, true)
	if !a.EqualsWithoutChildren(c) {
		t.Fatal("identical merge-sort-union plans must compare equal")
	}
	if a.HashCodeWithoutChildren() != c.HashCodeWithoutChildren() {
		t.Fatal("identical merge-sort-union plans must hash equal")
	}
	if a.EqualsWithoutChildren(NewRecordQueryMergeSortUnionPlan([]RecordQueryPlan{inner, inner}, kA, true, true)) {
		t.Fatal("different reverse flag must break equality")
	}
	if a.EqualsWithoutChildren(NewRecordQueryMergeSortUnionPlan([]RecordQueryPlan{inner, inner}, kA, false, false)) {
		t.Fatal("different removeDuplicates flag must break equality")
	}
}

// TestPlanIdentity_SemanticHashAliasInvariant_RFC176 pins the alias contract
// of the migrated hash (RFC-176 P2 gate): HashCodeWithoutChildren folds
// values.SemanticHashCode, which is alias-INVARIANT, while equality is
// alias-map-relative (the plan-level sites pass the EMPTY map, so identical
// aliases compare equal and renamed aliases do not). Alias-renamed twins must
// therefore share a hash bucket — hash-first memo lookup under ANY alias map
// would otherwise miss members that a non-empty map deems equal — while
// empty-map equality still separates them.
func TestPlanIdentity_SemanticHashAliasInvariant_RFC176(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	q1 := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q1"))
	q2 := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q2"))

	// Consistency precondition: the two reads ARE semantically equal under
	// {q1→q2} — the map-relative equality the alias-invariant hash serves.
	if !values.SemanticEqualsUnderAliasMap(q1, q2, values.AliasMap{
		values.NamedCorrelationIdentifier("q1"): values.NamedCorrelationIdentifier("q2"),
	}) {
		t.Fatal("q1 and q2 reads must be semantically equal under the map {q1→q2}")
	}

	type pair struct {
		name string
		a, b RecordQueryPlan
	}
	pairs := []pair{
		{"Map", NewRecordQueryMapPlan(inner, q1), NewRecordQueryMapPlan(inner, q2)},
		{
			"Projection", NewRecordQueryProjectionPlan([]values.Value{q1}, inner),
			NewRecordQueryProjectionPlan([]values.Value{q2}, inner),
		},
	}
	for _, p := range pairs {
		if p.a.HashCodeWithoutChildren() != p.b.HashCodeWithoutChildren() {
			t.Errorf("%s: hash must be alias-invariant — alias-renamed twins must share a hash bucket (%#x vs %#x)",
				p.name, p.a.HashCodeWithoutChildren(), p.b.HashCodeWithoutChildren())
		}
		if p.a.EqualsWithoutChildren(p.b) {
			t.Errorf("%s: equality under the EMPTY alias map is alias-literal — q1 vs q2 must NOT compare equal", p.name)
		}
	}
}

// TestPlanIdentity_NilValueArms_RFC176 pins the nil-Value handling of the
// migrated identity: nil result/default Values compare equal to nil, unequal
// to non-nil, and hash deterministically — no panics on any arm.
func TestPlanIdentity_NilValueArms_RFC176(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	fv := values.NewFlatFieldValue("A", values.UnknownType)

	type pair struct {
		name             string
		bothNil, oneNil  RecordQueryPlan
		bothNilB, nonNil RecordQueryPlan
	}
	pairs := []pair{
		{
			"MultiIntersection",
			NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{inner}, nil, nil),
			NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{inner}, nil, fv),
			NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{inner}, nil, nil),
			NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{inner}, nil, fv),
		},
		{
			"Map",
			NewRecordQueryMapPlan(inner, nil),
			NewRecordQueryMapPlan(inner, fv),
			NewRecordQueryMapPlan(inner, nil),
			NewRecordQueryMapPlan(inner, fv),
		},
		{
			"DefaultOnEmpty",
			NewRecordQueryDefaultOnEmptyPlan(inner, nil),
			NewRecordQueryDefaultOnEmptyPlan(inner, fv),
			NewRecordQueryDefaultOnEmptyPlan(inner, nil),
			NewRecordQueryDefaultOnEmptyPlan(inner, fv),
		},
		{
			"FirstOrDefault",
			NewRecordQueryFirstOrDefaultPlan(inner, nil),
			NewRecordQueryFirstOrDefaultPlan(inner, fv),
			NewRecordQueryFirstOrDefaultPlan(inner, nil),
			NewRecordQueryFirstOrDefaultPlan(inner, fv),
		},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			if !p.bothNil.EqualsWithoutChildren(p.bothNilB) {
				t.Error("nil Value vs nil Value must compare equal")
			}
			if p.bothNil.HashCodeWithoutChildren() != p.bothNilB.HashCodeWithoutChildren() {
				t.Error("nil Value vs nil Value must hash equal")
			}
			if p.bothNil.EqualsWithoutChildren(p.oneNil) {
				t.Error("nil Value vs non-nil Value must NOT compare equal")
			}
			if !p.oneNil.EqualsWithoutChildren(p.nonNil) {
				t.Error("same non-nil Value must compare equal")
			}
			if p.oneNil.HashCodeWithoutChildren() != p.nonNil.HashCodeWithoutChildren() {
				t.Error("same non-nil Value must hash equal")
			}
		})
	}
}

// TestPlanIdentity_VectorLiteralConstant_RFC176 pins slice-carrying constant
// literals in plan identity (PR #453 review): a projection over a vector
// literal (ConstantValue carrying []float64 / []float32 — the slice types
// the executor's evalFloat64Slice accepts beyond []any) must compare by
// ELEMENT-WISE equality, never Go's `==` on interfaces — which PANICS at
// runtime for non-comparable dynamic types. The pre-RFC-176 rendering
// compare never reached `==` for them (valueLiteralString rendered unknown
// slices as "?", which ALSO collapsed different vectors to one identity);
// the semantic migration made the panic reachable (projection
// EqualsWithoutChildren → values.EqualsWithoutChildren →
// constantValuesEqual). RED (panic) on the pre-fix commit.
func TestPlanIdentity_VectorLiteralConstant_RFC176(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	mk := func(v any) RecordQueryPlan {
		return NewRecordQueryProjectionPlan(
			[]values.Value{&values.ConstantValue{Value: v, Typ: values.UnknownType}}, inner)
	}

	// Equal-content DISTINCT slice instances: equal, no panic, hash coherent.
	a, b := mk([]float64{1, 0, 0}), mk([]float64{1, 0, 0})
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("equal-content []float64 literals must compare equal")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("equal-content []float64 literals must hash equal (equal⟹same-hash)")
	}
	// Different content: unequal AND hash-discriminated (the "?"-rendering
	// era collapsed different vectors to one identity).
	c := mk([]float64{0, 1, 0})
	if a.EqualsWithoutChildren(c) {
		t.Fatal("different []float64 literals must NOT compare equal")
	}
	if a.HashCodeWithoutChildren() == c.HashCodeWithoutChildren() {
		t.Fatal("different []float64 literals must not share a hash")
	}
	if a.EqualsWithoutChildren(mk([]float64{1, 0})) {
		t.Fatal("different-length []float64 literals must NOT compare equal")
	}

	// []float32 twin.
	f1, f2 := mk([]float32{1, 2}), mk([]float32{1, 2})
	if !f1.EqualsWithoutChildren(f2) {
		t.Fatal("equal-content []float32 literals must compare equal")
	}
	if f1.HashCodeWithoutChildren() != f2.HashCodeWithoutChildren() {
		t.Fatal("equal-content []float32 literals must hash equal")
	}
	if f1.EqualsWithoutChildren(mk([]float32{1, 3})) {
		t.Fatal("different []float32 literals must NOT compare equal")
	}

	// Cross-element-type: same numbers, different slice type — distinct
	// literals (a half-precision vector is not a double vector).
	if mk([]float64{1, 2}).EqualsWithoutChildren(mk([]float32{1, 2})) {
		t.Fatal("[]float64 vs []float32 literals must NOT compare equal")
	}

	// nil vs empty: deliberately EQUAL (len-based, the same choice as the
	// []byte arm — no caller distinguishes a missing vector from a
	// zero-length one) and hash-coherent.
	n, e := mk([]float64(nil)), mk([]float64{})
	if !n.EqualsWithoutChildren(e) {
		t.Fatal("nil and empty []float64 literals compare equal (len-based, matches the []byte arm)")
	}
	if n.HashCodeWithoutChildren() != e.HashCodeWithoutChildren() {
		t.Fatal("nil and empty []float64 literals must hash equal (equal⟹same-hash)")
	}

	// Signed zero: float == calls 0.0 equal to -0.0 while the %v hash renders
	// "[0]" vs "[-0]" — equal-but-different-hash, the invariant violation this
	// RFC exists to kill. Elements therefore compare BITWISE: signed zeros are
	// UNEQUAL (equality strictly finer than the hash — a refinement split,
	// never a wrong unification). RED under float == on the pre-fix commit.
	z, nz := mk([]float64{0}), mk([]float64{math.Copysign(0, -1)})
	if z.EqualsWithoutChildren(nz) {
		t.Fatal("+0 and -0 vector literals must NOT compare equal (bitwise element identity)")
	}
	// Identical-bit NaNs: EQUAL under bitwise identity (float == would call
	// them unequal) and hash-coherent (%v renders both as NaN).
	nan1, nan2 := mk([]float64{math.NaN()}), mk([]float64{math.NaN()})
	if !nan1.EqualsWithoutChildren(nan2) {
		t.Fatal("identical-bit NaN vector literals must compare equal (bitwise element identity)")
	}
	if nan1.HashCodeWithoutChildren() != nan2.HashCodeWithoutChildren() {
		t.Fatal("equal NaN vector literals must hash equal (equal⟹same-hash)")
	}
}
