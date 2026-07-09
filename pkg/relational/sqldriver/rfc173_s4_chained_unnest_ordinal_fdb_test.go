package sqldriver_test

import (
	"context"
	"fmt"
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
	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_RFC173S4_ChainedUnnestOrdinal is the RFC-173 S4 Slice 1 CERTIFICATE:
// the CHAINED lateral unnest (`FROM T4, T4.SARR AS X, X.SUB AS Y`) now takes its
// OWN ORDINAL result-value seed (unnestOrdinalSeed) instead of the name-model
// buildUnnestResultValue. The seed's OUTER positional run carries the first
// link's merged row [ID, SARR, SCARR, SUB, X] positionally, so the outer column
// ID resolves through the POSITIONAL seed — it no longer depends on the :1151
// enclosure keeping the chained row name-model.
//
// DISCRIMINATING data: each row's ID is distinct and its element's SUB values are
// distinct, so every (ID, Y) pair is unique. A mis-window (reading the outer
// column off the wrong slot) or a mis-bind (reading the wrong element) changes
// the pinned pairs — the certificate is a wrong-answer sentinel, not a
// smoke test.
//
// Proof the ORDINAL path fires (not the name-model fail-open): with the
// name-model fallback disabled in translateChainedUnnestJoin, every
// ordinal-eligible chained shape here still returns these EXACT rows (verified
// out-of-band during S4 Slice 1 bring-up). The decline probe below exercises the
// fail-open residual for a shape the ordinal gate rejects (the 3-link chain).
func TestFDB_RFC173S4_ChainedUnnestOrdinal(t *testing.T) {
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

	md := buildChainedUnnestMetadata(t)
	t4Desc := md.GetRecordType("T4").Descriptor
	sarrFD := t4Desc.Fields().ByName("SARR")
	elemDesc := sarrFD.Message()
	substructFD := elemDesc.Fields().ByName("SUBSTRUCT")
	elem2Desc := substructFD.Message()

	mkElem2 := func(deep ...int32) protoreflect.Value {
		m := dynamicpb.NewMessage(elem2Desc)
		dl := m.NewField(elem2Desc.Fields().ByName("DEEP")).List()
		for _, d := range deep {
			dl.Append(protoreflect.ValueOfInt32(d))
		}
		m.Set(elem2Desc.Fields().ByName("DEEP"), protoreflect.ValueOfList(dl))
		return protoreflect.ValueOfMessage(m)
	}
	mkElem := func(sub []int32, substruct ...protoreflect.Value) protoreflect.Value {
		m := dynamicpb.NewMessage(elemDesc)
		sl := m.NewField(elemDesc.Fields().ByName("SUB")).List()
		for _, s := range sub {
			sl.Append(protoreflect.ValueOfInt32(s))
		}
		m.Set(elemDesc.Fields().ByName("SUB"), protoreflect.ValueOfList(sl))
		ssl := m.NewField(substructFD).List()
		for _, ss := range substruct {
			ssl.Append(ss)
		}
		m.Set(substructFD, protoreflect.ValueOfList(ssl))
		return protoreflect.ValueOfMessage(m)
	}
	mkT4 := func(id int64, sarr ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		// The shadow scalar column SUB=777: a mis-bind reading the outer-row SUB
		// instead of the element's SUB array would surface 777s.
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(777))
		sl := m.NewField(sarrFD).List()
		for _, e := range sarr {
			sl.Append(e)
		}
		m.Set(sarrFD, protoreflect.ValueOfList(sl))
		return m
	}

	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		recs := []proto.Message{
			// ID=10: two elements. elem0 SUB[1,2] + SUBSTRUCT DEEP[7,8],[9];
			// elem1 SUB[3] + no SUBSTRUCT.
			mkT4(10,
				mkElem([]int32{1, 2}, mkElem2(7, 8), mkElem2(9)),
				mkElem([]int32{3}),
			),
			// ID=20: one element SUB[4,5,6] + SUBSTRUCT DEEP[100].
			mkT4(20, mkElem([]int32{4, 5, 6}, mkElem2(100))),
			// ID=30: one element with EMPTY SUB → the inner chained unnest is a
			// no-op (zero rows) for this owner element.
			mkT4(30, mkElem(nil)),
			// ID=40: EMPTY SARR → the outer unnest yields no rows at all.
			mkT4(40),
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

	queryRows := func(t *testing.T, sql string) (string, []map[string]any) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		explain := plan.Explain()
		var out []map[string]any
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
				m, _ := r.Datum.(map[string]any)
				out = append(out, m)
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", sql, eerr)
		}
		return explain, out
	}

	// assertNestedOrdinalShape pins the ORDINAL chained plan shape: nested
	// FlatMap-over-FlatMap (each link its own Explode-under-forEach), NEVER a
	// flat gathered select. This is the shape SelectMergeRule's chained-unnest
	// barrier preserves for BOTH row models — an ordinal chained first link must
	// stay nested (Java's generateCorrelatedFieldAccess nesting), because the
	// deferred ordinal compose direction cannot flatten it.
	assertNestedOrdinalShape := func(t *testing.T, explain string) {
		t.Helper()
		if !strings.Contains(explain, "FlatMap(outer=FlatMap(") {
			t.Fatalf("chained ordinal plan must be NESTED FlatMaps; plan=%s", explain)
		}
		if strings.Count(explain, "Explode(") != 2 {
			t.Fatalf("2-chain must have exactly 2 Explode legs; plan=%s", explain)
		}
	}

	collect := func(rows []map[string]any, cols ...string) []string {
		got := make([]string, 0, len(rows))
		for _, m := range rows {
			parts := make([]string, len(cols))
			for i, c := range cols {
				parts[i] = fmt.Sprintf("%v", m[c])
			}
			got = append(got, strings.Join(parts, "|"))
		}
		sort.Strings(got)
		return got
	}

	t.Run("2-chain (ID,Y) resolves the outer column through the ordinal seed", func(t *testing.T) {
		const q = `SELECT "ID", "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y"`
		explain, rows := queryRows(t, q)
		assertNestedOrdinalShape(t, explain)
		got := collect(rows, "ID", "Y")
		// Each (ID, Y) is unique: ID=10 → SUB {1,2,3}; ID=20 → SUB {4,5,6}.
		// ID=30 (empty SUB) and ID=40 (empty SARR) contribute NOTHING. A
		// mis-window on the outer column would scramble the IDs; a mis-bind on
		// the element would scramble Y.
		want := []string{"10|1", "10|2", "10|3", "20|4", "20|5", "20|6"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("2-chain ordinal rows\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("2-chain never surfaces the shadow outer column SUB=777", func(t *testing.T) {
		// `X.SUB` reads the ELEMENT's SUB array (via the ordinal collection's
		// ofOrdinal(owner)+name-suffix descent), NEVER the same-named outer-row
		// scalar SUB=777. A collapsed root read would surface 777.
		_, rows := queryRows(t, `SELECT "ID", "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y"`)
		for _, m := range rows {
			if fmt.Sprintf("%v", m["Y"]) == "777" {
				t.Fatalf("x.SUB read the shadow outer column SUB=777: %v", m)
			}
		}
	})

	t.Run("root shadow: first unnest alias collides with the outer SUB=777 column, roots at the ELEMENT slot", func(t *testing.T) {
		// The first unnest is aliased AS SUB — the SAME name as the outer scalar
		// column SUB=777. The chained collection must root at the ELEMENT'S SLOT in
		// the merged row (after the outer columns), NOT the first name match: a
		// FieldIndex("SUB") lookup returns the OUTER SUB=777 (earlier in the row),
		// rooting the second Explode at a scalar → wrong value / runtime failure.
		// With the explicit element-slot root, SUB.SUB descends the element's SUB
		// array and answers the SAME rows as the X-aliased 2-chain.
		const q = `SELECT "ID", "Y" FROM T4, T4."SARR" AS "SUB", "SUB"."SUB" AS "Y"`
		explain, rows := queryRows(t, q)
		assertNestedOrdinalShape(t, explain)
		got := collect(rows, "ID", "Y")
		want := []string{"10|1", "10|2", "10|3", "20|4", "20|5", "20|6"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("root-shadow chained rows (must ignore the shadow SUB=777)\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("WITH ORDINALITY inner ordinal is 1-based per owner element", func(t *testing.T) {
		const q = `SELECT "ID", "Y", "O" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" AT "O"`
		explain, rows := queryRows(t, q)
		assertNestedOrdinalShape(t, explain)
		got := collect(rows, "ID", "Y", "O")
		// ID=10 elem0 SUB[1,2] → O 1,2; elem1 SUB[3] → O 1. ID=20 SUB[4,5,6] → O 1,2,3.
		want := []string{"10|1|1", "10|2|2", "10|3|1", "20|4|1", "20|5|2", "20|6|3"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("AT-ordinal chained ordinal rows\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("WITH ORDINALITY at BOTH links binds independent ordinals", func(t *testing.T) {
		const q = `SELECT "ID", "Y", "OX", "OY" FROM T4, T4."SARR" AS "X" AT "OX", "X"."SUB" AS "Y" AT "OY"`
		_, rows := queryRows(t, q)
		got := collect(rows, "ID", "Y", "OX", "OY")
		// ID=10: elem OX=1 (SUB 1,2 → OY 1,2); elem OX=2 (SUB 3 → OY 1).
		// ID=20: elem OX=1 (SUB 4,5,6 → OY 1,2,3).
		want := []string{
			"10|1|1|1", "10|2|1|2", "10|3|2|1",
			"20|4|1|1", "20|5|1|2", "20|6|1|3",
		}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("AT-both-links ordinal rows\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("SELECT star column labels come from the ordinal seed RC", func(t *testing.T) {
		// The ordinal seed's RC field order [ID, SARR, SCARR, SUB, X, Y] IS the
		// star-expanded result column order — outer columns positionally then the
		// two chained element bindings. This root select (no projection wrapper)
		// implements only because SelectMergeRule's barrier keeps the ordinal
		// first link nested.
		const q = `SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y"`
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", q, perr)
		}
		got := embedded.ResultColumnLabelsForPlan(plan, md)
		want := []string{"ID", "SARR", "SCARR", "SUB", "X", "Y"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("SELECT * columns\n got=%v\nwant=%v", got, want)
		}
		// And it must actually EXECUTE (the root-select implementation regression
		// the SelectMergeRule barrier closed).
		_, rows := queryRows(t, q)
		if len(rows) != 6 {
			t.Fatalf("SELECT * ordinal chained: got %d rows, want 6", len(rows))
		}
	})

	// A chained unnest under an outer-column or element WHERE now ORDINALIZES (the coarse
	// chainedUnnestUnderFilter decline is retired) — its rows + end-to-end plan placement are
	// pinned in TestFDB_RFC173S4_FilteredChained. This cert keeps the UNFILTERED ordinal path
	// and the genuine (non-filter) declines below.

	t.Run("DECLINE: 3-link chain fails open to name-model and still answers", func(t *testing.T) {
		// The 3-link chain `… X.SUBSTRUCT AS Y, Y.DEEP AS Z` has a first link
		// (`(T4 ⋈ SARR) ⋈ SUBSTRUCT`) whose BASE is itself an unnest-right join —
		// clusterArity poison — so the ordinal gate REJECTS it and it fails open
		// to the name-model residual. It must STILL answer correctly.
		const q = `SELECT "ID", "Z" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y", "Y"."DEEP" AS "Z"`
		_, rows := queryRows(t, q)
		got := collect(rows, "ID", "Z")
		// ID=10 elem0 SUBSTRUCT[DEEP 7,8],[DEEP 9]; ID=20 elem SUBSTRUCT[DEEP 100].
		want := []string{"10|7", "10|8", "10|9", "20|100"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("3-chain name-model fallback rows\n got=%v\nwant=%v", got, want)
		}
	})
}
