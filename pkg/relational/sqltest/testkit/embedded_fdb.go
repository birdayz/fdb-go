package testkit

// FDB integration tests for the embedded SQL connection. Tests spin up a real
// FoundationDB container and verify that DDL SQL (CREATE/DROP DATABASE/SCHEMA)
// round-trips through the full stack: sql.DB → driver.Conn → parser →
// MetadataOperationsFactory → FDB.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	corequery "fdb.dev/pkg/relational/core/query"
	_ "fdb.dev/pkg/relational/sqldriver"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// clusterFilePath is written once in TestMain and shared across tests.
var clusterFilePath string

// RFC-048 W1 ("no unresolved reference") is now enforced STRUCTURALLY by
// ordinal (positional) column resolution, so this binary needs no armed hook. The ordinal
// PositionalRow is the sole runtime row and FieldValue.evaluateOrdinal is LOUD
// on a miss (*OrdinalResolutionError), never a silent name->NULL. A reference to
// a name absent from a complete row therefore FAILS its query outright — a
// strictly stronger guarantee than the retired report-and-continue hook. Every
// query in every test below is thus checked for free: a green binary IS the
// standing E2E proof of the RFC-048 success criterion ("no production code path
// emits an unresolved reference"). The unit-level proof of the loud-miss
// mechanism lives in
// pkg/recordlayer/query/plan/cascades/values/w1_unresolved_reference_test.go.

func Main(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		// No Docker — run non-FDB tests only.
		os.Exit(m.Run())
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFile: %v\n", err)
		os.Exit(1)
	}

	tmp, err := os.CreateTemp("", "fdb-sqldriver-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateTemp: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "WriteString: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	clusterFilePath = tmp.Name()

	os.Exit(runUnderLegIdentityCensus(m))
}

