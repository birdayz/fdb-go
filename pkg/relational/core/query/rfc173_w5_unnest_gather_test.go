package query

// RFC-173 W5 — white-box pins for the GATHERED multi-source lateral-unnest
// translation (the flat (N+1)-quantifier select, design ruling Q1 Option A).
// The FDB e2e (array_unnest_ordinality_fdb_test.go multi-source families)
// proves rows; these prove the SEED and QUANTIFIER shapes plus the commit-1
// fail-open decline boundary directly.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/query/logical"
)

// newDisjointUnnestTranslator builds a translator over a CUSTOM two-table
// schema with fully DISJOINT column names (every pair of demo-proto tables
// shares PRICE, which the commit-1 name-ambiguity gate rightly declines — the
// gather-admitting shape needs disjoint legs): SRC(SID, ARR) owns the array;
// AUX(XID, V) is the second source.
func newDisjointUnnestTranslator(t *testing.T) *cascadesTranslator {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("w5wb")
	b.AddTable("SRC", []metadata.ColumnSpec{
		metadata.NewColumnSpec("SID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewLongType(false), true), 2),
	}, []string{"SID"})
	b.AddTable("AUX", []metadata.ColumnSpec{
		metadata.NewColumnSpec("XID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("V", api.NewLongType(true), 2),
	}, []string{"XID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	return &cascadesTranslator{
		md:              tmpl.Underlying(),
		cteScope:        make(map[string]logical.LogicalOperator),
		cteExprScope:    make(map[string]expressions.RelationalExpression),
		cteColumnsScope: make(map[string][]values.Field),
	}
}

// gatheredFixture builds the canonical multi-source shape
// `FROM SRC s, AUX x, s.ARR AS EL [AT ord]` — the unnest join whose left is
// the 2-source comma cluster and whose owning source is the FIRST
// (non-rightmost) leg.
func gatheredFixture(asAlias, atAlias string) (*logical.LogicalJoin, *logical.LogicalUnnest) {
	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: asAlias, AtAlias: atAlias}
	left := inner(scan("SRC", "s"), scan("AUX", "x"))
	return logical.NewJoin(left, u, logical.JoinInner, ""), u
}

// TestRFC173W5_GatheredSeed_Shape pins the flat gathered translation: one
// select with N+1 quantifiers in FROM order; the Explode's collection is a
// BAKED reference to the OWNING source's own quantifier (the genuine
// per-source correlation the design ruling made decisive — never the
// whole-cluster concat); the seed = full ordinal leg runs + the W4c inner
// fields, asserted pristine for the full-baked AS+AT form.
func TestRFC173W5_GatheredSeed_Shape(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	j, u := gatheredFixture("EL", "ORD")
	innerCorr := values.NamedCorrelationIdentifier("EL")
	sel := tr.translateGatheredUnnestCluster(j, u, innerCorr, values.NotNullLong, "ARR")
	if sel == nil {
		t.Fatal("a gated 2-source cluster + owned unnest must gather, got nil (declined)")
	}
	gathered, isSel := sel.(*expressions.SelectExpression)
	if !isSel {
		t.Fatalf("gathered = %T, want *SelectExpression", sel)
	}
	quants := gathered.GetQuantifiers()
	if len(quants) != 3 {
		t.Fatalf("gathered select has %d quantifiers, want 3 (S, X-aux, X in FROM order)", len(quants))
	}
	if quants[0].GetAlias().Name() != "S" || quants[1].GetAlias().Name() != "X" || quants[2].GetAlias() != innerCorr {
		t.Fatalf("quantifier order/aliases = [%s %s %s], want FROM order [S X EL] (leg aliases flow UPPER via sourceAlias)",
			quants[0].GetAlias(), quants[1].GetAlias(), quants[2].GetAlias())
	}

	// The Explode: its collection is baked ofOrdinal over the OWNER's own
	// typed QOV — correlation "O", frontier-pinned, at the owner-type index of
	// the classified field.
	explode := quants[2].GetRangesOver().Members()[0]
	exp, isExp := explode.(*expressions.ExplodeExpression)
	if !isExp {
		t.Fatalf("inner quantifier ranges over %T, want *ExplodeExpression", explode)
	}
	coll, isFV := exp.GetCollectionValue().(*values.FieldValue)
	if !isFV || coll.Resolved == nil || !coll.Resolved.FrontierPinned {
		t.Fatalf("collection = %T (baked=%v), want a frontier-pinned baked FieldValue", exp.GetCollectionValue(), isFV && coll != nil && coll.Resolved != nil)
	}
	qov, isQOV := coll.Child.(*values.QuantifiedObjectValue)
	if !isQOV || !strings.EqualFold(qov.Correlation.Name(), "s") {
		t.Fatalf("collection correlates to %v, want the OWNING source o (the per-source edge, never the cluster concat)", coll.Child)
	}
	ownerType := tr.ordinalLegType(scan("SRC", "s"))
	wantIdx, _ := ownerType.FieldIndex("ARR")
	if acc, single := coll.Resolved.Single(); !single || acc.Ordinal != wantIdx {
		t.Fatalf("collection baked at ordinal %v, want %d (FieldIndex of the classified field on the OWNER type)", coll.Resolved, wantIdx)
	}

	// The seed: full O-run + full C-run + element + ordinal — all baked (AS+AT
	// is the full-baked form) — and pristine per AssertOrdinalJoinSeed.
	rc, isRC := gathered.GetResultValue().(*values.RecordConstructorValue)
	if !isRC || rc.AnchoredJoin {
		t.Fatalf("seed = %T (anchored=%v), want the ordinal RC", gathered.GetResultValue(), isRC && rc.AnchoredJoin)
	}
	values.AssertOrdinalJoinSeed(rc)
	cType := tr.ordinalLegType(scan("AUX", "x"))
	wantFields := len(ownerType.Fields) + len(cType.Fields) + 2
	if len(rc.Fields) != wantFields {
		t.Fatalf("seed has %d fields, want %d (O-run + TR-run + element + ordinal)", len(rc.Fields), wantFields)
	}
	if rc.Fields[wantFields-2].Name != "EL" || rc.Fields[wantFields-1].Name != "ORD" {
		t.Fatalf("inner field names = [%s %s], want [EL ORD] (the AS/AT aliases)",
			rc.Fields[wantFields-2].Name, rc.Fields[wantFields-1].Name)
	}
}

// TestRFC173W5_Gathered_DeclineBoundary pins the commit-1 fail-open scope:
// every declined shape returns nil so the caller keeps the name-model path
// that handles it today.
func TestRFC173W5_Gathered_DeclineBoundary(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)
	innerCorr := values.NamedCorrelationIdentifier("EL")

	// (a) ON-carrying cluster: dotted-projection resolution over the
	// partitioned flat output is the next commit's leg-window work (R18).
	uOn := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	onLeft := logical.NewJoinWithPredicate(scan("SRC", "s"), scan("AUX", "x"), logical.JoinInner,
		corrEq("x", "XID", "s", "SID"))
	jOn := logical.NewJoin(onLeft, uOn, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jOn, uOn, innerCorr, values.NotNullLong, "ARR"); got != nil {
		t.Fatal("an ON-carrying cluster must DECLINE (commit-1 scope; R18's class)")
	}

	// (b) NAME-AMBIGUOUS: a column name shared by two legs (same table twice)
	// — bare resolution over the flat row would diverge from the name model's
	// last-binding-wins.
	uDup := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	jDup := logical.NewJoin(inner(scan("SRC", "s"), scan("SRC", "s2")), uDup, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jDup, uDup, innerCorr, values.NotNullLong, "ARR"); got != nil {
		t.Fatal("cross-leg duplicate column names must DECLINE (bare-name ambiguity)")
	}

	// (c) SHADOWING: the element alias equals an outer column name (R16's
	// class — the name model resolves it with dedicated shadowing machinery).
	uShadow := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "V"}
	jShadow := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")), uShadow, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jShadow, uShadow, values.NamedCorrelationIdentifier("V"), values.NotNullLong, "ARR"); got != nil {
		t.Fatal("an element alias shadowing an outer column must DECLINE (R16's class)")
	}

	// (d) UNGATED left cluster (LEFT-outer box): flows name-model rows the
	// baked collection cannot consume.
	uLeft := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	jLeft := logical.NewJoin(
		logical.NewJoin(scan("SRC", "s"), scan("AUX", "x"), logical.JoinLeft, ""),
		uLeft, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jLeft, uLeft, innerCorr, values.NotNullLong, "ARR"); got != nil {
		t.Fatal("an ungated (LEFT-box) cluster must DECLINE")
	}

	// (e) SINGLE-SOURCE left: the W4c binary path owns N=1.
	uSingle := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	jSingle := logical.NewJoin(scan("SRC", "s"), uSingle, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jSingle, uSingle, innerCorr, values.NotNullLong, "ARR"); got != nil {
		t.Fatal("a single-source outer must DECLINE the gather (the W4c binary seed owns it)")
	}

	// (f) The owner must be a gathered PLAIN leg: segment 0 naming NO leg
	// declines (an out-of-scope or derived owner).
	uNoOwner := &logical.LogicalUnnest{Segments: []string{"z", "ORDER_ID"}, Alias: "EL"}
	jNoOwner := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")), uNoOwner, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jNoOwner, uNoOwner, innerCorr, values.NotNullLong, "ARR"); got != nil {
		t.Fatal("segment 0 naming no gathered leg must DECLINE")
	}

	// (g) Multi-segment field paths decline (the bake addresses ONE ordinal of
	// the owner's type).
	uDeep := &logical.LogicalUnnest{Segments: []string{"s", "A", "B"}, Alias: "EL"}
	jDeep := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")), uDeep, logical.JoinInner, "")
	if got := tr.translateGatheredUnnestCluster(jDeep, uDeep, innerCorr, values.NotNullLong, "A"); got != nil {
		t.Fatal("a multi-segment array path must DECLINE (single-ordinal bake)")
	}
}

// TestRFC173W5_Gathered_MixedElementSkipsAssert pins the NO-AT gathered form:
// a MIXED RC (baked outer runs + a direct bare-QOV element) — legitimately
// not AssertOrdinalJoinSeed-conformant, same rule as the W4c binary seed.
func TestRFC173W5_Gathered_MixedElementSkipsAssert(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	j, u := gatheredFixture("EL", "")
	sel := tr.translateGatheredUnnestCluster(j, u, values.NamedCorrelationIdentifier("EL"), values.NotNullLong, "ARR")
	if sel == nil {
		t.Fatal("the no-AT gathered form must translate")
	}
	rc := sel.(*expressions.SelectExpression).GetResultValue().(*values.RecordConstructorValue)
	last := rc.Fields[len(rc.Fields)-1]
	if _, isQOV := last.Value.(*values.QuantifiedObjectValue); !isQOV {
		t.Fatalf("no-AT element field = %T, want the DIRECT bare QOV (Java's primitive branch; the mixed RC skips the seed assert)", last.Value)
	}
	if last.Name != "EL" {
		t.Fatalf("element field name = %q, want the AS alias EL", last.Name)
	}
}
