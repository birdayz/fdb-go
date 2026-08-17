package cascades

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

//go:embed *.go
var memoAdmissionSourceFS embed.FS

type memoAdmissionHashSpyPlan struct {
	plans.RecordQueryPlan
	result      values.Value
	hashCalls   atomic.Int32
	resultCalls atomic.Int32
}

// GetResultValue counts its calls because it is the observable edge of
// ADMISSION: admitMemoExpression reaches an expression only through here, so a
// zero count is the proof that a member was not re-admitted.
func (p *memoAdmissionHashSpyPlan) GetResultValue() values.Value {
	p.resultCalls.Add(1)
	return p.result
}

func (p *memoAdmissionHashSpyPlan) HashCodeWithoutChildren() uint64 {
	p.hashCalls.Add(1)
	return 1
}

// EqualsPlanWithoutChildren and GetChildren are spelled out rather than left to
// the embedded nil RecordQueryPlan, which panics when promoted. The admission
// tests above never reach plan equality — they are refused during admission —
// but a test that admits a spy successfully does reach it.
func (p *memoAdmissionHashSpyPlan) EqualsPlanWithoutChildren(other plans.RecordQueryPlan) bool {
	o, ok := other.(*memoAdmissionHashSpyPlan)
	return ok && o == p
}

func (p *memoAdmissionHashSpyPlan) GetChildren() []plans.RecordQueryPlan { return nil }

type hostileMemoExpression struct {
	methodCalls atomic.Int32
}

type embeddedHostileMemoExpression struct {
	expressions.RelationalExpression
}

type memoAdmissionBatchRule struct {
	matcher matching.BindingMatcher
	yields  []expressions.RelationalExpression
}

type memoAdmissionBatchImplementationRule struct {
	matcher       matching.BindingMatcher
	yields        []expressions.RelationalExpression
	constraintRef *expressions.Reference
}

func (r *memoAdmissionBatchImplementationRule) Matcher() matching.BindingMatcher { return r.matcher }
func (r *memoAdmissionBatchImplementationRule) OnMatch(call *ImplementationRuleCall) {
	for _, expression := range r.yields {
		call.Yield(expression)
	}
	call.PushConstraint(r.constraintRef, []*properties.RequestedOrdering{
		properties.NewRequestedOrdering(nil, properties.DistinctnessNotDistinct, false),
	})
}

func (r *memoAdmissionBatchRule) Matcher() matching.BindingMatcher { return r.matcher }
func (r *memoAdmissionBatchRule) OnMatch(call *ExpressionRuleCall) {
	for _, expression := range r.yields {
		call.Yield(expression)
	}
}

func (e *hostileMemoExpression) called() {
	e.methodCalls.Add(1)
	panic("hostile memo expression method invoked")
}

func (e *hostileMemoExpression) GetResultValue() values.Value {
	e.called()
	return nil
}

func (e *hostileMemoExpression) GetQuantifiers() []expressions.Quantifier {
	e.called()
	return nil
}

func (e *hostileMemoExpression) CanCorrelate() bool {
	e.called()
	return false
}

func (e *hostileMemoExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	e.called()
	return nil
}

func (e *hostileMemoExpression) EqualsWithoutChildren(expressions.RelationalExpression, *expressions.AliasMap) bool {
	e.called()
	return false
}

func (e *hostileMemoExpression) HashCodeWithoutChildren() uint64 {
	e.called()
	return 0
}

func (e *hostileMemoExpression) ChildrenAsSet() bool {
	e.called()
	return false
}

func (e *hostileMemoExpression) WithQuantifiers([]expressions.Quantifier) (expressions.RelationalExpression, error) {
	e.called()
	return nil, nil
}

type memoAdmissionState struct {
	members             []expressions.RelationalExpression
	finals              []expressions.RelationalExpression
	resultType          []byte
	stage               expressions.PlannerStage
	aliasAwareDedups    int
	constraintTick      int64
	constraintGoal      int64
	constraintCommitted int64
	planProperties      any
	partialMatches      []any
}