// runUnderLegIdentityCensus runs the suite under the leg-identity census and
// then ASSERTS its invariants. This is the standing gate for CQ-61's retyping of
// RecordTypeLeg's identity from text to a CorrelationIdentifier.
//
// The census answers "does any leg get STORED under one spelling and LOOKED UP
// under another?" — the question that decides whether the leg-identity
// comparisons being EXACT (values.SameLeg, matching Java, which never case-folds
// an alias) binds a different row than folding would.
//
// The gate lives HERE, in TestMain, rather than in a test, because the question
// is about the traffic of the WHOLE corpus and Go gives tests no ordering: only
// after m.Run() is the population complete. It is unconditional rather than
// env-gated because a proof nothing in CI runs is not a proof — the previous
// env-gated form reported the real numbers only under a manual
// LEG_IDENTITY_CENSUS=1 invocation, while the in-CI assertion it delegated to
// saw six of the eight sites at zero.
//
// Enabling the counters always is affordable HERE and nowhere else: the gate
// exists so production never pays an atomic in the per-row executor loop, and a
// test binary is not production. The measured cost is inside the run-to-run
// noise of this suite.
func runUnderLegIdentityCensus(m *testing.M) int {
	values.ResetLegIdentityCensus()
	values.ResetDottedLegQualifierCensus()
	values.ResetSeedWindowReaderCensus()
	values.ResetAccessorPathCensus()
	values.ResetFieldValueMintCensus()
	values.ResetOrderingBridgeDottedCensus()
	values.ResetDottedRowTypeProducerCensus()
	values.ResetDottedWitnessAttribution()
	values.ResetQualifierRecoveryCensus()
	corequery.ResetUnnestLegMintCensus()
	cascades.ResetMergeSlotTypingCensus()
	values.SetLegIdentityCensusEnabled(true)
	code := m.Run()
	values.SetLegIdentityCensusEnabled(false)

	values.ReportLegIdentityCensus(os.Stderr, "sqldriver real-FDB corpus")
	// The ACCESSOR-NAME-PATH census: which arm of the one match-domain column
	// identity carries traffic. Its DECLINE-lazy-dotted class is the field-decision
	// ratchet's accessor_name_path entry, and zero vs non-zero there mean opposite
	// things about where that debt actually lives.
	values.DumpAccessorPathCensus(os.Stderr, "sqldriver real-FDB corpus")
	// ASSERTED, not merely printed. Both of these were printed only, which made
	// the numbers the lazy-render retirement rests on a report nothing checked —
	// they survived in one unparsed `why` string on the field-decision ratchet, so
	// a regression back to 4 declines and 21,865 rendered mints would have failed
	// nothing at all.
	if failed := assertAccessorPathCensus(os.Stderr); failed && code == 0 {
		code = 1
	}
	// The MINT census: who builds a lazy FieldValue and what they put in its
	// Field. The consumer census says a rendered Explain label reaches the
	// match-domain identity; this is the half that can name who wrote it.
	values.DumpFieldValueMintCensus(os.Stderr, "sqldriver real-FDB corpus")
	if failed := assertFieldValueMintCensus(os.Stderr); failed && code == 0 {
		code = 1
	}
	values.DumpOrderingBridgeDottedCensus(os.Stderr, "sqldriver real-FDB corpus")
	// The MERGE-SLOT TYPING census: what each slot of a positional merge ends up
	// STATING. It is the surviving half of the retired leg-local bakeability
	// census — the positional merge outlived the three-quantifier NLJ arm that
	// census existed to measure, and a live path with no instrument is how the
	// silent zero-rows defect it watches for gets back in (RFC-235).
	fmt.Fprintf(os.Stderr, "\n[sqldriver real-FDB corpus] %s\n", cascades.FormatMergeSlotTypingCensus())
	// The leg-column PROVENANCE census: the executor's last live reader of a
	// dotted leg-qualified column name, cut by whether the leg it matched by TEXT
	// also states an IDENTITY. That is the fact the reader's retirement rests on,
	// and it is a different fact from how often the reader fires.
	fmt.Fprintf(os.Stderr, "\n[sqldriver real-FDB corpus] %s\n", executor.FormatLegColumnProvenanceCensus())
	// The TRANSLATOR twin of the executor's leg-column provenance census: the two
	// readers that match a name-split qualifier against a leg table's text. They
	// hold no correlation and cannot be re-keyed, so what this measures is whether
	// the leg table's two spellings still agree on the one channel that has to
	// keep working until its counterparty carries parsed segments.
	fmt.Fprintf(os.Stderr, "\n[sqldriver real-FDB corpus] %s\n", values.FormatDottedLegQualifierCensus())
	// The seed-window READER census: the five keyed readers of an
	// OrdinalSeedLegWindows map, plus the two decline classes that are hard zeros.
	// Its predecessor measured whether a text key and an identity key selected the
	// same window; that question died with the text namespace, but the five
	// readers did not, and nothing else asserts they still run. It is here rather
	// than in a test for the reason every census on this path is: the population
	// is only complete after m.Run().
	fmt.Fprintf(os.Stderr, "\n[sqldriver real-FDB corpus] %s\n", values.FormatSeedWindowReaderCensus())
	// The SELECT RESULT-VALUE MINT census: the site that BUILDS a select's result
	// value, as against the three that flow it verbatim. The producer census
	// above can only name the FlatMap construction that handed a value over, and
	// three of its four sites pass sel.GetResultValue() through unchanged —
	// exactly as Java's three constructions do
	// (ImplementNestedLoopJoinRule.java:187,201,214). Their untyped counts are a
	// count of couriers; this is the author.
	fmt.Fprintf(os.Stderr, "\n[sqldriver real-FDB corpus] %s\n", values.FormatSelectResultMintCensus())
	// The UNNEST LEG-MINT census: which of the five call sites of the surviving
	// qualified-name unnest rebase the corpus reaches, and what names it mints.
	// Reported beside the leg-column provenance census because the assertion
	// below is a claim about BOTH populations at once — the acceptance condition
	// booked for retiring the executor's dotted arm ("dotted hits -> 0") was
	// booked against converting this mint, and whether that is even reachable
	// depends on whether these two name sets meet.
	// The DOTTED ROW-TYPE PRODUCER census: whether the GENERIC
	// RecordConstructorValue.Type path derives a LEG.COL-shaped row, i.e. whether
	// it is a second producer of the row a leg-table population would target.
	// The live guard adopts a populated leg table over an empty one and refuses
	// only two that DISAGREE, so the producer SET is what decides whether a
	// second derivation could state different boundaries for the same row.
	// RFC-212 §10.3 DELIVERABLE 1, gate-before-conversion: which producer named
	// the leg-type column each dotted-arm answer comes from. Decided BY IDENTITY
	// (owner correlation), not by name match.
	fmt.Fprintf(os.Stderr, "\n[sqldriver real-FDB corpus] %s\n", values.FormatDottedWitnessAttribution())
	fmt.Fprintf(os.Stderr, "\n[sqldriver real-FDB corpus] %s\n", values.FormatDottedRowTypeProducerCensus())
	fmt.Fprintf(os.Stderr, "\n[sqldriver real-FDB corpus] %s\n", corequery.FormatUnnestLegMintCensus())
	// The RFC-213 payoff census: how often a consumer that must decide on a plan's
	// result type is handed an UNRESOLVED one and declines. Declining is invisible
	// — it costs a proof or an optimization, never a wrong row — so the size of the
	// loss has to be counted rather than argued.
	fmt.Fprintf(os.Stderr, "\n[sqldriver real-FDB corpus] %s\n", cascades.FormatUnresolvedResultTypeCensus())

	// THE GATES, run through the reporter so a failure carries a `--- FAIL:` line
	// naming which one moved. They used to assert inline here, each writing prose
	// to stderr and bumping the exit code, which produced a red package with no
	// failure marker anywhere in its output — see census_gate_reporting_test.go
	// for what that cost and why the gates cannot simply become test functions.
	//
	// Their reports are emitted together at the end rather than interleaved with
	// the census dumps above, because the SET of gates that moved is the
	// diagnosis, and a set is easier to read collected than scattered through
	// thirty thousand lines of census.
	if RunCensusGates(os.Stderr, testing.Verbose(), []CensusGate{
		{"dottedWitnessAttribution", assertDottedWitnessAttributionCensus},
		{"dottedRowTypeProducer", assertDottedRowTypeProducerCensus},
		{"unnestLegMint", func(w io.Writer) bool {
			return corequery.AssertUnnestLegMintCensus(w, executor.LegColumnProvenanceDottedNames())
		}},
		{"dottedLegQualifier", assertDottedLegQualifierCensus},
		{"seedWindowReader", assertSeedWindowReaderCensus},
		{"nameSplit", assertNameSplitCensus},
		// The QUALIFIER RECOVERY census — the four DARK SPLITTERS the name-split
		// census names in its header and does not watch. Six sites, because the
		// parseColRef family contributes three DIFFERENT decisions with three
		// different counterparties and one merged number could answer the
		// conversion question for none of them.
		{"qualifierRecovery", assertQualifierRecoveryCensus},
		{"legIdentity", assertLegIdentityCensus},
		{"legColumnProvenance", assertLegColumnProvenanceCensus},
		// The MERGE-SLOT TYPING census is ASSERTED, not merely printed. Its whole
		// claim is that Untyped stays at ZERO, and a zero from a dead counter reads
		// identically to a zero from a clean population — the floor is what tells
		// them apart. It is the surviving half of the retired leg-local bakeability
		// census (RFC-235): the positional merge outlived the NLJ arm that census
		// measured, and a live path with no instrument is how the silent zero-rows
		// defect it watches for gets back in.
		{"mergeSlotTyping", assertMergeSlotTypingCensus},
		{"selectResultMint", assertSelectResultMintCensus},
		// RFC-213: the consumers must stay REACHED. There is no zero to defend —
		// the unresolved reads ARE the defect and their count is a measurement,
		// not a contract — but if these sites go dark, a later "unresolved is 0"
		// would be indistinguishable from having fixed it. Floored an order of
		// magnitude below the measured 15,909 classified reads.
		{"unresolvedResultType", assertUnresolvedResultTypeCensus},
	}) && code == 0 {
		code = 1
	}
	return code
}

