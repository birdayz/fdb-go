package sqldriver_test

// RFC-173 W5 commit 4 — the §5 dual-window ORACLE differential for the
// GATHERED multi-source classes (the design-ruling MANDATORY pin: "the
// oracle's producer-context rebind must extend to the gathered N-way Explode
// leg"). Each query runs twice through the same engine — ordinal (normal)
// and NAME-MODEL ORACLE (SetNameModelOracle) — and must agree row-for-row.
//
// These classes cannot ride the dualwindow conformance corpus (SQL-seeded;
// no SQL array-literal form), so the differential lives here with
// record-store seeding. RED-FIRST: before the NLJ-side oracle rebind
// (recoverOracleDatumSpans + oracleSwapFusedDatum + the values raw-map
// pinned-bare arm), the gathered projections read NIL oracle-side and the
// spanning WHERE dropped every row — and the PLAIN 3-way class (PA/PB/PC
// below) was silently broken the same way, undetected because the corpus
// never partitioned a projection through a merge sub-product. Dies with the
// oracle in Slice 4.

import (
	"context"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// NOT parallel: flips the process-global oracle. Go runs it to completion
// before any t.Parallel() test resumes, so the flip cannot leak.
func TestFDB_RFC173W5_OracleDualWindow(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	b := metadata.NewSchemaTemplateBuilder().SetName("w5oracle")
	b.AddTable("WSRC", []metadata.ColumnSpec{
		metadata.NewColumnSpec("SID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("WARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
	}, []string{"SID"})
	b.AddTable("WAUX", []metadata.ColumnSpec{
		metadata.NewColumnSpec("XID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("WV", api.NewIntegerType(true), 2),
	}, []string{"XID"})
	b.AddTable("PA", []metadata.ColumnSpec{metadata.NewColumnSpec("AID", api.NewLongType(false), 1), metadata.NewColumnSpec("AV", api.NewLongType(true), 2)}, []string{"AID"})
	b.AddTable("PB", []metadata.ColumnSpec{metadata.NewColumnSpec("BID", api.NewLongType(false), 1), metadata.NewColumnSpec("BV", api.NewLongType(true), 2)}, []string{"BID"})
	b.AddTable("PC", []metadata.ColumnSpec{metadata.NewColumnSpec("CID", api.NewLongType(false), 1), metadata.NewColumnSpec("CV", api.NewLongType(true), 2)}, []string{"CID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()

	mkScalar := func(table, idc, vc string, id, v int64) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(idc)), protoreflect.ValueOfInt64(id))
		m.Set(d.Fields().ByName(protoreflect.Name(vc)), protoreflect.ValueOfInt64(v))
		return m
	}
	wsrcDesc := md.GetRecordType("WSRC").Descriptor
	src := dynamicpb.NewMessage(wsrcDesc)
	src.Set(wsrcDesc.Fields().ByName("SID"), protoreflect.ValueOfInt64(1))
	fd := wsrcDesc.Fields().ByName("WARR")
	list := src.NewField(fd).List()
	list.Append(protoreflect.ValueOfInt32(7))
	list.Append(protoreflect.ValueOfInt32(8))
	src.Set(fd, protoreflect.ValueOfList(list))
	wauxDesc := md.GetRecordType("WAUX").Descriptor
	mkAux := func(id int64, v int32) proto.Message {
		m := dynamicpb.NewMessage(wauxDesc)
		m.Set(wauxDesc.Fields().ByName("XID"), protoreflect.ValueOfInt64(id))
		m.Set(wauxDesc.Fields().ByName("WV"), protoreflect.ValueOfInt32(v))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{
			src, mkAux(1, 5), mkAux(2, 7), // WV=7 keeps EL>WV discriminating
			mkScalar("PA", "AID", "AV", 1, 10), mkScalar("PA", "AID", "AV", 2, 20),
			mkScalar("PB", "BID", "BV", 1, 15), mkScalar("PB", "BID", "BV", 2, 5),
			mkScalar("PC", "CID", "CV", 1, 100),
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, sql string) []string {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
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
				m, isMap := executor.RowValue(r).(map[string]any)
				if !isMap {
					// A non-map Datum stringifying to "" on BOTH sides would
					// make the differential vacuously green — fail loudly.
					t.Fatalf("query %q: non-map row Datum %T — the differential compares map rows only", sql, executor.RowValue(r))
				}
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, k+"="+unnestSprint(m[k]))
				}
				out = append(out, strings.Join(parts, "|"))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", sql, eerr)
		}
		sort.Strings(out)
		return out
	}

	queries := []string{
		// The gathered family (trailing, AT, spanning WHERE, ON-carrying
		// dotted, enclosed spanning — commits 1-3's classes).
		`SELECT "EL" FROM WSRC, WAUX, WSRC."WARR" AS "EL"`,
		`SELECT "EL", "O" FROM WSRC, WAUX, WSRC."WARR" AS "EL" AT "O"`,
		`SELECT "EL", "WV" FROM WSRC, WAUX, WSRC."WARR" AS "EL" WHERE "EL" > "WV"`,
		`SELECT WSRC."SID", WAUX."WV", "EL" FROM WSRC INNER JOIN WAUX ON WAUX."XID" = WSRC."SID", WSRC."WARR" AS "EL"`,
		`SELECT "EL", "WV" FROM WSRC, WSRC."WARR" AS "EL", WAUX WHERE "EL" > "WV"`,
		// The PLAIN 3-way merge-sub-product class (the same oracle machinery;
		// silently broken pre-fix with the corpus blind to it).
		`SELECT PA."AV", PB."BV" FROM PA, PB, PC WHERE PA."AV" > PB."BV"`,
		`SELECT "AV", "CV" FROM PA, PB, PC WHERE PA."AID" = PB."BID"`,
	}
	for _, q := range queries {
		ordinal := run(t, q)
		executor.SetNameModelOracle(true)
		oracle := run(t, q)
		executor.SetNameModelOracle(false)
		if len(ordinal) == 0 {
			t.Errorf("no ordinal rows for %q — the differential needs non-empty ground truth", q)
		}
		if strings.Join(ordinal, ";") != strings.Join(oracle, ";") {
			t.Errorf("DUAL-WINDOW MISMATCH %q\n  ordinal=%v\n  oracle =%v", q, ordinal, oracle)
		}
	}
}