func snapshotMemoAdmissionState(reference *expressions.Reference) memoAdmissionState {
	constraints := reference.ConstraintsMap()
	var resultType []byte
	if view := reference.AdmissionView(); view != nil && view.ResultType() != nil {
		resultType = view.ResultType().CanonicalBytes()
	}
	return memoAdmissionState{
		members:             append([]expressions.RelationalExpression(nil), reference.Members()...),
		finals:              append([]expressions.RelationalExpression(nil), reference.FinalMembers()...),
		resultType:          resultType,
		stage:               reference.Stage(),
		aliasAwareDedups:    reference.AliasAwareDedups(),
		constraintTick:      constraints.CurrentTick(),
		constraintGoal:      constraints.WatermarkGoalTick(),
		constraintCommitted: constraints.WatermarkCommittedTick(),
		planProperties:      reference.GetPlanProperties(),
		partialMatches:      append([]any(nil), reference.GetAllPartialMatches()...),
	}
}

func assertMemoAdmissionStateEqual(t testing.TB, got, want memoAdmissionState) {
	t.Helper()
	if len(got.members) != len(want.members) || len(got.finals) != len(want.finals) {
		t.Fatalf("member population changed: got exploratory/final %d/%d, want %d/%d",
			len(got.members), len(got.finals), len(want.members), len(want.finals))
	}
	for i := range want.members {
		if got.members[i] != want.members[i] {
			t.Fatalf("exploratory member %d changed identity", i)
		}
	}
	for i := range want.finals {
		if got.finals[i] != want.finals[i] {
			t.Fatalf("final member %d changed identity", i)
		}
	}
	if !bytes.Equal(got.resultType, want.resultType) || got.stage != want.stage || got.aliasAwareDedups != want.aliasAwareDedups ||
		got.constraintTick != want.constraintTick || got.constraintGoal != want.constraintGoal ||
		got.constraintCommitted != want.constraintCommitted || got.planProperties != want.planProperties {
		t.Fatalf("Reference metadata changed: got %#v, want %#v", got, want)
	}
	if len(got.partialMatches) != len(want.partialMatches) {
		t.Fatalf("partial-match population changed: got %d, want %d", len(got.partialMatches), len(want.partialMatches))
	}
	for i := range want.partialMatches {
		if got.partialMatches[i] != want.partialMatches[i] {
			t.Fatalf("partial match %d changed identity", i)
		}
	}
}

func requireMemoAdmissionCode(t testing.TB, err error, want values.ResolutionErrorCode) {
	t.Helper()
	var coded interface {
		Code() values.ResolutionErrorCode
	}
	if !errors.As(err, &coded) || coded.Code() != want {
		t.Fatalf("error = %v, want stable memo code %d", err, want)
	}
}

func TestMemoAdmissionRejectsUnknownExpressionWithoutInvokingAnyMethod(t *testing.T) {
	t.Parallel()
	hostile := &hostileMemoExpression{}
	reference, err := InitialOf(hostile)
	requireMemoAdmissionCode(t, err, values.MemoUnsupportedExpression)
	if reference != nil {
		t.Fatal("unsupported expression returned a partial Reference")
	}
	if calls := hostile.methodCalls.Load(); calls != 0 {
		t.Fatalf("closed registry invoked %d hostile methods before rejection", calls)
	}
}

func TestMemoAdmissionRejectsEmbeddedExpressionWithoutInvokingPromotedMethods(t *testing.T) {
	t.Parallel()
	for _, withInner := range []bool{false, true} {
		withInner := withInner
		name := "nil_embedded_interface"
		if withInner {
			name = "hostile_embedded_interface"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hostile := &hostileMemoExpression{}
			embedded := &embeddedHostileMemoExpression{}
			if withInner {
				embedded.RelationalExpression = hostile
			}
			reference, err := InitialOf(embedded)
			requireMemoAdmissionCode(t, err, values.MemoUnsupportedExpression)
			if reference != nil {
				t.Fatal("embedded expression returned a partial Reference")
			}
			if calls := hostile.methodCalls.Load(); calls != 0 {
				t.Fatalf("closed registry invoked %d promoted hostile methods before rejection", calls)
			}
		})
	}
}