// legColumnProvenanceFloors is the minimum population the leg-column provenance
// census must report over the whole suite.
//
// RETIREMENT MEASURED, AND IT IS A DRAIN RATHER THAN A REROUTE. RFC-212 §11.3
// retitled the producer and the dotted arm went from 2 answers to 0. The
// statistic that separates those two readings is flatHit, which was 120 on ALL
// FIVE of the runs enumerated below — the one number on this census that did not
// move while the call total swung by nearly 300 — and is now 122. Exactly +2,
// exactly the two dotted hits that disappeared, landing in the arm they must land
// in when the name stops splitting: the FLAT lookup answers them instead. A
// reroute would show no compensating +2 in any sibling arm.
//
// The drift also runs the SAFE way. The AFTER run reported 2534 calls, inside the
// magnitude below; the BEFORE run reported 1174, an unexplained low outlier at
// less than half the band floor — and NOT a narrowed run (no -test.run, and the
// harness printed none of its narrowing notices). So the zero was measured on the
// LARGER population, 2.16x the before side, which is the direction that cannot
// manufacture a zero.
//
// RE-MEASURED over this corpus, 2026-08-06: dotted hits available 2 (`C.CV`,
// `I.QTY`), unstated 0, diverged 0 — STABLE across five full-suite runs. The
// CALL total is not stable and is quoted as a MAGNITUDE, ≈2.4–2.7k: 2394, 2474,
// 2554, 2554, 2674 across those same five runs (flatHit 120 every time; the
// movement is all in notDotted).
//
// The enumeration is kept because it is the argument. An earlier revision
// quoted three of those values as an exhaustive list — "2554, 2554, 2674" — and
// the next run landed outside all three; the run after that landed outside the
// range that correction then wrote. A bounded-looking enumeration of an
// unbounded quantity reads as a pin and decays into a wrong one, twice in a row
// here. Quote the magnitude or quote nothing, for the reason the leg-identity
// census states at its own population line — this site is sampled inside
// readers that rules drive, and the memo may explore a rule once or many times
// for one query depending on exploration order.
//
// PRESENCE is what holds; MULTIPLICITY is what moves. The dotted-HIT count is 2
// in every one of the five runs while the total swings by ~12%, and that is the
// distinction the floors below rest on: exploration order scales how often a
// shape is visited, it does not invent or delete the shapes the corpus contains.
//
// The number the retirement decision rests on is the DOTTED-HIT count, and that
// one is stable at 2.
//
// The previous reading in this block — "calls 52 (flatHit 40, notDotted 8),
// dotted hits available 4" — was wrong in BOTH directions and is kept here as
// the history it is: the call total was low by a factor of fifty (the corpus
// grew under it) while the dotted-hit count was HIGH by two. The second error is
// the dangerous one. This census's retirement decision rests entirely on the
// dotted-hit population, so a stale 4 overstates the reader's remaining reach by
// double, and nothing read the instrument to notice. The floors held throughout,
// which is the point of flooring rather than pinning — and also why a floor is
// no protection at all against a comment.
//
// Both floors are 1, not an order of magnitude below the measurement, because
// there is no order of magnitude below 2. What is being detected here is
// DISAPPEARANCE: the shapes that drive the dotted arm ceasing to be planned, or
// the reader ceasing to be reached. DottedHitIdentityAvailable is floored
// separately from Calls because the non-dotted arms carry all but 2 of the
// calls, so the
// denominator can look healthy while the arm the census exists for goes silent.
// RFC-212 §11.3 RETITLED the producer, and the dotted arm now answers ZERO
// times over the whole corpus (measured: available 2 -> 0). The
// DottedHitIdentityAvailable FLOOR is therefore retired with the population it
// guarded — it is unsatisfiable by construction now, and a floor that cannot be
// met is a build break rather than a guard.
//
// THE DANGEROUS DIRECTION HAS FLIPPED, and that is the whole point: this
// population was watched for COLLAPSE while the arm was live, because a zero read
// like good news. Now zero IS the news, so growth is the alarm — a non-zero means
// some producer is again naming a leg type's column with a dot-containing title,
// and the arm the retitling emptied is answering again. AssertLegColumnProvenanceCensus
// holds that at a hard zero; only the Calls floor remains, because the reader
// itself is still live on its FLAT arm and a census reaching it zero times would
// make that zero vacuous.
// legColumnProvenanceFloors is EMPTY, and the emptiness is the reconciliation.
//
// It used to floor Calls at 100 (measured between 300 and 1500 across runs, an
// unstable population that a tight floor would have red-flagged on churn alone),
// so that a reader nothing reaches could not report the same shape as a reader
// with nothing wrong. The reader is now retired: adaptLegPositional's
// layout-permutation gather is its only driver, and the exact-ordinal seed bakes
// against the chosen physical leg layout — Java's translateCorrelations
// behaviour, which that gather's own note named as the thing that would end it —
// so every leg row passes positionalMatchesLegType and the gather is skipped.
//
// The census asserts Calls == 0 unconditionally now, with revival as the alarm.
var legColumnProvenanceFloors = executor.LegColumnProvenanceFloors{}

