package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// POPULATING RecordType.Legs IS NOT BEHAVIOUR-NEUTRAL, AND THIS IS THE READER
// THAT PROVES IT.
//
// The argument these tests exist to refute is a real one and it is nearly right:
// `RecordType.Equals` and `Hash` IGNORE `Legs`, so attaching a leg table to a
// derived type moves no type identity — no interning key, no memo dedup, no plan
// equality. From that it is easy to conclude that populating `Legs` on a type
// that previously had none cannot change a plan.
//
// It can, and the reason survives even though the mechanism moved. Because
// `Equals` ignores `Legs`, ANY guard that cares about boundaries has to compare
// the leg tables SEPARATELY, before the equality fast path — otherwise two
// members stating the same fields under different leg tables resolve by
// whichever the memo scan reached first. That was measured: the same pair flowed
// a 2-leg row in one insertion order and a 0-leg row in the other.
//
// WHAT THESE TESTS DO AND DO NOT COVER. They drive `refineRowTypes`, which is
// NOT on the live path — `GetFlowedObjectType` carries the production guard, and
// its ruling on the populated-vs-empty pair is the OPPOSITE one: an empty table
// is an unstated gap, so the populated table is ADOPTED and only two disagreeing
// populated tables are refused (the reasoning is at that call site, including
// why declining instead lost real plans). So the arms below pin
// `refineRowTypes`'s own contract, not the planner's behaviour; the planner's is
// pinned by TestGetFlowedObjectTypeRefusesDisagreeingLegTables and
// TestGetFlowedObjectTypeRefusesConflictingLegTables, which drive both insertion
// orders.
//
// THE PRECONDITION THAT SURVIVES BOTH RULINGS. A leg table must be populated
// CONSISTENTLY across every producer of a given row — under the live rule a
// second producer is fatal when it states DIFFERENT boundaries rather than none
// — and the "Equals/Hash ignore Legs" argument establishes type identity only,
// never behaviour. Both are load-bearing for anything that proposes to make a
// previously-empty leg table non-empty.
//
// These are the committed form of that finding. Deleting them restores the
// unexamined "additive" claim.

func legFlat(name string, start, width int) values.RecordTypeLeg {
	return values.NewRecordTypeLeg(values.LegKindFlatRun,
		values.NamedCorrelationIdentifier(name), name, start, width)
}

func twoColRow(legs ...values.RecordTypeLeg) *values.RecordType {
	return &values.RecordType{
		Nullable: true,
		Fields: []values.Field{
			{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
		},
		Legs: legs,
	}
}

// The control: identical rows with NO leg table on either side refine.
func TestLegTablePopulation_EmptyBothSidesRefines(t *testing.T) {
	t.Parallel()
	if _, ok := refineRowTypes(twoColRow(), twoColRow()); !ok {
		t.Fatal("two identical leg-table-free rows failed to refine — the control for " +
			"the conflict below is broken, so the conflict proves nothing")
	}
}

// The finding: populating one side turns a refining pair into a conflict, even
// though the two types are Equal by the identity relation.
func TestLegTablePopulation_PopulatedAgainstEmptyIsAConflict(t *testing.T) {
	t.Parallel()
	empty := twoColRow()
	populated := twoColRow(legFlat("L", 0, 2))

	// The premise the "additive" argument rests on: identity is unchanged.
	if !empty.Equals(populated) {
		t.Fatal("RecordType.Equals now DISTINGUISHES a populated leg table from an " +
			"empty one.\n" +
			"  That is a bigger change than this test was written to guard: Legs would " +
			"have entered type IDENTITY, so populating it moves interning keys, memo " +
			"dedup and plan equality — not just the refinement below. Re-derive every " +
			"claim that calls a leg-table population additive.")
	}

	if _, ok := refineRowTypes(empty, populated); ok {
		t.Fatal("a POPULATED leg table refined against an EMPTY one.\n" +
			"  refineRowTypes checks legTablesAgree BEFORE the Equals fast path " +
			"precisely because Equals ignores Legs, and it treats an empty table as the " +
			"statement 'this row has no buried-leg boundaries' rather than as an " +
			"unstated gap.\n" +
			"  WHAT THIS RE-ARMS: with the conflict gone, the standing precondition on " +
			"any change that populates a previously-empty leg table — that EVERY " +
			"producer of that row must populate it consistently — is no longer enforced " +
			"here, and a half-populated row would silently resolve by whichever memo " +
			"member was scanned first. That is the pick-by-insertion-order the " +
			"refinement exists to refuse.")
	}
	// Symmetric, because a memo scan reaches the two members in either order.
	if _, ok := refineRowTypes(populated, empty); ok {
		t.Fatal("the conflict is ORDER-SENSITIVE: populated-then-empty refined while " +
			"empty-then-populated did not. A refinement whose answer depends on scan " +
			"order is the exact defect legTablesAgree is placed ahead of the fast path " +
			"to prevent.")
	}
}

// Two DIFFERENT populated tables over the same fields are also a conflict — the
// case that cannot be reached by Equals at all, and the reason the check sits
// ahead of the fast path rather than inside it.
func TestLegTablePopulation_DifferentTablesSameFieldsConflict(t *testing.T) {
	t.Parallel()
	one := twoColRow(legFlat("L", 0, 2))
	two := twoColRow(legFlat("L", 0, 1), legFlat("R", 1, 1))

	if !one.Equals(two) {
		t.Fatal("Equals distinguishes two different leg tables; see the sibling test's " +
			"message — this whole family of checks is then measuring something else")
	}
	if _, ok := refineRowTypes(one, two); ok {
		t.Fatal("two DIFFERENT leg tables over identical fields refined.\n" +
			"  They are Equal by identity, so the fast path would have returned " +
			"whichever member the memo scan reached first and the boundary-table " +
			"conflict would have been resolved by insertion order.")
	}
}
