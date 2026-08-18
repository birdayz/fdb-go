package expressions

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type preparedAliasAwareTestExpression struct {
	quantifier Quantifier
}

func (e *preparedAliasAwareTestExpression) GetResultValue() values.Value {
	return values.NewQueriedValue(nil, values.NotNullLong)
}

func (e *preparedAliasAwareTestExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.quantifier}
}
func (*preparedAliasAwareTestExpression) CanCorrelate() bool  { return false }
func (*preparedAliasAwareTestExpression) ChildrenAsSet() bool { return false }
func (*preparedAliasAwareTestExpression) HashCodeWithoutChildren() uint64 {
	return 0x232
}

func (*preparedAliasAwareTestExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (e *preparedAliasAwareTestExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*preparedAliasAwareTestExpression)
	if !ok {
		return false
	}
	if e.quantifier.GetAlias() == o.quantifier.GetAlias() {
		return true
	}
	if aliases == nil {
		return false
	}
	target, mapped := aliases.GetTarget(e.quantifier.GetAlias())
	return mapped && target == o.quantifier.GetAlias()
}

func (e *preparedAliasAwareTestExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("preparedAliasAwareTestExpression", len(quantifiers), 1); err != nil {
		return nil, err
	}
	return &preparedAliasAwareTestExpression{quantifier: quantifiers[0]}, nil
}
func (*preparedAliasAwareTestExpression) InternsAliasAware() bool { return true }

func TestReference_InitialOf_SingleMember(t *testing.T) {
	t.Parallel()
	e := &stubExpr{name: "x"}
	r := InitialOf(e)
	if r.Get() != e {
		t.Fatal("Get returned wrong member")
	}
	if got := r.Members(); len(got) != 1 || got[0] != e {
		t.Fatalf("members=%v, want [%v]", got, e)
	}
}

func TestReference_Insert_Dedup(t *testing.T) {
	t.Parallel()
	a := &stubExpr{name: "T"}
	b := &stubExpr{name: "T"} // structurally equal to a
	r := InitialOf(a)
	if inserted := r.Insert(b); inserted {
		t.Fatal("inserted a structurally-equal duplicate")
	}
	if len(r.Members()) != 1 {
		t.Fatalf("members size=%d after dup-insert, want 1", len(r.Members()))
	}
}

func TestReference_Insert_Distinct(t *testing.T) {
	t.Parallel()
	a := &stubExpr{name: "T"}
	c := &stubExpr{name: "U"} // structurally DIFFERENT
	r := InitialOf(a)
	if inserted := r.Insert(c); !inserted {
		t.Fatal("failed to insert structurally-different expression")
	}
	if len(r.Members()) != 2 {
		t.Fatalf("members size=%d after distinct insert, want 2", len(r.Members()))
	}
}

func TestReference_Get_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	r := &Reference{}
	if r.Get() != nil {
		t.Fatal("empty reference Get should return nil")
	}
}

func TestReference_Insert_SemanticEqualsFallback(t *testing.T) {
	t.Parallel()
	// Build two LogicalDistinct expressions with DIFFERENT inner
	// References pointing at structurally-equivalent Scans:
	//   d1 = Distinct(R1 → Scan(T))
	//   d2 = Distinct(R2 → Scan(T))   // different Reference pointer
	// sameChildReferences(d1, d2) returns false (R1 != R2), but
	// SemanticEquals(d1, d2, EmptyAliasMap) returns true (both Distinct
	// over structurally-equivalent Scans).
	//
	// Reference.Insert should treat them as duplicates via the
	// SemanticEquals fallback. The previous pointer-only contract
	// would have inserted both — this test pins the post-680e664a
	// behavior that the SemanticEquals fallback dedupes them.
	r1 := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	r2 := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	q1 := ForEachQuantifier(r1)
	q2 := ForEachQuantifier(r2)
	d1 := mustExpression(NewLogicalDistinctExpression(q1))
	d2 := mustExpression(NewLogicalDistinctExpression(q2))
	ref := InitialOf(d1)
	if inserted := ref.Insert(d2); inserted {
		t.Fatalf("Insert(d2) returned true — SemanticEquals fallback should have dedupd against d1")
	}
	if got := len(ref.Members()); got != 1 {
		t.Fatalf("Reference grew to %d members despite SemanticEquals dedup", got)
	}
}