// assertLegColumnProvenanceCensus checks the provenance census, dropping the
// population floors when -test.run narrows the corpus — the same split its two
// siblings make, for the same reason.
func assertLegColumnProvenanceCensus(w io.Writer) bool {
	floors := &legColumnProvenanceFloors
	if f := corpusNarrowing(); f != nil && f.Value.String() != "" {
		c, _ := executor.LegColumnProvenanceCensus()
		fmt.Fprintf(w, "leg-column provenance census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus). The partition and the divergence zero "+
			"still run, over the population this filter actually reached: calls %d, "+
			"dotted hits with an identity available %d. At zero they hold VACUOUSLY.\n",
			f.Value.String(), c.Calls, c.DottedHitIdentityAvailable)
		floors = nil
	}
	return executor.AssertLegColumnProvenanceCensus(w, floors)
}

// dottedLegQualifierFloors is EMPTY: the whole channel is retired.
//
// It used to floor the match attempts each translator dotted-leg reader made
// over the suite — 10 for flatColumnBake (measured 106) and 1 for legQOVBake
// (measured 4, with no order of magnitude to drop to) — because that census's
// two hard zeros hold vacuously over an empty population.
//
// Both readers were arms of the NAME-model bake (query.bakeFlatRefsAgainstColumns
// and query.bakeDottedRefsToLegQOV), which resolved a reference by splitting a
// column name at a dot. The ordinal model resolves by baked slot and those bakes
// are gone, so values.RecordDottedLegQualifier has no caller at all and the
// floors are unsatisfiable. The census asserts zero attempts unconditionally
// now, with revival as the alarm.
var dottedLegQualifierFloors = values.DottedLegQualifierFloors{}

// seedWindowReaderFloors is the minimum keyed-read count each seed-window reader
// must report over the whole suite.
//
// Set an ORDER OF MAGNITUDE below the measured population, like its siblings:
// what a floor detects is the site going DARK, not drift. The reader population
// churns with unrelated work — a query added anywhere in this suite moves every
// one of these numbers — and a floor pinned tight would red on that instead of
// on the thing it is watching for.
var seedWindowReaderFloors = func() values.SeedWindowReaderFloors {
	var f values.SeedWindowReaderFloors
	// RETIRED, so its alarm is GROWTH. The buried-leg EXISTS rebase measured 0
	// over the whole suite once select partitioning followed Java's predicate
	// placement: an existential's correlated predicates stay in the partition
	// holding the existential and never reach the NLJ beside an ordinal-seed outer.
	f.Retired[values.SeedWindowSiteExistentialRebase] = true
	// Measured over the whole sqldriver suite at the RFC-257 migration tree.
	f.Reads[values.SeedWindowSiteBoxLegRef] = 22             // measured 221
	f.Reads[values.SeedWindowSiteBoxSurvivorQOV] = 43        // measured 431
	f.Reads[values.SeedWindowSiteBoxSurvivorCorrelation] = 4 // measured 42
	f.Reads[values.SeedWindowSiteGatheredGroupSlot] = 207    // measured 2077
	// RFC-200's ACTIVATION TRIPWIRE. Measured NESTED-HIT 0 at every site: the
	// nested reader arm is correct and unreached, so gate (a)'s four mutation
	// directions are not writable and the branch merged with that stated.
	//
	// A non-zero is GOOD NEWS that requires ACTION, and it is asserted rather
	// than printed so that activation day is a red test with a hand-over in its
	// message instead of a number nobody diffs. See the failure text and
	// CQ-67's reopen trigger.
	f.NestedHitMustBeZero = true
	return f
}()

