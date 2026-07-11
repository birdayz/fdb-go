package sqldriver_test

// RFC-173 S4 Step B (c5a) — the COUPLED TWO-AXIS FLIP, e2e. A merge-opaque FULL
// OUTER box under a lateral unnest, under WHERE-EXISTS, now gates ORDINAL: the
// box births its whole leg-concat POSITIONALLY (axis 1 — the box-outer
// translateRef clears the :167 enclosure) AND the seed is admitted (axis 2 —
// unnestExistsSeedSafe), both through the ONE boxGatesFresh predicate. The
// per-leg-window rebase (channels 1+2) then disambiguates the box's two
// dup-named legs by their [Start,Width) windows.
//
// The correctness crux is CORRECT-LEG BIND on a QUALIFIED correlation into the
// NULL-SUPPLIED leg. FOA and FOB share a column name K. The FULL OUTER on
// AID=BID with no matching key leaves an A-present / B-null-supplied surviving
// row (FOA.ARR non-empty → it unnests). A correlation `FOC.CK = FOB.K` reads
// the NULL-supplied leg (FOB.K = NULL → EXISTS false), while `FOC.CK = FOA.K`
// reads the present leg (FOA.K = 100 → EXISTS true). A wrong-leg bind (the flat
// first-match / last-leg-wins hazard the windows fix) would read FOA.K for
// FOB.K and FLIP both polarities — so Q1/Q2 below are red under any mis-bind.
//
// Gates: (1) exact rows per query; (2) the §5 dual-window differential
// (ordinal-read == name-model-read of the SAME ordinal-seed plan — the ordinal
// path fired AND agrees with the trusted name model); (3) a NEGATIVE pin that a
// multi-source INNER cluster (`FROM FOA, FOB, FOA.ARR AS X`) stays name-model
// and answers correctly (boxGatesFresh excludes JoinInner — the R1 hazard: an
// ordinal cluster gate over a still-declining multi-source outer is wrong rows).
//
// NOT parallel: flips the process-global name-model oracle (SetNameModelOracle),
// exactly like TestFDB_RFC173W5_OracleDualWindow. Go runs it to completion
// before any t.Parallel() test resumes, so the flip cannot leak.

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

