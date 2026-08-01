package factory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// TestSecondPlanSeesOrderOnAnOrderedQuery is the second-plan oracle's ordering
// detector.
//
// It is the case the oracle exists for. Disabling MatchLeafRule takes away the
// ORDERINGS an index provides, so the second plan has to re-establish the sort
// by some other route — which makes a sorted query the single most likely place
// for the two plans to disagree, and an ordering bug the single most likely
// thing this perturbation surfaces. A comparator that sorts both sides first
// cannot see any of it: the two row sets below are equal as multisets and
// differ in every position, and a multiset compare returns "" for them while
// reporting the same "second-plan kept" count as a real check.
//
// The unordered half is not decoration. A sequence comparison applied
// unconditionally would report two correct plans as disagreeing whenever the
// query has no ORDER BY, because SQL grants no order there — so the switch has
// to be exercised in BOTH directions or a fix in one direction is indistinguish-
// able from a fix in neither.
func TestSecondPlanSeesOrderOnAnOrderedQuery(t *testing.T) {
	t.Parallel()
	row := func(id int64, b string) []any { return []any{id, b} }

	// Same rows, reversed. Multiset-equal, sequence-different.
	asc := [][]any{row(1, "a"), row(2, "b"), row(3, "c")}
	desc := [][]any{row(3, "c"), row(2, "b"), row(1, "a")}

	if d := factory.RowsDiffForTest(true, asc, desc); d == "" {
		t.Fatal("the second-plan oracle accepted two plans that returned the SAME ROWS IN A DIFFERENT ORDER " +
			"for a query carrying ORDER BY. That is the divergence class disabling MatchLeafRule is most " +
			"likely to produce, and the blessed scenario freezes the exact sequence — so a wrong order would " +
			"be committed as a frozen expectation with an oracle's name on it")
	}
	if d := factory.RowsDiffForTest(false, asc, desc); d != "" {
		t.Fatalf("an UNORDERED query's two plans were reported as disagreeing (%s); SQL grants no order "+
			"without ORDER BY, so this reports a correct engine as broken", d)
	}

	// A swap of two ADJACENT rows in the middle is the smallest real ordering
	// bug (an unstable or wrongly-keyed comparator), and the one a "compare the
	// first and last row" shortcut would miss.
	adjacent := [][]any{row(1, "a"), row(3, "c"), row(2, "b")}
	if d := factory.RowsDiffForTest(true, asc, adjacent); d == "" {
		t.Fatal("a single adjacent-pair transposition was accepted on an ordered query")
	}

	// Identical sequences must pass under both modes, or the oracle rejects
	// every correct engine and the corpus stops growing for the wrong reason.
	same := [][]any{row(1, "a"), row(2, "b"), row(3, "c")}
	if d := factory.RowsDiffForTest(true, asc, same); d != "" {
		t.Fatalf("two identical row sequences were reported as different: %s", d)
	}
	if d := factory.RowsDiffForTest(false, asc, desc[:0]); d == "" {
		t.Fatal("a row-count difference was accepted in unordered mode")
	}
}

// TestCrossEngineDistinguishesLargeInt64s pins that the cross-engine
// comparator can tell two adjacent large integers apart.
//
// 2^62 is not a hypothetical: it is one of rowdiff's boundary constants and it
// is in the committed corpus. Rendering numbers through %.9g gave it nine
// significant digits, so 2^62 and 2^62+1 both became "4.61168602e+18" and the
// oracle would have called Go and Java equal while they returned different
// values. A differential oracle that cannot see a difference is not a weak
// oracle, it is a green light.
func TestCrossEngineDistinguishesLargeInt64s(t *testing.T) {
	t.Parallel()
	const twoTo62 = int64(1) << 62 // 4611686018427387904

	for _, tc := range []struct {
		name     string
		go_, jav [][]any
	}{
		{"2^62 vs 2^62+1", [][]any{{twoTo62}}, [][]any{{twoTo62 + 1}}},
		{"2^62 vs 2^62-1", [][]any{{twoTo62}}, [][]any{{twoTo62 - 1}}},
		{"MaxInt64 vs MaxInt64-1", [][]any{{int64(1<<63 - 1)}}, [][]any{{int64(1<<63 - 2)}}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if d := factory.CrossEngineDiffForTest(tc.go_, tc.jav); d == "" {
				t.Fatalf("the cross-engine oracle called %v and %v EQUAL; two different values compared "+
					"equal means every disagreement in this range is invisible", tc.go_, tc.jav)
			}
		})
	}

	// The transport equality the comparator exists to preserve: Java's JSON
	// leg has one number type, so a BIGINT arrives as a float64 while Go's
	// driver returns an int64. Those must still compare EQUAL, or every integer
	// column reports the transport as an engine divergence.
	if d := factory.CrossEngineDiffForTest([][]any{{int64(42)}}, [][]any{{float64(42)}}); d != "" {
		t.Fatalf("int64(42) and float64(42) were reported as different (%s); that is JSON transport, not a "+
			"divergence, and reporting it would drown the leg in false findings", d)
	}
	if d := factory.CrossEngineDiffForTest([][]any{{twoTo62}}, [][]any{{float64(twoTo62)}}); d != "" {
		t.Fatalf("2^62 as int64 and as float64 were reported as different (%s); 2^62 is exactly "+
			"representable, so the transport is lossless here and the comparator must say so", d)
	}
	// Non-integral values must still be distinguishable to full precision.
	if d := factory.CrossEngineDiffForTest([][]any{{0.1}}, [][]any{{0.1 + 1e-17}}); d != "" {
		// 0.1+1e-17 rounds to 0.1 in float64; equality here is the CORRECT answer
		// and this line documents that the check below is not vacuous.
		t.Fatalf("two float64s that are bit-identical were reported as different: %s", d)
	}
	if d := factory.CrossEngineDiffForTest([][]any{{1.0000000000000002}}, [][]any{{1.0}}); d == "" {
		t.Fatal("two adjacent float64s were reported as equal; the rendering is losing precision")
	}
}