// assertSeedWindowReaderCensus checks the seed-window reader census, dropping
// the population floors when -test.run narrows the corpus — the same split its
// siblings make, for the same reason.
func assertSeedWindowReaderCensus(w io.Writer) bool {
	floors := &seedWindowReaderFloors
	if f := corpusNarrowing(); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "seed-window reader census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus). The two decline hard zeros "+
			"(QUALIFIED-NO-IDENTITY, CHILDLESS-BAKED) and the retired sites' zeros "+
			"still run, over whatever population this filter reached — at zero they "+
			"hold VACUOUSLY.\n",
			f.Value.String())
		// A revival is visible on any population, so retirement survives narrowing.
		floors = &values.SeedWindowReaderFloors{Retired: seedWindowReaderFloors.Retired}
	}
	return values.AssertSeedWindowReaderCensus(w, floors)
}

// nameSplitFloors is EMPTY: the whole channel is retired.
//
// It used to floor the population each splitting arm reported over the suite
// (legQOVSegmentsOf calls 1, measured 9; flatColumnBake calls 1 and splits 1,
// measured 2), because this census's content was a HARD ZERO on SPLIT-QUALIFIED
// and a hard zero over an empty population is the fake-green shape every
// instrument on this path was rebuilt to end.
//
// Both arms lived inside the NAME-model bake — query.legQOVSegmentsOf and
// query.bakeFlatRefsAgainstColumns, which decided qualification by counting a
// reference's name segments. The ordinal model decides by baked slot, both bakes
// are gone, and values.RecordNameSplit has no caller at all. So the CALL floors
// are unsatisfiable, and the SPLIT-QUALIFIED zero they protected is now trivially
// structural rather than a corpus fact worth guarding. The census asserts zero
// calls at every site unconditionally, with revival as the alarm.
var nameSplitFloors = values.NameSplitFloors{}

// assertNameSplitCensus checks the translator name-split census.
//
// There is NO -test.run exemption here, unlike its still-live siblings. Those
// drop their floors under a filter because a population floor describes the
// unfiltered suite. This channel is retired and its assertion is a REVIVAL
// alarm — "no site was reached at all" — which is exactly as true over a
// narrowed corpus as over the whole one. Skipping it under a filter would be
// the one direction that fails open.
func assertNameSplitCensus(w io.Writer) bool {
	return values.AssertNameSplitCensus(w, &nameSplitFloors)
}

// qualifierRecoveryFloors watches collapse at the live sites reached by this
// corpus. Derived UNNEST, projection-scope classification and the display-label
// strip are fully retired; their stable sites forbid all calls independently of
// these floors.
var qualifierRecoveryFloors = values.QualifierRecoveryFloors{
	Calls: [6]int{
		// recursiveRemap: no entry. The site is retired and has no caller; its
		// revival is watched by the Split declaration below, which fires on any
		// class but CARRIED — and a retired recorder cannot report CARRIED
		// either, because it cannot report at all.
		values.QualRecSiteExistsSortSplit: 4,
		// projQualVsScan: no entry. The site is unreached over this corpus and
		// the Split DECLARATION is what watches it. It never records CARRIED
		// (recordProjQualVsScan classifies bare/AGREED/DIVERGED/MANUFACTURED
		// only), so its Split floor and its Calls floor cover the identical
		// population and one of the two would be redundant.
	},
	// The SPLIT floors carry the weight at the remaining live splitters.
	Split: [6]int{
		// recursiveRemap carries no entry because its splitting arm is
		// structurally gone. projScopeClassify is stronger: every call is retired.
		//
		// projQualVsScan IS here, at 0, and the difference is the point: its
		// recorder and its call site both still stand, and what stopped is the
		// corpus REACHING them. That is a claim about this suite, so it is a
		// declaration that a filter may drop — not a tree fact.
		values.QualRecSiteProjQualVsScan:  0,
		values.QualRecSiteExistsSortSplit: 4,
	},
}

// qualifierRecoveryRetiredSplit names the sites whose splitting arm is gone from
// the TREE, not merely unreached by this corpus. Their alarm is inverted — any
// split is the arm coming back — and unlike a floor it is checked under a
// narrowed run too, because a tree fact holds over any population.
//
//   - recursiveRemap: values.RecordQualifierRecovery is not called with this site
//     anywhere in non-test sources; query.recursiveRemapValues is a retired
//     compatibility no-op.
var qualifierRecoveryRetiredSplit = func() (r [6]bool) {
	r[values.QualRecSiteRecursiveRemap] = true
	return r
}()