func TestReference_Insert_PanicsOnNil(t *testing.T) {
	t.Parallel()
	r := InitialOf(&stubExpr{name: "X"})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on Insert(nil)")
		}
	}()
	r.Insert(nil)
}

func TestReference_InsertFinal_AddsToFinalMembers(t *testing.T) {
	t.Parallel()
	r := InitialOf(&stubExpr{name: "logical"})

	final := &stubExpr{name: "physical"}
	ok := r.InsertFinal(final)
	if !ok {
		t.Fatal("InsertFinal returned false for new expression")
	}
	if len(r.FinalMembers()) != 1 || r.FinalMembers()[0] != final {
		t.Fatalf("FinalMembers=%v, want [%v]", r.FinalMembers(), final)
	}
	if len(r.AllMembers()) != 2 {
		t.Fatalf("AllMembers should have 2 (logical + final), got %d", len(r.AllMembers()))
	}
}

func TestReference_InsertFinal_Dedup(t *testing.T) {
	t.Parallel()
	r := &Reference{}
	e := &stubExpr{name: "x"}
	r.InsertFinal(e)
	ok := r.InsertFinal(e)
	if ok {
		t.Fatal("InsertFinal should return false for duplicate")
	}
	if len(r.FinalMembers()) != 1 {
		t.Fatalf("expected 1 final member after dedup, got %d", len(r.FinalMembers()))
	}
}

func TestReference_InsertFinal_NotInExploratoryMembers(t *testing.T) {
	t.Parallel()
	r := &Reference{}
	e := &stubExpr{name: "a"}
	r.InsertFinal(e)
	if len(r.Members()) != 0 {
		t.Fatalf("InsertFinal should NOT add to exploratory Members, got %v", r.Members())
	}
	if len(r.FinalMembers()) != 1 || r.FinalMembers()[0] != e {
		t.Fatalf("InsertFinal should add to FinalMembers, got %v", r.FinalMembers())
	}
	if len(r.AllMembers()) != 1 || r.AllMembers()[0] != e {
		t.Fatalf("AllMembers should include final, got %v", r.AllMembers())
	}
}

func TestReference_FinalMembers_EmptyByDefault(t *testing.T) {
	t.Parallel()
	r := InitialOf(&stubExpr{name: "x"})
	if len(r.FinalMembers()) != 0 {
		t.Fatalf("FinalMembers should be empty by default, got %d", len(r.FinalMembers()))
	}
}

func TestReference_InsertFinal_NilPanics(t *testing.T) {
	t.Parallel()
	r := &Reference{}
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on InsertFinal(nil)")
		}
	}()
	r.InsertFinal(nil)
}

