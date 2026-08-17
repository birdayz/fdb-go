package docscheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestUnexportedFuncsAreReferenced is the corpus reading of the detector in
// unreferenced_func_detector_test.go — see there for what the shape is and why
// it is worth gating. This file holds the LEDGER and the vacuity guards.

const (
	// unexportedFuncPopulationMin guards the population whose collapse would
	// make a clean result meaningless. The scan sees ~3,400 unexported
	// no-receiver funcs; a reading far below that means the file walk, the
	// parse, or the candidacy filter broke, and "no violations" would then be a
	// statement about the instrument rather than about the tree.
	unexportedFuncPopulationMin = 2500
	// unexportedFuncPackageMin is the second vacuity guard, on a different
	// axis: the population above could in principle be met by one enormous
	// package while the walk missed every other directory.
	unexportedFuncPackageMin = 90
)

// unreferencedFuncDisposition records why one unexported, production-unreferenced
// function is still in the tree.
//
// AUTHORITY DIRECTION, stated once: this map is the source of truth for the
// gate's exceptions. Any prose elsewhere quotes it; nothing quotes prose back
// into it. That is the same direction `knownFieldDecisionDebt` uses, and for the
// same reason — a doc and a list that can each be edited independently drift,
// and the drift always resolves in favour of whichever one nobody re-ran.
//
// A RATCHET, NOT AN ALLOWLIST. The gate fails on a violation missing from this
// map, and it fails just as loudly on an entry here that is no longer a
// violation — so wiring a function back into production, or deleting it, forces
// deleting its line rather than letting the list rot.
//
// EVERY ENTRY CARRIES AN INDIVIDUAL JUSTIFICATION, and "justification pending"
// is not one. A ledger of N entries under a shared hand-wave is a false green
// wearing a list's clothing: it reports "N known exceptions" while proving
// nothing about any of them. The shape is enforced mechanically rather than by
// convention — see TestUnreferencedFuncLedgerEntriesAreJustified — because the
// three dead twins this gate exists for ALL looked like routine leftovers right
// up to the moment someone asked what the live counterpart did with the same
// input, and two of them were protecting a live defect.
//
// `tag` is one of a closed set, and they partition the ledger:
//
//   - dispositionKeep:     production genuinely does not call it and that is
//     correct. `why` must say what WOULD call it and why nothing does.
//   - dispositionTwin:     a second implementation of something production does
//     elsewhere. `counterpart` is REQUIRED and names the live function, because
//     this is the shape that has hidden a live defect here three times; an entry
//     whose author cannot name the counterpart has not done the comparison, and
//     the function should be deleted rather than listed.
//   - dispositionInFlight: its removal lands in an identified change already in
//     review. `why` must name that change, so the entry expires with it instead
//     of outliving it silently.
//   - dispositionDefect:   deleting it is blocked because the deletion ARMS a
//     defect the gate found — a declared safety net points at this function
//     instead of at the live one, and repointing it turns that net red. This tag
//     is the honest answer when the finding is real and its fix needs a review
//     gate this change does not carry; it is NOT a place to park work, and
//     `unreferencedFuncDefectMax` caps it so it cannot quietly become one. `why`
//     must state the bug, the exact repointing that arms it, and what the fix needs.
const (
	dispositionKeep     = "keep"
	dispositionTwin     = "twin"
	dispositionInFlight = "in-flight"
	dispositionDefect   = "defect"
)

// unreferencedFuncDefectMax caps the defect tag. A ledger that can absorb an
// unbounded number of "known bugs, not fixed here" entries has stopped being a
// ratchet and become a filing cabinet — the exact failure this repo names when
// it says a filed finding rots into invisibility. Raising this number is a
// deliberate act that shows up in a diff; drifting past it is not possible.
const unreferencedFuncDefectMax = 2

type unreferencedFuncDisposition struct {
	tag         string
	counterpart string // required for dispositionTwin: the live function doing the same job
	why         string
}