// assertQualifierRecoveryCensus checks the dark-splitter census, dropping the
// population floors when -test.run narrows the corpus. The DIVERGED hard zero
// and the witness saturation guards still run — they are defects over any
// population — while the floors describe the unfiltered suite and are
// meaningless under a filter.
func assertQualifierRecoveryCensus(w io.Writer) bool {
	floors := &qualifierRecoveryFloors
	if f := corpusNarrowing(); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "qualifier recovery census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus). The DIVERGED hard zero and the witness "+
			"saturation guards still run, over whatever population this filter reached — "+
			"at zero the former holds VACUOUSLY.\n", f.Value.String())
		floors = nil
	}
	// No AllowedDiverged: every split in this corpus comes from a production
	// producer because its input is SQL, so a disagreement here is a defect and
	// the zero is a BARE zero. The translator harness needs an allowlist; this
	// one must never grow one.
	return values.AssertQualifierRecoveryCensus(w,
		&values.QualifierRecoveryExpectations{
			Floors:       floors,
			RetiredSplit: qualifierRecoveryRetiredSplit,
			RetiredCalls: [6]bool{
				values.QualRecSiteDerivedUnnestSource: true,
				values.QualRecSiteProjScopeClassify:   true,
				values.QualRecSiteDisplayLabelStrip:   true,
			},
		},
		"sqldriver real-FDB corpus")
}

// assertDottedLegQualifierCensus checks the translator dotted-leg census.
//
// No -test.run exemption, for the same reason as assertNameSplitCensus above:
// the channel is retired and this is its revival alarm, which holds over any
// population.
func assertDottedLegQualifierCensus(w io.Writer) bool {
	return values.AssertDottedLegQualifierCensus(w, &dottedLegQualifierFloors)
}

// legIdentityFloors is the minimum population each site must report over the
// whole suite. A site at ZERO makes every zero asserted about it vacuous, which
// is precisely how the previous form of this gate passed while proving nothing.
//
// The floors are set an order of magnitude below the measured populations, and
// that gap is doing TWO jobs. The corpus grows and shrinks with unrelated work,
// so a floor at the measured value would fail on churn rather than on a site
// going dark — and the populations themselves are NOT stable run to run.
// Successive full-suite measurements of the same tree gave text-vs-identity
// 3115 / 3270 / 3320; only rowLegsBinder (285) and buriedLegWindow (567)
// repeated exactly. The variance is planning-side: the memo may explore a rule
// once or many times for one query depending on exploration order, and several
// sites sit inside rules. What the floor detects is COLLAPSE — a producer
// stopping, a reader being routed around, a rule that no longer fires — not
// drift, and it is set loosely enough that the observed variance cannot reach
// it.
//
// FOUR OF THE EIGHT SITES ARE NO LONGER FLOORED, and the reason splits in two.
// A floor watches for collapse; once zero is the steady state a floor is
// unsatisfiable and the danger inverts to GROWTH, so each of the four moves to
// the guard that matches what it now is rather than being lowered or dropped:
//
//   - RETIRED (legIdentityRetired, a fact about the TREE):
//     hoistLegRefsOntoMergedRow and expressionOutputLegs. Neither site appears in
//     any non-test source — values.RecordLegIdentity{Leg,Comparison,Conversion}
//     is never called with either constant — so their floors of 64 and 256 were
//     unsatisfiable rather than merely generous.
//
//   - DISPLACED (legIdentityDeclaredEmpty, a fact about this CORPUS):
//     rowLegsBinder.GetCorrelationBinding and buriedLegWindow. Both readers still
//     stand and both are still reachable. What changed is the route:
//     frontierRowContext dispatches on pr.Layout FIRST and only falls through to
//     the leg-name binder for a row that carries none, and under the ordinal
//     model an admitted physical row always carries its exact layout. Their
//     declarations are checked in the stale direction, so the day a Layout-less
//     row reaches them again the gate says so instead of quietly re-arming a
//     path nothing measures.
//
// The remaining four are floored as before. The NLJ site's total counts Cascades
// rule FIRINGS rather than queries, so it is the most refire-sensitive; its
// floor is the loosest for that reason.
var legIdentityFloors = map[values.LegIdentitySite]int64{
	values.LegSiteTextVsIdentity:         256,
	values.LegSiteFinalizeSeedWindows:    128,
	values.LegSiteNLJPlanAlias:           4096,
	values.LegSiteOrdinalSlotInLegWindow: 8,
}

// legIdentityRetired names the two sites with no production caller anywhere in
// the tree. Any traffic at either is a revival, and the check runs under a
// -test.run filter too — a tree fact holds over the empty population a filter
// leaves behind, and skipping it there is the one direction that fails open.
var legIdentityRetired = map[values.LegIdentitySite]string{
	values.LegSiteLeftOuterExistential: "hoistLegRefsOntoMergedRow's drift check was removed with the " +
		"name-model hoist; no non-test source records at this site.",
	values.LegSiteSelectOutputLegs: "expressionOutputLegs compared a quantifier against sourceAliases " +
		"text; the producer is gone and no non-test source records at this site.",
}

