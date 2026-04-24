package embedded

import (
	"testing"
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
	sq := &selectQuery{projCols: []string{"id", "v"}}
	if got := buildOrderByAliases(sq); got != nil {
		t.Fatalf("no aliases: expected nil, got %v", got)
	}
	// Some aliases.
	sq = &selectQuery{
		projCols:    []string{"id", "v"},
		projAliases: []string{"", "value"},
	}
	got := buildOrderByAliases(sq)
	if got["VALUE"] != "v" {
		t.Fatalf("expected VALUE→v mapping, got %v", got)
	}
	if _, has := got["ID"]; has {
		t.Fatalf("unexpected alias for empty-alias projection: %v", got)
	}
}
