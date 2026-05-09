package embedded

import (
	"context"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
)

// Unit tests for naturalOrderSatisfies / naturalOrderSatisfiesReverse /
// scanPropsForOrder. Covers the pre-existing prefix/direction checks and
// the equality-prefix relaxation introduced in dayshift-46 (leading +
// non-leading equated cols strip from naturalOrder and from orderBy
// before the prefix match).

func asc(col string) orderByClause {
	return orderByClause{colName: col, ascending: true}
}

func desc(col string) orderByClause {
	return orderByClause{colName: col, ascending: false}
}

func TestNaturalOrderSatisfies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		orderBy      []orderByClause
		naturalOrder []string
		equated      map[string]bool
		aliases      map[string]string
		want         bool
	}{
		{"empty orderBy + naturalOrder", nil, []string{"a"}, nil, nil, true},
		{"empty naturalOrder", []orderByClause{asc("a")}, nil, nil, nil, false},
		{"ASC prefix match", []orderByClause{asc("a")}, []string{"a", "b"}, nil, nil, true},
		{"ASC full match", []orderByClause{asc("a"), asc("b")}, []string{"a", "b"}, nil, nil, true},
		{"ASC mismatched col", []orderByClause{asc("b")}, []string{"a", "b"}, nil, nil, false},
		{"DESC breaks ASC check", []orderByClause{desc("a")}, []string{"a"}, nil, nil, false},
		{"orderBy longer than naturalOrder", []orderByClause{asc("a"), asc("b"), asc("c")}, []string{"a", "b"}, nil, nil, false},
		{"alias resolves to underlying col", []orderByClause{asc("pk")}, []string{"id"}, nil, map[string]string{"PK": "id"}, true},

		// Equality-prefix relaxation
		{
			name:         "leading equated col strips",
			orderBy:      []orderByClause{asc("b"), asc("c")},
			naturalOrder: []string{"a", "b", "c"},
			equated:      map[string]bool{"A": true},
			want:         true,
		},
		{
			name:         "non-leading equated col strips",
			orderBy:      []orderByClause{asc("a"), asc("c")},
			naturalOrder: []string{"a", "b", "c"},
			equated:      map[string]bool{"B": true},
			want:         true,
		},
		{
			name:         "orderBy on equated col is stripped",
			orderBy:      []orderByClause{desc("a"), asc("b")},
			naturalOrder: []string{"a", "b"},
			equated:      map[string]bool{"A": true},
			want:         true,
		},
		{
			name:         "all orderBy cols equated → trivially satisfied",
			orderBy:      []orderByClause{desc("a"), asc("b")},
			naturalOrder: []string{"a", "b", "c"},
			equated:      map[string]bool{"A": true, "B": true},
			want:         true,
		},
		{
			name:         "bail: orderBy col not in stripped naturalOrder",
			orderBy:      []orderByClause{asc("c")},
			naturalOrder: []string{"a", "b"},
			equated:      map[string]bool{"A": true},
			want:         false,
		},
		{
			name:         "bail: everything in naturalOrder equated, orderBy on non-natural col",
			orderBy:      []orderByClause{asc("z")},
			naturalOrder: []string{"a", "b"},
			equated:      map[string]bool{"A": true, "B": true},
			want:         false,
		},
		{
			name:         "case-insensitive equated lookup",
			orderBy:      []orderByClause{asc("B")},
			naturalOrder: []string{"a", "b"},
			equated:      map[string]bool{"A": true}, // upper key in equated
			want:         true,
		},
		// Qualifier-strip on table-aliased ORDER BY column refs.
		// nightshift-60: `ORDER BY a.id` on `FROM t AS a` should match
		// naturalOrder=["id"]. The qualifier is stripped before
		// comparison.
		{
			name:         "qualified ORDER BY col strips alias prefix",
			orderBy:      []orderByClause{asc("a.id")},
			naturalOrder: []string{"id"},
			want:         true,
		},
		{
			name:         "qualified ORDER BY col with case-insensitive col match",
			orderBy:      []orderByClause{asc("A.ID")},
			naturalOrder: []string{"id"},
			want:         true,
		},
		{
			name:         "qualified ORDER BY col bails when col mismatches",
			orderBy:      []orderByClause{asc("a.name")},
			naturalOrder: []string{"id"},
			want:         false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := naturalOrderSatisfies(tc.orderBy, tc.naturalOrder, tc.equated, tc.aliases)
			if got != tc.want {
				t.Fatalf("naturalOrderSatisfies = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNaturalOrderSatisfiesReverse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		orderBy      []orderByClause
		naturalOrder []string
		equated      map[string]bool
		aliases      map[string]string
		want         bool
	}{
		{"empty orderBy → false (forward is cheaper)", nil, []string{"a"}, nil, nil, false},
		{"DESC prefix match", []orderByClause{desc("a")}, []string{"a", "b"}, nil, nil, true},
		{"DESC full match", []orderByClause{desc("a"), desc("b")}, []string{"a", "b"}, nil, nil, true},
		{"ASC breaks DESC check", []orderByClause{asc("a")}, []string{"a"}, nil, nil, false},
		{"mixed directions bail", []orderByClause{desc("a"), asc("b")}, []string{"a", "b"}, nil, nil, false},

		// Equality-prefix relaxation
		{
			name:         "leading equated + DESC next col → reverse ok",
			orderBy:      []orderByClause{desc("b")},
			naturalOrder: []string{"a", "b"},
			equated:      map[string]bool{"A": true},
			want:         true,
		},
		{
			name:         "all orderBy cols equated → no real direction → forward wins",
			orderBy:      []orderByClause{desc("a")},
			naturalOrder: []string{"a", "b"},
			equated:      map[string]bool{"A": true},
			want:         false,
		},
		{
			name:         "equated leading + DESC + ASC: mixed after stripping → bail",
			orderBy:      []orderByClause{desc("a"), asc("b")},
			naturalOrder: []string{"a", "b"},
			equated:      map[string]bool{"A": true},
			// After stripping a (equated), effective orderBy = [b ASC].
			// ASC against reverse direction → bail.
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := naturalOrderSatisfiesReverse(tc.orderBy, tc.naturalOrder, tc.equated, tc.aliases)
			if got != tc.want {
				t.Fatalf("naturalOrderSatisfiesReverse = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScanPropsForOrder(t *testing.T) {
	t.Parallel()
	// Forward when orderBy empty.
	if _, rev := scanPropsForOrder(nil, []string{"a"}, nil, nil); rev {
		t.Fatal("empty orderBy: expected forward, got reverse")
	}
	// Reverse when all-DESC prefix.
	if _, rev := scanPropsForOrder([]orderByClause{desc("a")}, []string{"a"}, nil, nil); !rev {
		t.Fatal("DESC prefix: expected reverse, got forward")
	}
	// Forward when all-ASC prefix.
	if _, rev := scanPropsForOrder([]orderByClause{asc("a")}, []string{"a"}, nil, nil); rev {
		t.Fatal("ASC prefix: expected forward, got reverse")
	}
	// Relaxation: WHERE a=1 ORDER BY b DESC on (a,b) → reverse.
	if _, rev := scanPropsForOrder(
		[]orderByClause{desc("b")},
		[]string{"a", "b"},
		map[string]bool{"A": true},
		nil,
	); !rev {
		t.Fatal("equated-prefix + DESC: expected reverse, got forward")
	}
	// Relaxation: WHERE a=1 ORDER BY a DESC (all equated) → forward
	// (no real direction).
	if _, rev := scanPropsForOrder(
		[]orderByClause{desc("a")},
		[]string{"a", "b"},
		map[string]bool{"A": true},
		nil,
	); rev {
		t.Fatal("all-equated DESC: expected forward, got reverse")
	}
}

// TestScanSatisfiesOrderBy covers the nightshift-60 helper that
// reports whether a pushdown branch's natural emission order (forward
// or reverse) satisfies the user's ORDER BY clause.
func TestScanSatisfiesOrderBy(t *testing.T) {
	t.Parallel()
	// Empty naturalOrder + non-empty orderBy → false.
	if scanSatisfiesOrderBy([]orderByClause{asc("a")}, nil, nil, nil) {
		t.Fatal("empty naturalOrder + non-empty orderBy: expected false")
	}
	// ASC orderBy matches forward.
	if !scanSatisfiesOrderBy([]orderByClause{asc("a")}, []string{"a", "b"}, nil, nil) {
		t.Fatal("ASC prefix: expected satisfies (forward)")
	}
	// DESC orderBy matches reverse.
	if !scanSatisfiesOrderBy([]orderByClause{desc("a")}, []string{"a", "b"}, nil, nil) {
		t.Fatal("DESC prefix: expected satisfies (reverse)")
	}
	// Mixed direction: neither forward nor reverse satisfies.
	if scanSatisfiesOrderBy([]orderByClause{asc("a"), desc("b")}, []string{"a", "b"}, nil, nil) {
		t.Fatal("mixed direction: expected false")
	}
	// Qualified col name: stripped to bare for comparison.
	if !scanSatisfiesOrderBy([]orderByClause{asc("t.a")}, []string{"a"}, nil, nil) {
		t.Fatal("qualified ASC: expected satisfies")
	}
}

// TestIndexBranchSatisfiesOrderBy covers the nightshift-60
// secondary-index-branch flavour of scanSatisfiesOrderBy, which
// computes the (idxCols ++ pkCols) candidate emission order for
// the supplied secondary index and asks whether the user's ORDER
// BY is satisfied by it forward or reverse.
func TestIndexBranchSatisfiesOrderBy(t *testing.T) {
	t.Parallel()
	// nil idx → false (declined).
	if indexBranchSatisfiesOrderBy(nil, []string{"id"}, []orderByClause{asc("v")}, nil, nil) {
		t.Fatal("nil idx: expected false")
	}
	// Single-col index on `v` over PK `id`. Natural emission is (v, id).
	idxV := recordlayer.NewIndex("idx_v", recordlayer.Field("v"))
	if !indexBranchSatisfiesOrderBy(idxV, []string{"id"}, []orderByClause{asc("v")}, nil, nil) {
		t.Fatal("ORDER BY v on (v, id) index: expected satisfies")
	}
	// ORDER BY v DESC also satisfies via reverse-scan.
	if !indexBranchSatisfiesOrderBy(idxV, []string{"id"}, []orderByClause{desc("v")}, nil, nil) {
		t.Fatal("ORDER BY v DESC: expected satisfies (reverse)")
	}
	// ORDER BY w (not in idx) → does not satisfy.
	if indexBranchSatisfiesOrderBy(idxV, []string{"id"}, []orderByClause{asc("w")}, nil, nil) {
		t.Fatal("ORDER BY non-idx col: expected false")
	}
	// Composite index on (region, tag) over PK (id). Natural emission
	// is (region, tag, id). ORDER BY region is a single-col prefix.
	idxRT := recordlayer.NewIndex("idx_rt", recordlayer.Concat(recordlayer.Field("region"), recordlayer.Field("tag")))
	if !indexBranchSatisfiesOrderBy(idxRT, []string{"id"}, []orderByClause{asc("region")}, nil, nil) {
		t.Fatal("ORDER BY region on (region, tag, id) index: expected satisfies")
	}
	// ORDER BY tag (not the leading idx col) → does not satisfy.
	if indexBranchSatisfiesOrderBy(idxRT, []string{"id"}, []orderByClause{asc("tag")}, nil, nil) {
		t.Fatal("ORDER BY non-prefix idx col: expected false")
	}
	// ORDER BY tag with region equated → eq strips region from natural
	// order, leaving (tag, id); ORDER BY tag is a prefix. Satisfies.
	if !indexBranchSatisfiesOrderBy(idxRT, []string{"id"}, []orderByClause{asc("tag")}, map[string]bool{"REGION": true}, nil) {
		t.Fatal("ORDER BY tag with region equated: expected satisfies via eq-strip")
	}
}

// TestPKOrderingSatisfiesOrderBy covers the nightshift-60 gate used
// on PK pushdown branches.
func TestPKOrderingSatisfiesOrderBy(t *testing.T) {
	t.Parallel()
	// Empty orderBy → trivially satisfied.
	if !pkOrderingSatisfiesOrderBy(nil, []string{"id"}, nil, nil) {
		t.Fatal("empty orderBy: expected satisfies")
	}
	// All-equated orderBy → satisfied.
	if !pkOrderingSatisfiesOrderBy(
		[]orderByClause{asc("id")},
		[]string{"id"},
		map[string]bool{"ID": true},
		nil,
	) {
		t.Fatal("all-equated orderBy: expected satisfies")
	}
	// PK-prefix ASC → satisfies (forward).
	if !pkOrderingSatisfiesOrderBy([]orderByClause{asc("id")}, []string{"id"}, nil, nil) {
		t.Fatal("PK ASC: expected satisfies")
	}
	// PK-prefix DESC → satisfies (reverse).
	if !pkOrderingSatisfiesOrderBy([]orderByClause{desc("id")}, []string{"id"}, nil, nil) {
		t.Fatal("PK DESC: expected satisfies")
	}
	// Non-PK col → does not satisfy.
	if pkOrderingSatisfiesOrderBy([]orderByClause{asc("name")}, []string{"id"}, nil, nil) {
		t.Fatal("non-PK col: expected does not satisfy")
	}
	// Composite PK + ORDER BY first PK col only → satisfies (PK prefix).
	if !pkOrderingSatisfiesOrderBy([]orderByClause{asc("a")}, []string{"a", "b"}, nil, nil) {
		t.Fatal("composite PK + ORDER BY first col: expected satisfies")
	}
	// Composite PK + ORDER BY both PK cols ASC → satisfies (full PK prefix).
	if !pkOrderingSatisfiesOrderBy([]orderByClause{asc("a"), asc("b")}, []string{"a", "b"}, nil, nil) {
		t.Fatal("composite PK ORDER BY both ASC: expected satisfies")
	}
	// Composite PK + ORDER BY second-only with first equated → satisfies
	// (equated leading col strips, leaves ORDER BY b vs PK suffix [b]).
	if !pkOrderingSatisfiesOrderBy(
		[]orderByClause{asc("b")},
		[]string{"a", "b"},
		map[string]bool{"A": true},
		nil,
	) {
		t.Fatal("composite PK ORDER BY second-col with first equated: expected satisfies")
	}
	// Composite PK + mixed direction → does NOT satisfy (cursor is forward
	// or reverse, not mixed).
	if pkOrderingSatisfiesOrderBy([]orderByClause{asc("a"), desc("b")}, []string{"a", "b"}, nil, nil) {
		t.Fatal("composite PK mixed direction: expected does not satisfy")
	}
	// Aliased ORDER BY column resolves through aliasToUnderlying.
	// `ORDER BY pk` where the projection aliased `id AS pk` should
	// satisfy because the alias maps back to the PK col.
	if !pkOrderingSatisfiesOrderBy(
		[]orderByClause{asc("pk")},
		[]string{"id"},
		nil,
		map[string]string{"PK": "id"},
	) {
		t.Fatal("aliased ORDER BY col: expected satisfies via aliasToUnderlying")
	}
}

func TestAllOrderByEquated(t *testing.T) {
	t.Parallel()
	if allOrderByEquated(nil, map[string]bool{"A": true}, nil) {
		t.Fatal("empty orderBy: expected false")
	}
	if allOrderByEquated([]orderByClause{asc("a")}, nil, nil) {
		t.Fatal("nil equated: expected false")
	}
	if !allOrderByEquated([]orderByClause{asc("a"), desc("b")}, map[string]bool{"A": true, "B": true}, nil) {
		t.Fatal("both equated: expected true")
	}
	if allOrderByEquated([]orderByClause{asc("a"), desc("b")}, map[string]bool{"A": true}, nil) {
		t.Fatal("one not equated: expected false")
	}
	// Alias resolution: orderBy uses alias, equated uses underlying col.
	if !allOrderByEquated(
		[]orderByClause{asc("pk")},
		map[string]bool{"ID": true},
		map[string]string{"PK": "id"},
	) {
		t.Fatal("alias → underlying: expected true")
	}
}

func TestBuildOrderByAliases(t *testing.T) {
	t.Parallel()
	// No aliases at all.
	sq := &selectQuery{selectClassification: selectClassification{projCols: []string{"id", "v"}}}
	if got := buildOrderByAliases(sq); got != nil {
		t.Fatalf("no aliases: expected nil, got %v", got)
	}
	// Some aliases.
	sq = &selectQuery{selectClassification: selectClassification{
		projCols:    []string{"id", "v"},
		projAliases: []string{"", "value"},
	}}
	got := buildOrderByAliases(sq)
	if got["VALUE"] != "v" {
		t.Fatalf("expected VALUE→v mapping, got %v", got)
	}
	if _, has := got["ID"]; has {
		t.Fatalf("unexpected alias for empty-alias projection: %v", got)
	}
}

func TestCollectEquatedCols(t *testing.T) {
	t.Parallel()
	// Parsing WHERE expressions through the real SQL parser is
	// heavy for a unit test; the inner flattenAndPredicates +
	// extractColOpLiteral machinery is exercised by the yamsql
	// order_by_elimination scenarios. Here we just pin the nil
	// cases that callers rely on for a safe fallthrough.
	if got := collectEquatedCols(context.TODO(), nil, nil); got != nil {
		t.Fatalf("nil where: got non-nil map %v", got)
	}
}

func TestDedupeAny(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []any
		want []any
	}{
		{"nil", nil, nil},
		{"empty", []any{}, []any{}},
		{"single", []any{int64(1)}, []any{int64(1)}},
		{"all unique", []any{int64(1), int64(2), int64(3)}, []any{int64(1), int64(2), int64(3)}},
		{"trailing dup", []any{int64(1), int64(1)}, []any{int64(1)}},
		{"interleaved", []any{int64(2), int64(1), int64(2), int64(3), int64(1)}, []any{int64(2), int64(1), int64(3)}},
		{"all same", []any{int64(5), int64(5), int64(5)}, []any{int64(5)}},
		{"mixed types unique", []any{int64(1), "foo", int64(1)}, []any{int64(1), "foo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := dedupeAny(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if !valuesEqual(got[i], tc.want[i]) {
					t.Fatalf("at %d: got %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ORDER BY column dedup is case-insensitive: SQL identifiers fold
// to the same key regardless of source casing.
func TestOrderByDedup_CaseInsensitive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		sql      string
		wantErr  string
		wantCols []string
	}{
		{
			name:    "different case, same col → duplicate",
			sql:     "SELECT id FROM t ORDER BY id, ID",
			wantErr: "duplicate",
		},
		{
			name:    "mixed case, same col via positional",
			sql:     "SELECT id, name FROM t ORDER BY ID, id",
			wantErr: "duplicate",
		},
		{
			name:     "different cols no dedup",
			sql:      "SELECT id, name FROM t ORDER BY id, name",
			wantCols: []string{"ID", "NAME"},
		},
		{
			name:     "qualified vs bare — NOT deduped (alias resolution is later)",
			sql:      "SELECT t.id, t.id FROM t AS t ORDER BY t.id, id",
			wantCols: []string{"T.ID", "ID"},
		},
		{
			name:    "qualified same-case is a dup",
			sql:     "SELECT id FROM t AS t ORDER BY t.id, T.ID",
			wantErr: "duplicate",
		},
		{
			// Direction is not part of the dedup key — `ORDER BY x
			// ASC, x DESC` is still a duplicate column reference
			// (can't sort the same col by two directions).
			name:    "same col, different direction is still a dup",
			sql:     "SELECT id FROM t ORDER BY id ASC, id DESC",
			wantErr: "duplicate",
		},
		{
			// Positional + named reference to the same column: ORDER
			// BY 1 and ORDER BY id (where id is output col 1) fold
			// to the same colName string, so dedup fires.
			name:    "positional + named for same col",
			sql:     "SELECT id FROM t ORDER BY 1, id",
			wantErr: "duplicate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, err := parser.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			stmt := root.Statements().AllStatement()[0].SelectStatement()
			sq, err := extractSelectParts(stmt)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil (orderBy=%v)", tc.wantErr, sq.orderBy)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(sq.orderBy) != len(tc.wantCols) {
				t.Fatalf("orderBy len: got %d, want %d", len(sq.orderBy), len(tc.wantCols))
			}
			for i, c := range tc.wantCols {
				if sq.orderBy[i].colName != c {
					t.Fatalf("orderBy[%d]: got %q, want %q", i, sq.orderBy[i].colName, c)
				}
			}
		})
	}
}
