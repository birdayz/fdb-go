package query

import (
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestUnnestAliasReject pins the ONE shared unnest alias-collision predicate:
// AS==AT (the map-keyed overwrite hazard) and AT-only spelling the reserved
// default element name `_0` (which would duplicate the seed leg RecordType's
// field names and fire values.NewRecordType's unique-names constructor
// assert — a producer invariant; see unnestAliasReject's doc). Sound alias
// shapes must pass: the AS name replaces the reserved default, so
// `AS v AT "_0"` is legal, as is AT-only `"_1"` (the element slot is `_0`).
func TestUnnestAliasReject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		alias   string
		atAlias string
		reject  bool
	}{
		{"no aliases", "", "", false},
		{"AS only", "v", "", false},
		{"AS and distinct AT", "v", "o", false},
		{"AS == AT", "v", "v", true},
		{"AS == AT case-insensitive", "v", "V", true},
		{"AT-only reserved _0", "", "_0", true},
		{"AT-only _1 (element slot is _0, no dup)", "", "_1", false},
		{"AS frees the reserved name for AT", "v", "_0", false},
		{"AS spelled _0 with distinct AT", "_0", "o", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := unnestAliasReject(&logical.LogicalUnnest{Alias: tc.alias, AtAlias: tc.atAlias})
			if tc.reject {
				if got == nil {
					t.Fatalf("Alias=%q AtAlias=%q: want DuplicateAlias rejection, got nil", tc.alias, tc.atAlias)
				}
				if got.Code != api.ErrCodeDuplicateAlias {
					t.Fatalf("Alias=%q AtAlias=%q: code = %v, want ErrCodeDuplicateAlias", tc.alias, tc.atAlias, got.Code)
				}
			} else if got != nil {
				t.Fatalf("Alias=%q AtAlias=%q: want nil (sound aliases), got %v", tc.alias, tc.atAlias, got)
			}
		})
	}
}
