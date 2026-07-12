package sqldriver_test

// RFC-173 Slice-3 Face A (B2-B) cert — the under-EXISTS LEFT/RIGHT box unnest
// at verdict None takes the GATHERED ordinal cluster (admitted by
// unnestExistentialGatherOK / admitExistentialGather), zeroing the P4/P5
// name-model producers it fired before. The row pins prove correctness against
// real FDB (the dup-named A.K/B.K leg the qualified EXISTS correlation must
// disambiguate — a name-model conflates it); the census pins prove the model
// (admitted LEFT box → 0 producers; the INNER cluster, E-1a's class, stays
// name-model). R1 (SelectMergeRule flattening the gathered select into the
// SARG-losing N-way existential wrap) is asserted absent in every plan.

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
	rquery "fdb.dev/pkg/relational/core/query"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestFDB_RFC173Slice3B2bFaceA(t *testing.T) {
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

	// A(AID=1, K=100, ARR=[7,8]); B(BID=2, K=110) — A.K/B.K dup-named, the
	// leg the qualified EXISTS correlation must disambiguate. LEFT JOIN A,B on
	// AID=BID: 1≠2 → A row survives with B null-supplied (B.K=NULL).
	b := metadata.NewSchemaTemplateBuilder().SetName("s3s0")
	b.AddTable("A", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("B", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	// EE(CK): matches A.K=100 (CK=100) or B.K (never, B.K=NULL). The
	// leg-correlation discriminator.
	b.AddTable("EE", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CK", api.NewLongType(false), 1),
	}, []string{"CK"})
	// EEV(VK): matches an ELEMENT value (VK=7 ∈ ARR). The R2 element-correlation
	// discriminator, kept separate from EE so leg vs element stay independent.
	b.AddTable("EEV", []metadata.ColumnSpec{
		metadata.NewColumnSpec("VK", api.NewLongType(false), 1),
	}, []string{"VK"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()

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
			mk1("B", "BID", 2), // B.K left unset → NULL
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

	// run executes sql through the production planner, returning sorted rows
	// and the plan EXPLAIN.
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

	const from = `FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X"`
	// pin asserts the B2-B gathered path answers the admitted LEFT-box+EXISTS
	// shape correctly. The plan must NOT flatten to an N-way existential wrap
	// (R1: that would be the SARG-losing implementNWayJoinWithExistential).
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
				t.Errorf("R1: plan flattened to an N-way existential wrap (SARG-losing)\n  %s", plan)
			}
		})
	}

	// LEFT box + EXISTS on the PRESENT leg (A.K=100 matches EE) → {7,8}.
	pin("leftK_present", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")
	// EXISTS on the NULL-SUPPLIED leg (B.K=NULL → no match) → {}; a wrong-leg
	// bind reads A.K=100 (matches) → {7,8} RED — the dup-named discriminator.
	pin("rightK_nullsupplied", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`)
	// NOT EXISTS on the null-supplied leg → keep → {7,8}; a wrong-leg bind
	// drops → {} RED.
	pin("notexists_rightK", `SELECT "X" `+from+` WHERE NOT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`,
		"map[X:7]", "map[X:8]")
	// element correlation E.V = X → only element 7 matches EEV → {7}.
	pin("element_corr", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`,
		"map[X:7]")
	// single-source control (unchanged c5a-certified shape).
	pin("ctl_single_source", `SELECT "X" FROM A, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")

	// BAKEABLE box-leg CONJUNCT under EXISTS: `A.K = 100` is a box-leg conjunct
	// (resolves in A's buried window → verdict Bakeable), AND EXISTS on A.K. The
	// gather RECORDS the box legTypes and the merge BAKES the conjunct over that
	// record (ofOrdinal on the buried window), instead of the name-model
	// qualified-key rebase. A.K=100 → conjunct true, EXISTS matches → {7,8}.
	pin("bakeable_conjunct_present", `SELECT "X" `+from+` WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")
	// the conjunct on the NULL-SUPPLIED leg (B.K IS NULL): `B.K = 110` is false
	// (B.K=NULL) → no rows; a wrong-window bake reading A.K=100 → still no match
	// on 110, so use a discriminating value. B.K=NULL so any equality is false →
	// {} — a stale name-model rebase over the positional row would also give {}
	// (both keep the box-leg conjunct correct); the census pin below is the
	// model discriminator.
	pin("bakeable_conjunct_nullsupplied", `SELECT "X" `+from+` WHERE B."K" = 110 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`)
	// FULL box + Bakeable conjunct (D4-(ii): FULL admits ONLY at Bakeable): the
	// FULL OUTER box's A.K=100 conjunct bakes over the gather record. A survives
	// the FULL OUTER (AID=1≠BID=2, B null-supplied), A.K=100 → {7,8}.
	const ffrom = `FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X"`
	pin("full_bakeable_conjunct", `SELECT "X" `+ffrom+` WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")

	// CENSUS (the B2-B checkbox): the admitted LEFT-box+EXISTS shape must fire
	// ZERO name-model producers (it took the gathered ordinal cluster), while a
	// FULL-box+EXISTS+verdict-None shape is NOT admitted (D4-(ii): FULL+None
	// rides the certified binary seed) and its producers still fire. Both
	// answer correct rows either way; the census is the model discriminator.
	countProducers := func(t *testing.T, sql string) int {
		t.Helper()
		n := 0
		rquery.SetProducerCensusObserver(func(rquery.ProducerCensusRecord) { n++ })
		defer rquery.SetProducerCensusObserver(nil)
		if _, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil); err != nil {
			t.Fatalf("plan %q: %v", sql, err)
		}
		return n
	}
	t.Run("census_left_box_admits_zero_producers", func(t *testing.T) {
		if n := countProducers(t, `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`); n != 0 {
			t.Fatalf("admitted LEFT-box+EXISTS fired %d name-model producer(s), want 0 (the gather must own it)", n)
		}
	})
	t.Run("census_bakeable_conjunct_zero_producers", func(t *testing.T) {
		if n := countProducers(t, `SELECT "X" `+from+` WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`); n != 0 {
			t.Fatalf("admitted LEFT-box+Bakeable-conjunct fired %d name-model producer(s), want 0 (the merge bakes over the record)", n)
		}
	})
	t.Run("census_full_bakeable_zero_producers", func(t *testing.T) {
		if n := countProducers(t, `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`); n != 0 {
			t.Fatalf("admitted FULL-box+Bakeable-conjunct fired %d name-model producer(s), want 0", n)
		}
	})
	// INNER-CLUSTER control: B2-B's shape arm requires a NON-INNER box left, so
	// a multi-source INNER cluster under EXISTS (`FROM A, B, A.arr AS X`) is NOT
	// admitted (its owner is JoinInner) and stays name-model → producers fire.
	// This is the E-1a class (the next slice); B2-B must not touch it. (Measured:
	// the admitted LEFT box fires 2 producers with the admission off, 0 with it
	// on — this control keeps its producers either way.)
	t.Run("census_inner_cluster_stays_name_model", func(t *testing.T) {
		if n := countProducers(t, `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`); n == 0 {
			t.Fatal("multi-source INNER cluster under EXISTS fired 0 producers — B2-B must leave it name-model (E-1a owns it)")
		}
	})
}