// legIdentityDeclaredEmpty names the two readers the ordinal layout DISPLACED
// rather than deleted. They are declarations about this corpus, not the tree —
// the code is still reachable by a positional row carrying no exact layout —
// and they are checked in the stale direction so the declaration cannot outlive
// its condition.
var legIdentityDeclaredEmpty = map[values.LegIdentitySite]string{
	values.LegSiteRowLegsBinder: "frontierRowContext binds through pr.Layout before it reaches the " +
		"leg-name binder, and an admitted physical row always carries one.",
	values.LegSiteBuriedLegWindow: "the buried-window walk sits under the same layout dispatch, so a " +
		"row with an exact layout never descends into it.",
}

// assertLegIdentityCensus checks the whole-suite census and reports whether it
// failed.
//
// Only the population FLOORS are corpus-shaped, so only they are dropped when
// -test.run narrows the run: FIVE zeros hold over ANY population, one query or
// eighty thousand firings — fold-only, unstated, retired-verdict divergence,
// text-vs-identity divergence and mixed instrument — and a filtered invocation
// checks them exactly as the full suite does. The earlier form returned before all
// of them and announced only that the floors were unchecked, so a focused run
// reported a passing gate while five assertions had silently not run.
//
// The enumeration is kept HONEST deliberately. It read "four" while
// values.AssertLegIdentityCensus ran five, and the omitted one was the
// retired-verdict zero — the assertion that compares this site's converted answer
// against the text predicate it replaced, i.e. the only one that measures the
// conversion rather than the representation. An enumeration that drops the
// headline check reads as reassurance about the wrong thing.
func assertLegIdentityCensus(w io.Writer) bool {
	floors := legIdentityFloors
	if f := corpusNarrowing(); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "leg-identity census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus; the floors describe the whole suite). "+
			"The fold-only, unstated, retired-verdict-divergence, text-vs-identity and "+
			"instrument-channel zeros ARE still checked — over whatever population this "+
			"filter reached. At zero they hold VACUOUSLY, which is exactly what the "+
			"dropped floors exist to prevent; only the unfiltered suite makes them a "+
			"proof.\n",
			f.Value.String())
		floors = nil
	}
	return values.AssertLegIdentityCensusWith(w, values.LegIdentityExpectations{
		Floors:        floors,
		Retired:       legIdentityRetired,
		DeclaredEmpty: legIdentityDeclaredEmpty,
	})
}

// openTestDB returns a *sql.DB wired to the test FDB container.
// Skips the test if Docker is not available.
func OpenDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s", strings.ToUpper(dbPath), clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// orientationGateFloors is RFC-200 step 3d”s live/latent discriminator.
//
// MEASURED over this corpus: calls 438 (not-a-seed 96, tiled-by-2 342,
// tiled-by-other 0); of the tiled-by-2, unverifiable 84, matched 197, declined
// 61; and 72 firings where the MAP count differs from the TILE count — of which
// DECLINED zero.
//
// That last pair is the whole explanation for why 3d' moved no plan. 72 firings
// that the old map-count gate skipped are now checked, and every one of them
// matches; the 61 declines were all firings the old gate already checked and
// already declined. So the step changes no decision on this corpus while making
// 72 previously-unanswerable firings answerable.
//
// Floored an order of magnitude below, like every population floor on this path:
// what a floor detects here is the shape going DARK, not drift.
// RE-MEASURED at RFC-226, because §1c relaxed this exact gate's type comparison
// and a bound nobody re-read after changing the thing it watches is not a bound.
// Current corpus: calls 506 (not-a-seed 102, tiled-by-2 404, tiled-by-other 0);
// unverifiable 104, matched 232, declined 68; MapCountDiffers 92, of which
// DECLINED 0.
//
// WHAT THIS CHANGE MOVES: NOTHING, on the pre-existing corpus. Established by
// the only control that answers that question — the PRE-CHANGE baseline, not a
// mutation of the branch:
//
//	master aba271454        calls 496  unverifiable 104  matched 224  DECLINED 66
//	branch, probe file OUT  calls 496  unverifiable 104  matched 224  DECLINED 66
//	branch, probe file IN   calls 506  unverifiable 104  matched 232  DECLINED 68
//
// The middle row is the whole answer: with this branch's engine changes applied
// and ONLY its new test queries removed, the census is bit-identical to master.
// So the +10 calls / +8 matched / +2 declined are NEW FIRINGS contributed by
// projection_result_type_probe_fdb_test.go's two WHERE-EXISTS queries — not
// existing firings that flipped INTO declining, which is the reading the census
// alone cannot rule out and which would have been a real alarm.
//
// A PRIOR REVISION OF THIS COMMENT GOT THAT WRONG, and the error is kept visible
// because the reasoning was seductive: it isolated §1c's arm by MUTATING THE
// BRANCH (removing the unstated-field arm of recordFieldsMatch — calls 504,
// matched 230, declined 68) and concluded "Declined does not move at all". That
// measures what §1c's ARM does, not what this CHANGE does, and it swept the
// whole 61 -> 68 drift into "corpus growth". Only 61 -> 66 is growth; 66 -> 68
// is this branch. The unverifiable claim survives intact — master already reads
// 104, so 84 -> 104 is growth.
//
// ON THE CEILING, stated precisely because the loose phrasing gives away the
// stronger claim: DeclinedCeiling is not "200, unchanged". Master aba271454 has
// flatMapProducerFloors gates the FlatMap result-value producer census.
//
// The floors are ORDER-OF-MAGNITUDE below the measurement, like every other
// per-site floor on this path: they exist to catch a site going dark, not to
// re-bless a corpus count that moves whenever a test file is added.
//
// The FlatMap PRODUCER census that used to sit beside this one is retired with
// the three-quantifier NLJ arm it measured (RFC-235): its whole subject was the
// declined-leg residue that arm produced, and there is no residue without the
// arm. What it established still holds and is recorded in RFC-235 rather than
// here — the refusal was on SHAPE, not on missing types.
//
// The untyped-QOV mints those sites emit are a SEPARATE live Java divergence
// (CQ-96) and are floored below so they stay counted.
//
// TWO OF THOSE THREE ARE COURIERS, NOT AUTHORS, and the mint census beside this
// one is what says so: implementExistentialSelect and yieldExistsFlatMap flow
// sel.GetResultValue() verbatim (Java's three constructions do the same,
// ImplementNestedLoopJoinRule.java:187,201,214), and 1086 of their untyped
// traffic is minted by the SQL translator. These floors keep the traffic
// counted; selectResultMintFloors is where the divergence itself is booked.

