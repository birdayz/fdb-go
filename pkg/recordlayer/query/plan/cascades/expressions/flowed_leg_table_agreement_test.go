package expressions

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// GetFlowedObjectType MUST NOT let two members of one Reference resolve their
// LEG BOUNDARIES by whichever the memo scan reached first.
//
// `RecordType.Equals` deliberately ignores `Legs` — leg boundaries are physical
// information, not type identity — so an equality check over the member row
// types CANNOT see this disagreement. That is why the agreement scan has to ask
// about leg tables separately, and why the check has to sit BEFORE the equality
// comparison rather than after it.
//
// What a lost leg table costs is not "no legs". A row that has forgotten its
// boundaries reads downstream as ONE run spanning the whole concat keyed by the
// box's rightmost leaf, so an alias-qualified column resolves at
// runOffset+ordinal — inside the FIRST leg. Two tables with an identically named,
// identically typed column therefore return each other's values silently, and an
// EXISTS built over the wrong one inverts.
//
// THE TWO ORDERS ARE THE ENTIRE POINT. A single order proves nothing: before
// the guard, this exact pair flowed a 2-leg row when the tiling member was
// inserted first and a 0-leg row when it was not — the disagreement resolved by
// insertion order, which is the unpredictable wrong-slot read.
//
// The ruling being pinned is POPULATED-WINS: an empty table is an unstated gap,
// so the stated boundaries are adopted, and only two DIFFERENT populated tables
// conflict. Both arms are driven, because the ruling is only safe if the
// conflict arm still fires — populated-wins with no conflict check would accept
// two genuinely different tilings and pick one.
func TestGetFlowedObjectTypeRefusesDisagreeingLegTables(t *testing.T) {
	t.Parallel()

	// Both members state the SAME two-column row. They differ ONLY in whether
	// the result value states where each leg starts.
	row := func() *values.RecordType {
		return &values.RecordType{Fields: []values.Field{
			{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "K", Ordinal: 1, FieldType: values.NotNullLong},
		}}
	}

	tiling := &legSeedStubExpr{name: "tiling", typ: row(), tile: true}
	flat := &legSeedStubExpr{name: "flat", typ: row(), tile: false}

	// Establish the fixture actually produces the disagreement, or the test
	// below is asserting over two identical inputs and cannot fail.
	tiled := values.WithSeedTilingLegs(row(), tiling.GetResultValue())
	untiled := values.WithSeedTilingLegs(row(), flat.GetResultValue())
	tiledLegs := len(tiled.(*values.RecordType).Legs)
	untiledLegs := len(untiled.(*values.RecordType).Legs)
	if tiledLegs < 2 || untiledLegs != 0 {
		t.Fatalf("fixture states %d legs vs %d; want >=2 vs 0 — without a real "+
			"disagreement this test cannot detect one", tiledLegs, untiledLegs)
	}
	if !tiled.Equals(untiled) {
		t.Fatal("the two member rows are not Equals — then the ordinary equality " +
			"check would already catch this and the leg table would not be " +
			"load-bearing. The fixture must differ ONLY in Legs.")
	}

	for _, order := range []struct {
		name  string
		first *legSeedStubExpr
		then  *legSeedStubExpr
	}{
		{"tiling first", tiling, flat},
		{"flat first", flat, tiling},
	} {
		t.Run(order.name, func(t *testing.T) {
			t.Parallel()
			ref := InitialOf(order.first)
			if !ref.Insert(order.then) {
				t.Fatal("fixture failed to retain the second member")
			}
			got, err := NamedForEachQuantifier(
				values.NamedCorrelationIdentifier("Q"), ref).GetFlowedObjectType()
			if err != nil {
				t.Fatalf("populated-vs-empty leg tables must ADOPT the stated "+
					"boundaries, not decline: %v", err)
			}
			rt, ok := got.(*values.RecordType)
			if !ok {
				t.Fatalf("flowed type is %T, want *values.RecordType", got)
			}
			if len(rt.Legs) != 2 {
				t.Fatalf("flowed row states %d legs, want 2 in BOTH orders.\n"+
					"Equals ignores Legs, so nothing else in this scan can see the "+
					"difference — a row that states none does not read downstream as "+
					"\"no legs\" but as ONE run spanning the whole concat, so an "+
					"alias-qualified read of the SECOND leg lands in the first.",
					len(rt.Legs))
			}
		})
	}
}

