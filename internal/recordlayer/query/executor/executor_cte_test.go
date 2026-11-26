package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestRecursiveLevelUnion_SingleLevel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")

	initial := mustExecutorConstruct(plans.NewRecordQueryTempTableInsertPlan(
		mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
			Value: []any{int64(1)},
			Typ:   values.NewArrayType(false, values.NullableLong),
		})),
		insertAlias, false,
	))
	recursive := mustExecutorConstruct(plans.NewRecordQueryTempTableInsertPlan(
		mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
			Value: []any{},
			Typ:   values.NewArrayType(false, values.NullableLong),
		})),
		insertAlias, false,
	))

	plan := mustExecutorConstruct(plans.NewRecordQueryRecursiveLevelUnionPlan(initial, recursive, scanAlias, insertAlias))
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestRecursiveLevelUnion_EmptyRecursive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")

	initial := mustExecutorConstruct(plans.NewRecordQueryTempTableInsertPlan(
		mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
			Value: []any{"root"},
			Typ:   values.NewArrayType(false, values.TypeString),
		})),
		insertAlias, false,
	))

	recursive := mustExecutorConstruct(plans.NewRecordQueryTempTableInsertPlan(
		mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
			Value: []any{},
			Typ:   values.NewArrayType(false, values.TypeString),
		})),
		insertAlias, false,
	))

	plan := mustExecutorConstruct(plans.NewRecordQueryRecursiveLevelUnionPlan(initial, recursive, scanAlias, insertAlias))

	evalCtx := EmptyEvaluationContext()
	cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (initial only, recursive produces nothing)", len(results))
	}
}

func TestRecursiveDfsJoin_Preorder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	root := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{"A"},
		Typ:   values.NewArrayType(false, values.TypeString),
	}))
	child := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{},
		Typ:   values.NewArrayType(false, values.TypeString),
	}))

	prior := values.NamedCorrelationIdentifier("prior")
	plan := mustExecutorConstruct(plans.NewRecordQueryRecursiveDfsJoinPlan(root, child, prior, plans.DfsPreorder))

	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (leaf node with no children)", len(results))
	}
}

func TestRecursiveDfsJoin_Postorder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	root := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{"A"},
		Typ:   values.NewArrayType(false, values.TypeString),
	}))
	child := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{},
		Typ:   values.NewArrayType(false, values.TypeString),
	}))

	prior := values.NamedCorrelationIdentifier("prior")
	plan := mustExecutorConstruct(plans.NewRecordQueryRecursiveDfsJoinPlan(root, child, prior, plans.DfsPostorder))

	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (leaf node with no children)", len(results))
	}
}

// TestRecursiveResume_CorruptTokensFailLoudly pins the resume boundary on
// every recursive arm: a foreign/corrupt token parses LOUDLY as a typed
// ContinuationParseError — never the C1 misroute (raw bytes fed to the seed
// plan, silently accepted as a scan key suffix: wrong seed set,
// re-emission, no error). All three arms RESUME now: UNION ALL level union
// (RecursiveUnionCursor), UNION ALL DFS (RecursiveCursor), and the
// DISTINCT extension (deterministic position-replay).
func TestRecursiveResume_CorruptTokensFailLoudly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")
	mkPlan := func() *plans.RecordQueryRecursiveLevelUnionPlan {
		elementType := values.NullableLong
		initial := mustExecutorConstruct(plans.NewRecordQueryTempTableInsertPlan(
			mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
				Value: []any{int64(1)},
				Typ:   values.NewArrayType(false, elementType),
			})),
			insertAlias, false,
		))
		recursive := mustExecutorConstruct(plans.NewRecordQueryTempTableInsertPlan(
			mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
				Value: []any{},
				Typ:   values.NewArrayType(false, elementType),
			})),
			insertAlias, false,
		))
		return mustExecutorConstruct(plans.NewRecordQueryRecursiveLevelUnionPlan(initial, recursive, scanAlias, insertAlias))
	}

	// A mid-stream list-index token from a prior page (4 bytes) — exactly
	// the shape the misroute silently swallowed. For a UNION ALL plan the
	// streaming resume PARSES the token as a RecursiveCursorContinuation —
	// a foreign/corrupt token fails LOUDLY (never a silent restart).
	fakeToken := []byte{0, 0, 0, 1}
	_, err := ExecutePlan(ctx, mkPlan(), nil, EmptyEvaluationContext(), fakeToken, recordlayer.DefaultExecuteProperties())
	var parseErr *recordlayer.ContinuationParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("a corrupt token on a streaming recursive CTE must fail loudly with ContinuationParseError, got %v", err)
	}

	// The DISTINCT extension resumes by position-replay; its token is the
	// lossless-codec {position, boundary row} pair — the same foreign
	// bytes fail its decode as the same typed parse error.
	distinct := mustExecutorConstruct(plans.NewRecordQueryRecursiveLevelUnionPlanDistinct(
		mkPlan().GetInitialState(), mkPlan().GetRecursiveState(), scanAlias, insertAlias))
	_, err = ExecutePlan(ctx, distinct, nil, EmptyEvaluationContext(), fakeToken, recordlayer.DefaultExecuteProperties())
	if !errors.As(err, &parseErr) {
		t.Fatalf("a corrupt token on a DISTINCT recursive CTE must fail loudly with ContinuationParseError, got %v", err)
	}

	// The DFS twin streams via the RecursiveCursor port; its token is the
	// RecursiveContinuation proto — foreign bytes fail the parse.
	dfs := mustExecutorConstruct(plans.NewRecordQueryRecursiveDfsJoinPlan(mkPlan().GetInitialState(), mkPlan().GetRecursiveState(), scanAlias, plans.DfsPreorder))
	_, err = ExecutePlan(ctx, dfs, nil, EmptyEvaluationContext(), fakeToken, recordlayer.DefaultExecuteProperties())
	if !errors.As(err, &parseErr) {
		t.Fatalf("a corrupt token on a recursive DFS join must fail loudly with ContinuationParseError, got %v", err)
	}

	// A protobuf-VALID token with zero levels (unknown-field-only — field
	// 15 varint 0) is equally corrupt: RecursiveCursor never emits one
	// (terminals serialize nil), and treating it as exhausted silently
	// served ZERO ROWS.
	zeroLevels := []byte{0x78, 0x00}
	_, err = ExecutePlan(ctx, dfs, nil, EmptyEvaluationContext(), zeroLevels, recordlayer.DefaultExecuteProperties())
	if !errors.As(err, &parseErr) {
		t.Fatalf("a zero-level recursive token must fail loudly, not serve zero rows, got %v", err)
	}

	// nil continuation still executes normally.
	cursor, err := ExecutePlan(ctx, mkPlan(), nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("fresh execution must still work: %v", err)
	}
	cursor.Close()
}

