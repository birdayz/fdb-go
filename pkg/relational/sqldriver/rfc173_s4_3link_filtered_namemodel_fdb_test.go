package sqldriver_test

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/core/embedded"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestFDB_RFC173S4_ThreeLinkFilteredNameModel is the Slice-2c regression sentinel for the
// SCOPE boundary the slice introduces: a FILTERED 3+-link chain declines to name-model (via
// clusterArity poison) and must answer CORRECTLY — the positional bake is ORDINAL-seed only.
// the first cut applied the positional bake to the name-model fallback (gated on
// isChainedUnnest, not on whether the seed ordinalized): a mixed-inner-ref clause (`t.id = z`,
// `t.id=1 OR z=20`) then rewrote outer refs to `ofOrdinal(QOV(Y))` against the NAME-keyed row →
// `DEEP not resolvable, ordinal -1` malformed plan. The `ordinalSeed` discriminator routes a
// name-model seed to the NAME-KEY rebase. This pins the previously-unprobed axis.
func TestFDB_RFC173S4_ThreeLinkFilteredNameModel(t *testing.T) {
	t.Parallel()
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

	md := buildChainedUnnestMetadata(t)
	t4Desc := md.GetRecordType("T4").Descriptor
	sarrFD := t4Desc.Fields().ByName("SARR")
	elemDesc := sarrFD.Message()
	substructFD := elemDesc.Fields().ByName("SUBSTRUCT")
	elem2Desc := substructFD.Message()

	mkElem2 := func(deep ...int32) protoreflect.Value {
		m := dynamicpb.NewMessage(elem2Desc)
		m.Set(elem2Desc.Fields().ByName("LEAF"), protoreflect.ValueOfInt64(0))
		dl := m.NewField(elem2Desc.Fields().ByName("DEEP")).List()
		for _, d := range deep {
			dl.Append(protoreflect.ValueOfInt32(d))
		}
		m.Set(elem2Desc.Fields().ByName("DEEP"), protoreflect.ValueOfList(dl))
		return protoreflect.ValueOfMessage(m)
	}
	mkElem := func(sub []int32, substruct ...protoreflect.Value) protoreflect.Value {
		m := dynamicpb.NewMessage(elemDesc)
		m.Set(elemDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(0))
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
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(999))
		sl := m.NewField(sarrFD).List()
		for _, e := range sarr {
			sl.Append(e)
		}
		m.Set(sarrFD, protoreflect.ValueOfList(sl))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{
			mkT4(1, mkElem([]int32{1, 7}, mkElem2(11, 12), mkElem2(13))),
			mkT4(2, mkElem([]int32{9}, mkElem2(20))),
			mkT4(11, mkElem([]int32{5}, mkElem2(11))), // ID=11 matches Z=11 (straddle detector)
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	want := func(name, q string, exp []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("%s: plan error (must stay name-model, not decline the plan): %v\n  sql: %s", name, perr, q)
		}
		var out []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			cur, cErr := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cur.Close()
			rows, rErr := executor.CollectAll(ctx, cur)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				out = append(out, fmt.Sprintf("%v", r.Datum))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("%s: exec error (a positional bake against the name-model 3-link row strands here): %v\n  sql: %s", name, eerr, q)
		}
		sort.Strings(out)
		if fmt.Sprintf("%v", out) != fmt.Sprintf("%v", exp) {
			t.Fatalf("%s: rows = %v, want %v\n  sql: %s", name, out, exp, q)
		}
	}

	base := `FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y", "Y"."DEEP" AS "Z"`
	// MIXED-inner-ref OR: ID=1 → {11,12,13}; Z=20 from ID=2 → {20}. → {11,12,13,20}.
	want("threelink_mixed_OR", `SELECT "Z" `+base+` WHERE T4."ID" = 1 OR "Z" = 20`,
		[]string{"map[Z:11]", "map[Z:12]", "map[Z:13]", "map[Z:20]"})
	// STRADDLING over a 3-link — a single AND-free comparison mixing outer + the DEEPEST element
	// (the cleanest isolation of the name-key→positional regression). T4(11) Z=11 matches ID=11.
	want("threelink_straddling_match", `SELECT T4."ID", "Z" `+base+` WHERE T4."ID" = "Z"`,
		[]string{"map[T4.ID:11 Z:11]"})
	// AND with inner ref (AND-splits, no mixed clause): ID=1 ∩ Z>11 → {12,13}.
	want("threelink_and_inner", `SELECT "Z" `+base+` WHERE T4."ID" = 1 AND "Z" > 11`,
		[]string{"map[Z:12]", "map[Z:13]"})
	// PURE-outer OR (scan-pushable, never the positional bake): ID=1 OR ID=2 → {11,12,13,20}.
	want("threelink_pure_outer_OR", `SELECT "Z" `+base+` WHERE T4."ID" = 1 OR T4."ID" = 2`,
		[]string{"map[Z:11]", "map[Z:12]", "map[Z:13]", "map[Z:20]"})
	// A 2-CHAIN buried behind a trailing table (not the direct rightmost source) → declines to
	// name-model too, so a straddle mixing outer + the inner element must NOT positional-bake.
	// T4(1) Y∈{1,7}, ID=1=Y=1 → {Y:1} × 3 T4C rows = {1,1,1}.
	want("buried_2chain_straddle",
		`SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y", T4 AS "T4C" WHERE T4."ID" = "Y"`,
		[]string{"map[Y:1]", "map[Y:1]", "map[Y:1]"})
}
