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
	checkRegistered()
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
// env-gated because a proof nothing in CI runs is not a proof.
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
	// ASSERTED, not merely printed: the lazy-render retirement rests on these
	// numbers, and a printed report fails nothing.
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
	// Nothing else asserts the five readers still run. It is here rather
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
	// naming which one moved; census_gate_reporting_test.go explains why the
	// gates cannot simply become test functions.
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

// legColumnProvenanceFloors is EMPTY: the reader is retired.
//
// RFC-212 §11.3 retitled the producer, so the dotted arm answers zero times over
// the corpus (those names resolve through the FLAT lookup instead), and
// adaptLegPositional's layout-permutation gather, the reader's only driver, is
// skipped because the exact-ordinal seed bakes against the chosen physical leg
// layout (Java's translateCorrelations behaviour), so every leg row passes
// positionalMatchesLegType.
//
// The dangerous direction is therefore growth, not collapse: a non-zero means
// some producer again names a leg type's column with a dot-containing title.
// The census asserts Calls == 0 unconditionally, with revival as the alarm.
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
// Both readers (query.bakeFlatRefsAgainstColumns and query.bakeDottedRefsToLegQOV)
// were arms of the name-model bake, which resolved a reference by splitting a
// column name at a dot. The ordinal model resolves by baked slot, so
// values.RecordDottedLegQualifier has no caller and any floor would be
// unsatisfiable. The census asserts zero attempts unconditionally, with revival
// as the alarm.
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
// Both splitting arms (query.legQOVSegmentsOf and
// query.bakeFlatRefsAgainstColumns) lived inside the name-model bake, which
// decided qualification by counting a reference's name segments. The ordinal
// model decides by baked slot and values.RecordNameSplit has no caller, so the
// SPLIT-QUALIFIED zero is structural rather than a corpus fact. The census
// asserts zero calls at every site unconditionally, with revival as the alarm.
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
// whole suite. A site at ZERO makes every zero asserted about it vacuous.
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
// FOUR OF THE EIGHT SITES ARE NOT FLOORED, and the reason splits in two.
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
// checks them exactly as the full suite does. Keep this list in step with
// values.AssertLegIdentityCensus; the retired-verdict zero is the one that
// measures the conversion rather than the representation.
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

// selectResultMintFloors gates the select result-value MINT census.
//
// This is the site that builds a select's result QuantifiedObjectValue;
// implementExistentialSelect and yieldExistsFlatMap only flow
// sel.GetResultValue() verbatim, exactly as Java's three
// RecordQueryFlatMapPlan constructions do
// (ImplementNestedLoopJoinRule.java:187,201,214), so the census books the
// author, not the couriers.
//
// Java's guarantee is structural: a simple select's result value is
// overQuantifier.getFlowedObjectValue() (GraphExpansion.java:401),
// QuantifiedObjectValue.of has no untyped overload
// (QuantifiedObjectValue.java:187), and Quantifier.getFlowedObjectType is a
// Verify.verify plus requireNonNull (Quantifier.java:801-810). Go has the same
// guarantee: every mint is typed, so the census asserts the untyped zero
// unconditionally, with revival as the alarm.
//
// The call total counts rule firings and is not deterministic run to run
// (≈1000); the typed ratio is. The call floor sits an order of magnitude below
// the smallest observation — when re-measuring, check the ratio, not the digit.
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