func TestMemoAdmissionRejectsKnownTypedNilWithoutMethodInvocation(t *testing.T) {
	t.Parallel()
	var typedNil *expressions.FullUnorderedScanExpression
	reference, err := InitialOf(typedNil)
	requireMemoAdmissionCode(t, err, values.MemoUnsupportedExpression)
	if reference != nil {
		t.Fatal("typed-nil expression returned a partial Reference")
	}
}

func TestMemoAdmissionCheckedFactoriesPublishExactLaneAndStage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		build       func(expressions.RelationalExpression) (*expressions.Reference, error)
		stage       expressions.PlannerStage
		exploratory int
		final       int
	}{
		{name: "initial", build: InitialOf, stage: expressions.StageCanonical, exploratory: 1},
		{name: "final", build: FinalOf, stage: expressions.StagePlanned, final: 1},
		{name: "final at canonical", build: func(expression expressions.RelationalExpression) (*expressions.Reference, error) {
			return FinalOfAtStage(expression, expressions.StageCanonical)
		}, stage: expressions.StageCanonical, final: 1},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			reference, err := testCase.build(fixtureScan("FACTORY"))
			if err != nil {
				t.Fatalf("checked factory: %v", err)
			}
			if reference.Stage() != testCase.stage || len(reference.Members()) != testCase.exploratory || len(reference.FinalMembers()) != testCase.final {
				t.Fatalf("factory shape = stage %v lanes %d/%d, want %v %d/%d",
					reference.Stage(), len(reference.Members()), len(reference.FinalMembers()),
					testCase.stage, testCase.exploratory, testCase.final)
			}
			resultType, err := reference.ResultType()
			if err != nil {
				t.Fatalf("ResultType: %v", err)
			}
			if !resultType.Equals(values.NewRelationType(values.NotNullLong)) {
				t.Fatalf("ResultType = %v, want RELATION<LONG>", resultType)
			}
		})
	}
}

func TestMemoAdmissionCheckedFactoryRejectsUnknownStage(t *testing.T) {
	t.Parallel()
	reference, err := FinalOfAtStage(fixtureScan("UNKNOWN_STAGE"), expressions.PlannerStage(255))
	requireMemoAdmissionCode(t, err, values.MemoBatchConflict)
	if reference != nil {
		t.Fatal("unknown planner stage returned a partial Reference")
	}
}

func TestMemoAdmissionWholeBatchChecksTypeBeforeHashInBothOrders(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		reverse := reverse
		name := "valid_then_invalid"
		if reverse {
			name = "invalid_then_valid"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reference, err := InitialOf(fixtureScan("BASE"))
			if err != nil {
				t.Fatalf("InitialOf: %v", err)
			}
			constraints := reference.ConstraintsMap()
			constraints.PushProperty("ordering", "seed", nil)
			constraints.StartExploration()
			reference.SetPlanProperties("properties-sentinel")
			reference.AddPartialMatch("candidate", "match")
			before := snapshotMemoAdmissionState(reference)
			versionProbe, err := prepareReferenceMemberBatch(reference, nil)
			if err != nil {
				t.Fatalf("prepare version probe: %v", err)
			}

			validSpy := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullLong)}
			invalidSpy := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullString)}
			valid := &scanPlanExpression{plan: validSpy}
			disagreeing := &scanPlanExpression{plan: invalidSpy}
			intents := []referenceMemberIntent{
				{set: expressions.ReferenceExploratoryMembers, expression: valid},
				{set: expressions.ReferenceExploratoryMembers, expression: disagreeing},
			}
			if reverse {
				intents[0], intents[1] = intents[1], intents[0]
			}
			batch, err := prepareReferenceMemberBatch(reference, intents)
			if batch != nil {
				t.Fatal("disagreeing batch returned a prepared object")
			}
			requireMemoAdmissionCode(t, err, values.MemoResultTypeMismatch)
			if validCalls, invalidCalls := validSpy.hashCalls.Load(), invalidSpy.hashCalls.Load(); validCalls != 0 || invalidCalls != 0 {
				t.Fatalf("expression hashes called before whole-batch exact type admission: valid=%d invalid=%d", validCalls, invalidCalls)
			}
			if err := versionProbe.commit(); err != nil {
				t.Fatalf("failed preparation changed Reference version: %v", err)
			}
			after := snapshotMemoAdmissionState(reference)
			assertMemoAdmissionStateEqual(t, after, before)
		})
	}
}

