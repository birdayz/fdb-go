package sqldriver_test

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
	"fdb.dev/pkg/relational/core/embedded"
	rquery "fdb.dev/pkg/relational/core/query"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestFDB_RFC173Slice3E1a(t *testing.T) {
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
	md := slice3B2bMetadata(t)

	mkA := func(aid, k int64, vals ...int32) proto.Message {
		d := md.GetRecordType("A").Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("AID"), protoreflect.ValueOfInt64(aid))
		m.Set(d.Fields().ByName("K"), protoreflect.ValueOfInt64(k))
		fd := d.Fields().ByName("ARR")
		l := m.NewField(fd).List()
		for _, v := range vals {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(l))
		return m
	}
	mk1 := func(table, f string, v int64) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(f)), protoreflect.ValueOfInt64(v))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{
			mkA(1, 100, 7, 8),
			mk1("B", "BID", 2),
			mk1("EE", "CK", 100),
			mk1("EEV", "VK", 7),
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	runQ := func(t *testing.T, sql string) ([]string, string, error) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			return nil, "", perr
		}
		explain := plan.Explain()
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
				out = append(out, unnestSprint(r.Datum))
			}
			return nil, nil
		})
		sort.Strings(out)
		return out, explain, eerr
	}

	const from = `FROM A, B, A."ARR" AS "X"`
	// pin: the INNER cluster under EXISTS ordinalizes and answers correctly. The
	// plan must NOT flatten to the SARG-losing N-way existential wrap (R1), and
	// the dup-named A.K/B.K legs must be DISAMBIGUATED by the alias-aware per-leg
	// bake — a name-model or alias-count-blind bind conflates them (RED rows).
	pin := func(name, sql string, want ...string) {
		t.Run(name, func(t *testing.T) {
			rows, plan, err := runQ(t, sql)
			if err != nil {
				t.Fatalf("%q: %v", sql, err)
			}
			sort.Strings(want)
			if strings.Join(rows, ",") != strings.Join(want, ",") {
				t.Fatalf("rows = %v, want %v\n  plan: %s", rows, want, plan)
			}
			if strings.Contains(plan, "NWayJoinWithExistential") {
				t.Errorf("R1: plan flattened to the SARG-losing N-way existential wrap\n  %s", plan)
			}
		})
	}

	// EXISTS on the PRESENT leg (A.K=100 matches EE) → {7,8}. A wrong-leg bind
	// reading B.K=NULL would give {} (RED).
	pin("faceB_AK", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, "map[X:7]", "map[X:8]")
	// EXISTS on the NULL leg (B.K=NULL → no match) → {}. A wrong-leg bind reading
	// A.K=100 (matches) would give {7,8} (RED) — the dup-named discriminator.
	pin("faceB_BK", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`)
	// NOT EXISTS on the NULL leg → keep → {7,8}; a wrong-leg bind (A.K matches)
	// would drop → {} (RED).
	pin("faceB_notBK", `SELECT "X" `+from+` WHERE NOT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`, "map[X:7]", "map[X:8]")
	// ELEMENT correlation EEV.VK = X → only element 7 matches EEV(VK=7) → {7}. An
	// unbaked element ref (mis-resolved over the NLJ layout) gives {} (RED).
	pin("faceB_element", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`, "map[X:7]")
	// a WHERE conjunct alongside the EXISTS, both on the present leg → {7,8}.
	pin("faceB_conj_and_exists", `SELECT "X" `+from+` WHERE A."AID" = 1 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, "map[X:7]", "map[X:8]")
}

// TestRFC173Slice3E1aCensus pins that the E-1a INNER cluster under EXISTS fires
// ZERO name-model producers (the ordinal gather owns it) — the model
// discriminator the row pins cannot be. DELIBERATELY NOT t.Parallel(): the
// producer census observer is process-global, so it must run in Go's serial
// phase where no sibling translation is in flight (see the B2-B census twin).
func TestRFC173Slice3E1aCensus(t *testing.T) { //nolint:paralleltest // process-global observer, must be serial
	md := slice3B2bMetadata(t)
	const from = `FROM A, B, A."ARR" AS "X"`
	countProducers := func(sql string) int {
		n := 0
		rquery.SetProducerCensusObserver(func(rquery.ProducerCensusRecord) { n++ })
		defer rquery.SetProducerCensusObserver(nil)
		if _, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil); err != nil {
			t.Fatalf("plan %q: %v", sql, err)
		}
		return n
	}
	// The admitted single-esq INNER cluster shapes fire ZERO name-model
	// producers (the gather owns them). Before E-1a these fired the P5 unnest
	// producer; after, the alias-aware ordinal bake replaces it.
	for _, tc := range []struct{ name, sql string }{
		{"inner_AK", `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"inner_BK", `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`},
		{"inner_element", `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
	} {
		if n := countProducers(tc.sql); n != 0 {
			t.Fatalf("%s: fired %d name-model producer(s), want 0 (the gather must own it)", tc.name, n)
		}
	}
}