// selectResultMintFloors gates the select result-value MINT census.
//
// This is the site that BUILT the untyped QuantifiedObjectValue Java cannot
// express. The producer census beside it reported that population at
// implementExistentialSelect and yieldExistsFlatMap — both of which flow
// sel.GetResultValue() verbatim and build nothing, exactly as Java's three
// RecordQueryFlatMapPlan constructions do
// (ImplementNestedLoopJoinRule.java:187,201,214). Booking the divergence against
// a courier is what this census corrected.
//
// Java's own guarantee is structural: a simple select's result value is
// overQuantifier.getFlowedObjectValue() (GraphExpansion.java:401),
// QuantifiedObjectValue.of has no untyped overload
// (QuantifiedObjectValue.java:187), and Quantifier.getFlowedObjectType is a
// Verify.verify plus requireNonNull (Quantifier.java:801-810).
//
// GO NOW HAS THE SAME GUARANTEE, and the floor that kept the gap counted is
// therefore gone. Measured over the whole real-FDB corpus:
//
//	translator buildExistsSelect(MINT)  calls 1006 | typedQOV 1006 | UNTYPED 0
//
// Every mint is TYPED, where every mint used to be untyped. The untyped floor
// (100, an order of magnitude below a measured 1086) is unsatisfiable against a
// population the constructor makes unrepresentable, so the census asserts the
// zero unconditionally instead, with the alarm pointing at revival.
//
// THE TOTAL IS NOT DETERMINISTIC AND THE RATIO IS. Consecutive full-suite runs
// measured 1086, 1004 and 1006 — these are RULE FIRINGS, and the memo explores a
// rule a different number of times per query run to run, exactly as the FlatMap
// producer census's own totals move. What did not move is the ratio: 100%
// untyped before, 100% typed after. So the call floor is calibrated an order of
// magnitude below the smallest observation, and anyone re-measuring should
// expect a different digit and check the RATIO.
var selectResultMintFloors = func() values.SelectResultMintFloors {
	var f values.SelectResultMintFloors
	f.Calls = [values.SelectResultMintSiteCount]int{100}
	return f
}()

func assertSelectResultMintCensus(w io.Writer) bool {
	floors := &selectResultMintFloors
	if f := corpusNarrowing(); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "select mint census: per-site floors NOT checked "+
			"(-test.run=%q narrowed the corpus). The partition still runs — it holds "+
			"over ANY population.\n", f.Value.String())
		floors = nil
	}
	return values.AssertSelectResultMintCensus(w, floors)
}

// mergeSlotTypingFloor is the measured population of positional-merge slots the
// real-FDB corpus builds (22,354 on the run this was derived from; the total
// moves with rule firings, ~22.4k), floored well below it so an ordinary corpus
// edit does not trip the guard while a collapse still does.
const mergeSlotTypingFloor = 2000

// assertMergeSlotTypingCensus checks the merge-slot typing census against its
// partition identity, its floor, and its hard zero.
//
// It is dropped under -test.run for the same reason its siblings are: a narrowed
// corpus measures a subset, and a subset cannot satisfy a whole-suite floor.
func assertMergeSlotTypingCensus(w io.Writer) bool {
	if f := corpusNarrowing(); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "merge-slot typing census: floor NOT checked (-test.run=%q narrowed the "+
			"corpus). The partition identity and the Untyped zero still run — both hold over any "+
			"population.\n", f.Value.String())
		return cascades.AssertMergeSlotTypingCensus(w, cascades.MergeSlotTypingCensus(), 0)
	}
	return cascades.AssertMergeSlotTypingCensus(w, cascades.MergeSlotTypingCensus(), mergeSlotTypingFloor)
}