// unreferencedFuncLedger is keyed by funcSite.key() — "<repo-relative path> # <name>".
var unreferencedFuncLedger = map[string]unreferencedFuncDisposition{
	"pkg/recordlayer/tuple_ordering.go # tupleOrderingUnpack": {
		tag: dispositionKeep,
		why: "not dead — NOT-YET-CONNECTED, and deleting it would delete the answer rather than the problem. It is " +
			"a faithful port of Java's TupleOrdering.unpack (line-for-line on the 7-bit repack, the 0x80|(npad<<4) " +
			"terminator, and both error strings), and Java runs that decoder in PRODUCTION at two sites: " +
			"OrderFunctionKeyExpression.evaluateInverseInternal and FromOrderedBytesValue.eval. Go has the " +
			"corresponding Value type in cascades/values/value_ordered_bytes.go and BOTH halves are stubbed to " +
			"`return nil, nil` — a silent NULL — with a comment conceding that real eval needs exactly this " +
			"function. So the live consumer is the stub, and the decoder is the thing the stub is missing. What " +
			"keeps it off the ledger's delete list is that its 4 tests are the only thing holding the decoder " +
			"honest until FromOrderedBytesValue.Evaluate is wired; the encoder half (tupleOrderingPack) IS live, " +
			"so an unpack that drifted from it would be found here and nowhere else. Retires when the stub is wired",
	},
	"pkg/recordlayer/query/executor/ordinal_join.go # ordinalJoinSpansAcceptingNested": {
		tag:         dispositionTwin,
		counterpart: "values.OrdinalSeedLegWindowsAcceptingNested / values.OrdinalSeedLegLayout (cascades/values/ordinal_seed_layout.go), live at left_outer_existential.go:370",
		why: "the only entry here whose first verdict was DELETE and was refuted by reading it. Its counterpart is " +
			"not another executor path — it is the PLANNER's wide walk, which is live production code, and this " +
			"is the executor half of a bit-for-bit cross-agreement matrix over the two. Production not calling it " +
			"is the design, not the rot: the two walks are deliberately INDEPENDENT implementations of the same " +
			"leg-window derivation, because a disagreement about which field is a leg and which is an element " +
			"shifts the offset of every field after it, and the agreement assertions are the only thing that " +
			"would catch such a drift. The divergence they exist for is recorded as MEASURED, not anticipated " +
			"(ordinal_join.go: the planner's narrow walk once declined a seed whose leg run carried a nested " +
			"boundary while the executor's narrow walk accepted it). The narrow entries are frozen and decline " +
			"every shape this one accepts, so there is nothing to repoint the assertions at — deleting this " +
			"deletes the oracle and leaves the live planner walk unwatched. It also reaches positionalMergeSpans " +
			"and nestedSubLegsAreExpressible, which are reachable only through it and go with it if it ever goes",
	},
	"pkg/fdbgo/transport/conn.go # withMonitorCadence": {
		tag: dispositionKeep,
		why: "zero production references BY CONSTRUCTION rather than by rot: Dial forwards no options, so a dial " +
			"option can only ever have test callers. What keeps it here is that the connection monitor's real " +
			"cadence (1s loop, 2s timeout — both now the non-simulated column of flow/Knobs.cpp:102-103) puts " +
			"its minimum kill several seconds out, and the one test that genuinely depends on the monitor " +
			"firing waits 2s — so at production cadence that test would go red, and the knob is what makes the " +
			"monitor testable at all rather than a decoration. Read the ten call sites with care though: three " +
			"are inert (the handshake fails and returns before the monitor goroutine starts), and several " +
			"exercise a monitor that never pings because SendFrame does not populate the pending set. The " +
			"mixed-column cadence divergence that used to be listed here is FIXED, not pending: Go ran the " +
			"loop time's SIMULATED 0.75s against the timeout's REAL 2s, a pair the C client never uses",
	},
	// DEFECT (2). Both are the shape this gate was built to find: a declared
	// safety net pointed at the dead twin instead of the live path, so the net
	// is green for a reason unrelated to what it claims to watch. Neither is
	// deletable here — repointing arms the finding, and turning the resulting
	// red into green is a planner change needing the Cascades architectural
	// review gate this change does not carry. They are recorded with the exact
	// repointing that arms them so the next person starts where this one stopped.
	"pkg/recordlayer/query/plan/cascades/planning_cost_model.go # scanProvableMaxCard": {
		tag: dispositionDefect,
		why: "TestRFC195_Criterion2AgreesWithTheProvenBound calls itself the FORK-VISIBILITY GATE and promises that " +
			"for every data-access shape the two cardinality derivations reach the same verdict. Its scan arm " +
			"calls THIS function, which criterion 2 stopped calling: the live logical walk's scan arm takes " +
			"scanPlanProvableMaxCard(scan, ctx). So the gate cannot see the context-enrichment axis, the only " +
			"axis on which the two can differ, and its shape table stamps a primary key on every scan, where " +
			"both derivations read the same field and agree trivially. Repoint the arm at " +
			"scanPlanProvableMaxCard(p, ctx) and add an UNSTAMPED scan and it goes red: criterion 2 proves max=1 " +
			"under a PK-resolving context while the proven bound returns unbounded, which is the condition the " +
			"gate fatals on. Latent today because PrimaryScanRule stamps under the same conditions the context " +
			"fallback needs. Note TODO.md CQ-30 is stale on this function: it lists it among four to delete via " +
			"plan-stamping work, but it is ALREADY dead and its deletion is gated only on the red above",
	},
	// RFC-224 resolved the former one-final-invariant finding: the replacement
	// VerifyExtractionIsUnambiguous walks the selector/extraction path, reports
	// reach and dead ends, and checks physical-property retention coherence.
	// TestExtractionIsUnambiguous is the end-to-end gate; focused mutation pins
	// live beside the verifier in cascades/final_member_invariant_test.go.
}

