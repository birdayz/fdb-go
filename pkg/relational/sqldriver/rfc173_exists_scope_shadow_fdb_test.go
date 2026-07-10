package sqldriver_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_RFC173_ExistsInnerShadow certs the EXISTS inner-scope SHADOWING
// semantics (RFC-173 S4 collision-mint sub-slice): when the EXISTS inner FROM
// re-declares a name that is also an outer FROM leg, the inner qualified ref
// binds the INNER re-declared source — Java SemanticAnalyzer.
// resolveAcrossFragments resolves innermost-fragment-first (standard SQL
// shadowing). Every expected row set below is the LIVE answer of the
// 4.12.11.0 Java conformance server on this exact fixture (RunWithSetup
// probe, RFC-173 S4 record).
//
// The mechanism under test is the collision MINT (buildCorrelatedExists): a
// single-table correlated inner is BORN under a unique correlation identity,
// so its join predicate can never carry the ambiguous SQL name. Pre-mint,
// the ambiguous name made existsInnerScopeCollidesOuter force the ANCHORED
// seed, whose rebase reinterpreted the inner-bound ref as an OUTER-leg read
// and outer-routed it per row — wrong rows on the shared surface (three-way
// discriminating dataset: inner-shadow, per-outer-row, and FOD-first-row
// answers are pairwise distinct on every colliding query here).
//
// ST: R1(ID=1,C=100,ARR{10,200}), R2(ID=2,C=5,ARR{20,300}),
//
//	R3(ID=3,C=1000,ARR{4})            → min(ST.C) = 5.
//
// MA: M1(ID=11,C=11,ARR{10,11,12}), M2(ID=12,C=5,ARR{20,21}) — the R5o
// C/ARR values on pks 11/12.
// OT: (ID=1000,K=50). All pks DISJOINT across record types (they share the
// store extent keyed by primary key; a colliding pk silently overwrites).
func TestFDB_RFC173_ExistsInnerShadow(t *testing.T) {
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

	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	optl := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	pkg := "fdb.test.existsshadow"
	tn := func(n string) *string { return proto.String("." + pkg + "." + n) }
	arrTable := func(name string) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("ID"), Number: proto.Int32(1), Label: optl, Type: i64},
			{Name: proto.String("C"), Number: proto.Int32(2), Label: optl, Type: i64},
			{Name: proto.String("ARR"), Number: proto.Int32(3), Label: rep, Type: i64},
		}}
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("existsshadow_test.proto"), Package: proto.String(pkg), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			arrTable("ST"),
			arrTable("MA"),
			{Name: proto.String("OT"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: optl, Type: i64},
				{Name: proto.String("K"), Number: proto.Int32(2), Label: optl, Type: i64},
			}},
			{Name: proto.String("UnionDescriptor"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("_ST"), Number: proto.Int32(1), Label: optl, Type: msg, TypeName: tn("ST")},
				{Name: proto.String("_MA"), Number: proto.Int32(2), Label: optl, Type: msg, TypeName: tn("MA")},
				{Name: proto.String("_OT"), Number: proto.Int32(3), Label: optl, Type: msg, TypeName: tn("OT")},
			}},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatal(err)
	}
	mb := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	mb.GetRecordType("ST").SetPrimaryKey(recordlayer.Field("ID"))
	mb.GetRecordType("MA").SetPrimaryKey(recordlayer.Field("ID"))
	mb.GetRecordType("OT").SetPrimaryKey(recordlayer.Field("ID"))
	md, err := mb.Build()
	if err != nil {
		t.Fatal(err)
	}

	mkRow := func(desc protoreflect.MessageDescriptor, id, c int64, arr ...int64) *dynamicpb.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("C"), protoreflect.ValueOfInt64(c))
		l := m.NewField(desc.Fields().ByName("ARR")).List()
		for _, v := range arr {
			l.Append(protoreflect.ValueOfInt64(v))
		}
		m.Set(desc.Fields().ByName("ARR"), protoreflect.ValueOfList(l))
		return m
	}
	stDesc := md.GetRecordType("ST").Descriptor
	maDesc := md.GetRecordType("MA").Descriptor
	otDesc := md.GetRecordType("OT").Descriptor

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, m := range []*dynamicpb.Message{
			mkRow(stDesc, 1, 100, 10, 200),
			mkRow(stDesc, 2, 5, 20, 300),
			mkRow(stDesc, 3, 1000, 4),
			mkRow(maDesc, 11, 11, 10, 11, 12), // MA pks 11/12 — disjoint from ST 1..3
			mkRow(maDesc, 12, 5, 20, 21),
		} {
			if _, e := store.SaveRecord(m); e != nil {
				return nil, e
			}
		}
		ot := dynamicpb.NewMessage(otDesc)
		ot.Set(otDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(1000))
		ot.Set(otDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(50))
		if _, e := store.SaveRecord(ot); e != nil {
			return nil, e
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	want := func(name, q string, exp []string, planContains string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("%s: plan error: %v\n  sql: %s", name, perr, q)
		}
		if planContains != "" && !strings.Contains(plan.Explain(), planContains) {
			t.Fatalf("%s: plan does not contain %q\n  plan: %s\n  sql: %s", name, planContains, plan.Explain(), q)
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
			t.Fatalf("%s: exec error: %v\n  sql: %s", name, eerr, q)
		}
		sort.Strings(out)
		sort.Strings(exp)
		if fmt.Sprintf("%v", out) != fmt.Sprintf("%v", exp) {
			t.Fatalf("%s: rows = %v, want %v (Java live rows)\n  sql: %s\n  plan: %s", name, out, exp, q, plan.Explain())
		}
	}

	// Colliding inner under a lateral unnest — the R5o class on the
	// discriminating dataset. Java: X > min(ST.C)=5 → {10,200,20,300}; the
	// element 4 (R3) is EXCLUDED — pins min-C, not exists-nonempty.
	// Pre-mint per-outer-row gave {200,20,300}; FOD-first-row {200,300}.
	// The placement assertion pins the fix's mechanism: the correlation
	// conjunct evaluates UNDER the ∃ (a filter on the inner scan below the
	// FirstOrDefault), not as an outer pre-filter.
	want("colliding_unnest",
		`SELECT "X" FROM ST, ST."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM ST WHERE ST."C" < "X")`,
		[]string{"map[X:10]", "map[X:200]", "map[X:20]", "map[X:300]"},
		"FirstOrDefault(PredicatesFilter(Scan(ST)")

	// NOT EXISTS polarity twin: X <= min(C)=5 → only R3's element 4. Was
	// already correct pre-mint (negation forbids the outer-routing hoist) —
	// pinned so the mint can't regress the polarity that worked.
	want("colliding_unnest_notexists",
		`SELECT "X" FROM ST, ST."ARR" AS "X" WHERE NOT EXISTS (SELECT 1 FROM ST WHERE ST."C" < "X")`,
		[]string{"map[X:4]"},
		"")

	// Plain-table colliding twin (no unnest): every ST×OT pair keeps
	// (∃ inner C=5 < K=50) → 3 rows. The rename path answered this correctly
	// pre-mint; pinned as rows-invariance for the mint.
	want("colliding_plain",
		`SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM ST WHERE ST."C" < OT."K")`,
		[]string{"map[OT.K:50]", "map[OT.K:50]", "map[OT.K:50]"},
		"")

	// Non-correlated colliding inner (clean-build path, JoinPredicate nil):
	// EXISTS(∃C<50 → C=5) → constant true; X>15 keeps {200,20,300}. Rows
	// were correct pre-mint (the filter lives INSIDE the self-contained
	// inner plan); pinned as the mint's no-op boundary.
	want("colliding_noncorrelated",
		`SELECT "X" FROM ST, ST."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM ST WHERE ST."C" < 50) AND "X" > 15`,
		[]string{"map[X:200]", "map[X:20]", "map[X:300]"},
		"")

	// Non-colliding control (inner ST vs outer {OT, S2}): the mint must not
	// disturb the already-correct rename path. 1 OT × 3 S2 rows, EXISTS
	// constant-true per pair → 3 rows.
	want("noncolliding_control",
		`SELECT OT."K" FROM OT, ST AS "S2" WHERE EXISTS (SELECT 1 FROM ST WHERE ST."C" < OT."K")`,
		[]string{"map[OT.K:50]", "map[OT.K:50]", "map[OT.K:50]"},
		"")

	// NESTED EXISTS with a COLLIDING middle — the nestedOuterScopes
	// threading cert: the middle re-declares MA (an outer
	// leg's name) and the inner-inner correlates on the MIDDLE scan's C
	// (`ST.C < MA.C`). The nested scope must carry the MINTED correlation so
	// the inner-inner reference binds the runtime-real middle binding, not
	// the outer leg. Java: ∃ MA row (C < X ∧ ∃ST: ST.C < C): C=11 qualifies
	// (ST.C=5<11), C=5 does not (no ST.C<5) → 11 < X → {12,20,21}.
	want("nested_colliding_middle",
		`SELECT "X" FROM MA, MA."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM MA WHERE MA."C" < "X" AND EXISTS (SELECT 1 FROM ST WHERE ST."C" < MA."C"))`,
		[]string{"map[X:12]", "map[X:20]", "map[X:21]"},
		"")

	// Nested EXISTS, non-colliding middle: middle ST correlates to outer
	// OT.K, inner-inner to middle ST.C. Java: ∃ST (C<50 ∧ ∃MA: MA.C<C):
	// only C=5 passes C<50, and no MA.C<5 → EXISTS false → 0 rows.
	want("nested_noncolliding_middle",
		`SELECT OT."K" FROM OT WHERE EXISTS (SELECT 1 FROM ST WHERE ST."C" < OT."K" AND EXISTS (SELECT 1 FROM MA WHERE MA."C" < ST."C"))`,
		nil,
		"")

	// NOT EXISTS AROUND the nested colliding pair — a review-battery harvest:
	// this composition was ALSO wrong pre-mint (returned [] where the
	// complement of the positive answer is due). Java live: all 5 elements
	// minus {12,20,21} = {10,11}. The simple NOT-EXISTS twin above was
	// correct pre-mint; this nested composition was not — polarity
	// correctness was twin-only until the mint.
	want("notexists_around_nested_colliding",
		`SELECT "X" FROM MA, MA."ARR" AS "X" WHERE NOT EXISTS (SELECT 1 FROM MA WHERE MA."C" < "X" AND EXISTS (SELECT 1 FROM ST WHERE ST."C" < MA."C"))`,
		[]string{"map[X:10]", "map[X:11]"},
		"")

	// Nested colliding middle where the inner-inner correlates on ID — a
	// column the OUTER leg also has (the shared-column axis from the review
	// battery). Java live: the inner-inner is constant-true on this data
	// (∃ST.ID<MA.ID+10 always), so the condition reduces to ∃MA.C<X → X>5 →
	// all 5. Pre-mint this answered {12,20,21} (per-outer-row).
	want("nested_colliding_shared_id_col",
		`SELECT "X" FROM MA, MA."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM MA WHERE MA."C" < "X" AND EXISTS (SELECT 1 FROM ST WHERE ST."ID" < MA."ID" + 10))`,
		[]string{"map[X:10]", "map[X:11]", "map[X:12]", "map[X:20]", "map[X:21]"},
		"")

	// An outer leg legally QUOTED as "Q$44" — the alias spelling the mint's
	// own namespace (the mint-collision surface; mintDistinctUpper skips a
	// colliding candidate, pinned at unit level in mint_distinct_test.go).
	// Java live: ∃OT.K=50 > C holds only for the C=5 row → [5].
	want("qdollar_quoted_outer_alias",
		`SELECT "Q$44"."C" FROM ST AS "Q$44" WHERE EXISTS (SELECT 1 FROM OT WHERE OT."K" > "Q$44"."C")`,
		[]string{"map[C:5]"},
		"")

	// TRIPLE-nested colliding chain (review-battery shape, Java live: 0
	// rows). The innermost ∃ST.C<M3.C holds only for M3.C=11; level-2 then
	// needs 11 < MA.C, which no MA row satisfies → outer EXISTS false for
	// every element. Pins the mint's composition through THREE scope levels
	// (each level's nested scope carries the minted middle identity).
	want("triple_nested_colliding",
		`SELECT "X" FROM MA, MA."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM MA WHERE MA."C" < "X" AND EXISTS (SELECT 1 FROM MA AS "M3" WHERE "M3"."C" < MA."C" AND EXISTS (SELECT 1 FROM ST WHERE ST."C" < "M3"."C")))`,
		nil,
		"")

	// Clean-path colliding inner with NO WHERE at all (the fast-path class
	// the guard narrowing covers: JoinPredicate nil, plan a bare scan the
	// rename re-identifies). EXISTS(SELECT 1 FROM ST) is constant-true →
	// all 5 elements (Java live). Rows are model-invariant here — the
	// narrowing flips the SEED, pinned white-box by
	// TestRFC173_ExistsGuardNarrowing.
	want("cleanpath_nowhere_colliding",
		`SELECT "X" FROM ST, ST."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM ST)`,
		[]string{"map[X:10]", "map[X:200]", "map[X:20]", "map[X:300]", "map[X:4]"},
		"")

	// The counter-shift sentinel: quoted user aliases spelling the mint
	// namespace ("Q$1" CTE + "Q$3" unnest alias) around an EXISTS. Every
	// subquery-level identifier (the inner mint AND esq.Alias via
	// mintSubqueryAlias) skips user-visible names, so this must PLAN and
	// answer regardless of the process-global counter's state. Java live:
	// elements X with ∃OT.K=50 > X → {10,20,4}. Pre-fix, a counter
	// alignment assigned "Q$3"-colliding identities and a VALID query
	// failed to plan (best-expression error) — history-dependent.
	want("qdollar_cte_counter_shift",
		`WITH "Q$1" AS (SELECT MA."ID" FROM MA) SELECT "Q$3" FROM ST, ST."ARR" AS "Q$3" WHERE EXISTS (SELECT 1 FROM OT WHERE OT."K" > "Q$3")`,
		[]string{"map[Q$3:10]", "map[Q$3:20]", "map[Q$3:4]"},
		"")

	// MULTI-SOURCE colliding inner — the FORWARD SENTINEL for mint-per-leg.
	// The inner {OI, ST} re-declares the outer leg ST and the predicate
	// references it; an unminted multi-source inner keeps its SQL leg names,
	// so the join-level reinterpretation would answer per-outer-row (1 row)
	// where Java's inner-shadow semantics answer 3 (live-verified) — the
	// scope-ambiguity decline converts that silent-wrong to a loud 0A000.
	// When mint-per-leg lands, this pin flips to the Java rows and the
	// conformance gap closes.
	wantDecline := func(name, q, wantSub string) {
		t.Helper()
		_, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr == nil {
			t.Fatalf("%s: must decline loud (scope-ambiguous), planned OK instead\n  sql: %s", name, q)
		}
		if !strings.Contains(perr.Error(), wantSub) {
			t.Fatalf("%s: decline = %v, want substring %q\n  sql: %s", name, perr, wantSub, q)
		}
	}
	wantDecline("multisource_colliding_declines",
		`SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE ST."C" < OT."K")`,
		"scope-ambiguous")

	// The NESTED-BYPASS sentinel: a nested constant-true EXISTS must not
	// disable the decline (the Case-1 branch used to return before the
	// check, carrying the ambiguous ref out as the join predicate — one
	// outer-dependent row where Java answers 3, live-verified). The check
	// now runs on the full walked predicate BEFORE the nested branches.
	wantDecline("multisource_colliding_nested_bypass",
		`SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE ST."C" < OT."K" AND EXISTS (SELECT 1 FROM MA WHERE MA."C" > 0))`,
		"scope-ambiguous")

	// Polarity harvest: the q5 NOT-EXISTS twin was ALSO silent-wrong
	// pre-decline ([50,50] where Java's complement of the positive 3 rows
	// is 0 rows, live-verified). Declines at plan time, before polarity
	// can matter.
	wantDecline("multisource_colliding_notexists",
		`SELECT OT."K" FROM ST, OT WHERE NOT EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE ST."C" < OT."K")`,
		"scope-ambiguous")

	// Leg-position harvest: the FIRST-leg-collides variant was silent-wrong
	// pre-decline too ([50] where Java answers 3, live-verified).
	wantDecline("multisource_colliding_first_leg",
		`SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM ST, OT AS "OI" WHERE ST."C" < OT."K")`,
		"scope-ambiguous")

	// The MINTED-MIDDLE no-over-fire control: the middle single-table inner
	// (MA AS "MID") is minted, so ONLY its Q$N identity binds at runtime —
	// the innermost multi-source inner re-declaring MID locally cannot
	// collide with a display alias that never binds. Testing display names
	// here 0A000'd this valid query; the decline tests ACTUALLY-BOUND outer
	// names (CorrelationName when present). Java live: middle ∃MA with
	// C < 50 ∧ innermost ∃(MID,ST) with local MID.C < 100 → true → [50].
	want("minted_middle_local_shadow_answers",
		`SELECT OT."K" FROM OT WHERE EXISTS (SELECT 1 FROM MA AS "MID" WHERE "MID"."C" < OT."K" AND EXISTS (SELECT 1 FROM MA AS "MID", ST WHERE "MID"."C" < 100))`,
		[]string{"map[K:50]"},
		"")

	// The no-over-fire control: a NON-colliding multi-source inner (leg
	// names {ST, MI} disjoint from the outer {MA, OT}), CORRELATED on an
	// outer TABLE column so it genuinely reaches the fallback's multi-source
	// arm. (The element-correlated variant dies upstream in the pre-existing
	// multi-table-FROM-referencing-element 0AF00 and never reaches the
	// decline.) Java live: ∃(ST,MI) with ST.C < OT.K=50 → constant-true →
	// 2 MA × 1 OT rows → [50, 50].
	want("multisource_noncolliding_answers",
		`SELECT OT."K" FROM MA, OT WHERE EXISTS (SELECT 1 FROM ST, MA AS "MI" WHERE ST."C" < OT."K")`,
		[]string{"map[OT.K:50]", "map[OT.K:50]"},
		"")
}