func TestMemoAdmissionRuleDriverRejectsLateInvalidYieldAtomically(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		reverse := reverse
		name := "valid_then_invalid"
		if reverse {
			name = "invalid_then_valid"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reference, err := InitialOf(fixtureScan("DRIVER_BASE"))
			if err != nil {
				t.Fatalf("InitialOf: %v", err)
			}
			reference.ConstraintsMap().PushProperty("driver", "seed", nil)
			reference.ConstraintsMap().StartExploration()
			reference.SetPlanProperties("driver-properties")
			reference.AddPartialMatch("driver-candidate", "driver-match")
			memo := NewMemo(reference)
			before := snapshotMemoAdmissionState(reference)
			beforeMemoMembers := memo.TotalMembers()
			beforeMemoReferences := len(memo.References())
			validSpy := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullLong)}
			invalidSpy := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullString)}
			valid := &scanPlanExpression{plan: validSpy}
			disagreeing := &scanPlanExpression{plan: invalidSpy}
			yields := []expressions.RelationalExpression{valid, disagreeing}
			if reverse {
				yields[0], yields[1] = yields[1], yields[0]
			}
			rule := &memoAdmissionBatchRule{
				matcher: NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("memo-admission-driver"),
				yields:  yields,
			}

			got, err := FireExpressionRuleWithMemo(rule, reference, EmptyPlanContext(), memo)
			if got != nil {
				t.Fatalf("failed driver returned yielded expressions: %v", got)
			}
			requireMemoAdmissionCode(t, err, values.MemoResultTypeMismatch)
			if validCalls, invalidCalls := validSpy.hashCalls.Load(), invalidSpy.hashCalls.Load(); validCalls != 0 || invalidCalls != 0 {
				t.Fatalf("driver hashed before whole-batch admission: valid=%d invalid=%d", validCalls, invalidCalls)
			}
			assertMemoAdmissionStateEqual(t, snapshotMemoAdmissionState(reference), before)
			if memo.TotalMembers() != beforeMemoMembers || len(memo.References()) != beforeMemoReferences {
				t.Fatalf("failed driver changed memo topology: members %d→%d refs %d→%d",
					beforeMemoMembers, memo.TotalMembers(), beforeMemoReferences, len(memo.References()))
			}
		})
	}
}

func TestMemoAdmissionImplementationDriverRejectsLateInvalidYieldAtomically(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		reverse := reverse
		name := "valid_then_invalid"
		if reverse {
			name = "invalid_then_valid"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reference, err := InitialOf(fixtureScan("IMPLEMENTATION_DRIVER_BASE"))
			if err != nil {
				t.Fatalf("InitialOf: %v", err)
			}
			reference.ConstraintsMap().PushProperty("driver", "seed", nil)
			reference.ConstraintsMap().StartExploration()
			reference.SetPlanProperties("implementation-driver-properties")
			reference.AddPartialMatch("implementation-driver-candidate", "implementation-driver-match")
			constraintRef, err := InitialOf(fixtureScan("IMPLEMENTATION_DRIVER_CHILD"))
			if err != nil {
				t.Fatalf("child InitialOf: %v", err)
			}
			constraintMap := NewConstraintMap()
			beforeConstraintTick := constraintRef.ConstraintsMap().CurrentTick()
			memo := NewMemo(reference)
			before := snapshotMemoAdmissionState(reference)
			beforeMemoMembers := memo.TotalMembers()
			beforeMemoReferences := len(memo.References())
			validSpy := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullLong)}
			invalidSpy := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullString)}
			yields := []expressions.RelationalExpression{
				&scanPlanExpression{plan: validSpy},
				&scanPlanExpression{plan: invalidSpy},
			}
			if reverse {
				yields[0], yields[1] = yields[1], yields[0]
			}
			rule := &memoAdmissionBatchImplementationRule{
				matcher:       NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("memo-admission-implementation-driver"),
				yields:        yields,
				constraintRef: constraintRef,
			}

			got, err := FireImplementationRuleWithContext(rule, reference, EmptyPlanContext(), memo, constraintMap)
			if got != nil {
				t.Fatalf("failed implementation driver returned yielded expressions: %v", got)
			}
			requireMemoAdmissionCode(t, err, values.MemoResultTypeMismatch)
			if validCalls, invalidCalls := validSpy.hashCalls.Load(), invalidSpy.hashCalls.Load(); validCalls != 0 || invalidCalls != 0 {
				t.Fatalf("implementation driver hashed before whole-batch admission: valid=%d invalid=%d", validCalls, invalidCalls)
			}
			assertMemoAdmissionStateEqual(t, snapshotMemoAdmissionState(reference), before)
			if tick := constraintRef.ConstraintsMap().CurrentTick(); tick != beforeConstraintTick {
				t.Fatalf("failed implementation driver changed constraint tick: %d → %d", beforeConstraintTick, tick)
			}
			if _, ok := Get(constraintMap, constraintRef, RequestedOrderingConstraintKey); ok {
				t.Fatal("failed implementation driver published a staged constraint")
			}
			if memo.TotalMembers() != beforeMemoMembers || len(memo.References()) != beforeMemoReferences {
				t.Fatalf("failed implementation driver changed memo topology: members %d→%d refs %d→%d",
					beforeMemoMembers, memo.TotalMembers(), beforeMemoReferences, len(memo.References()))
			}
		})
	}
}

