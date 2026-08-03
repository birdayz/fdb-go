package sqldriver_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

// TestFDB_ArrayCardinality is the RFC-143 Phase 1 end-to-end proof: the
// `CARDINALITY(array) → INT` scalar function, ported from Java's
// arrays-cardinality.yamsql. It exercises the dedicated CardinalityValue
// (not the generic ScalarFunctionValue) reached by the by-name dispatch in
// expr.walkCardinality, covering:
//
//   - element count: [] → 0, [x] → 1, [x,y] → 2
//   - NULL array → NULL (a nullable array column with a NULL row)
//   - NOT NULL array column (no NULL row)
//   - non-array argument (CARDINALITY(scalar) / CARDINALITY(constant)) →
//     CANNOT_CONVERT_TYPE (the clean SQLSTATE 22000, NOT a silent nil)
//   - WHERE CARDINALITY(arr) = N (full-scan PredicatesFilter, correct rows)
//   - WHERE CARDINALITY(arr) IS [NOT] NULL (null-test predicate)
//   - ORDER BY CARDINALITY(arr) ASC/DESC (InMemorySort, exact order)
//   - result-set column TYPE = INTEGER (Java's Type.primitiveType(INT))
//   - EXPLAIN renders cardinality(...) (the Cascades path, not text fallback)
//
// The rows with array columns are written via the record-store API
// (dynamicpb repeated fields) so this test's fixtures are independent of
// the SQL INSERT path (which supports array literals — see
// TestFDB_ArrayLiteralInsertValues). Phase 1 has NO index support (that's Phase 2); every
// CARDINALITY query plans to a full SCAN here.
//
// Nested-struct arrays (CARDINALITY(struct.int_arr)) — a yamsql case — are
// not exercised here because the metadata builder cannot seed a STRUCT
// column ("unsupported DataType code STRUCT"); that path lands with struct
// column support / Phase 2's index-DDL work.
func TestFDB_ArrayCardinality(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	// DUMMY: a single scalar-only row, for the non-array CARDINALITY error
	// cases (CARDINALITY("ID") / CARDINALITY(1)). TAB1: nullable INT array.
	// TAB1_NN: NOT NULL INT array.
	b := metadata.NewSchemaTemplateBuilder().SetName("card")
	b.AddTable("DUMMY", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewIntegerType(false), 1),
	}, []string{"ID"})
	b.AddTable("TAB1", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewIntegerType(false), 1),
		metadata.NewColumnSpec("INT_ARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
	}, []string{"ID"})
	b.AddTable("TAB1_NN", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewIntegerType(false), 1),
		metadata.NewColumnSpec("INT_ARR", api.NewArrayType(api.NewIntegerType(false), false), 2),
	}, []string{"ID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	dummyDesc := md.GetRecordType("DUMMY").Descriptor
	tab1Desc := md.GetRecordType("TAB1").Descriptor
	tab1nnDesc := md.GetRecordType("TAB1_NN").Descriptor

	setIntArr := func(m *dynamicpb.Message, d protoreflect.MessageDescriptor, name string, vals []int32) {
		fd := d.Fields().ByName(protoreflect.Name(name))
		pvals := make([]protoreflect.Value, len(vals))
		for i, v := range vals {
			pvals[i] = protoreflect.ValueOfInt32(v)
		}
		setArrayField(m, fd, pvals...)
	}
	// tab1Rec builds a TAB1 record. arr=nil leaves the array field UNSET, the
	// wire representation of a NULL array. A nullable array column is stored as
	// the NullableArrayWrapper (setArrayField writes the wrapper), so an EMPTY
	// array (present wrapper, empty list) stays distinct from NULL (absent
	// wrapper); a NOT NULL array column stays a flat repeated field, where an
	// empty list is wire-indistinguishable from unset.
	tab1Rec := func(d protoreflect.MessageDescriptor, id int32, arr []int32) proto.Message {
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("ID"), protoreflect.ValueOfInt32(id))
		if arr != nil {
			setIntArr(m, d, "INT_ARR", arr)
		}
		return m
	}

	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		recs := []proto.Message{
			func() proto.Message {
				m := dynamicpb.NewMessage(dummyDesc)
				m.Set(dummyDesc.Fields().ByName("ID"), protoreflect.ValueOfInt32(1))
				return m
			}(),
			// TAB1: id0 NULL array, id1 empty, id2 one elem, id3 two elems.
			tab1Rec(tab1Desc, 0, nil),
			tab1Rec(tab1Desc, 1, []int32{}),
			tab1Rec(tab1Desc, 2, []int32{101}),
			tab1Rec(tab1Desc, 3, []int32{201, 202}),
			// TAB1_NN: NOT NULL, sizes 0/1/2 (no NULL row).
			tab1Rec(tab1nnDesc, 1, []int32{}),
			tab1Rec(tab1nnDesc, 2, []int32{101}),
			tab1Rec(tab1nnDesc, 3, []int32{201, 202}),
		}
		for _, r := range recs {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// queryOrdered plans + executes a SELECT, returning the "k=v|k=v" row
	// strings in EXECUTION order plus the EXPLAIN string.
	queryOrdered := func(t *testing.T, sql string) (string, []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		explain := plan.Explain()
		var out []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			cursor, cErr := executor.ExecutePlan(ctx, plan, store,
				executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cursor.Close()
			rows, rErr := executor.CollectAll(ctx, cursor)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				// SLOT order, not sorted map keys: the sorted form re-sorted a
				// permuted row to the identical string and had already lost any
				// duplicate output name last-wins.
				out = append(out, positionalNamedPipeSprint(r))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", sql, eerr)
		}
		return explain, out
	}

	query := func(t *testing.T, sql string) (string, []string) {
		t.Helper()
		explain, out := queryOrdered(t, sql)
		sort.Strings(out)
		return explain, out
	}

	assertRows := func(t *testing.T, sql string, want []string) string {
		t.Helper()
		explain, got := query(t, sql)
		sort.Strings(want)
		if !unnestEqualStrs(got, want) {
			t.Fatalf("query %q\n got=%v\nwant=%v\nplan=%s", sql, got, want, explain)
		}
		return explain
	}

	assertRowsOrdered := func(t *testing.T, sql string, want []string) string {
		t.Helper()
		explain, got := queryOrdered(t, sql)
		if !unnestEqualStrs(got, want) {
			t.Fatalf("ordered query %q\n got=%v\nwant=%v\nplan=%s", sql, got, want, explain)
		}
		return explain
	}

	// The CARDINALITY projection's row key is the projection's canonical explain
	// name (cardinality(...)), not a user alias, since the raw executor keys by
	// the computed value. cardOf extracts the single non-ID numeric/NULL value
	// per row so the set comparison is on (id, card) pairs.
	cardPairs := func(t *testing.T, sql string) []string {
		t.Helper()
		_, rows := queryOrdered(t, sql)
		got := make([]string, 0, len(rows))
		for _, r := range rows {
			var idPart, cardPart string
			for _, kv := range strings.Split(r, "|") {
				if strings.HasPrefix(kv, "ID=") {
					idPart = kv
					continue
				}
				// The cardinality column: prefer the explicit alias CARD if present,
				// else the canonical cardinality(...) key.
				if strings.HasPrefix(strings.ToUpper(kv), "CARD=") ||
					strings.Contains(strings.ToLower(kv), "cardinality(") {
					cardPart = kv
				}
			}
			val := "?"
			if i := strings.IndexByte(cardPart, '='); i >= 0 {
				val = cardPart[i+1:]
			}
			id := "?"
			if i := strings.IndexByte(idPart, '='); i >= 0 {
				id = idPart[i+1:]
			}
			got = append(got, "id="+id+",card="+val)
		}
		sort.Strings(got)
		return got
	}

	// --- Element-count semantics on the NOT NULL array column. ---
	//
	// A NOT NULL array column is a plain repeated proto field (no
	// NullableArrayWrapper), so an EMPTY array and an unset array ARE
	// wire-indistinguishable. That does not make them NULL: the column's TYPE
	// forbids NULL, so the read materializes an absent repeated field as the
	// EMPTY ARRAY and CARDINALITY counts 0. Java's own corpus asserts exactly
	// this over exactly this data (arrays-cardinality.yamsql:74-78 — `INSERT
	// INTO "tab1_nn" … VALUES (1, []), (2, [1]), (3, [1, 2])` then `SELECT
	// CARDINALITY("int_arr") FROM "tab1_nn"` → `unorderedResult: [{0}, {1},
	// {2}]`).
	//
	// The NULLABLE column TAB1 reaches the same answers by a different route:
	// it stores the wrapper, so [] is a present wrapper with an empty list and
	// a genuine NULL is an absent wrapper — see "count on nullable array
	// column" below, which is where a NULL count legitimately appears.
	t.Run("count on not-null array column", func(t *testing.T) {
		// [] → 0, {x} → 1, {x,y} → 2. A <nil> for id1 is the NOT NULL column
		// reading back as SQL NULL.
		want := []string{"id=1,card=0", "id=2,card=1", "id=3,card=2"}
		gotPairs := cardPairs(t, `SELECT "ID", CARDINALITY("INT_ARR") AS "CARD" FROM TAB1_NN`)
		sort.Strings(want)
		if !unnestEqualStrs(gotPairs, want) {
			t.Fatalf("got=%v want=%v", gotPairs, want)
		}
	})

	t.Run("explain renders cardinality and type is INTEGER", func(t *testing.T) {
		sql := `SELECT CARDINALITY("INT_ARR") FROM TAB1_NN`
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan: %v", perr)
		}
		ex := plan.Explain()
		// The Cascades path renders cardinality(<child>); the text fallback would
		// not. Asserting the rendered function pins the dedicated-Value path.
		if !strings.Contains(strings.ToLower(ex), "cardinality(") {
			t.Fatalf("explain missing cardinality(...): %s", ex)
		}
		// Phase 1: NO index — a full SCAN, not an ISCAN.
		if strings.Contains(ex, "ISCAN") {
			t.Fatalf("Phase 1 must full-scan, got index scan: %s", ex)
		}
		types := embedded.ResultColumnTypesForPlan(plan, md)
		if len(types) != 1 || types[0] != "INTEGER" {
			t.Fatalf("column types = %v, want [INTEGER]", types)
		}
	})

	// --- Counts on a nullable-array column (populated vs empty vs unset). ---
	t.Run("count on nullable array column", func(t *testing.T) {
		// id0 (array field never written) reads as NULL; id1 ([]) counts 0 —
		// empty array is distinct from NULL through the NullableArrayWrapper
		// (present wrapper, empty list) — Java's semantics. id2 → 1, id3 → 2.
		got := cardPairs(t, `SELECT "ID", CARDINALITY("INT_ARR") AS "CARD" FROM TAB1`)
		want := []string{
			"id=0,card=<nil>", "id=1,card=0", "id=2,card=1", "id=3,card=2",
		}
		sort.Strings(want)
		if !unnestEqualStrs(got, want) {
			t.Fatalf("got=%v want=%v", got, want)
		}
	})

	// --- Non-array argument → CANNOT_CONVERT_TYPE (the clean error). ---
	t.Run("non-array scalar column rejects with CANNOT_CONVERT_TYPE", func(t *testing.T) {
		_, perr := embedded.PlanRecordQueryWithMetadata(`SELECT CARDINALITY("ID") FROM DUMMY`, md, nil)
		if perr == nil {
			t.Fatal("expected error for CARDINALITY(scalar), got nil")
		}
		requireSQLSTATE(t, perr, api.ErrCodeCannotConvertType)
	})
	t.Run("non-array constant rejects with CANNOT_CONVERT_TYPE", func(t *testing.T) {
		_, perr := embedded.PlanRecordQueryWithMetadata(`SELECT CARDINALITY(1) FROM DUMMY`, md, nil)
		if perr == nil {
			t.Fatal("expected error for CARDINALITY(1), got nil")
		}
		requireSQLSTATE(t, perr, api.ErrCodeCannotConvertType)
	})

	// --- WHERE CARDINALITY(arr) = N (full-scan PredicatesFilter). ---
	t.Run("where cardinality equals N", func(t *testing.T) {
		plan := assertRows(t, `SELECT "ID" FROM TAB1 WHERE CARDINALITY("INT_ARR") = 1`, []string{
			"ID=2",
		})
		unnestMustContain(t, plan, "PredicatesFilter")
		unnestMustNotContain(t, plan, "ISCAN")
	})
	t.Run("where cardinality equals zero matches the empty array", func(t *testing.T) {
		// empty array is distinct from NULL through the NullableArrayWrapper
		// (present wrapper, empty list) — Java's semantics: id1's [] counts 0.
		assertRows(t, `SELECT "ID" FROM TAB1 WHERE CARDINALITY("INT_ARR") = 0`, []string{
			"ID=1",
		})
	})

	// --- WHERE CARDINALITY(arr) IS [NOT] NULL. ---
	// On the nullable column CARDINALITY is NULL only for the truly unset row
	// (id0): empty array is distinct from NULL through the NullableArrayWrapper
	// (present wrapper, empty list) — Java's semantics — so id1's [] counts 0
	// and is NOT NULL. This genuinely exercises the null-test predicate over a
	// CardinalityValue.
	t.Run("where cardinality IS NULL matches the unset array", func(t *testing.T) {
		plan := assertRows(t, `SELECT "ID" FROM TAB1 WHERE CARDINALITY("INT_ARR") IS NULL`, []string{
			"ID=0",
		})
		unnestMustContain(t, plan, "PredicatesFilter")
	})
	t.Run("where cardinality IS NOT NULL matches the present arrays", func(t *testing.T) {
		assertRows(t, `SELECT "ID" FROM TAB1 WHERE CARDINALITY("INT_ARR") IS NOT NULL`, []string{
			"ID=1", "ID=2", "ID=3",
		})
	})
	t.Run("where cardinality IS NULL on NOT NULL array matches nothing", func(t *testing.T) {
		// A NOT NULL array column is never NULL, so CARDINALITY over it is
		// never NULL either — not even for id1's EMPTY array, whose count is
		// 0. The flat repeated field makes empty and unset wire-identical,
		// but the TYPE forbids NULL, so the read materializes [] and the
		// count is 0. No row qualifies.
		assertRows(t, `SELECT "ID" FROM TAB1_NN WHERE CARDINALITY("INT_ARR") IS NULL`, nil)
	})
	t.Run("where cardinality IS NOT NULL on NOT NULL array matches every row", func(t *testing.T) {
		assertRows(t, `SELECT "ID" FROM TAB1_NN WHERE CARDINALITY("INT_ARR") IS NOT NULL`, []string{
			"ID=1", "ID=2", "ID=3",
		})
	})
	t.Run("where cardinality equals zero on NOT NULL array matches the empty row", func(t *testing.T) {
		// The positive form of the same fact: id1's [] counts 0, exactly as
		// the nullable column's [] does.
		assertRows(t, `SELECT "ID" FROM TAB1_NN WHERE CARDINALITY("INT_ARR") = 0`, []string{
			"ID=1",
		})
	})

	// --- ORDER BY CARDINALITY(arr) ASC/DESC (InMemorySort, exact order). ---
	t.Run("order by cardinality ascending", func(t *testing.T) {
		plan := assertRowsOrdered(t, `SELECT "ID" FROM TAB1_NN ORDER BY CARDINALITY("INT_ARR")`, []string{
			"ID=1", "ID=2", "ID=3", // cards 0,1,2 — id1's [] counts 0, not NULL
		})
		unnestMustContain(t, plan, "InMemorySort")
		unnestMustNotContain(t, plan, "ISCAN")
	})
	t.Run("order by cardinality descending", func(t *testing.T) {
		assertRowsOrdered(t, `SELECT "ID" FROM TAB1_NN ORDER BY CARDINALITY("INT_ARR") DESC`, []string{
			"ID=3", "ID=2", "ID=1", // cards 2,1,0
		})
	})
}
