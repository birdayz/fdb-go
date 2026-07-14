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
				out = append(out, unnestSprint(executor.RowValue(r)))
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
	// NOTE (RFC-173 B, the last P5 residual): a SUBQUERY-carrying box-leg conjunct
	// (`A.K = (SELECT MIN(EE.CK) FROM EE)`) is classified Unbakeable and stays
	// name-model. It is NOT pinned here — runQ uses raw ExecutePlan and does not
	// pre-evaluate scalar subqueries (WithScalarSubqueries), so a scalar-subquery
	// shape cannot execute in this harness at all; verifying its ordinalization
	// needs a FULL sqldriver db.QueryContext test (see RFC ## LEFT FOR COMPLETION §B).
	// FULL box + Bakeable conjunct (D4-(ii): FULL admits ONLY at Bakeable): the
	// FULL OUTER box's A.K=100 conjunct bakes over the gather record. A survives
	// the FULL OUTER (AID=1≠BID=2, B null-supplied), A.K=100 → {7,8}.
	const ffrom = `FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X"`
	pin("full_bakeable_conjunct", `SELECT "X" `+ffrom+` WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")

	// CHECKS-4/5 AXIS (review-required): the admission omits the buried-outer-only
	// and whole-row pre-checks the design named — these shapes ARE admitted and
	// gathered. Verified correct-or-loud across ~19 esq shapes; pin the sharp
	// representatives so a future admission/rebase change can't regress the axis
	// to silent-wrong with no red test (the dimensional gap the enclosed-CTE arc
	// kept punishing).
	// UNCORRELATED EXISTS (buried-outer-only / whole-row-independent): EE has a
	// row → EXISTS always true → all unnested rows kept → {7,8}.
	pin("uncorrelated_exists", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE)`,
		"map[X:7]", "map[X:8]")
	// COMPOUND buried correlation (arithmetic on the buried leg): A.K + 0 = 100
	// matches EE.CK=100 → {7,8}; a mis-baked buried ref would misresolve → RED.
	pin("arith_on_buried_leg", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K" + 0)`,
		"map[X:7]", "map[X:8]")
	// TWO-TABLE esq spanning BOTH a leg (A.K) and the ELEMENT (X) — a
	// pre-existing LOUD 0AF00 guard (multi-table EXISTS referencing the element
	// is unsupported). FLIP-SENTINEL: if a future admission/rebase makes this
	// gather, this pin catches the regression to silent-wrong.
	t.Run("twotable_leg_and_element_loud", func(t *testing.T) {
		_, _, err := runQ(t, `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE, EEV WHERE EE."CK" = A."K" AND EEV."VK" = "X")`)
		// The decline is a stable, deliberate 0AF00 — pin the code, not just
		// err != nil. A code pin is strictly stronger: it also catches the shape
		// breaking for an UNRELATED reason (which would silently retire the
		// silent-wrong→loud sentinel this exists to be), and it matches the
		// loud0AF00 pattern the enclosed-CTE battery in this package uses.
		if err == nil {
			t.Fatal("two-table esq spanning a leg and the element must be LOUD (unsupported), got rows")
		}
		if !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("expected a 0AF00 decline, got: %v", err)
		}
	})
}

// slice3B2bMetadata builds the A/B/EE/EEV schema shared by the B2-B row cert
// (FDB execution) and census cert (planning only). A(AID=1,K=100,ARR=[7,8]);
// B(BID=2,K=110) — A.K/B.K dup-named, the leg a qualified EXISTS correlation
// must disambiguate; EE(CK) is the leg-correlation table, EEV(VK) the element
// one.
func slice3B2bMetadata(tb testing.TB) *recordlayer.RecordMetaData {
	tb.Helper()
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
	b.AddTable("EE", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CK", api.NewLongType(false), 1),
	}, []string{"CK"})
	b.AddTable("EEV", []metadata.ColumnSpec{
		metadata.NewColumnSpec("VK", api.NewLongType(false), 1),
	}, []string{"VK"})
	tmpl, err := b.Build()
	if err != nil {
		tb.Fatal(err)
	}
	return tmpl.Underlying()
}

// TestRFC173Slice3B2bFaceACensus is the B2-B model discriminator (the checkbox
// the gather actually FIRED). It is DELIBERATELY NOT t.Parallel(): the producer
// census observer is a PROCESS GLOBAL (SetProducerCensusObserver, whose own doc
// requires no translation in flight), so the assertions must run in Go's
// serial phase — a parallel sibling's concurrent translation would pollute the
// count (a real data race). Planning-only, so it needs no FDB / Docker.
func TestRFC173Slice3B2bFaceACensus(t *testing.T) { //nolint:paralleltest // process-global observer, must be serial
	md := slice3B2bMetadata(t)
	const from = `FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X"`
	countProducers := func(sql string) int {
		n := 0
		rquery.SetProducerCensusObserver(func(rquery.ProducerCensusRecord) { n++ })
		defer rquery.SetProducerCensusObserver(nil)
		if _, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil); err != nil {
			t.Fatalf("plan %q: %v", sql, err)
		}
		return n
	}
	// The admitted box+EXISTS shapes fire ZERO name-model producers (the gather
	// owns them). Measured: the LEFT box fires 2 producers with the admission
	// off, 0 with it on — the model discriminator the row pins can't be.
	for _, tc := range []struct{ name, sql string }{
		{"left_box_none", `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"left_box_bakeable", `SELECT "X" ` + from + ` WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"full_box_bakeable", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
	} {
		if n := countProducers(tc.sql); n != 0 {
			t.Fatalf("%s: fired %d name-model producer(s), want 0 (the gather must own it)", tc.name, n)
		}
	}
	// Positive control: the observer must FIRE when it should (so the
	// 0-assertions above aren't vacuous). The single-esq INNER cluster is now
	// ordinalized by E-1a (0 producers — pinned in the E-1a cert), so this uses a
	// MULTI-esq INNER cluster, which the single-esq admission excludes → it stays
	// name-model → the P5 unnest-result-value producer fires during TRANSLATION.
	// (This shape then strands at physicalization — a pre-existing multi-esq-
	// under-EXISTS limitation, orthogonal to the census — so it counts producers
	// tolerant of the plan error: the firing happens in the translator, before
	// the physical planner declines.)
	countTolerant := func(sql string) int {
		n := 0
		rquery.SetProducerCensusObserver(func(rquery.ProducerCensusRecord) { n++ })
		defer rquery.SetProducerCensusObserver(nil)
		_, _ = embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		return n
	}
	if n := countTolerant(`SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`); n == 0 {
		t.Fatal("multi-esq INNER cluster fired 0 producers during translation — the observer must fire on a name-model shape (observer-fires control)")
	}
}