// unreferencedFuncScanRoots are the trees the gate reads. Everything tracked
// under them is in scope; nothing is excluded by path. Generated files and
// `go:build ignore` programs are dropped by PROPERTY inside the detector, never
// by a path pattern, because a path list under-excludes the generators whose
// output lands outside gen/ and under-includes any directory added later.
var unreferencedFuncScanRoots = []string{"pkg/", "cmd/", "conformance/"}

// scanUnexportedFuncs runs the detector over every tracked Go file under the
// scan roots, grouped by directory (a Go package's compilation unit).
func scanUnexportedFuncs(t *testing.T) (candidates, packages int, violations []funcSite, testRefs map[string]int) {
	t.Helper()
	root := sourceTreeRoot(t)

	byDir := map[string]map[string]string{}
	for _, rel := range trackedGoFiles(t, root) {
		inScope := false
		for _, prefix := range unreferencedFuncScanRoots {
			if strings.HasPrefix(rel, prefix) {
				inScope = true
				break
			}
		}
		if !inScope {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if os.IsNotExist(err) {
			// A file the index still lists but the working tree no longer has:
			// a deletion staged partway, which happens routinely in a tree
			// several people are editing. It contributes no candidates and no
			// references either way, so skipping is exact rather than lenient.
			// The loud alarm for an index that disagrees with the tree belongs
			// to TestSourceCommentHygiene, which already raises it; a second
			// copy here would only make one mistake fail two unrelated gates.
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		dir := filepath.Dir(rel)
		if byDir[dir] == nil {
			byDir[dir] = map[string]string{}
		}
		byDir[dir][rel] = string(b)
	}

	testRefs = map[string]int{}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		out, err := scanPackageSources(byDir[dir])
		if err != nil {
			t.Fatalf("scan %s: %v (every tracked .go file must parse)", dir, err)
		}
		candidates += out.Candidates
		violations = append(violations, out.Unreferenced...)
		for k, v := range out.TestRefs {
			testRefs[k] += v
		}
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].key() < violations[j].key() })
	return candidates, len(byDir), violations, testRefs
}