func TestReferencePreparedApplyAndReadsAreDefensive(t *testing.T) {
	t.Parallel()
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))
	finalScan := mustExpression(NewFullUnorderedScanExpression([]string{"FINAL"}, values.NotNullLong))
	reference := &Reference{}
	view := reference.AdmissionView()
	relation, err := values.ExactRelationOf(scan.GetResultValue().Type())
	if err != nil {
		t.Fatalf("ExactRelationOf: %v", err)
	}
	incoming := []RelationalExpression{scan}
	finalIncoming := []RelationalExpression{finalScan}
	if err := reference.ApplyPreparedMemberBatch(view, relation, incoming, finalIncoming, 0); err != nil {
		t.Fatalf("ApplyPreparedMemberBatch: %v", err)
	}

	// Mutating every caller/read slice must not rewrite Reference storage.
	incoming[0] = nil
	finalIncoming[0] = nil
	members := reference.Members()
	if len(members) != 1 || members[0] != scan {
		t.Fatalf("stored members = %v, want admitted scan", members)
	}
	finalMembers := reference.FinalMembers()
	if len(finalMembers) != 1 || finalMembers[0] != finalScan {
		t.Fatalf("stored final members = %v, want admitted final scan", finalMembers)
	}
	members[0] = nil
	finalMembers[0] = nil
	all := reference.AllMembers()
	all[0] = nil
	all[1] = nil
	admissionMembers := reference.AdmissionView().Members(ReferenceExploratoryMembers)
	admissionMembers[0] = nil
	admissionFinals := reference.AdmissionView().Members(ReferenceFinalMembers)
	admissionFinals[0] = nil
	if got, gotFinal := reference.Get(), reference.FinalMembers()[0]; got != scan || gotFinal != finalScan {
		t.Fatalf("caller mutated Reference members through a returned slice: got exploratory/final %v/%v", got, gotFinal)
	}

	resultType, err := reference.ResultType()
	if err != nil {
		t.Fatalf("ResultType: %v", err)
	}
	want := values.NewRelationType(values.NotNullLong)
	if !resultType.Equals(want) {
		t.Fatalf("ResultType = %v, want %v", resultType, want)
	}
	// ResultType hands back the SHARED thawed graph. Mutating it here would write
	// through to an INTERNED handle — corrupting every other value flowing
	// RELATION<LONG NOT NULL>, including in tests running in parallel — so what is
	// asserted is the sharing itself, which nothing else in this file observes and
	// which a reintroduced defensive copy would silently undo. See RFC-234.
	again, err := reference.ResultType()
	if err != nil || !again.Equals(want) {
		t.Fatalf("ResultType is not stable across reads: (%v, %v)", again, err)
	}
	if resultType != again {
		t.Fatalf("ResultType returned two graphs (%p, %p); the defensive copy is back",
			resultType, again)
	}
}

func TestReferencePreparedApplyRejectsBeforeMutation(t *testing.T) {
	t.Parallel()
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))
	reference := &Reference{}
	view := reference.AdmissionView()
	notRelation, err := values.SnapshotExactType(values.NotNullLong)
	if err != nil {
		t.Fatalf("SnapshotExactType: %v", err)
	}
	err = reference.ApplyPreparedMemberBatch(view, notRelation, []RelationalExpression{scan}, nil, 0)
	var coded interface {
		Code() values.ResolutionErrorCode
	}
	if !errors.As(err, &coded) || coded.Code() != values.MemoMissingRelationWrapper {
		t.Fatalf("ApplyPreparedMemberBatch error = %v, want MemoMissingRelationWrapper", err)
	}
	if len(reference.AllMembers()) != 0 {
		t.Fatalf("failed prepared apply published members: %v", reference.AllMembers())
	}
}

func TestReferencePreparedApplyRejectsLateInvalidMemberBeforeMutation(t *testing.T) {
	t.Parallel()
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, values.NotNullLong))
	reference := &Reference{}
	view := reference.AdmissionView()
	relation, err := values.ExactRelationOf(values.NotNullLong)
	if err != nil {
		t.Fatalf("ExactRelationOf: %v", err)
	}
	err = reference.ApplyPreparedMemberBatch(view, relation, []RelationalExpression{scan}, []RelationalExpression{nil}, 0)
	var coded interface {
		Code() values.ResolutionErrorCode
	}
	if !errors.As(err, &coded) || coded.Code() != values.MemoUnsupportedExpression {
		t.Fatalf("ApplyPreparedMemberBatch error = %v, want MemoUnsupportedExpression", err)
	}
	if len(reference.AllMembers()) != 0 {
		t.Fatalf("late-invalid prepared apply published members: %v", reference.AllMembers())
	}
	// Reusing the original view proves the failed apply changed neither member
	// storage nor the version checked by a subsequent valid commit.
	if err := reference.ApplyPreparedMemberBatch(view, relation, []RelationalExpression{scan}, nil, 0); err != nil {
		t.Fatalf("valid apply after late rejection: %v", err)
	}
	if got := reference.Members(); len(got) != 1 || got[0] != scan {
		t.Fatalf("valid apply after late rejection stored %v, want admitted scan", got)
	}
}

