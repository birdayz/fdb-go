package sqldriver_test

// Pins the ResultSet column-type metadata of a NESTED struct-member projection
// whose leaf type deriveProjectionColumnDef cannot state from the value alone.
//
// That is the one shape which reaches deriveColumnsFromProjection's
// `TypeName == "" || == "UNKNOWN"` fall-through with a MULTI-accessor reference,
// and the fall-through is `innerByName[fv.Field]` — a lookup keyed by the
// reference's display NAME. Every leaf whose type the value can state types
// itself out of the gate before the lookup runs, so reaching it needs a leaf kind
// cascadesTypeName has no name for AND no descriptor to fall back on. A struct
// MEMBER always lacks the descriptor half: descriptorForColumn matches by BARE
// name across the join-leaf descriptors and a member is not a top-level field of
// any of them. ARRAY and BYTES supplied the other half.
//
// TWO SEPARATE DEFECTS MET HERE, and the test carries an arm for each because
// each one alone was enough to produce a wrong answer:
//
//  1. The MINT. fuseNestedAccessors named the fused reference after the struct
//     ROOT, so the fall-through looked up a genuinely different column of a
//     different type and found it. Over a base scan the hit was then discarded by
//     the arm's own `ic.TypeName != "UNKNOWN"` guard (a scan derives a struct
//     column UNKNOWN), which is why the whole suite stayed green with it present;
//     put a PROJECTION underneath and the guard passes, and
//     `SELECT q.s.vals FROM (SELECT s FROM t) AS q` reported STRUCT for a BIGINT
//     ARRAY member. MEASURED, not inferred.
//  2. The TYPE DERIVATION. cascadesTypeName had no ARRAY and no BYTES arm, so a
//     member of either kind reported UNKNOWN where the IDENTICAL column at top
//     level reported the element type / BINARY. One column, two answers,
//     depending only on whether it was reached through a struct.
//
// Fixing (2) also removes (1)'s precondition — with the leaf type stateable the
// fall-through is never entered for these shapes — so this test alone does NOT
// discriminate a mint regression; that invariant is pinned directly by
// TestFusedNestedReferenceIsNamedAfterItsLeaf in the expr package, which is
// where it belongs. What this test guards is the USER-VISIBLE answer, and it
// goes red if either fix regresses on its own (both directions measured).
//
// The top-level arms are the controls: they say the nested answers are the
// engine's answers for those column kinds rather than nested-specific accidents.
// The array answer is CQ-74's truncation (the bare element type) — when the
// driver learns to carry the array type, both arms move together.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_NestedArrayLeafDoesNotInheritTheStructRootsMetadata(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nlmeta")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nlmeta")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nlmeta "+
			"CREATE TYPE AS STRUCT elt (q BIGINT, r STRING) "+
			"CREATE TYPE AS STRUCT sarr (vals BIGINT ARRAY, label STRING, bin BYTES, structs elt ARRAY) "+
			"CREATE TABLE t (id BIGINT, s sarr, top BIGINT ARRAY, topbin BYTES, "+
			"topstructs elt ARRAY, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nlmeta/s WITH TEMPLATE nlmeta")
	dsn := fmt.Sprintf("fdbsql:///testdb_nlmeta?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// One row is enough: the assertion is on metadata, which the engine derives
	// from the plan and not from the data.
	if _, e := db.ExecContext(ctx,
		"INSERT INTO t (id, s, top, topbin, topstructs) VALUES "+
			"(1, ([1, 2], 'a', ?, [(5, 'z')]), [7, 8], ?, [(6, 'w')])",
		[]byte{0xab}, []byte{0xcd}); e != nil {
		t.Fatalf("insert: %v", e)
	}

	probe := func(q string) (name, typeName string) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cts, err := rows.ColumnTypes()
		if err != nil {
			t.Fatalf("ColumnTypes %q: %v", q, err)
		}
		if len(cts) != 1 {
			t.Fatalf("query %q: %d columns, want 1", q, len(cts))
		}
		return cts[0].Name(), cts[0].DatabaseTypeName()
	}

	// The control: a TOP-LEVEL array column. Whatever the engine reports here is
	// the engine's answer for "a column whose type is an array".
	topName, topType := probe("SELECT top FROM t")
	if topName != "TOP" {
		t.Errorf("SELECT top: column name = %q, want TOP", topName)
	}

	// The struct ROOT, so the wrong-bind target's own metadata is a MEASURED
	// value in this test rather than an assumed one.
	rootName, rootType := probe("SELECT s FROM t")
	if rootName != "S" {
		t.Errorf("SELECT s: column name = %q, want S", rootName)
	}

	// The subject. A wrong bind is visibly wrong because rootType is asserted
	// distinct from topType just below: the nested leaf can only report one of
	// them, and which one says which column the fall-through looked up.
	leafName, leafType := probe("SELECT s.vals FROM t")
	if leafName != "VALS" {
		t.Errorf("SELECT s.vals: column name = %q, want VALS (the LEAF, not the struct root S)", leafName)
	}

	// The discriminator. Without this the two arms could agree by coincidence
	// and the leaf assertion would prove nothing.
	if rootType == topType {
		t.Fatalf("the struct root and a top-level array report the same type name %q, "+
			"so this test can no longer tell which column `s.vals` inherited from — "+
			"give the struct member a leaf type that differs from the struct's own",
			rootType)
	}
	if leafType == rootType {
		t.Errorf("SELECT s.vals: DatabaseTypeName = %q — the STRUCT ROOT's type (S reports %q). "+
			"The projection's fall-through looked the reference up by its display name and "+
			"found the enclosing struct column, so an ARRAY member is reported as the struct. "+
			"Want the array answer %q that `SELECT top` gives.",
			leafType, rootType, topType)
	}
	if leafType != topType {
		t.Errorf("SELECT s.vals: DatabaseTypeName = %q, want %q — the same answer a TOP-LEVEL "+
			"array column gives. A nested array leaf and a top-level array column are the same "+
			"kind of column and must report the same metadata.",
			leafType, topType)
	}

	// The SECOND leaf type valueTypeName had no name for. Arrays and BYTES are
	// the two, and they are asserted together because fixing only the one a test
	// happens to name leaves the other reporting UNKNOWN for the same reason.
	binName, binType := probe("SELECT s.bin FROM t")
	if binName != "BIN" {
		t.Errorf("SELECT s.bin: column name = %q, want BIN", binName)
	}
	_, topBinType := probe("SELECT topbin FROM t")
	if binType != topBinType {
		t.Errorf("SELECT s.bin: DatabaseTypeName = %q, want %q — the answer a TOP-LEVEL "+
			"BYTES column gives. A struct member has no stored descriptor to resolve "+
			"against (descriptorForColumn matches BARE names against join-leaf descriptors), "+
			"so its type name can only come from the value's own type; a leaf kind that "+
			"derivation cannot name falls through to UNKNOWN.",
			binType, topBinType)
	}

	// THE REPEATED-MESSAGE SUB-CASE OF THE ARRAY ARM, which the other arms do not
	// drive. `cascadesTypeName`'s ARRAY arm RECURSES into the element type, so its
	// answer is only as settled as the element's — and the element kinds it can
	// meet are not all covered by an array-of-scalar test. For a STRUCT array the
	// recursion lands on the RECORD arm and yields "STRUCT" where, before the
	// ARRAY arm existed, the value fell to "" and then to UNKNOWN. That is a
	// changed answer with nothing asserting it, which is a new branch shipping
	// untested however reasonable it looks.
	//
	// The paired assertion is the same one the `bin`/`topbin` pair makes and for
	// the same reason: a struct-array MEMBER and a struct-array TOP-LEVEL COLUMN
	// are the same kind of column, so they must report the same metadata.
	//
	// DO NOT "SIMPLIFY" THIS ARM DOWN TO THAT PAIRED CHECK. Here — unlike in the
	// arms above — the equality is NOT the detector, and the explicit "STRUCT"
	// assertion below is not belt-and-braces. It is the SOLE detector, MEASURED:
	// disable `cascadesTypeName`'s ARRAY arm and BOTH sides fall to UNKNOWN
	// together, so the equality still HOLDS and the paired check passes the
	// mutation silently. Only the value assertion goes red.
	//
	// THE RULE, because this is not a quirk of one arm: A PAIR WHOSE TWO SIDES
	// SHARE A DERIVATION CANNOT BE CHECKED BY COMPARING THEM TO EACH OTHER. The
	// pairs above are sound STRUCTURALLY rather than luckily — each straddles the
	// descriptor/value boundary, so its two sides cannot fail in lockstep:
	//
	//	pair                  nested side          top-level side              lockstep?
	//	vals / top            ARRAY -> Long        protoKindToTypeName(Int64)  no
	//	bin / topbin          BYTES                protoKindToTypeName(Bytes)  no
	//	structs / topstructs  ARRAY -> RECORD      *the same* ARRAY -> RECORD  YES
	//
	// The last row is the one that matters and the reason is specific:
	// protoKindToTypeName has NO MessageKind case (it falls to `default` ->
	// "UNKNOWN"), so a repeated MESSAGE cannot be named from the descriptor at
	// all. The top-level twin therefore FALLS THROUGH to the value derivation and
	// lands on the very recursion the nested member uses. Both sides lose it
	// together, and only an assertion on the VALUE can see that.
	//
	// MEASURED, both directions, with the ARRAY arm disabled: the `vals`/`top`
	// equality FIRES (nested UNKNOWN vs top-level BIGINT), and the
	// `structs`/`topstructs` equality does NOT — only the value check below.
	// Deleting that check is exactly the class of edit this change exists to
	// prevent, and the arm would still look like a well-formed paired test
	// afterwards.
	structsName, structsType := probe("SELECT s.structs FROM t")
	if structsName != "STRUCTS" {
		t.Errorf("SELECT s.structs: column name = %q, want STRUCTS (the LEAF)", structsName)
	}
	topStructsName, topStructsType := probe("SELECT topstructs FROM t")
	if topStructsName != "TOPSTRUCTS" {
		t.Errorf("SELECT topstructs: column name = %q, want TOPSTRUCTS", topStructsName)
	}
	if structsType != topStructsType {
		t.Errorf("SELECT s.structs: DatabaseTypeName = %q, but the TOP-LEVEL twin "+
			"`topstructs` (the same `elt ARRAY` type) reports %q. One column kind, two "+
			"answers, decided only by whether it was reached through a struct — the exact "+
			"divergence the ARRAY and BYTES arms were added to remove, reappearing in the "+
			"ARRAY arm's REPEATED MESSAGE sub-case.\n"+
			"\tBOTH sides reach cascadesTypeName's recursion into the element type "+
			"(ARRAY -> RECORD -> \"STRUCT\"), which is why they normally agree. The "+
			"top-level twin gets there by FALLING THROUGH the descriptor path, not by "+
			"skipping it: protoKindToTypeName has no MessageKind case at all, so a repeated "+
			"message takes its `default` and answers UNKNOWN, and the value derivation is "+
			"what actually names the column. So a divergence here is NOT a disagreement "+
			"between the descriptor and the value — look for one side having stopped "+
			"falling through, not for protoFieldTypeName having learned a new answer.",
			structsType, topStructsType)
	}
	// THE SOLE DETECTOR FOR THIS ARM — see the block above before touching it.
	// The paired equality cannot catch a regression here because both sides share
	// a derivation and fall to UNKNOWN together. MEASURED at "STRUCT" on both
	// sides: the element type, so the array truncation (CQ-74) is applied
	// consistently for a MESSAGE element and not only for scalar ones.
	if structsType != "STRUCT" {
		t.Errorf("a struct-array column reports DatabaseTypeName %q on BOTH sides, want "+
			"STRUCT.\n"+
			"\tTHIS ASSERTION IS THE ONLY ONE THAT CAN FAIL HERE: the paired check against "+
			"`topstructs` above still HOLDS in this state, because both sides reach "+
			"cascadesTypeName's ARRAY arm and lose it together (protoKindToTypeName has no "+
			"MessageKind case, so a repeated MESSAGE answers UNKNOWN from the descriptor "+
			"and the top-level twin falls through to the value's own type just as the "+
			"nested member does). UNKNOWN on both sides is precisely the pre-ARRAY-arm "+
			"behaviour this arm exists to detect.\n"+
			"\tIf the driver has learned to carry the array type, this and the `vals`/`top` "+
			"pair move together.", structsType)
	}

	// The arm that showed the mint defect producing a wrong answer rather than a
	// swallowed near-miss: a PROJECTION under the reference derives the struct
	// column as STRUCT (a stated type), so the fall-through's
	// `ic.TypeName != "UNKNOWN"` guard no longer discards the wrong lookup's hit.
	// Measured at STRUCT for a BIGINT ARRAY member before the fixes.
	derivedName, derivedType := probe("SELECT q.s.vals FROM (SELECT s FROM t) AS q")
	if derivedName != "VALS" {
		t.Errorf("derived-table nested leaf: column name = %q, want VALS", derivedName)
	}
	if derivedType == rootType {
		t.Errorf("SELECT q.s.vals FROM (SELECT s FROM t) AS q: DatabaseTypeName = %q — "+
			"the ENCLOSING STRUCT's type (SELECT s reports %q) for a BIGINT ARRAY member. "+
			"The projection's UNKNOWN-type fall-through looked the reference up by its "+
			"display name; naming a fused nested reference after its struct ROOT makes that "+
			"lookup hit a different column of a different type.",
			derivedType, rootType)
	}
	// The leaf must land on the same answer the un-nested reference gives, so
	// the assertion above cannot be satisfied by degrading it to something else
	// that merely differs from the struct.
	if derivedType != leafType {
		t.Errorf("SELECT q.s.vals through a derived table: DatabaseTypeName = %q, but the same "+
			"member read directly reports %q. One reference, two answers.",
			derivedType, leafType)
	}
}
