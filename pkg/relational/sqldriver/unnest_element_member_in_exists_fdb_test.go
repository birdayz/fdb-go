package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_UnnestElementMemberInExistsDivergesFromJava pins a KNOWN GO-ONLY
// DIVERGENCE, and it is a sentinel rather than a blessing.
//
// READ THIS FIRST, BECAUSE AN EARLIER VERSION OF THIS TEST GOT IT WRONG. The
// 42703 below is NOT a resolution rule correctly declining a shape neither
// engine supports. Java answers both of these queries with rows, and Go refuses
// them. Under this repo's conformance principle that is a BUG TO FIX, not
// behaviour to pin — so nothing here should be read as saying the refusal is
// right. What is pinned is that the refusal is CURRENTLY TRUE, so that the day
// it stops being true is visible rather than silent.
//
// JAVA RESOLVES IT, and structurally rather than by special case.
// SemanticAnalyzer.resolveAcrossFragments (SemanticAnalyzer.java:383-401) walks
// `getParentMaybe()` to the root fragment — arity-blind and subquery-blind, with
// 42703 only after the chain is exhausted — and
// LogicalOperator.generateCorrelatedFieldAccess (LogicalOperator.java:307-354)
// emits one output Expression per struct field for a struct-array element
// (`convertToExpressions(resultingQuantifier)`), which is exactly the shape Go's
// unnestVirtualScopeSourceWithElement builds WHEN IT IS GIVEN THE FIELDS. Java's
// own conformance corpus drives both forms from inside an EXISTS body and
// expects rows — valid-identifiers.yamsql:221 (flat member,
// `x."level1$field.3" = 20` -> [{2}]) and :226 (NESTED member,
// `x."level1$field.2"."level2$field.1" = 91` -> [{1}]).
//
// THE GO SITE IS A DROPPED ARGUMENT, not a rule.
// `existsSubqueryPlanner.addCorrelatedJoinScopeSource`
// (pkg/relational/core/embedded/logical_predicate.go:9302) calls
// `unnestVirtualScopeSource(j)` — i.e. `unnestVirtualScopeSourceWithElement(j,
// nil)` — so the element column is typed UNKNOWN with no StructFields and no
// descent can resolve. The sibling `unnestScopeSourceAdder` passes
// `unnestElementStructFields(scope, j)` and types the column RECORD, which is
// precisely why the same member reference answers OUTSIDE an EXISTS. The EXISTS
// site has `analyzer` and `p.md` in hand. Nothing in pkg/relational asserts the
// narrowing is intended.
//
// WHEN THAT SCOPE IS FIXED, CONVERT THIS TEST — assert the rows Java returns.
// Do NOT delete it: the arms below are the only thing standing between the fix
// and the unguarded arity site described next, and converting them is how the
// two land together.
//
// The refusal also happens to make an unguarded arity site unreachable today,
// which is why the site is booked rather than fixed:
//
// THE SITE. `bakeUnnestElementRefOrdinal` (cascades_translator.go) skips an
// unpinned MULTI-ACCESSOR reference — keyed `!SourceRelativeBaked()` — and,
// unlike its outer-leg sibling, sets no failure flag when it does. Its safety
// net `unnestExistsRefSurvivesUnbaked` then skips the same shape on the same key
// as "safe", on a comment asserting that multi-accessor nodes are
// machinery-owned. That reading is refuted — a user-written nested descent is
// ONE unpinned multi-accessor FieldValue — so on paper a nested ELEMENT
// reference would survive UNBAKED into the baked inner predicate while the net
// declares the tree clean, which is the silent direction.
//
// IT DOES NOT REPRODUCE, and the reason is the divergence above, one step
// upstream: the EXISTS scope carries no struct fields, so no such node ever
// reaches the bake. That is an accident of a bug, NOT a guarantee — which is
// why the site is booked rather than closed, and why fixing the scope must
// carry the bake fix with it. MEASURED, and the refusal does NOT discriminate
// nesting — `x.ek` and `x.d.dk` are refused alike:
//
//	SELECT 1 FROM u WHERE u.uk = x.ek     -> 42703: column "EK" does not exist
//	SELECT 1 FROM u WHERE u.uk = x.d.dk   -> 42703: column "DK" does not exist
//
// The arms below are what keep that from being a claim nobody re-checks:
//
//   - the channel is LIVE. A bare SCALAR element through the same buried-conjunct
//     path answers, so a green here is not a green from a shape that never
//     planned. Without this arm the whole test could pass because the family
//     stopped working, which reads identically to "the defect is unreachable".
//   - members DO resolve outside the EXISTS, so the refusal is EXISTS-scoped
//     rather than "struct element members do not work at all". A projection of
//     the nested member returns real values.
//   - both member forms are refused INSIDE the EXISTS, flat and nested, which is
//     what says the nested case is not specially broken — it is refused with its
//     flat twin, by a rule that never looks at arity.
//
// WHAT RE-ARMS THE SITE: either 42703 arm starting to answer — which is not a
// hypothetical future feature but an OWED REPAIR of the dropped argument at
// logical_predicate.go:9302. When it lands, a nested element reference reaches
// bakeUnnestElementRefOrdinal, is skipped without a flag, is waved through by
// unnestExistsRefSurvivesUnbaked, and survives unbaked. So the scope fix and the
// bake fix are one change: the two functions need the treatment ordinal_seed.go's
// bake got — resolve the root from Accessors[0] and fuse Accessors[1:], or
// decline with ok=false, never skip. Convert this test to assert Java's rows in
// the same commit; deleting it is how the two halves come apart.
func TestFDB_UnnestElementMemberInExistsDivergesFromJava(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_uelem")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_uelem")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE uelem_tmpl "+
		"CREATE TYPE AS STRUCT deep (dk BIGINT) "+
		"CREATE TYPE AS STRUCT elem (ek BIGINT, d deep) "+
		"CREATE TABLE t (id BIGINT, sarr BIGINT ARRAY, arr elem ARRAY, PRIMARY KEY(id)) "+
		"CREATE TABLE v (vid BIGINT, vk BIGINT, PRIMARY KEY(vid)) "+
		"CREATE TABLE u (uk BIGINT, PRIMARY KEY(uk))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uelem/s WITH TEMPLATE uelem_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_uelem?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, [100, 200], [(10, (100)), (20, (200))]), (2, [300], [(30, (300))])")
	mustExec(t, db, ctx, "INSERT INTO v VALUES (1, 7), (2, 8)")
	mustExec(t, db, ctx, "INSERT INTO u VALUES (100), (300)")

	run := func(q string) ([]string, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a sql.NullInt64
			if err := rows.Scan(&a); err != nil {
				return nil, err
			}
			if a.Valid {
				out = append(out, fmt.Sprint(a.Int64))
			} else {
				out = append(out, "NULL")
			}
		}
		return out, rows.Err()
	}

	// --- the channel is LIVE (anti-vacuity: a green must not come from a family
	// that stopped planning).
	const bareElem = "SELECT t.id FROM t JOIN v ON t.id = v.vid, t.sarr AS x " +
		"WHERE EXISTS (SELECT 1 FROM u WHERE u.uk = x) ORDER BY t.id"
	got, err := run(bareElem)
	if err != nil {
		t.Fatalf("the buried-conjunct element channel no longer plans, so every refusal "+
			"below is vacuous — this test would report the defect unreachable because "+
			"NOTHING runs.\n  %s\n  %v", bareElem, err)
	}
	if strings.Join(got, ",") != "1,2" {
		t.Errorf("bare scalar element through the buried conjunct = %v, want [1 2]", got)
	}

	// --- members DO resolve outside the EXISTS, so the refusal is EXISTS-scoped.
	const outsideExists = "SELECT x.d.dk FROM t, t.arr AS x ORDER BY x.d.dk"
	nested, err := run(outsideExists)
	if err != nil {
		t.Errorf("a NESTED element member must still resolve outside an EXISTS; if it "+
			"does not, the refusal below is about struct elements generally rather than "+
			"about the EXISTS scope.\n  %s\n  %v", outsideExists, err)
	} else if strings.Join(nested, ",") != "100,200,300" {
		t.Errorf("nested element projection = %v, want [100 200 300]", nested)
	}

	// --- and BOTH member forms are refused INSIDE the EXISTS, on the same rule.
	for _, tc := range []struct{ name, query, wantCol string }{
		{
			"flat member",
			"SELECT t.id FROM t JOIN v ON t.id = v.vid, t.arr AS x " +
				"WHERE EXISTS (SELECT 1 FROM u WHERE u.uk = x.ek) ORDER BY t.id",
			"EK",
		},
		{
			"nested member",
			"SELECT t.id FROM t JOIN v ON t.id = v.vid, t.arr AS x " +
				"WHERE EXISTS (SELECT 1 FROM u WHERE u.uk = x.d.dk) ORDER BY t.id",
			"DK",
		},
	} {
		rows, err := run(tc.query)
		if err == nil {
			t.Errorf("%s: an element member inside an EXISTS now RESOLVES (returned %v).\n  %s\n"+
				"GOOD NEWS, NOT A REGRESSION — this is the Java-conformant behaviour "+
				"(valid-identifiers.yamsql:221,226 return rows for both forms), so the "+
				"divergence this test pins has been closed. Two things follow.\n"+
				"  1. CONVERT this test: assert the rows rather than the refusal. Deleting it "+
				"removes the only sentinel on the arity site below.\n"+
				"  2. VERIFY the same change fixed bakeUnnestElementRefOrdinal and "+
				"unnestExistsRefSurvivesUnbaked. A NESTED element reference now reaches them; "+
				"both skip an unpinned multi-accessor node — the first without setting a "+
				"failure flag, the second while reporting the tree clean — so it survives "+
				"UNBAKED into the baked inner predicate. Resolve the root from Accessors[0] "+
				"and fuse Accessors[1:], or decline with ok=false; never skip.",
				tc.name, rows, tc.query)
			continue
		}
		if !strings.Contains(err.Error(), "42703") || !strings.Contains(err.Error(), tc.wantCol) {
			t.Errorf("%s: refused, but not by the pinned resolution rule (want 42703 naming %q).\n"+
				"  %s\n  %v\nA refusal that MOVED may no longer sit upstream of the element "+
				"bake, which is the only reason that site is unreachable.", tc.name, tc.wantCol, tc.query, err)
		}
	}
}
