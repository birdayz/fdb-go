package factory_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestFDB_NestedPathOnAMultiLegOuterSide is an UNBOOKED engine defect, found by
// the nested generator and isolated here.
//
// A three-way join whose third leg is a LEFT OUTER join fails when the ON
// condition reads a NESTED path from the already-joined (outer) side:
//
//	SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id
//	                         LEFT JOIN nt AS r ON m.n.sk = r.n.sk
//
//	correlated FieldValue "N" (correlation "M") evaluated against an
//	unbound/unrecognized context (*RowEvalContext (multi-leg row cannot serve
//	a source-relative ordinal)) — no frontier row resolved (planner/executor bug)
//
// The engine names it a planner/executor bug in its own message, and it is not
// one of the booked nested-path gaps: RFC-204 §4.4 removed the arity cap and
// every other nested join shape below answers. It fails LOUDLY rather than
// returning wrong rows, which is the good direction — but a query that a user
// can write and that the planner cannot execute is a defect, not a limit.
//
// The three conditions are all required, which is what makes the arms below
// worth keeping as a set rather than collapsing to the one failing query:
//
//   - a MULTI-LEG row on the outer side (the two-way forms answer),
//   - a LEFT OUTER third leg (the three-way INNER form answers),
//   - the nested path read FROM that multi-leg side (a nested path on the
//     NULL-extended right side answers, and so does a flat column on the left).
//
// Fixing it is a Cascades change and gated accordingly; this pin is what stops
// it drifting. It fails when the defect is fixed, at which point the failing
// arms flip to value assertions — the same discipline
// TestFDB_NestedProjectionAxes uses for its booked gaps.
func TestFDB_NestedPathOnAMultiLegOuterSide(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	db := openFactorySchema(t, ctx, "zznestouter", nestedProbeDDL)
	if _, err := db.ExecContext(ctx, nestedProbeInsert); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The defect's signature. Matching on it rather than on "any error" is what
	// separates THIS bug from a nested path that stopped resolving at all.
	const signature = "multi-leg row cannot serve a source-relative ordinal"

	for _, tc := range []struct {
		name  string
		query string
		// want is the expected result; a nil want means the arm must FAIL with
		// the signature above.
		want []string
	}{
		// --- the shapes that must keep working ---------------------------
		{
			name:  "two-way-inner-nested-on",
			query: "SELECT l.id FROM nt AS l JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			want:  []string{"1", "2", "3"},
		},
		{
			name:  "two-way-outer-nested-on",
			query: "SELECT l.id FROM nt AS l LEFT JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			want:  []string{"1", "2", "3"},
		},
		{
			// Three legs, nested ON, but the third leg is INNER: the multi-leg
			// row serves the ordinal fine, so outer-ness is load-bearing.
			name: "three-way-inner-nested-on",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
			want: []string{"1", "2", "3"},
		},
		{
			// Three legs, OUTER third leg, but a FLAT column on the multi-leg
			// side: so it is the NESTING that breaks it, not the outer join.
			name: "three-way-outer-flat-on",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON m.sk = r.sk ORDER BY l.id",
			want: []string{"1", "2", "3"},
		},
		{
			// Nested on the NULL-EXTENDED side only. That side is a single-leg
			// row, so it resolves.
			name: "three-way-outer-nested-on-preserved-side",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON m.sk = r.n.sk ORDER BY l.id",
			want: []string{"1", "2", "3"},
		},

		// --- the defect --------------------------------------------------
		{
			name: "three-way-outer-nested-on-both-sides",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
		},
		{
			name: "three-way-outer-nested-on-multileg-side-only",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON m.n.sk = r.sk ORDER BY l.id",
		},
		{
			// The nested path reads the FIRST leg rather than the second; the
			// composite row is what cannot serve it, so which leg is immaterial.
			name: "three-way-outer-nested-on-first-leg",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
		},
	} {
		got, err := queryRowStrings(ctx, db, tc.query)
		if tc.want == nil {
			switch {
			case err == nil:
				t.Errorf("%s: the multi-leg outer nested-path defect is FIXED — it now answers %v.\n"+
					"  %s\nDrop this arm's nil `want` and pin those rows; verify they are CORRECT first, "+
					"because an unbound correlation that starts resolving can resolve to the wrong leg.",
					tc.name, got, tc.query)
			case !strings.Contains(err.Error(), signature):
				t.Errorf("%s: still fails, but no longer on the pinned defect (%q).\n  %s\n  %v",
					tc.name, signature, tc.query, err)
			default:
				t.Logf("DEFECT (pinned) %s", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: a shape that MUST work now errors — the defect's blast radius grew.\n  %s\n  %v",
				tc.name, tc.query, err)
			continue
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: WRONG ROWS.\n  %s\n  want %v\n  got  %v", tc.name, tc.query, tc.want, got)
			continue
		}
		t.Logf("OK              %s", tc.name)
	}
}
