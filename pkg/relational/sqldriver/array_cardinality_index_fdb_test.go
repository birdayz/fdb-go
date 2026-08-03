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

// TestFDB_ArrayCardinalityIndex is the RFC-143 Phase 2 end-to-end proof: a
// CARDINALITY() index makes WHERE CARDINALITY(arr) = N / IS [NOT] NULL and
// ORDER BY CARDINALITY(arr) use INDEX scans, ported from Java's
// arrays-cardinality.yamsql index test block. Every subtest asserts the
// OPTIMIZATION fires via EXPLAIN (the plan is an IndexScan over the cardinality
// index, not a full Scan + PredicatesFilter / InMemorySort) AND that the rows
// are correct.
//
// The full chain is exercised: the index DDL builder (AddCardinalityIndex →
// CardinalityFunctionKeyExpression root), the KeyExpression→Value bridge
// (CardinalityValue(FieldValue) on both the candidate and query sides), and the
// reworked ordered-index-scan + predicate matching that bind by Value-tree
// equality rather than FieldValue-name strings.
//
// Data semantics follow the array WRITE representation: a NULLABLE array
// column stores the NullableArrayWrapper, so an empty array (present wrapper,
// empty list) stays distinct from NULL/unset — cardinality 0 vs NULL. A NOT
// NULL array column stays a flat repeated field, where empty is wire-
// indistinguishable from unset (NULL key). The index key agrees.
//
// Array literals are not expressible in SQL INSERT here, so rows are written
// via the record-store API (dynamicpb repeated fields), and the cardinality
// index is defined programmatically via AddCardinalityIndex — the same DDL
// metadata the SQL `AS SELECT CARDINALITY(...)` path produces (pinned
// separately in TestCardinalityIndexDDL_Metadata).
func TestFDB_ArrayCardinalityIndex(t *testing.T) {
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

	// TAB_IDX: nullable INT array, with a CARDINALITY() index.
	// TAB_IDX_NN: NOT NULL INT array, with a CARDINALITY() index.
	// TAB_PLAIN: a plain INTEGER column with a plain-field index — the
	//   no-regression control for the 6c ordered-index-scan rework.
	b := metadata.NewSchemaTemplateBuilder().SetName("cardidx")
	b.AddTable("TAB_IDX", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewIntegerType(false), 1),
		metadata.NewColumnSpec("INT_ARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
	}, []string{"ID"})
	b.AddCardinalityIndex("TAB_IDX", "TAB_IDX_CARD", "INT_ARR")

	b.AddTable("TAB_IDX_NN", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewIntegerType(false), 1),
		metadata.NewColumnSpec("INT_ARR", api.NewArrayType(api.NewIntegerType(false), false), 2),
	}, []string{"ID"})
	b.AddCardinalityIndex("TAB_IDX_NN", "TAB_IDX_NN_CARD", "INT_ARR")

	b.AddTable("TAB_PLAIN", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewIntegerType(false), 1),
		metadata.NewColumnSpec("V", api.NewIntegerType(true), 2),
	}, []string{"ID"})
	b.AddIndex("TAB_PLAIN", "TAB_PLAIN_V", []string{"V"}, false)

	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	idxDesc := md.GetRecordType("TAB_IDX").Descriptor
	nnDesc := md.GetRecordType("TAB_IDX_NN").Descriptor
	plainDesc := md.GetRecordType("TAB_PLAIN").Descriptor

	setIntArr := func(m *dynamicpb.Message, d protoreflect.MessageDescriptor, name string, vals []int32) {
		fd := d.Fields().ByName(protoreflect.Name(name))
		pvals := make([]protoreflect.Value, len(vals))
		for i, v := range vals {
			pvals[i] = protoreflect.ValueOfInt32(v)
		}
		setArrayField(m, fd, pvals...)
	}
	arrRec := func(d protoreflect.MessageDescriptor, id int32, arr []int32, set bool) proto.Message {
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("ID"), protoreflect.ValueOfInt32(id))
		if set {
			setIntArr(m, d, "INT_ARR", arr)
		}
		return m
	}
	plainRec := func(id int32, v int32, set bool) proto.Message {
		m := dynamicpb.NewMessage(plainDesc)
		m.Set(plainDesc.Fields().ByName("ID"), protoreflect.ValueOfInt32(id))
		if set {
			m.Set(plainDesc.Fields().ByName("V"), protoreflect.ValueOfInt32(v))
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
			// TAB_IDX: id0 NULL array, id1 empty, id2 size 1, id3 size 2.
			arrRec(idxDesc, 0, nil, false),
			arrRec(idxDesc, 1, []int32{}, true),
			arrRec(idxDesc, 2, []int32{101}, true),
			arrRec(idxDesc, 3, []int32{201, 202}, true),
			// TAB_IDX_NN: sizes 0/1/2 (no NULL row).
			arrRec(nnDesc, 1, []int32{}, true),
			arrRec(nnDesc, 2, []int32{101}, true),
			arrRec(nnDesc, 3, []int32{201, 202}, true),
			// TAB_PLAIN: V = 10, 20, 30, plus a NULL-V row (id4) for the
			// IS [NOT] NULL plain-field index null-range control.
			plainRec(1, 10, true),
			plainRec(2, 20, true),
			plainRec(3, 30, true),
			plainRec(4, 0, false),
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

	// queryOrdered plans + executes, returning (explain, rows in execution order).
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

	assertSetWithExplain := func(t *testing.T, sql string, wantRows []string, mustContain []string, mustNotContain []string) {
		t.Helper()
		explain, got := queryOrdered(t, sql)
		sort.Strings(got)
		want := append([]string(nil), wantRows...)
		sort.Strings(want)
		if !unnestEqualStrs(got, want) {
			t.Fatalf("query %q rows\n got=%v\nwant=%v\nplan=%s", sql, got, want, explain)
		}
		for _, sub := range mustContain {
			if !strings.Contains(explain, sub) {
				t.Fatalf("query %q EXPLAIN must contain %q (the optimization must fire), got: %s", sql, sub, explain)
			}
		}
		for _, sub := range mustNotContain {
			if strings.Contains(explain, sub) {
				t.Fatalf("query %q EXPLAIN must NOT contain %q (optimization did not fire — fell back), got: %s", sql, sub, explain)
			}
		}
	}

	assertOrderedWithExplain := func(t *testing.T, sql string, wantRows []string, mustContain []string, mustNotContain []string) {
		t.Helper()
		explain, got := queryOrdered(t, sql)
		if !unnestEqualStrs(got, wantRows) {
			t.Fatalf("ordered query %q rows\n got=%v\nwant=%v\nplan=%s", sql, got, wantRows, explain)
		}
		for _, sub := range mustContain {
			if !strings.Contains(explain, sub) {
				t.Fatalf("ordered query %q EXPLAIN must contain %q, got: %s", sql, sub, explain)
			}
		}
		for _, sub := range mustNotContain {
			if strings.Contains(explain, sub) {
				t.Fatalf("ordered query %q EXPLAIN must NOT contain %q, got: %s", sql, sub, explain)
			}
		}
	}

	// --- WHERE CARDINALITY(arr) = N → equality-range index scan. ---
	t.Run("where equals uses index (nullable)", func(t *testing.T) {
		// card=1 matches only id2 (the [101] array). The index must be used:
		// an IndexScan with an equality range, never a full Scan+PredicatesFilter.
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_IDX" WHERE CARDINALITY("INT_ARR") = 1`,
			[]string{"ID=2"},
			[]string{"IndexScan(TAB_IDX_CARD"},
			[]string{"Scan(TAB_IDX)", "PredicatesFilter"})
	})
	t.Run("where equals uses index (not null)", func(t *testing.T) {
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_IDX_NN" WHERE CARDINALITY("INT_ARR") = 2`,
			[]string{"ID=3"},
			[]string{"IndexScan(TAB_IDX_NN_CARD"},
			[]string{"Scan(TAB_IDX_NN)", "PredicatesFilter"})
	})

	// --- The EMPTY array on the NOT NULL column, through the INDEX. ---
	//
	// This is the axis where the index and the base table can silently
	// disagree. id1's array is EMPTY, so nothing is written for the repeated
	// field and the stored bytes are identical to an unset one. The WRITE side
	// derives the cardinality key from the array; the READ side materializes
	// the absent repeated field as [] because the column's type forbids NULL.
	// Both must land on 0 — an index that keyed NULL here would answer these
	// two queries differently from the same queries over a full scan, and the
	// covering variant would differ from the fetching one.
	t.Run("empty array on NOT NULL column counts 0 through the index", func(t *testing.T) {
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_IDX_NN" WHERE CARDINALITY("INT_ARR") = 0`,
			[]string{"ID=1"},
			[]string{"IndexScan(TAB_IDX_NN_CARD"},
			[]string{"Scan(TAB_IDX_NN)", "PredicatesFilter"})
	})
	t.Run("NOT NULL column has no NULL cardinality in the index", func(t *testing.T) {
		// The column cannot be NULL, so the index holds no NULL key at all.
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_IDX_NN" WHERE CARDINALITY("INT_ARR") IS NULL`,
			nil,
			[]string{"IndexScan(TAB_IDX_NN_CARD"},
			[]string{"Scan(TAB_IDX_NN)", "PredicatesFilter"})
	})
	t.Run("NOT NULL column is entirely NOT NULL through the index", func(t *testing.T) {
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_IDX_NN" WHERE CARDINALITY("INT_ARR") IS NOT NULL`,
			[]string{"ID=1", "ID=2", "ID=3"},
			[]string{"IndexScan(TAB_IDX_NN_CARD"},
			[]string{"Scan(TAB_IDX_NN)", "PredicatesFilter"})
	})

	// --- WHERE CARDINALITY(arr) IS [NOT] NULL → null-range index scan. ---
	t.Run("is null uses index null-range", func(t *testing.T) {
		// Only id0 (unset array) has a NULL cardinality key → the [null]
		// equality range. id1's [] counts 0 — empty array is distinct from NULL
		// through the NullableArrayWrapper (present wrapper, empty list) —
		// Java's semantics.
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_IDX" WHERE CARDINALITY("INT_ARR") IS NULL`,
			[]string{"ID=0"},
			[]string{"IndexScan(TAB_IDX_CARD"},
			[]string{"Scan(TAB_IDX)", "PredicatesFilter"})
	})
	t.Run("is not null uses index null-range", func(t *testing.T) {
		// empty array is distinct from NULL through the NullableArrayWrapper
		// (present wrapper, empty list) — Java's semantics: id1 (card 0) is
		// NOT NULL alongside the populated id2/id3.
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_IDX" WHERE CARDINALITY("INT_ARR") IS NOT NULL`,
			[]string{"ID=1", "ID=2", "ID=3"},
			[]string{"IndexScan(TAB_IDX_CARD"},
			[]string{"Scan(TAB_IDX)", "PredicatesFilter"})
	})

	// --- ORDER BY CARDINALITY(arr) ASC/DESC → ordered index scan (no sort). ---
	t.Run("order by asc uses index (no in-memory sort)", func(t *testing.T) {
		// Index order: NULL first (id0 unset), then card 0 (id1's []), 1, 2 —
		// ids 0, 1, 2, 3.
		assertOrderedWithExplain(t,
			`SELECT "ID" FROM "TAB_IDX" ORDER BY CARDINALITY("INT_ARR")`,
			[]string{"ID=0", "ID=1", "ID=2", "ID=3"},
			[]string{"IndexScan(TAB_IDX_CARD"},
			[]string{"InMemorySort", "Sort("})
	})
	t.Run("order by desc uses index reverse (no in-memory sort)", func(t *testing.T) {
		// REVERSE: card 2, 1, 0, then the NULL (unset) last — ids 3, 2, 1, 0.
		assertOrderedWithExplain(t,
			`SELECT "ID" FROM "TAB_IDX" ORDER BY CARDINALITY("INT_ARR") DESC`,
			[]string{"ID=3", "ID=2", "ID=1", "ID=0"},
			[]string{"IndexScan(TAB_IDX_CARD", "REVERSE"},
			[]string{"InMemorySort", "Sort("})
	})

	// --- Covering scan: SELECT "ID" is index-resident (id is the PK, carried
	//     in the index entry), so a covering index scan suffices. ---
	t.Run("covering index scan for index-resident projection", func(t *testing.T) {
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_IDX_NN" WHERE CARDINALITY("INT_ARR") = 1`,
			[]string{"ID=2"},
			[]string{"IndexScan(TAB_IDX_NN_CARD", "COVERING"},
			[]string{"Scan(TAB_IDX_NN)"})
	})

	// --- NO-REGRESSION CONTROL: a plain-field index + plain-field ORDER BY /
	//     WHERE must still bind to the index after the 6c rule rework. ---
	t.Run("plain-field WHERE still uses index (no regression)", func(t *testing.T) {
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_PLAIN" WHERE "V" = 20`,
			[]string{"ID=2"},
			[]string{"IndexScan(TAB_PLAIN_V"},
			[]string{"Scan(TAB_PLAIN)"})
	})
	t.Run("plain-field ORDER BY still uses index (no regression)", func(t *testing.T) {
		// id4 has NULL V, which sorts first in the index (ASC).
		assertOrderedWithExplain(t,
			`SELECT "ID" FROM "TAB_PLAIN" ORDER BY "V"`,
			[]string{"ID=4", "ID=1", "ID=2", "ID=3"},
			[]string{"IndexScan(TAB_PLAIN_V"},
			[]string{"InMemorySort", "Sort("})
	})
	t.Run("plain-field ORDER BY DESC still uses index reverse (no regression)", func(t *testing.T) {
		// REVERSE: V=30,20,10 then the NULL row (id4) last.
		assertOrderedWithExplain(t,
			`SELECT "ID" FROM "TAB_PLAIN" ORDER BY "V" DESC`,
			[]string{"ID=3", "ID=2", "ID=1", "ID=4"},
			[]string{"IndexScan(TAB_PLAIN_V", "REVERSE"},
			[]string{"InMemorySort", "Sort("})
	})

	// --- Plain-field nullable index + IS [NOT] NULL: the value-index null-range
	//     binding (the general, Java-aligned change cardinality required) must
	//     return correct rows via the index, not just the cardinality column. ---
	t.Run("plain-field IS NULL uses index null-range with correct rows", func(t *testing.T) {
		// Only id4 has a NULL V.
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_PLAIN" WHERE "V" IS NULL`,
			[]string{"ID=4"},
			[]string{"IndexScan(TAB_PLAIN_V"},
			[]string{"Scan(TAB_PLAIN)"})
	})
	t.Run("plain-field IS NOT NULL uses index null-range with correct rows", func(t *testing.T) {
		assertSetWithExplain(t,
			`SELECT "ID" FROM "TAB_PLAIN" WHERE "V" IS NOT NULL`,
			[]string{"ID=1", "ID=2", "ID=3"},
			[]string{"IndexScan(TAB_PLAIN_V"},
			[]string{"Scan(TAB_PLAIN)"})
	})
}