func TestReferenceRejectedPreparedApplyDoesNotCompressForwardingChain(t *testing.T) {
	t.Parallel()
	root := &Reference{}
	middle := &Reference{forwardedTo: root}
	leaf := &Reference{forwardedTo: middle}
	if got := ForEachQuantifier(leaf).GetRangesOver(); got != root {
		t.Fatalf("read-only Quantifier forwarding resolved to %p, want root %p", got, root)
	}
	if leaf.forwardedTo != middle {
		t.Fatal("Quantifier.GetRangesOver compressed a forwarding read")
	}
	view := leaf.AdmissionView()
	notRelation, err := values.SnapshotExactType(values.NotNullLong)
	if err != nil {
		t.Fatalf("SnapshotExactType: %v", err)
	}
	err = leaf.ApplyPreparedMemberBatch(view, notRelation, nil, nil, 0)
	var coded interface {
		Code() values.ResolutionErrorCode
	}
	if !errors.As(err, &coded) || coded.Code() != values.MemoMissingRelationWrapper {
		t.Fatalf("ApplyPreparedMemberBatch error = %v, want MemoMissingRelationWrapper", err)
	}
	if leaf.forwardedTo != middle || middle.forwardedTo != root || root.forwardedTo != nil {
		t.Fatal("failed prepared ingress compressed or rewrote the forwarding chain")
	}
}

func TestPreparedMemberDuplicateDoesNotMutateForwardingOrCorrelationCaches(t *testing.T) {
	t.Parallel()
	root := InitialOf(&typedStubExpr{name: "leaf", typ: values.NotNullLong})
	middle := &Reference{forwardedTo: root}
	leaf := &Reference{forwardedTo: middle}
	member := &preparedAliasAwareTestExpression{quantifier: NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("member"),
		leaf,
	)}
	incoming := &preparedAliasAwareTestExpression{quantifier: NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("incoming"),
		root,
	)}

	duplicate, aliasAwareOnly := PreparedMemberDuplicate([]RelationalExpression{member}, incoming)
	if !duplicate || !aliasAwareOnly {
		t.Fatalf("PreparedMemberDuplicate = (%v, %v), want alias-aware-only duplicate", duplicate, aliasAwareOnly)
	}
	if leaf.forwardedTo != middle || middle.forwardedTo != root || root.forwardedTo != nil {
		t.Fatal("prepared equality compressed or rewrote a child forwarding chain")
	}
	if root.correlatedToCache != nil || middle.correlatedToCache != nil || leaf.correlatedToCache != nil {
		t.Fatal("prepared equality populated a shared Reference correlation cache")
	}
}

func TestPreparedMemberDuplicatePreservesSemanticFallback(t *testing.T) {
	t.Parallel()
	leftChild := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	rightChild := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	left := mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(leftChild)))
	right := mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(rightChild)))
	duplicate, aliasAwareOnly := PreparedMemberDuplicate([]RelationalExpression{left}, right)
	if !duplicate || aliasAwareOnly {
		t.Fatalf("PreparedMemberDuplicate semantic fallback = (%v, %v), want (true, false)", duplicate, aliasAwareOnly)
	}
}

func TestReferenceResultTypeDistinguishesEmptyAndLegacyUnadmitted(t *testing.T) {
	t.Parallel()
	_, emptyErr := (&Reference{}).ResultType()
	var emptyCoded interface {
		Code() values.ResolutionErrorCode
	}
	if !errors.As(emptyErr, &emptyCoded) || emptyCoded.Code() != values.MemoEmptyReference {
		t.Fatalf("empty ResultType error = %v, want MemoEmptyReference", emptyErr)
	}

	legacy := InitialOf(&stubExpr{name: "legacy"})
	_, legacyErr := legacy.ResultType()
	var legacyCoded interface {
		Code() values.ResolutionErrorCode
	}
	if !errors.As(legacyErr, &legacyCoded) || legacyCoded.Code() != values.MemoInvalidHandle {
		t.Fatalf("legacy ResultType error = %v, want MemoInvalidHandle", legacyErr)
	}
}