func TestMemoAdmissionRejectsDoubleRelationBeforeHash(t *testing.T) {
	t.Parallel()
	reference, err := InitialOf(fixtureScan("DOUBLE_RELATION_BASE"))
	if err != nil {
		t.Fatalf("InitialOf: %v", err)
	}
	before := snapshotMemoAdmissionState(reference)
	spy := &memoAdmissionHashSpyPlan{
		result: values.NewQueriedValue(nil, values.NewRelationType(values.NotNullLong)),
	}
	doubled := &scanPlanExpression{plan: spy}
	batch, err := prepareReferenceMemberBatch(reference, []referenceMemberIntent{{
		set: expressions.ReferenceExploratoryMembers, expression: doubled,
	}})
	if batch != nil {
		t.Fatal("double-RELATION expression returned a prepared batch")
	}
	requireMemoAdmissionCode(t, err, values.MemoDoubleRelationWrapper)
	if calls := spy.hashCalls.Load(); calls != 0 {
		t.Fatalf("double-RELATION expression hash called %d times before rejection", calls)
	}
	assertMemoAdmissionStateEqual(t, snapshotMemoAdmissionState(reference), before)
}

func TestMemoAdmissionSuccessfulBatchPublishesAllMembersAtOnce(t *testing.T) {
	t.Parallel()
	reference, err := InitialOf(fixtureScan("BASE"))
	if err != nil {
		t.Fatalf("InitialOf: %v", err)
	}
	one := fixtureScan("ONE")
	oneDuplicate := fixtureScan("ONE")
	two := fixtureScan("TWO")
	batch, err := prepareReferenceMemberBatch(reference, []referenceMemberIntent{
		{set: expressions.ReferenceExploratoryMembers, expression: one},
		{set: expressions.ReferenceExploratoryMembers, expression: oneDuplicate},
		{set: expressions.ReferenceExploratoryMembers, expression: two},
	})
	if err != nil {
		t.Fatalf("prepareReferenceMemberBatch: %v", err)
	}
	if got := len(reference.Members()); got != 1 {
		t.Fatalf("pre-commit Reference has %d members, want 1", got)
	}
	if len(batch.inserted) != 3 || !batch.inserted[0] || batch.inserted[1] || !batch.inserted[2] {
		t.Fatalf("prepared insertion outcomes = %v, want [true false true]", batch.inserted)
	}
	if err := batch.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got := reference.Members()
	if len(got) != 3 || got[1] != one || got[2] != two {
		t.Fatalf("post-commit members = %v, want BASE, ONE, TWO in batch order", got)
	}
}