func TestFDB_RFC173S4_C5a_FullOuterUnnestExists(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("c5afull")
	// FOA and FOB SHARE the column name K — the dup-named leg the qualified
	// correlation must disambiguate. FOA also carries the unnested array ARR.
	b.AddTable("FOA", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("FOB", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	b.AddTable("FOC", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("CK", api.NewLongType(true), 2),
	}, []string{"CID"})
	// FOD is the third K-named table, used ONLY by the clustered-leg-FULL-box
	// case: `(FOA JOIN FOD) FULL OUTER FOB`. Its K disambiguates a BURIED leaf
	// (FOD sits inside the clustered inner leg, not a top-level box leg).
	b.AddTable("FOD", []metadata.ColumnSpec{
		metadata.NewColumnSpec("DID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"DID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()

	foaDesc := md.GetRecordType("FOA").Descriptor
	foa := dynamicpb.NewMessage(foaDesc)
	foa.Set(foaDesc.Fields().ByName("AID"), protoreflect.ValueOfInt64(1))
	foa.Set(foaDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(100))
	arrFd := foaDesc.Fields().ByName("ARR")
	arr := foa.NewField(arrFd).List()
	arr.Append(protoreflect.ValueOfInt32(7))
	arr.Append(protoreflect.ValueOfInt32(8))
	foa.Set(arrFd, protoreflect.ValueOfList(arr))

	fobDesc := md.GetRecordType("FOB").Descriptor
	fob := dynamicpb.NewMessage(fobDesc)
	// BID=2 ≠ AID=1: no join match, so the FULL OUTER leaves FOA row 1 with a
	// NULL-supplied B side — FOB.K = NULL in that surviving row. (555 is this
	// unmatched FOB row's OWN K; it never survives into an unnested row because
	// its ARR source, FOA, is null-supplied there — so 555 is inert to the
	// discriminators below.) The correct-leg-bind discriminator is FOA.K=100 vs
	// FOB.K=NULL, resolved by leg window, not by the 555 value.
	fob.Set(fobDesc.Fields().ByName("BID"), protoreflect.ValueOfInt64(2))
	fob.Set(fobDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(555))

	focDesc := md.GetRecordType("FOC").Descriptor
	foc := dynamicpb.NewMessage(focDesc)
	foc.Set(focDesc.Fields().ByName("CID"), protoreflect.ValueOfInt64(1))
	foc.Set(focDesc.Fields().ByName("CK"), protoreflect.ValueOfInt64(100))
	foc2 := dynamicpb.NewMessage(focDesc) // c5b: matches the buried FOD.K=200
	foc2.Set(focDesc.Fields().ByName("CID"), protoreflect.ValueOfInt64(2))
	foc2.Set(focDesc.Fields().ByName("CK"), protoreflect.ValueOfInt64(200))

	fodDesc := md.GetRecordType("FOD").Descriptor
	fod := dynamicpb.NewMessage(fodDesc)
	// DID=1 joins FOA on AID=DID=1; K=200 is the buried-leaf discriminator,
	// distinct from FOA.K=100 and FOB.K/NULL, read by the direct `FOD.K = 200`
	// predicate (a wrong-leg resolve → 100/NULL ≠ 200 → 0 rows).
	fod.Set(fodDesc.Fields().ByName("DID"), protoreflect.ValueOfInt64(1))
	fod.Set(fodDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(200))

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{foa, fob, foc, foc2, fod} {
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
				// A single-column projection may arrive as a bare scalar OR a
				// one-key map depending on the plan shape; normalize both to the
				// column value(s), sorted by key, so the expected rows compare
				// cleanly under either window.
				if m, isMap := r.Datum.(map[string]any); isMap {
					keys := make([]string, 0, len(m))
					for k := range m {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					parts := make([]string, 0, len(keys))
					for _, k := range keys {
						parts = append(parts, unnestSprint(m[k]))
					}
					out = append(out, strings.Join(parts, "|"))
				} else {
					out = append(out, unnestSprint(r.Datum))
				}
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", sql, eerr)
		}
		sort.Strings(out)
		return out
	}

	// positional runs the query through the DEFAULT (ordinal / positional) path.
	positional := func(t *testing.T, sql string) []string { return run(t, sql) }
	// named runs the SAME plan under the process-global name-model oracle
	// (positional births suppressed, baked refs resolve by display name).
	named := func(t *testing.T, sql string) []string {
		executor.SetNameModelOracle(true)
		defer executor.SetNameModelOracle(false)
		return run(t, sql)
	}
	// assertRows is the PRIMARY gate: the ordinal (positional) path must produce
	// EXACTLY these rows. Correct-leg bind, EXISTS semantics, and null-supplied
	// handling are all pinned here — no name-model dependency.
	assertRows := func(t *testing.T, label, sql string, want []string) {
		t.Helper()
		got := positional(t, sql)
		if strings.Join(got, ";") != strings.Join(want, ";") {
			t.Errorf("%s: ordinal rows = %v, want %v\n  sql: %s", label, got, want, sql)
		}
	}

	const from = `FROM FOA FULL OUTER JOIN FOB ON FOA."AID" = FOB."BID", FOA."ARR" AS "X"`

	// Q1 — EXISTS on the NULL-SUPPLIED leg (FOB.K = NULL in the A-present
	// surviving row): never matches FOC, so the row is excluded → 0 rows. This is
	// the correct-leg-bind DISCRIMINATOR: a wrong-leg bind would read FOA.K=100
	// (matches FOC) → {7,8} (RED). The per-leg windows resolve the QUALIFIED
	// FOB.K to leg B (NULL), not leg A's same-named column.
	q1 := `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOB."K")`
	assertRows(t, "Q1/EXISTS-null-supplied-leg", q1, nil)

	// Q2 — NOT EXISTS on the NULL-SUPPLIED leg: the row is included → {7,8}. A
	// wrong-leg bind reads FOA.K=100 (EXISTS true) → NOT-EXISTS drops it → {}
	// (RED). The mirror discriminator.
	q2 := `SELECT "X" ` + from + ` WHERE NOT EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOB."K")`
	assertRows(t, "Q2/NOT-EXISTS-null-supplied-leg", q2, []string{"7", "8"})

	// Q3 — EXISTS on the PRESENT leg (FOA.K = 100 → matches FOC): included →
	// {7,8}. Pins the present-leg correlation and the EXISTS semantics.
	q3 := `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOA."K")`
	assertRows(t, "Q3/EXISTS-present-leg", q3, []string{"7", "8"})

	// Q4 — NOT EXISTS on the PRESENT leg: EXISTS true → excluded → 0 rows.
	assertRows(t, "Q4/NOT-EXISTS-present-leg",
		`SELECT "X" `+from+` WHERE NOT EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOA."K")`,
		nil)

	// c5b: clustered-INNER-leg FULL box ORDINALIZES a BURIED EXISTS ref. The FULL
	// box's left leg is an INNER cluster (FOA JOIN FOD); FOD is a BURIED leaf. A
	// correlation `FOC.CK = FOD.K` INSIDE EXISTS (no outer conjunct → the
	// outer-conjunct narrowing does not fire) must resolve FOD.K by its
	// [Start,Width) window in the box's positional row — the buried-leaf machinery
	// (channels 1+2 + the executor below-FOD hoist). FOD.K=200; FOC has CK=200 →
	// EXISTS true → {7,8}. Before the c5b relaxation (legExposesBuriedOuterBox
	// admits the INNER cluster) this shape was name-model; now it ordinalizes and
	// the buried ref resolves (red-first: `field FOD.K not resolvable` if the
	// buried window is not consulted).
	assertRows(t, "c5b/clustered-buried-exists-resolves",
		`SELECT "X" FROM FOA INNER JOIN FOD ON FOA."AID" = FOD."DID" `+
			`FULL OUTER JOIN FOB ON FOA."AID" = FOB."BID", FOA."ARR" AS "X" `+
			`WHERE EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOD."K")`,
		[]string{"7", "8"})

	// c5b CORRECT-LEG discriminator. The `FOD.K > 150` conjunct INSIDE the EXISTS
	// reads the buried FOD.K by value: FOD.K=200 (>150, true) → the FOC.CK=200 row
	// matches → {7,8}. A mis-bind of the buried FOD.K to FOA.K=100 (100>150 false)
	// or FOB.K=NULL (NULL>150 false) → the AND is false → EXISTS false → 0 rows.
	// So {7,8} PROVES the buried ref resolves to FOD's leaf, not a same-named
	// sibling leg (the buried [Start,Width) window disambiguation).
	assertRows(t, "c5b/clustered-buried-correct-leg",
		`SELECT "X" FROM FOA INNER JOIN FOD ON FOA."AID" = FOD."DID" `+
			`FULL OUTER JOIN FOB ON FOA."AID" = FOB."BID", FOA."ARR" AS "X" `+
			`WHERE EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOD."K" AND FOD."K" > 150)`,
		[]string{"7", "8"})

	// c5b FIRST buried leaf — the mirror on FOA.K (leaf 1, dup-named K with FOD
	// leaf 2 and the top FOB leg). `FOA.K < 150`: FOA.K=100 (<150) → the FOC.CK=100
	// row matches → {7,8}. A mis-bind to FOD.K=200 (200<150 false) or FOB.K=NULL →
	// 0 rows. So BOTH buried leaves (FOA.K and FOD.K), all three named K, resolve
	// to their OWN [Start,Width) window — the dup-named buried disambiguation holds
	// end-to-end, not just for FOD.K.
	assertRows(t, "c5b/clustered-buried-first-leaf",
		`SELECT "X" FROM FOA INNER JOIN FOD ON FOA."AID" = FOD."DID" `+
			`FULL OUTER JOIN FOB ON FOA."AID" = FOB."BID", FOA."ARR" AS "X" `+
			`WHERE EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOA."K" AND FOA."K" < 150)`,
		[]string{"7", "8"})

	// NON-EXISTS outer conjunct on a box leg — the sibling of the EXISTS
	// regression, in the plain filter-over-unnest path. `(a FULL b), a.arr WHERE
	// a.col=V` (NO EXISTS) also ordinalizes the box (AXIS 1 fires regardless of
	// EXISTS), and the regular conjunct merges into the name-keyed unnest SELECT →
	// a positional box leaves it unresolvable. The narrowing (now set in BOTH the
	// EXISTS and non-EXISTS filter paths, checked in unnestExistsSeedSafe before
	// the !unnestUnderExistential early return) declines the box to name-model →
	// correct. Both the scan-legged and clustered-INNER shapes are pinned; each
	// was red-first as `field "…" not resolvable ... malformed plan`.
	assertRows(t, "NONEXISTS-CONJUNCT/scan-box-stays-name-model",
		`SELECT "X" FROM FOA FULL OUTER JOIN FOB ON FOA."AID" = FOB."BID", FOA."ARR" AS "X" WHERE FOA."K" = 100`,
		[]string{"7", "8"})
	assertRows(t, "NONEXISTS-CONJUNCT/clustered-box-stays-name-model",
		`SELECT "X" FROM FOA INNER JOIN FOD ON FOA."AID" = FOD."DID" `+
			`FULL OUTER JOIN FOB ON FOA."AID" = FOB."BID", FOA."ARR" AS "X" WHERE FOD."K" = 200`,
		[]string{"7", "8"})

	// NONEXISTS-CONJUNCT / no-shared-columns FULL box. FOA and FOC share NO
	// column name, so the box concat has no duplicate → the gathered path's
	// name-ambiguous decline does NOT fire. Without the unnestBoxLegConjunct
	// decline in the gathered OUTER-box arm, the box gathers positionally as one
	// opaque leg and the box-leg WHERE conjunct merges by name with no per-leg
	// window → `field "FOA.K" not resolvable … ordinal -1` (malformed plan,
	// red-first). The gathered arm now declines to the binary name-model path,
	// where the qualified key resolves. Both a FOA leg conjunct and a FOC leg
	// conjunct must resolve. FOA.AID=1=FOC.CID matches → FOA.ARR=[7,8].
	assertRows(t, "NONEXISTS-CONJUNCT/noshare-fullbox-first-leg",
		`SELECT "X" FROM FOA FULL OUTER JOIN FOC ON FOA."AID" = FOC."CID", FOA."ARR" AS "X" WHERE FOA."K" = 100`,
		[]string{"7", "8"})
	assertRows(t, "NONEXISTS-CONJUNCT/noshare-fullbox-second-leg",
		`SELECT "X" FROM FOA FULL OUTER JOIN FOC ON FOA."AID" = FOC."CID", FOA."ARR" AS "X" WHERE FOC."CK" = 100`,
		[]string{"7", "8"})

	// INNER-cluster leg conjunct (NOT a box) — end-to-end CORRECTNESS pin: an
	// INNER cluster gathers each leg with its OWN positional window, so a leg
	// conjunct resolves through the gathered path and yields the right rows.
	// This is NOT the over-decline guard — the rows {7,8} are OVER-DETERMINED
	// (name-model yields them too, so an INNER over-decline would still pass
	// here); the DECISION-level anti-over-decline sentinel is the white-box
	// TestRFC173W5_Gathered_OuterConjunctCoupling (INNER arm, flag SET → gathers
	// an ordinal seed). FOA INNER FOC on AID=CID matches → FOA.ARR=[7,8].
	assertRows(t, "NONEXISTS-CONJUNCT/inner-cluster-leg-resolves",
		`SELECT "X" FROM FOA JOIN FOC ON FOA."AID" = FOC."CID", FOA."ARR" AS "X" WHERE FOC."CK" = 100`,
		[]string{"7", "8"})

	// OUTER-CONJUNCT regression pin. A regular (NON-EXISTS) WHERE conjunct on a
	// box leg (FOA.K=100, OUTSIDE the EXISTS) on a multi-alias FULL box cannot yet
	// be baked positionally (it merges into the name-keyed unnest SELECT, out of
	// the executor's below-FOD hoist reach), so the ordinal seed would leave it
	// unresolvable → malformed plan. The narrowing declines such a shape to
	// name-model (unnestExistsSeedSafe → unnestBoxLegConjunct), which
	// resolves FOA.K via the qualified key → correct {7,8}. RED before the
	// narrowing (`field "FOA.K" not resolvable ... malformed plan`); pre-c5a this
	// shape was name-model and worked, so the ordinal lift MUST NOT regress it.
	assertRows(t, "OUTER-CONJUNCT/box-leg-stays-name-model",
		`SELECT "X" `+from+` WHERE FOA."K" = 100 AND EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOA."K")`,
		[]string{"7", "8"})

	// SINGLE-SOURCE control: the same outer conjunct on a SINGLE-source unnest
	// (no box) must still ordinalize and answer correctly — the narrowing only
	// declines the MULTI-alias box, not the single-source pristine prefix.
	assertRows(t, "OUTER-CONJUNCT/single-source-ok",
		`SELECT "X" FROM FOA, FOA."ARR" AS "X" WHERE FOA."K" = 100 AND EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOA."K")`,
		[]string{"7", "8"})

	// LEFT — the AXIS-1 over-breadth regression pin. Step B admits an OUTER box
	// at AXIS 1 (the box-outer positional birth), but ONLY a FULL box may take
	// the ordinal seed (clusterArity==1); a LEFT/RIGHT box has clusterArity>=2 so
	// its seed stays name-model. Birthing a LEFT box positional while its seed is
	// name-model would strand the name-model builder over a positional row — it
	// reads FOA.K by an ABSENT qualified key → NULL → EXISTS(100=NULL) false → 0
	// rows (WRONG; correct is {7,8}). AXIS 1 must therefore fire ONLY when the
	// seed will actually ordinalize the box. FOA LEFT JOIN FOB (A row 1, no B
	// match) keeps the A row, B null-supplied; unnest FOA.ARR → [7,8]; EXISTS on
	// FOA.K=100 matches FOC → {7,8}. RED before the AXIS-1 narrowing.
	assertRows(t, "LEFT/box-under-EXISTS-correct-rows",
		`SELECT "X" FROM FOA LEFT JOIN FOB ON FOA."AID" = FOB."BID", FOA."ARR" AS "X" `+
			`WHERE EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOA."K")`,
		[]string{"7", "8"})

	// IS NULL on the null-supplied FULL leg — the nullability-marker probe. The
	// FULL box types both legs NOT NULL (legsOfGatedJoin marks neither
	// null-supplying), diverging from Java's all-FULL-columns-nullable. This pins
	// that the divergence is ANSWER-INVARIANT for a null-CONSUMING predicate too:
	// FOB.K IS NULL must evaluate on the RUNTIME nil (true for the surviving row),
	// NOT be constant-folded to false off the NOT-NULL static type. Expected
	// {7,8}; a type-marker leak into IS-NULL folding → 0 rows (RED).
	assertRows(t, "IS-NULL-on-null-supplied-FULL-leg",
		`SELECT "X" `+from+` WHERE FOB."K" IS NULL`,
		[]string{"7", "8"})

	// §7 dup-name observable — PROOF the ORDINAL seed fired AND strictly fixes a
	// name-model conflation. The name-model oracle resolves dup-named box columns
	// LAST-LEG-WINS (a documented RFC-173 carve-out — the name-keyed Datum cannot
	// keep two same-named legs distinct). So the oracle reads the QUALIFIED FOA.K
	// (first leg) as the LAST leg's K (FOB.K = NULL) and gets a DIFFERENT answer
	// than the positional path, which resolves FOA.K by its own [Start,Width)
	// window. Toggling the oracle CHANGING the answer proves a positional row was
	// born (the flip fired); a name-model REVERT would make these AGREE and turn
	// this red. This is why the dual-window differential is NOT a valid gate for
	// dup-named box seeds — the name model is the broken reference here. The Q2
	// check just below (LAST-leg correlation, where last-leg-wins is accidentally
	// correct → oracle agrees) completes the asymmetry that characterizes it.
	if pos, nm := positional(t, q3), named(t, q3); strings.Join(pos, ";") == strings.Join(nm, ";") {
		t.Errorf("Q3 §7: positional and name-model agree (%v) — the positional seed did not fire, "+
			"or the name model no longer conflates dup-named legs; the ordinal path must resolve "+
			"FOA.K by its window while the name oracle conflates last-leg-wins", pos)
	}
	// Q2 correlates on the LAST leg (FOB.K), where last-leg-wins is accidentally
	// correct — so the oracle AGREES with positional ({7,8} both). The asymmetry
	// (agree on last leg, differ on first leg) precisely characterizes the
	// name-model conflation the per-leg windows eliminate.
	if pos, nm := positional(t, q2), named(t, q2); strings.Join(pos, ";") != strings.Join(nm, ";") {
		t.Errorf("Q2 §7: name-model disagrees with positional on the LAST-leg correlation "+
			"(name=%v, positional=%v) — last-leg-wins should be accidentally correct here", nm, pos)
	}

	// NEGATIVE (R1) — a multi-source INNER cluster stays NAME-MODEL (boxGatesFresh
	// excludes JoinInner) and must answer correctly. FOA × FOB = 1 combined row
	// (FOA.ARR=[7,8]); EXISTS on FOA.K=100 matches → {7,8}. If Step B wrongly
	// gated this ordinal, the seed would decline the multi-source outer → wrong
	// rows. Correct rows here prove the R1 hazard is avoided.
	assertRows(t, "R1/multi-source-INNER-stays-name-model",
		`SELECT "X" FROM FOA, FOB, FOA."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOA."K")`,
		[]string{"7", "8"})

	// CLUSTERED-LEG FULL box — the BURIED-LEAF dimension stays NAME-MODEL (the
	// c5a gate's simple-legs narrowing). clusterArity(FULL)==1 UNCONDITIONALLY, so
	// a naive gate would admit `(FOA JOIN FOD) FULL OUTER FOB`, but its left leg
	// is a CLUSTERED join whose buried leaves the positional seed does not yet
	// concat (`(A JOIN B) FULL OUTER C` births only C's columns → a qualified
	// `FOD.K` is unresolvable → malformed plan). boxGatesFresh EXCLUDES a box with
	// a join leg, so this shape declines to the name-model builder, which resolves
	// buried `FOD.K` via the qualified key — CORRECT rows. This pins that the c5a
	// lift does NOT reach the clustered-leg box (buried-leaf ordinalization under
	// FULL is a follow-up item-3 slice). The direct predicate `FOD.K = 200`
	// (FOD.K=200 buried; FOA.K=100, FOB.K=NULL) discriminates: correct bind →
	// {7,8}; a mis-bind → 0 rows. (FOA JOIN FOD on AID=DID=1) survives the FULL
	// OUTER with FOB null-supplied (AID=1 ≠ BID=2); unnest FOA.ARR → [7,8].
	assertRows(t, "CLUSTERED/buried-leaf-FULL-box-stays-name-model",
		`SELECT "X" FROM FOA INNER JOIN FOD ON FOA."AID" = FOD."DID" `+
			`FULL OUTER JOIN FOB ON FOA."AID" = FOB."BID", FOA."ARR" AS "X" `+
			`WHERE FOD."K" = 200 AND EXISTS (SELECT 1 FROM FOC WHERE FOC."CK" = FOA."K")`,
		[]string{"7", "8"})
}
