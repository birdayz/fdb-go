package embedded

// Direct unit tests for the parse-tree-aware helpers in
// where_extractors.go. These helpers underpin every pushdown shape in
// pk_pushdown.go / secondary_index_pushdown.go / in_list_pushdown.go
// / like_prefix_pushdown.go / pk_prefix_pushdown.go, but until now had
// only incidental coverage via integration tests that drive a full
// SELECT through the executor. Per RFC-025 §"Strong unit-test
// coverage per package", these helpers are pure enough to deserve
// their own focused test file — failures here surface the broken
// helper directly, not as a downstream pushdown regression.
//
// Test file mix:
//   - flipComparisonOp + extractPKUserFields: pure functions, no parser
//     setup needed. Table-driven for full operator / shape coverage.
//   - extractColumnRef + flattenAndPredicates: ANTLR-driven. Use the
//     existing parseSelect helper from logical_builder_test.go to get
//     real parse-tree contexts cheaply.

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// ----- flipComparisonOp ---------------------------------------------------

// TestFlipComparisonOp_Inverse pins the operator-flip table.
// Used when the literal appears on the LHS of a comparison and the
// pushdown wants col-on-left semantics: `5 < id` → `id > 5` etc.
func TestFlipComparisonOp_Inverse(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{">", "<"},
		{">=", "<="},
		{"<", ">"},
		{"<=", ">="},
		{"=", "="},   // symmetric — = stays =
		{"<>", "<>"}, // not in the flip table — stays as-is
		{"!=", "!="}, // not in the flip table — stays as-is
		{"", ""},     // boundary — empty input doesn't crash
	}
	for _, tc := range cases {
		t.Run(tc.in+"→"+tc.want, func(t *testing.T) {
			t.Parallel()
			if got := flipComparisonOp(tc.in); got != tc.want {
				t.Fatalf("flipComparisonOp(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFlipComparisonOp_DoubleFlipIdentity pins the inverse-of-inverse
// invariant: flipping any input twice returns the original. Catches
// any future asymmetric edits to the flip table.
func TestFlipComparisonOp_DoubleFlipIdentity(t *testing.T) {
	t.Parallel()
	for _, op := range []string{">", ">=", "<", "<=", "=", "<>", "!="} {
		op := op
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			if got := flipComparisonOp(flipComparisonOp(op)); got != op {
				t.Fatalf("double-flip %q: got %q, want %q", op, got, op)
			}
		})
	}
}

// ----- extractPKUserFields -----------------------------------------------

// TestExtractPKUserFields_Composite pins the happy path:
// a CompositeKeyExpression returns its concatenated child field names,
// excluding the RecordTypeKey prefix (FieldNames on RecordTypeKey is
// empty).
func TestExtractPKUserFields_Composite(t *testing.T) {
	t.Parallel()
	pk := recordlayer.Concat(
		recordlayer.RecordTypeKey(),
		recordlayer.Field("order_id"),
		recordlayer.Field("line_no"),
	)
	got := extractPKUserFields(pk)
	want := []string{"order_id", "line_no"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i, n := range want {
		if got[i] != n {
			t.Fatalf("name[%d]: got %q, want %q", i, got[i], n)
		}
	}
}

// TestExtractPKUserFields_BareField pins the documented gap: a bare
// FieldKeyExpression PK (intermingled-table form) returns nil.
// Pushdown then bails to scan, which is correct — see the comment in
// where_extractors.go pinning the "intermingled tables correctly"
// invariant.
func TestExtractPKUserFields_BareField(t *testing.T) {
	t.Parallel()
	pk := recordlayer.Field("id") // bare FieldKeyExpression
	if got := extractPKUserFields(pk); got != nil {
		t.Fatalf("bare FieldKeyExpression PK: got %v, want nil", got)
	}
}

// TestExtractPKUserFields_RecordTypeKeyOnly pins the degenerate
// shape: a CompositeKeyExpression whose only child is RecordTypeKey
// (no user fields). FieldNames returns an empty slice.
func TestExtractPKUserFields_RecordTypeKeyOnly(t *testing.T) {
	t.Parallel()
	pk := recordlayer.Concat(recordlayer.RecordTypeKey())
	got := extractPKUserFields(pk)
	if len(got) != 0 {
		t.Fatalf("RecordTypeKey-only Composite: got %v, want empty", got)
	}
}

// ----- extractColumnRef ---------------------------------------------------

// TestExtractColumnRef_BareColumn pins the simplest case: a single
// FullColumnName atom returns just the column name.
func TestExtractColumnRef_BareColumn(t *testing.T) {
	t.Parallel()
	atom := firstWhereAtom(t, "SELECT 1 FROM t WHERE id = 1")
	got, ok := extractColumnRef(atom)
	if !ok {
		t.Fatalf("expected ok, got false")
	}
	if !strings.EqualFold(got, "id") {
		t.Fatalf("name: got %q, want id", got)
	}
}

// TestExtractColumnRef_QualifiedColumn pins that a `t.col` qualifier
// strips down to the bare column name. Pushdown helpers compare
// column names against the table's FieldNames list which uses the
// bare form.
func TestExtractColumnRef_QualifiedColumn(t *testing.T) {
	t.Parallel()
	atom := firstWhereAtom(t, "SELECT 1 FROM t WHERE t.id = 1")
	got, ok := extractColumnRef(atom)
	if !ok {
		t.Fatalf("expected ok, got false")
	}
	if !strings.EqualFold(got, "id") {
		t.Fatalf("name: got %q, want id", got)
	}
}

// TestExtractColumnRef_NotAColumn pins the negative path: a literal,
// a function call, or any non-FullColumnName atom returns ok=false.
// This is the boundary the pushdown caller relies on to fall back to
// scan when the LHS isn't a bare column ref.
func TestExtractColumnRef_NotAColumn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sql  string
	}{
		{"literal", "SELECT 1 FROM t WHERE 5 = 5"},
		{"function call", "SELECT 1 FROM t WHERE UPPER(name) = 'X'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			atom := firstWhereAtom(t, tc.sql)
			if _, ok := extractColumnRef(atom); ok {
				t.Fatalf("expected ok=false, got true")
			}
		})
	}
}

// ----- flattenAndPredicates -----------------------------------------------

// TestFlattenAndPredicates_SingleLeaf pins the boundary: a single
// non-AND predicate is its own one-element flat list.
func TestFlattenAndPredicates_SingleLeaf(t *testing.T) {
	t.Parallel()
	expr := whereExpr(t, "SELECT 1 FROM t WHERE id = 1")
	got, ok := flattenAndPredicates(expr)
	if !ok {
		t.Fatalf("expected ok, got false")
	}
	if len(got) != 1 {
		t.Fatalf("len: got %d, want 1", len(got))
	}
}

// TestFlattenAndPredicates_NestedAnd pins the recursion: nested
// `(a = 1) AND (b = 2) AND (c = 3)` flattens to three leaves
// regardless of association.
func TestFlattenAndPredicates_NestedAnd(t *testing.T) {
	t.Parallel()
	expr := whereExpr(t, "SELECT 1 FROM t WHERE a = 1 AND b = 2 AND c = 3")
	got, ok := flattenAndPredicates(expr)
	if !ok {
		t.Fatalf("expected ok, got false")
	}
	if len(got) != 3 {
		t.Fatalf("len: got %d, want 3", len(got))
	}
}

// TestFlattenAndPredicates_TopLevelOrFails pins the OR-bail contract
// at the AND-walk level: when OR appears as the logical CONNECTIVE
// at a position the recursion is trying to flatten, the function
// returns false. Without this, pushdown would pick a subset of the
// predicates and produce wrong results.
func TestFlattenAndPredicates_TopLevelOrFails(t *testing.T) {
	t.Parallel()
	expr := whereExpr(t, "SELECT 1 FROM t WHERE a = 1 OR b = 2")
	if _, ok := flattenAndPredicates(expr); ok {
		t.Fatalf("expected ok=false on top-level OR, got true")
	}
}

// TestFlattenAndPredicates_NestedOrSurvivesAsLeaf pins the LAYERED
// contract: nested OR inside parens is NOT flagged here — the
// parenthesised expression is opaque to the AND-walker and surfaces
// as a single leaf in the result. The pushdown extractor (next
// layer) is responsible for rejecting that leaf because it doesn't
// match the col-op-literal shape.
//
// This documents an interface seam: flattenAndPredicates sees
// connectives, downstream extractColOpLiteral et al. see leaf
// shapes. Each layer rejects what it owns. A future hardening pass
// could add deep OR detection here, but it would duplicate work the
// extractor already does correctly.
func TestFlattenAndPredicates_NestedOrSurvivesAsLeaf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		sql       string
		wantLeafN int
	}{
		{"nested OR right", "SELECT 1 FROM t WHERE a = 1 AND (b = 2 OR c = 3)", 2},
		{"nested OR left", "SELECT 1 FROM t WHERE (a = 1 OR b = 2) AND c = 3", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr := whereExpr(t, tc.sql)
			leaves, ok := flattenAndPredicates(expr)
			if !ok {
				t.Fatalf("expected ok=true (parenthesised OR is opaque to flattener), got false")
			}
			if len(leaves) != tc.wantLeafN {
				t.Fatalf("len(leaves): got %d, want %d", len(leaves), tc.wantLeafN)
			}
			// At least one leaf is a parenthesised composite — the
			// downstream extractor will reject it because it's not
			// col-op-literal shaped. We don't assert which leaf is
			// which here; the layered-rejection contract is what
			// matters.
		})
	}
}

// ----- helpers ------------------------------------------------------------

// whereExpr parses sql, asserts it has a WHERE clause, and returns
// the IExpressionContext under it.
func whereExpr(t *testing.T, sql string) antlrgen.IExpressionContext {
	t.Helper()
	sq := parseSelect(t, sql)
	if sq.whereExpr == nil {
		t.Fatalf("no WHERE clause in %q", sql)
	}
	return sq.whereExpr.Expression()
}

// firstWhereAtom parses sql and returns the LHS atom of the first
// binary comparison in the WHERE clause. Used by extractColumnRef
// tests that need a single ExpressionAtom, not the full Expression.
func firstWhereAtom(t *testing.T, sql string) antlrgen.IExpressionAtomContext {
	t.Helper()
	expr := whereExpr(t, sql)
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		t.Fatalf("expected PredicatedExpressionContext, got %T", expr)
	}
	bcp, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		t.Fatalf("expected BinaryComparisonPredicateContext, got %T", pred.ExpressionAtom())
	}
	return bcp.GetLeft()
}