func TestMemoAdmissionPreparedBatchConflictDoesNotPartiallyApply(t *testing.T) {
	t.Parallel()
	reference, err := InitialOf(fixtureScan("BASE"))
	if err != nil {
		t.Fatalf("InitialOf: %v", err)
	}
	preparedMember := fixtureScan("PREPARED")
	batch, err := prepareReferenceMemberBatch(reference, []referenceMemberIntent{{
		set: expressions.ReferenceExploratoryMembers, expression: preparedMember,
	}})
	if err != nil {
		t.Fatalf("prepareReferenceMemberBatch: %v", err)
	}
	intervening := fixtureScan("INTERVENING")
	if !reference.Insert(intervening) {
		t.Fatal("fixture intervening mutation was unexpectedly deduplicated")
	}
	err = batch.commit()
	requireMemoAdmissionCode(t, err, values.MemoBatchConflict)
	got := reference.Members()
	if len(got) != 2 || got[0].GetResultValue() == nil || got[1] != intervening {
		t.Fatalf("conflicted commit changed members: %v", got)
	}
	for _, member := range got {
		if member == preparedMember {
			t.Fatal("conflicted commit partially published its prepared member")
		}
	}
}

func TestMemoAdmissionPreparedBatchConflictsWithStageTransition(t *testing.T) {
	t.Parallel()
	reference, err := InitialOf(fixtureScan("BASE"))
	if err != nil {
		t.Fatalf("InitialOf: %v", err)
	}
	preparedMember := fixtureScan("PREPARED_FOR_OLD_STAGE")
	batch, err := prepareReferenceMemberBatch(reference, []referenceMemberIntent{{
		set: expressions.ReferenceExploratoryMembers, expression: preparedMember,
	}})
	if err != nil {
		t.Fatalf("prepareReferenceMemberBatch: %v", err)
	}
	reference.AdvanceStagePreservingMembers(expressions.StagePlanned)
	beforeCommit := snapshotMemoAdmissionState(reference)
	err = batch.commit()
	requireMemoAdmissionCode(t, err, values.MemoBatchConflict)
	assertMemoAdmissionStateEqual(t, snapshotMemoAdmissionState(reference), beforeCommit)
	for _, member := range reference.AllMembers() {
		if member == preparedMember {
			t.Fatal("stage-conflicted commit partially published its prepared member")
		}
	}
}

func TestMemoAdmissionApplyAdapterHasOneRootCaller(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	// PreparedMemberDuplicateWithHashes is watched INSTEAD OF, not alongside,
	// PreparedMemberDuplicate: the admission boundary calls the hash-hoisting
	// form, and the plain one now forwards to it for callers that have no
	// precomputed hashes. Watching both would demand a root call to a function
	// this package deliberately does not call, so the gate would fail on a
	// correct tree. What the gate protects is unchanged — exactly one dedup
	// entry point into the memo, in memo_admission.go.
	counts := map[string][]string{
		"ApplyPreparedMemberBatch":          nil,
		"PreparedMemberDuplicateWithHashes": nil,
	}
	entries, err := memoAdmissionSourceFS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded cascades sources: %v", err)
	}
	for _, entry := range entries {
		filename := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			continue
		}
		source, err := memoAdmissionSourceFS.ReadFile(filename)
		if err != nil {
			t.Fatalf("read %s: %v", filename, err)
		}
		file, err := parser.ParseFile(fset, filename, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filename, err)
		}
		for _, imported := range file.Imports {
			if imported.Name != nil && imported.Name.Name == "." &&
				imported.Path.Value == `"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"` {
				t.Fatalf("%s dot-imports expressions and can evade the prepared-apply source gate", filename)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, watched := counts[selector.Sel.Name]; watched {
				position := fset.Position(selector.Pos())
				counts[selector.Sel.Name] = append(counts[selector.Sel.Name],
					fmt.Sprintf("%s:%d", position.Filename, position.Line))
			}
			return true
		})
	}
	for name, sites := range counts {
		if len(sites) != 1 || !strings.HasPrefix(sites[0], "memo_admission.go:") {
			t.Fatalf("%s call sites = %v, want exactly one root call in memo_admission.go", name, sites)
		}
	}
}

