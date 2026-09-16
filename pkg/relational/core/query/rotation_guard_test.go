package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestSubtreeUnnestsOffAlias pins the rotateBuriedChainedSpine defense-in-depth
// guard: a trailing leg that laterally unnests off a spine link's element alias
// cannot be reparented below the spine links, so it must be detected and decline
// the rotation. (Unreachable on today's SQL surface — a lateral unnest owned by a
// spine element is peeled as a FORK, never a plain trailing leg — but the guard
// makes the rotation safe-by-construction, not safe-by-reachability.)
func TestSubtreeUnnestsOffAlias(t *testing.T) {
	spine := map[string]struct{}{"X": {}, "Y": {}}

	// A plain base-scan trailing leg (the tested surface, `, T4 AS T4C`) has no
	// lateral construct — rotation-safe.
	if subtreeUnnestsOffAlias(&logical.LogicalScan{Table: "T4", Alias: "T4C"}, spine) {
		t.Fatal("a plain scan trailing leg must be rotation-safe (no lateral unnest off the spine)")
	}

	ownerLayout := &values.RecordType{Fields: []values.Field{
		{Name: "FOO", Ordinal: 0, FieldType: values.NewArrayType(false, values.NotNullLong)},
		{Name: "BAR", Ordinal: 1, FieldType: values.NewArrayType(false, values.NotNullLong)},
		{Name: "ARR", Ordinal: 2, FieldType: values.NewArrayType(false, values.NotNullLong)},
	}}

	// A trailing leg that laterally unnests off a SPINE element (`X.FOO AS W`, X a
	// spine link element) would be stranded by the rotation — must be flagged.
	lateralUnnest, _ := rawBoundUnnest(t, []string{"X", "FOO"}, "W", "", "X", ownerLayout, 0)
	lateral := &logical.LogicalJoin{
		Left:  &logical.LogicalScan{Table: "T4", Alias: "T4C"},
		Right: lateralUnnest,
		Kind:  logical.JoinInner,
	}
	if !subtreeUnnestsOffAlias(lateral, spine) {
		t.Fatal("a trailing leg unnesting off a spine element must be flagged (rotation would strand it)")
	}

	// Case-insensitive owner match (aliases compare uppercased throughout).
	lowerCase, _ := rawBoundUnnest(t, []string{"y", "BAR"}, "W", "", "Y", ownerLayout, 1)
	if !subtreeUnnestsOffAlias(lowerCase, spine) {
		t.Fatal("owner match must be case-insensitive (y == spine Y)")
	}

	// An unnest off a NON-spine source (`Z.ARR AS W`) is independent of the links —
	// rotation-safe.
	nonSpine, _ := rawBoundUnnest(t, []string{"Z", "ARR"}, "W", "", "Z", ownerLayout, 2)
	if subtreeUnnestsOffAlias(nonSpine, spine) {
		t.Fatal("an unnest off a non-spine source is rotation-safe")
	}
}
