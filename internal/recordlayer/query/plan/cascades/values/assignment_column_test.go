package values

import "testing"

// TestResolveAssignmentColumn drives every arm: an exact match, which wins over
// a case-folded one; one case-folded match; none; two exact (a duplicated
// name) and two case-folded (ambiguous); the empty column. The escape-token
// names are the population a second decode aliased (`a__1b` decoding to
// `a$b`): each resolves to itself only.
func TestResolveAssignmentColumn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		names    []string
		column   string
		idx, hit int
	}{
		{"exact", []string{"ID", "a$b", "a__1b"}, "a__1b", 2, 1},
		{"exact, the other escape", []string{"ID", "a__1b", "a$b"}, "a$b", 2, 1},
		{"exact beats a fold", []string{"Name", "NAME"}, "NAME", 1, 1},
		{"one fold", []string{"ID", "KeepCase"}, "KEEPCASE", 1, 1},
		{"none", []string{"ID", "a$b"}, "a__01b", -1, 0},
		{"a second decode is not applied", []string{"ID", "a$b"}, "a__1b", -1, 0},
		{"a dot and an escape token", []string{"ID", "enum_type.enum__1"}, "enum_type.enum__1", 1, 1},
		{"two exact", []string{"X", "X"}, "X", -1, 2},
		{"two folds", []string{"Name", "nAME"}, "NAME", -1, 2},
		{"empty", []string{""}, "", -1, 0},
	} {
		idx, hits := ResolveAssignmentColumn(c.names, c.column)
		if idx != c.idx || hits != c.hit {
			t.Errorf("%s: ResolveAssignmentColumn(%q, %q) = (%d, %d), want (%d, %d)", c.name, c.names, c.column, idx, hits, c.idx, c.hit)
		}
	}
}