// A member the Reference has ALREADY admitted is not re-admitted and its hash
// is not re-derived when a later batch prepares against the same Reference.
//
// This is the dimension the three "rejects late invalid yield" tests above
// cannot see: every disagreeing member there arrives as an INTENT, so they
// stay green whether existing members are re-proved or skipped. The skip is
// what keeps preparation off an O(N²) curve — a six-way star prepares
// thousands of batches against References that accumulate members, and
// re-admitting each existing member re-thaws its Type graph out of the exact
// handle and re-walks its result Value through FNV.
//
// WHAT RE-ARMS THE FULL PASS. Skipping is sound only because an existing
// member was admitted and type-checked by THIS boundary when it was inserted,
// so its proof is one the Reference already holds. That rests on the boundary
// being the only door into a Reference's member sets, which
// TestMemoAdmissionApplyAdapterHasOneRootCaller pins. If a second caller ever
// appears, members can enter unproved and this skip stops being safe.
func TestMemoAdmissionDoesNotReproveAlreadyAdmittedMembers(t *testing.T) {
	t.Parallel()
	reference, err := InitialOf(fixtureScan("REPROVE_BASE"))
	if err != nil {
		t.Fatalf("InitialOf: %v", err)
	}

	firstSpy := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullLong)}
	first := &scanPlanExpression{plan: firstSpy}
	batch, err := prepareReferenceMemberBatch(reference, []referenceMemberIntent{
		{set: expressions.ReferenceExploratoryMembers, expression: first},
	})
	if err != nil {
		t.Fatalf("prepare first batch: %v", err)
	}
	if err := batch.commit(); err != nil {
		t.Fatalf("commit first batch: %v", err)
	}
	if got := firstSpy.resultCalls.Load(); got == 0 {
		t.Fatalf("first batch admitted its own intent %d times, want a non-zero count"+
			" — a zero here means the spy never reached admission and the"+
			" post-commit zero below would prove nothing", got)
	}

	firstSpy.resultCalls.Store(0)
	firstSpy.hashCalls.Store(0)

	secondSpy := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullLong)}
	second := &scanPlanExpression{plan: secondSpy}
	batch, err = prepareReferenceMemberBatch(reference, []referenceMemberIntent{
		{set: expressions.ReferenceExploratoryMembers, expression: second},
	})
	if err != nil {
		t.Fatalf("prepare second batch: %v", err)
	}
	if err := batch.commit(); err != nil {
		t.Fatalf("commit second batch: %v", err)
	}

	if got := firstSpy.resultCalls.Load(); got != 0 {
		t.Errorf("already-admitted member re-admitted %d times by the second batch, want 0", got)
	}
	// The hash memo caches on first READ, and the batch that inserts a member
	// hashes it into its own local slice without seeding the memo — so the
	// FIRST batch after an insertion derives that member's hash once more, and
	// every batch after that reads it from the memo. Deliberately not made
	// zero: seeding the memo at insertion would mean writing to the Reference
	// during preparation, and preparation is required to leave the Reference
	// untouched until commit. One derivation per member is already the
	// property that matters, which is why the third batch below is the real
	// assertion — it is the one that distinguishes constant-per-member from
	// once-per-batch.
	if got := firstSpy.hashCalls.Load(); got > 1 {
		t.Errorf("already-admitted member's hash derived %d times by the second batch, want at most 1", got)
	}
	if got := secondSpy.resultCalls.Load(); got == 0 {
		t.Error("the second batch's own intent was never admitted")
	}

	// A third batch settles `second` into the memo the same way, and a FOURTH
	// then has every prior member cached — so every earlier spy must be flat at
	// zero across it. That is the assertion that separates constant-per-member
	// from once-per-batch: with per-intent hashing, these counts would keep
	// rising for as long as batches keep arriving.
	thirdSpy := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullLong)}
	batch, err = prepareReferenceMemberBatch(reference, []referenceMemberIntent{
		{set: expressions.ReferenceExploratoryMembers, expression: &scanPlanExpression{plan: thirdSpy}},
	})
	if err != nil {
		t.Fatalf("prepare third batch: %v", err)
	}
	if err := batch.commit(); err != nil {
		t.Fatalf("commit third batch: %v", err)
	}

	// `third` is deliberately NOT in this list: it was inserted by the batch
	// immediately before the one under test, so it is the member still owed its
	// single memo-seeding derivation, and it is asserted separately below. Every
	// member older than that must be flat.
	settled := []struct {
		name string
		spy  *memoAdmissionHashSpyPlan
	}{{"first", firstSpy}, {"second", secondSpy}}
	for _, probe := range append(settled, struct {
		name string
		spy  *memoAdmissionHashSpyPlan
	}{"third", thirdSpy}) {
		probe.spy.resultCalls.Store(0)
		probe.spy.hashCalls.Store(0)
	}

	fourthSpy := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullLong)}
	batch, err = prepareReferenceMemberBatch(reference, []referenceMemberIntent{
		{set: expressions.ReferenceExploratoryMembers, expression: &scanPlanExpression{plan: fourthSpy}},
	})
	if err != nil {
		t.Fatalf("prepare fourth batch: %v", err)
	}
	if err := batch.commit(); err != nil {
		t.Fatalf("commit fourth batch: %v", err)
	}
	for _, probe := range settled {
		if got := probe.spy.resultCalls.Load(); got != 0 {
			t.Errorf("%s member re-admitted %d times by the fourth batch, want 0", probe.name, got)
		}
		if got := probe.spy.hashCalls.Load(); got != 0 {
			t.Errorf("%s member's hash re-derived %d times by the fourth batch, want 0"+
				" — a non-zero here means the per-member cost is paid per BATCH,"+
				" which is the O(intents × members) curve this exists to remove", probe.name, got)
		}
	}
	if got := thirdSpy.resultCalls.Load(); got != 0 {
		t.Errorf("the previous batch's member was re-admitted %d times, want 0", got)
	}
	if got := thirdSpy.hashCalls.Load(); got > 1 {
		t.Errorf("the previous batch's member had its hash derived %d times, want at most 1"+
			" (the one-time memo seeding described above)", got)
	}
	if got := fourthSpy.resultCalls.Load(); got == 0 {
		t.Error("the fourth batch's own intent was never admitted")
	}
	if got := len(reference.Members()); got != 5 {
		t.Errorf("Reference has %d members, want 5 (seed + four admitted intents)", got)
	}
}