// TestCTEDedupKeyer_DuplicatedColumnRefuses pins the split between the two ways
// a canonical dedup column can fail to bind, because folding them into one
// sentinel was SILENT ROW LOSS.
//
// The keyer resolves each canonical column against the leg's row layout and
// marks the ones it cannot resolve. Absent is routine and keys as NULL —
// dimension-preserving with the seed leg. DUPLICATED resolves to nothing for a
// different reason entirely: the value is there, at two slots. Keying it NULL
// makes two rows that differ ONLY in that column produce the same key, and
// UNION DISTINCT then drops one. The rows are gone with no error and no
// diagnostic, which is strictly worse than the arbitrary-but-real column a
// first-match lookup would have returned.
//
// The 42702 ambiguous-reference rejection is not a defence here and the
// distinction matters: 42702 is about a REFERENCE that names two columns, while
// this is about a ROW that carries a name twice. A recursive-CTE leg over a
// join body emits exactly that row without ever writing an ambiguous reference.
func TestCTEDedupKeyer_DuplicatedColumnRefuses(t *testing.T) {
	t.Parallel()

	// The leg layout declares K twice — one leg-concat of two sources, the shape
	// `SELECT x.k, y.k FROM t x, t y` produces.
	dupType := &values.RecordType{Fields: []values.Field{
		{Name: "K", Ordinal: 0},
		{Name: "K", Ordinal: 1},
	}}
	// Fixture guard: this is only a duplicate-name test while the name really is
	// declared twice.
	if hits := dupType.FieldNameHits("K"); hits != 2 {
		t.Fatalf("fixture: the layout must declare K twice, it declares it %d time(s)", hits)
	}

	k := newCTEDedupKeyer([]string{"K"})
	rowA := NewPositionalRow(dupType)
	rowA.Slots = []any{int64(1), int64(7)}
	rowB := NewPositionalRow(dupType)
	rowB.Slots = []any{int64(1), int64(8)}

	keyA, errA := k.key(QueryResult{Positional: rowA})
	if errA == nil {
		keyB, _ := k.key(QueryResult{Positional: rowB})
		if keyA == keyB {
			t.Fatalf("two rows differing in the duplicated dedup column keyed IDENTICALLY (%q) — "+
				"UNION DISTINCT drops one of them and the query silently loses a row", keyA)
		}
		t.Fatalf("the keyer answered %q for a dedup column the layout declares twice. Any "+
			"answer here is a guess: it either keys NULL (collapsing rows that differ in "+
			"this column) or picks one of the duplicates (deduping on an arbitrary "+
			"column). It must refuse", keyA)
	}
	if !strings.Contains(errA.Error(), "declared 2 times") {
		t.Fatalf("the keyer refused, but the message does not name the duplicated column "+
			"or its count, so nothing points at the leg layout that has to be fixed: %v", errA)
	}

	// THE OTHER DIRECTION, and the reason this needs its own assertion: making
	// the ambiguous case loud must NOT make the ABSENT case loud. A canonical
	// column missing from one leg's layout is the ordinary dimension-preserving
	// shape (the seed leg names a column the recursive leg does not), and it
	// keys as NULL.
	absent := newCTEDedupKeyer([]string{"MISSING"})
	present := &values.RecordType{Fields: []values.Field{{Name: "K", Ordinal: 0}}}
	rowC := NewPositionalRow(present)
	rowC.Slots = []any{int64(1)}
	if _, err := absent.key(QueryResult{Positional: rowC}); err != nil {
		t.Fatalf("a canonical column ABSENT from the layout was refused: %v.\n"+
			"  Absent and duplicated are different facts. Absent keys as NULL and always "+
			"has; refusing it would fail every recursive CTE whose legs do not declare "+
			"identical column sets.", err)
	}
}