// legSeedStubExpr is a member whose result value either DOES or does NOT state
// leg boundaries, over the same row type. tile=true builds a record constructor
// of two one-column legs, which SeedTilingLegs recognizes as a tiling; tile=false
// returns an opaque queried value, which it cannot read boundaries from.
type legSeedStubExpr struct {
	name string
	typ  *values.RecordType
	tile bool
	legs [2]string
}

func (s *legSeedStubExpr) GetResultValue() values.Value {
	if !s.tile {
		return values.NewQueriedValue(nil, s.typ)
	}
	leg := func(alias string) values.Value {
		qov, err := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier(alias),
			&values.RecordType{Fields: []values.Field{
				{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
			}})
		if err != nil {
			panic(err)
		}
		// ResolveOrdinalSeedField, not ResolveFieldOrdinals: only the seed
		// constructor stamps FrontierPinned, and the seed-window walk refuses
		// any slot that is not frontier-pinned. An ordinary semantic field
		// access here yields zero windows and the fixture silently states no
		// disagreement at all.
		field, err := values.ResolveOrdinalSeedField(qov, 0)
		if err != nil {
			panic(err)
		}
		return field
	}
	names := s.legs
	if names[0] == "" {
		names = [2]string{"FOA", "FOB"}
	}
	return values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "K", Value: leg(names[0])},
		values.RecordConstructorField{Name: "K", Value: leg(names[1])},
	)
}

func (s *legSeedStubExpr) GetQuantifiers() []Quantifier    { return nil }
func (s *legSeedStubExpr) CanCorrelate() bool              { return false }
func (s *legSeedStubExpr) ChildrenAsSet() bool             { return false }
func (s *legSeedStubExpr) HashCodeWithoutChildren() uint64 { return 0 }

func (s *legSeedStubExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *legSeedStubExpr) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*legSeedStubExpr)
	return ok && o.name == s.name
}

func (s *legSeedStubExpr) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("legSeedStubExpr", len(quantifiers), 0); err != nil {
		return nil, err
	}
	return s, nil
}

// TestGetFlowedObjectTypeRefusesConflictingLegTables is the other half of the
// populated-wins ruling, and it is what keeps that ruling honest: adopting a
// stated table is only safe while two members that state DIFFERENT tables are
// still refused. Without this arm, populated-wins would silently pick one of two
// rival tilings — the same insertion-order defect, one level down.
func TestGetFlowedObjectTypeRefusesConflictingLegTables(t *testing.T) {
	t.Parallel()

	row := func() *values.RecordType {
		return &values.RecordType{Fields: []values.Field{
			{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "K", Ordinal: 1, FieldType: values.NotNullLong},
		}}
	}
	// Same row, same two one-column legs, DIFFERENT leg aliases — so the tables
	// tile identically and disagree only on whose columns those are, which is
	// exactly the disagreement that produces a wrong-source read.
	ab := &legSeedStubExpr{name: "ab", typ: row(), tile: true, legs: [2]string{"FOA", "FOB"}}
	cd := &legSeedStubExpr{name: "cd", typ: row(), tile: true, legs: [2]string{"FOC", "FOD"}}

	left := values.WithSeedTilingLegs(row(), ab.GetResultValue()).(*values.RecordType)
	right := values.WithSeedTilingLegs(row(), cd.GetResultValue()).(*values.RecordType)
	if len(left.Legs) != 2 || len(right.Legs) != 2 {
		t.Fatalf("fixture states %d and %d legs; both must be populated or this "+
			"drives the populated-vs-empty arm instead", len(left.Legs), len(right.Legs))
	}

	ref := InitialOf(ab)
	if !ref.Insert(cd) {
		t.Fatal("fixture failed to retain the conflicting member")
	}
	got, err := NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("Q"), ref).GetFlowedObjectType()
	var disagreement *MemberResultTypeDisagreementError
	if !errors.As(err, &disagreement) {
		t.Fatalf("two members stating DIFFERENT leg tables resolved to (%v, %v), "+
			"want MemberResultTypeDisagreementError", got, err)
	}
}