// The hash the boundary hoists must be the hash the dedup tiers would have
// derived themselves. A hoisted value that disagrees with
// HashCodeWithoutChildren silently disables tier 2 (mHash == eHash gates the
// semantic comparison), which shows up not as an error but as a DUPLICATE
// member admitted into the memo — so this asserts the memo answer, not just
// the arithmetic.
func TestMemoAdmissionHoistedHashesDoNotWeakenDedup(t *testing.T) {
	t.Parallel()
	reference, err := InitialOf(fixtureScan("HOIST_BASE"))
	if err != nil {
		t.Fatalf("InitialOf: %v", err)
	}
	// Two distinct expression objects over an identical plan: not pointer-equal,
	// so only the hash-gated semantic tier can recognize the second as a
	// duplicate of the first.
	plan := &memoAdmissionHashSpyPlan{result: values.NewQueriedValue(nil, values.NotNullLong)}
	batch, err := prepareReferenceMemberBatch(reference, []referenceMemberIntent{
		{set: expressions.ReferenceExploratoryMembers, expression: &scanPlanExpression{plan: plan}},
	})
	if err != nil {
		t.Fatalf("prepare seeding batch: %v", err)
	}
	if err := batch.commit(); err != nil {
		t.Fatalf("commit seeding batch: %v", err)
	}
	seeded := len(reference.Members())

	batch, err = prepareReferenceMemberBatch(reference, []referenceMemberIntent{
		{set: expressions.ReferenceExploratoryMembers, expression: &scanPlanExpression{plan: plan}},
	})
	if err != nil {
		t.Fatalf("prepare duplicate batch: %v", err)
	}
	if batch.inserted[0] {
		t.Error("a semantically equal member was admitted as new — the hoisted" +
			" member hash disagreed with HashCodeWithoutChildren, so the" +
			" hash-gated semantic dedup tier never ran")
	}
	if err := batch.commit(); err != nil {
		t.Fatalf("commit duplicate batch: %v", err)
	}
	if got := len(reference.Members()); got != seeded {
		t.Errorf("Reference grew from %d to %d members on a duplicate", seeded, got)
	}
}
