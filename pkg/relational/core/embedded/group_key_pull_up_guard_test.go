package embedded

// The post-aggregate PULL-UP guard: a reference that answers to MORE THAN ONE
// grouping key does not determine a slot, so the engine stops instead of binding
// the first match.
//
// This is Java's structure, not an invention. Expressions.pullUp asserts
// `pulledUpExpressionMap.get(subExpression).size() == 1` with AMBIGUOUS_COLUMN
// before taking the element (Expressions.java:112 at 4.12.11.0); its HAVING twin
// Expression.pullUp ends in a bare Iterables.getOnlyElement (Expression.java:246)
// which throws on more than one without that SQLSTATE. Both guard LOCALLY at the
// pull-up and delegate to no upstream duplicate-key gate. Go raises 42702 on both
// halves — the deliberate refinement is spending the precise code where Java
// throws unclassified.
//
// WHY THESE ARE UNIT PINS. The three guarded sites are NOT equally reachable
// from SQL, and the corpus reading alone would ship one of them untested:
//
//   - the FieldValue rebase walk (rebasePostAggregateGroupKeyValue) is driven
//     end-to-end by TestFDB_ComputedGroupKeyRereadBindsItsOwnSlot's
//     under_a_join_… arm;
//   - the exact-boundary binder (bindPostAggregateValueToNativeOrdinals) is
//     driven by TestFDB_OrderedGroupedScalarSubquery_QualifiedJoinKeyIdentity's
//     repeated_equivalent_single_source_keys_are_refused_42702 arm;
//   - the COMPUTED walk (rebasePostAggregateComputedGroupKey) is reachable from
//     no SQL shape in the corpus. Measured: disabling its `> 1` test leaves the
//     whole //pkg/relational/sqldriver target green at 6158 subtests. The
//     duplicate gate refuses the byte-identical computed twin before the walk
//     runs, and the parenthesised twin — the one spelling that slips the gate —
//     yields keys [RecordConstructorValue, ArithmeticValue], which a reference
//     matches exactly ONCE. So it is a ported assert with no live caller, and
//     without the arm below it would be an untested branch that a later
//     normalization change makes live.
//
// The population is stated deliberately: the 6158-subtest reading is the
// //pkg/relational/sqldriver target only. The yamsql corpus and
// //pkg/relational/core/embedded were NOT instrumented for reachability.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// gpugAgg builds an aggregate whose group keys are exactly the given values.
func gpugAgg(keys ...values.Value) *logical.LogicalAggregate {
	gks := make([]logical.GroupKey, 0, len(keys))
	for _, k := range keys {
		gks = append(gks, logical.GroupKey{Value: k, Display: k.Name()})
	}
	return &logical.LogicalAggregate{Input: logical.NewScan("t", ""), GroupKeys: gks}
}

// gpugComputed is a NON-FieldValue grouping key — the node kind the computed
// walk owns. Two separately-built copies are semantically equal, which is what
// makes a single reference answer to both.
func gpugComputed(col string) values.Value {
	return &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.FieldValue{Field: col, Typ: values.UnknownType},
		Right: values.LiteralValue(int64(1)),
	}
}

// TestGroupKeyPullUpGuard_ComputedWalkRefusesAMultiMatch drives the arm the SQL
// corpus cannot reach. Two semantically-equal computed keys, one reference: the
// walk must decline to bind and record the ambiguity rather than take slot 0.
func TestGroupKeyPullUpGuard_ComputedWalkRefusesAMultiMatch(t *testing.T) {
	t.Parallel()

	agg := gpugAgg(gpugComputed("C1"), gpugComputed("C1"))
	var amb groupKeyPullUpAmbiguity
	out := rebasePostAggregateComputedGroupKey(gpugComputed("C1"), agg, &amb)

	if !amb.hit {
		t.Fatalf("two semantically equal computed group keys did not record an "+
			"ambiguity: the computed pull-up walk bound a reference that answers "+
			"to BOTH keys, which is the first-match Java refuses "+
			"(Expressions.java:112). got out=%T", out)
	}
	err := amb.err()
	if err == nil {
		t.Fatal("ambiguity recorded but err() returned nil — the verdict cannot reach a caller")
	}
	if !strings.Contains(err.Error(), "42702") {
		t.Errorf("got %v, want SQLSTATE 42702 (AMBIGUOUS_COLUMN)", err)
	}
	// The VALUE, not only the relationship: a guard that fires with an empty or
	// wrong column name is indistinguishable from one that fires correctly if
	// only the SQLSTATE is asserted.
	if !strings.Contains(err.Error(), "C1") {
		t.Errorf("got %v, want the message to name the duplicated key C1", err)
	}
	// The node must be left ALONE on a decline. Returning a bound ordinal here
	// would mean the guard recorded the ambiguity and then answered anyway.
	if fv, bound := out.(*values.FieldValue); bound {
		t.Errorf("declined reference was still rewritten to %v — on an ambiguity "+
			"the walk must return the node untouched", fv)
	}
}

// TestGroupKeyPullUpGuard_ComputedWalkStillBindsAUniqueMatch is the control that
// keeps the guard from passing by refusing everything. One matching key, and the
// reference must bind to ITS slot — asserted as a VALUE (slot 1, not 0), because
// a walk that always answered slot 0 would satisfy a mere "it bound something".
func TestGroupKeyPullUpGuard_ComputedWalkStillBindsAUniqueMatch(t *testing.T) {
	t.Parallel()

	agg := gpugAgg(gpugComputed("OTHER"), gpugComputed("C1"))
	var amb groupKeyPullUpAmbiguity
	out := rebasePostAggregateComputedGroupKey(gpugComputed("C1"), agg, &amb)

	if amb.hit {
		t.Fatalf("a UNIQUE match was reported ambiguous (%v) — the guard is "+
			"counting keys it did not match", amb.err())
	}
	fv, bound := out.(*values.FieldValue)
	if !bound {
		t.Fatalf("a unique computed-key match did not bind: got %T", out)
	}
	if fv.Resolved == nil || fv.Resolved.Last().Ordinal != 1 {
		t.Errorf("bound to %v, want the aggregate output slot 1 — the key's index "+
			"in GroupKeys IS its slot (the row is [keys..., aggregates...]), which "+
			"is the loop index Java pulls up by "+
			"(CompensateRecordConstructorRule.java:88-92)", fv.Resolved)
	}
}

// TestGroupKeyPullUpGuard_RecordsOnlyTheFirstVerdict pins the collector's own
// contract: the verdict is one fact per statement however many references share
// it, matching Java's message naming a single sub-expression.
func TestGroupKeyPullUpGuard_RecordsOnlyTheFirstVerdict(t *testing.T) {
	t.Parallel()

	var amb groupKeyPullUpAmbiguity
	if err := amb.err(); err != nil {
		t.Fatalf("a collector with no ambiguity must yield no error, got %v", err)
	}
	amb.record("FIRST")
	amb.record("SECOND")
	err := amb.err()
	if err == nil {
		t.Fatal("record() did not arm the collector")
	}
	if !strings.Contains(err.Error(), "FIRST") || strings.Contains(err.Error(), "SECOND") {
		t.Errorf("got %v, want the FIRST recorded name only — a collector that "+
			"overwrites reports whichever reference the walk happened to visit last",
			err)
	}
}