// TestTLPEligibilityRejectsOffset pins that OFFSET is refused on its own.
//
// The partition is over the FULL result. `p`, `NOT p` and `p IS NULL` each skip
// their own first N rows, so the three branches together lose up to 3N rows the
// unfiltered query keeps — the property does not merely weaken, it is false.
//
// The guard is currently UNREACHABLE from the generator, and that is exactly
// why it is pinned here rather than left implied: rowdiff only ever draws an
// offset inside the LIMIT arm (gen.go), and its renderer only emits OFFSET
// inside a LIMIT clause, so today an offset-only spec would render no OFFSET at
// all and the partition would survive by accident. Eligibility must be a
// statement about the SPEC, not about what a renderer two files away happens to
// drop. If this goes red, someone has made eligibility depend on the renderer
// again.
func TestTLPEligibilityRejectsOffset(t *testing.T) {
	t.Parallel()
	where := &rowdiff.BoolNode{Leaf: &rowdiff.Pred{Col: "A"}}

	if !factory.TLPEligibleForTest(rowdiff.Query{Where: where}) {
		t.Fatal("a plain WHERE query was rejected; the eligibility predicate now excludes everything")
	}
	if factory.TLPEligibleForTest(rowdiff.Query{Where: where, Offset: 3}) {
		t.Fatal("a query with OFFSET and no LIMIT was accepted as TLP-eligible: each branch would skip its " +
			"own prefix and the three branches could not reassemble the unfiltered result")
	}
	if factory.TLPEligibleForTest(rowdiff.Query{Where: where, Limit: 5, Offset: 3}) {
		t.Fatal("a LIMIT+OFFSET query was accepted as TLP-eligible")
	}
}

// TestNoCommittedScenarioCarriesAnOffset is the corpus-side half of the guard
// above: the 900 files already on disk must not contain one.
//
// The eligibility fix changes which candidates a FUTURE batch admits; it says
// nothing about what past batches wrote. A committed scenario whose four
// renderings carry an OFFSET would be a frozen expectation blessed by a
// property that does not hold for it, and no amount of forward-looking guard
// removes it.
func TestNoCommittedScenarioCarriesAnOffset(t *testing.T) {
	t.Parallel()
	matches, err := filepath.Glob(filepath.Join(corpusDir, "*.yamsql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("no committed scenarios under %s; this gate is vacuous", corpusDir)
	}
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToUpper(string(data)), " OFFSET ") {
			t.Errorf("%s carries an OFFSET; the TLP partition it was blessed by does not hold for it", m)
		}
	}
}

// TestCommittedOrderednessMatchesTheCandidate is the second half of the
// ordering fix: it pins the value the oracle was actually handed.
//
// The comparator detector above proves that a sequence comparison CAN see an
// ordering divergence. It says nothing about whether the oracle is told to use
// one — and `ordered` is a single bool threaded from Candidate.Ordered() into
// both the second-plan comparison and the scenario's Unordered flag. Only the
// flag survives into a file, so the flag is the observable that pins the value:
// every committed scenario whose regenerated candidate is ordered must freeze
// its rows as a SEQUENCE, and every unordered one must not.
//
// A mismatch in either direction is a live defect, not a style question. An
// ordered candidate frozen with unordered:true asserts less than the oracle
// checked; an unordered candidate frozen with unordered:false asserts an order
// SQL never promised, and the scenario flakes the first time a plan changes.
func TestCommittedOrderednessMatchesTheCandidate(t *testing.T) {
	t.Parallel()
	files := loadCorpus(t)
	bySeed := map[uint64][]factory.Candidate{}
	checked := 0
	for _, f := range files {
		h := f.Header
		cands, ok := bySeed[h.Seed]
		if !ok {
			cands = factory.Candidates(h.Seed)
			bySeed[h.Seed] = cands
		}
		var cand *factory.Candidate
		for i := range cands {
			if cands[i].QueryIndex == h.QueryIndex && cands[i].ProjIndex == h.Projection {
				cand = &cands[i]
				break
			}
		}
		if cand == nil {
			continue // TestFactoryDeterminism owns this failure
		}
		for i, test := range f.Doc.Tests {
			if test.Unordered == cand.Ordered() {
				t.Errorf("%s tests[%d]: candidate.Ordered()=%v but the frozen expectation says unordered=%v. "+
					"The same value decides how the second-plan oracle compared these rows, so the file "+
					"either asserts more than was checked or less.", f.Path, i, cand.Ordered(), test.Unordered)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("zero committed files were checked — this gate is vacuous")
	}
	// Both classes must be present, or the gate only ever exercises one arm.
	var ordered, unordered int
	for _, f := range files {
		if len(f.Doc.Tests) > 0 && f.Doc.Tests[0].Unordered {
			unordered++
		} else {
			ordered++
		}
	}
	if ordered == 0 || unordered == 0 {
		t.Fatalf("the corpus holds %d ordered and %d unordered scenarios; with either at zero this gate "+
			"cannot tell a correct threading from a hardcoded constant", ordered, unordered)
	}
	t.Logf("checked %d committed files: %d ordered, %d unordered", checked, ordered, unordered)
}
