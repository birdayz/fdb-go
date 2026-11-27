package values

import "testing"

// TestProjectionResultValueNeverHoldsOneNameTwice pins RFC-229 §2.2.2's
// PROPERTY, which Go already satisfies — by a different mechanism from Java's,
// and the difference is worth stating because §2.2.2 prescribes Java's
// mechanism and adopting it literally would have been a regression.
//
// THE PROPERTY: a projection's stored output names are a set, never a bag. Two
// slots holding one name is the shape a name-keyed reader resolves by first
// match, which is the conflation RFC-228 deleted such a lookup to avoid.
//
// JAVA'S MECHANISM, read rather than assumed: Expressions.underlyingAsColumns
// (Expressions.java:269-288) counts by the UNQUALIFIED Identifier::getName
// (computeCountsByName, :209-214) and maps every COLLIDING column's name to
// Optional.empty(); Type.Record.normalizeFields then re-mints the empty name as
// `"_" + i` (Type.java:2646-2651). Visible in the reference corpus at
// right-deep-plan-tests.yamsql:33-37, where `SELECT t1.name, t2.name` explains
// as `(q2.NAME AS _0, q1.NAME AS _1)`.
//
// GO'S MECHANISM: NewRecordConstructorValue suffixes later occurrences `_2`,
// `_3`. Same property, different spelling, and the LATER slot moves rather than
// both.
//
// WHY GO'S IS NOT CHANGED TO JAVA'S. Java's drop is on the INTERNAL column of
// the plan's result Value, not on the reported label — RelationalStructMetaData
// .getColumnLabel delegates straight to getColumnName over the field name
// (RelationalStructMetaData.java:82-89), and the metadata path
// Expressions.getStructType (:246-262) applies NO collision drop at all, using
// `Identifier::toString` and falling back to `_index` only for a genuinely
// absent name. Applying the internal rule to Go's user-visible label was
// MEASURED and reddens SEVEN sqldriver test functions that deliberately pin the
// opposite — the seven are named in RFC-229 §8.3, counted from `grep -E "^--- FAIL"
// ... | sort -u` over a dedicated sqldriver_test run rather than eyeballed. (An
// earlier draft of this comment said "nine", read off a truncated listing; §8.3
// retracts it explicitly. An unscoped count is the shape this repository keeps
// getting wrong even when the argument resting on it is right, so the two
// statements are kept in sync deliberately.) Among the seven is
// TestFDB_DuplicateBareLeafKeepsTwoColumns — whose own
// mint is documented as having reddened seven suites with WRONG ROWS when
// removed; that the two sevens coincide is a coincidence, not one measurement
// cited twice. So the label question is a separate, unanalysed change; this test
// pins the property §2.2.2 actually asks for, at the layer it actually lives.
func TestProjectionResultValueNeverHoldsOneNameTwice(t *testing.T) {
	t.Parallel()

	nested := func(leaf string, ordinal int) *fieldValue {
		return &fieldValue{Field: "N", Typ: UnknownType, Resolved: &fieldPath{
			Accessors: []resolvedAccessor{{Field: "N", Ordinal: 0}, {Field: leaf, Ordinal: ordinal}},
		}}
	}

	for _, tc := range []struct {
		name        string
		projections []Value
		aliases     []string
		want        []string
	}{{
		name:        "two references of one bare name",
		projections: []Value{newFlatFieldValue("K", UnknownType), newFlatFieldValue("K", UnknownType)},
		want:        []string{"K", "K_2"},
	}, {
		name:        "two aliases of one spelling",
		projections: []Value{newFlatFieldValue("A", UnknownType), newFlatFieldValue("B", UnknownType)},
		aliases:     []string{"X", "X"},
		want:        []string{"X", "X_2"},
	}, {
		name:        "three of a kind, so the suffix counter is driven past two",
		projections: []Value{newFlatFieldValue("K", UnknownType), newFlatFieldValue("K", UnknownType), newFlatFieldValue("K", UnknownType)},
		want:        []string{"K", "K_2", "K_3"},
	}, {
		name: "TWO MEMBERS OF ONE STRUCT ROOT — the collision RFC-229 §2.3 removed",
		// This is the control that makes the whole file mean something. Before
		// §2.3 these two took the SAME name `N` and were separated only by the
		// dedup below, which renames the SECOND slot — so `n.co` was reported
		// as `N_2`, a name it has no relation to. They are distinct at the
		// source now, and the dedup never fires.
		projections: []Value{nested("SK", 1), nested("CO", 2)},
		want:        []string{"N.SK", "N.CO"},
	}, {
		name:        "distinct names are left alone",
		projections: []Value{newFlatFieldValue("A", UnknownType), newFlatFieldValue("B", UnknownType)},
		want:        []string{"A", "B"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rv, err := ProjectionResultValue(tc.projections, tc.aliases)
			if err != nil {
				t.Fatalf("ProjectionResultValue: %v", err)
			}
			if len(rv.Fields) != len(tc.want) {
				t.Fatalf("got %d fields, want %d", len(rv.Fields), len(tc.want))
			}
			seen := map[string]int{}
			for i, f := range rv.Fields {
				if f.Name != tc.want[i] {
					t.Errorf("slot %d named %q, want %q", i, f.Name, tc.want[i])
				}
				seen[f.Name]++
			}
			// The PROPERTY, asserted independently of the exact spellings
			// above. The spellings are a mechanism and could legitimately
			// change; two slots sharing a name could not.
			for n, c := range seen {
				if c > 1 {
					t.Errorf("%d slots share the stored name %q — a name-keyed "+
						"reader resolves that by first match, serving one column "+
						"where the other was asked for", c, n)
				}
			}
		})
	}
}