func TestUnexportedFuncsAreReferenced(t *testing.T) {
	t.Parallel()
	candidates, packages, violations, testRefs := scanUnexportedFuncs(t)
	// Logged, not merely guarded: a reader of a green run should be able to see
	// WHAT ran, rather than infer from the absence of a failure that anything did.
	t.Logf("scanned %d unexported no-receiver funcs across %d packages; %d unreferenced from production, %d on the ledger",
		candidates, packages, len(violations), len(unreferencedFuncLedger))

	// A green from an empty set is the dominant false positive: if the file
	// walk broke, this loop would examine nothing and pass silently.
	if problems := checkScanVacuity(candidates, packages); len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}

	unlisted, stale := reconcileLedger(violations, testRefs, unreferencedFuncLedger)
	if len(unlisted) > 0 {
		t.Errorf("%d unexported func(s) with no production reference and no ledger entry:\n%s\n\n"+
			"Nothing in the package calls these, so no test that exercises them is testing code that runs. "+
			"Twice in this tree a function in exactly this state was protecting a live defect — a range helper "+
			"whose single-range return type could not express the second range a DOUBLE bound needs (27 green "+
			"test call sites, rows silently dropped), and a compiled key-expression twin whose test asserted a "+
			"nested record-type key returns nil, pinning a silent data-loss bug as correct.\n\n"+
			"So the question is not whether it is dead. It is what the LIVE counterpart does with the same input. "+
			"Delete it, wire it back in, or — only after that comparison — add an entry to unreferencedFuncLedger "+
			"with an individual justification naming the counterpart.",
			len(unlisted), strings.Join(unlisted, "\n"))
	}

	if len(stale) > 0 {
		t.Errorf("%d ledger entry/entries no longer name an unreferenced func:\n  %s\n\n"+
			"Either the function gained a production caller, or it was renamed, moved, or deleted. "+
			"Delete the line — this is a ratchet, and an entry that stops matching is how an allowlist "+
			"rots into a permanent exemption nobody re-reads.",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// reconcileLedger is the ratchet, both directions: `unlisted` are violations
// with no ledger entry, `stale` are ledger entries that no longer name a
// violation. Split out from the process globals so both arms take explicit
// state and can be driven from a unit pin — the stale arm in particular is one
// nothing exercises until the day someone fixes a listed function, which is
// exactly when a broken arm would be read as "the fix was clean".
func reconcileLedger(
	violations []funcSite,
	testRefs map[string]int,
	ledger map[string]unreferencedFuncDisposition,
) (unlisted, stale []string) {
	seen := map[string]bool{}
	for _, v := range violations {
		key := v.key()
		seen[key] = true
		if _, known := ledger[key]; known {
			continue
		}
		unlisted = append(unlisted, fmt.Sprintf("  %s:%d\t%s\t(prod=0, test=%d)", v.Path, v.Line, v.Name, testRefs[key]))
	}
	for key := range ledger {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	return unlisted, stale
}

// ledgerPlaceholders are the spellings of a justification that is not one. A
// ledger whose entries say "pending" reports N known exceptions while proving
// nothing about any of them, which is strictly worse than N failures: it reads
// as reviewed.
//
// They are PHRASES matched on word boundaries, not bare substrings, and that is
// load-bearing rather than fussy. A bare "pending" rejects a justification that
// says the monitor pings only when the pending set is non-empty; a bare "todo"
// rejects one that cites TODO.md — both are entries doing exactly what this
// ledger asks for, refused by the instrument meant to encourage them. A check
// whose false positives land on the best entries trains its own removal.
var ledgerPlaceholders = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(justification|reason|rationale|analysis|investigation|verdict)\s+pending\b`),
	regexp.MustCompile(`(?i)\bpending\s+(justification|investigation|analysis|review of this)\b`),
	regexp.MustCompile(`(?i)\btbd\b`),
	regexp.MustCompile(`(?i)\bto be determined\b`),
	regexp.MustCompile(`(?i)\bfixme\b`),
	regexp.MustCompile(`(?i)\btodo\b(?:[^.]|$)`), // "TODO.md" is a citation, a bare TODO is a deferral
	regexp.MustCompile(`(?i)\b(needs|requires|wants)\s+(further\s+)?(investigation|analysis|a look)\b`),
	regexp.MustCompile(`(?i)\b(not|never)\s+(yet\s+)?investigated\b`),
	regexp.MustCompile(`(?i)\b(should|must|need to|needs to|someone)\s+(\w+\s+)?look\s+into\b`),
	regexp.MustCompile(`(?i)\b(reason|why|purpose)\s+(is\s+)?(unclear|unknown)\b`),
	regexp.MustCompile(`(?i)\bno\s+(reason|justification)\s+(given|recorded)\b`),
	regexp.MustCompile(`(?i)\bsee\s+above\b`),
	regexp.MustCompile(`(?i)\bditto\b`),
	regexp.MustCompile(`(?i)\bn/a\b`),
}

// ledgerJustificationMin is a floor on the reason's length. It is not a proxy
// for quality — nothing is — but it does make the one failure mode this ledger
// must not have (a word where an argument belongs) cost more than it saves.
const ledgerJustificationMin = 120

// checkLedgerEntry returns the problems with one entry. Split out from the
// process globals so every arm is drivable from a unit pin rather than only by
// whatever the ledger happens to contain today.
func checkLedgerEntry(key string, d unreferencedFuncDisposition) []string {
	var problems []string
	switch d.tag {
	case dispositionKeep, dispositionInFlight, dispositionDefect:
		if d.counterpart != "" {
			problems = append(problems, fmt.Sprintf("tag %q must not carry a counterpart (%q) — only a twin has one", d.tag, d.counterpart))
		}
	case dispositionTwin:
		if strings.TrimSpace(d.counterpart) == "" {
			problems = append(problems, "tag \"twin\" requires `counterpart`: name the LIVE function doing the same job. "+
				"An author who cannot name it has not compared the two, and the comparison is the entire point — "+
				"delete the function instead of listing it")
		}
	case "":
		problems = append(problems, "no disposition tag")
	default:
		problems = append(problems, fmt.Sprintf("unknown disposition tag %q (want one of %q, %q, %q, %q)",
			d.tag, dispositionKeep, dispositionTwin, dispositionInFlight, dispositionDefect))
	}

	why := strings.TrimSpace(d.why)
	if len(why) < ledgerJustificationMin {
		problems = append(problems, fmt.Sprintf("justification is %d chars, need at least %d — say what the live "+
			"path does with the same input and why this one is still here", len(why), ledgerJustificationMin))
	}
	for _, p := range ledgerPlaceholders {
		if m := p.FindString(why); m != "" {
			problems = append(problems, fmt.Sprintf("justification contains the placeholder %q — that is a "+
				"deferral, not a reason. Say what the live path does with the same input", strings.TrimSpace(m)))
		}
	}
	if key != "" && !strings.Contains(key, " # ") {
		problems = append(problems, fmt.Sprintf("key %q is not \"<path> # <name>\"", key))
	}
	return problems
}

// TestUnreferencedFuncLedgerEntriesAreJustified enforces the ledger's SHAPE.
// It cannot judge whether a justification is TRUE — no test can — but it does
// make the specific failure this ledger must not have, an entry that defers its
// own reasoning, impossible to write.
func TestUnreferencedFuncLedgerEntriesAreJustified(t *testing.T) {
	t.Parallel()

	// Guard the population in the direction that matters HERE. The corpus gate
	// above guards against the SCAN collapsing; this one guards against the
	// LEDGER emptying, which would make every arm below vacuous while reporting
	// a clean bill of health for a list nobody is keeping.
	for key, d := range unreferencedFuncLedger {
		if problems := checkLedgerEntry(key, d); len(problems) > 0 {
			t.Errorf("ledger entry %q:\n  - %s", key, strings.Join(problems, "\n  - "))
		}
	}
	for _, problem := range checkLedgerShape(unreferencedFuncLedger) {
		t.Error(problem)
	}
}

// checkScanVacuity guards the two populations whose collapse would make a clean
// gate result a statement about the INSTRUMENT rather than about the tree. Both
// axes are separate because the first can be satisfied by a handful of enormous
// packages while the walk misses every other directory.
//
// A pure function over explicit counts, not an inline check on the process
// globals, so the FAILING side of each floor is driven by a unit pin. The
// corpus only ever exercises these in their passing state, which is precisely
// how a reversed comparison or a dead branch survives until the day the scan
// actually collapses — the one day it is read as "the fix was clean".
func checkScanVacuity(candidates, packages int) []string {
	var problems []string
	if candidates < unexportedFuncPopulationMin {
		problems = append(problems, fmt.Sprintf(
			"scanned only %d unexported no-receiver funcs across %d packages, expected at least %d — "+
				"the walk, the parse, or the candidacy filter broke, so a clean result here would be a "+
				"statement about the instrument and not about the tree",
			candidates, packages, unexportedFuncPopulationMin))
	}
	if packages < unexportedFuncPackageMin {
		problems = append(problems, fmt.Sprintf(
			"scanned %d packages, expected at least %d — the population floor can be met by a few large "+
				"packages while the walk misses every other directory, so both axes are guarded",
			packages, unexportedFuncPackageMin))
	}
	return problems
}

// checkLedgerShape guards the ledger itself: that it has not emptied (which
// would make every per-entry arm vacuous) and that the defect queue has not
// grown past its cap.
//
// Split out for the same reason as checkScanVacuity — and with more urgency,
// because the empty-ledger arm is UNREACHABLE from the corpus by construction:
// it can only fire on a tree whose ledger is empty, which is never the tree the
// suite runs against while any entry exists.
func checkLedgerShape(ledger map[string]unreferencedFuncDisposition) []string {
	var problems []string
	if len(ledger) == 0 {
		return []string{"the ledger is empty — if every exception was genuinely retired that is good news, " +
			"but this test then checks nothing, so reconcile it with the new expected value rather than " +
			"leaving the floor pointing at the old one: the alarm has flipped from 'an entry is unjustified' " +
			"to 'an entry came back'"}
	}
	var defects []string
	for key, d := range ledger {
		if d.tag == dispositionDefect {
			defects = append(defects, key)
		}
	}
	sort.Strings(defects)
	if len(defects) > unreferencedFuncDefectMax {
		problems = append(problems, fmt.Sprintf(
			"%d entries tagged %q, the cap is %d:\n  %s\n\n"+
				"Each one is a known bug whose fix this ledger is deferring. A list that can absorb an "+
				"unbounded number of those has stopped being a ratchet and become a filing cabinet — fix "+
				"one before adding another, or raise the cap deliberately and say in the diff why the "+
				"queue is allowed to grow.",
			len(defects), dispositionDefect, unreferencedFuncDefectMax, strings.Join(defects, "\n  ")))
	}
	return problems
}
