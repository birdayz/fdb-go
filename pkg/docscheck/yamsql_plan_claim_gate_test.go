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

// A yamsql scenario whose COMMENTS claim a plan shape must ASSERT that plan
// shape. This gate exists because the corpus accumulated files that documented
// an optimization in prose and checked only rows — and rows cannot see a plan.
// A narrowed range scan and a full scan with a residual filter return the same
// rows; a sort-free index scan and an in-memory sort return the same rows. So a
// file could describe sort elimination, prefix pushdown, or a correlated
// FlatMap and pass green against a tree that had none of it.
//
// That is not hypothetical. `like_prefix_pushdown.yaml` described a LIKE prefix
// pushdown, in the present tense, with a six-line eligibility scope — for a
// pushdown that has never existed in this tree. Every one of its LIKE queries
// planned as a full scan. It read as covered for as long as it existed, and it
// also contradicted itself (the header said interior wildcards were ineligible
// while a section lower down said they narrowed), because nothing could tell
// either statement from the other. Three more files claimed operators they did
// not plan: a "FlatMap LEFT OUTER mode" that is a NestedLoopJoin, a NOT EXISTS
// file claiming an "NLJ fallback" path it never reaches, and a recursive-CTE
// file naming RecursiveLevelUnionPlan where the plan is a RecursiveDfsJoin.
//
// THE VOCABULARY IS DERIVED, NOT LISTED. The operator names come from
// `plan_shape.golden`, the corpus-wide rendering of every planned query — so a
// new operator enters the gate's vocabulary the moment it first appears in a
// plan, with nobody maintaining a word list. The filter that keeps the gate
// quiet is also a rule rather than a list: only InternalCaps identifiers count
// (`FlatMap`, `InMemorySort`, `PredicatesFilter`). That mechanically drops the
// operator names that are ordinary English — `Scan`, `Project`, `Limit`,
// `Fetch`, `Distinct`, `Union`, `Intersection` — which is what makes the gate
// usable: "the intersection of the DISTINCT path with the JOIN evaluator" is
// prose, not a claim about an `Intersection` operator, and a gate that could
// not tell them apart would be allowlisted into meaninglessness within a month.
//
// WHAT GREEN HERE DOES NOT PROVE. This catches a plan claim written with an
// operator name, a `…Rule` identifier, or the word "pushdown". It does not
// catch one written entirely in paraphrase ("the sort goes away", "it only
// reads the matching keys"). No prose gate is closed under paraphrase. Read a
// pass as "no file names an operator or rule it does not assert", never as "no
// file makes an unchecked plan claim".

const (
	yamsqlCorpusDir  = "pkg/relational/conformance/yamsql/testdata"
	planShapeGolden  = "pkg/relational/conformance/explaindiff/testdata/plan_shape.golden"
	yamsqlCorpusMin  = 300 // the corpus is ~349 files; a collapse below this means the glob broke
	planVocabularyMn = 10  // the golden renders far more than this; fewer means the parse broke
)

// planClaimVocabulary returns the plan-operator identifiers the gate treats as
// claims, derived from the rendered plans in plan_shape.golden.
//
// Only InternalCaps names survive: an initial capital, at least one lowercase,
// and a later capital. That admits every compound operator name and excludes
// both the single-word operators that double as English (Scan, Project, Limit)
// and the ALLCAPS scalar functions the same lines contain (SUM, COALESCE, CAST).
func planClaimVocabulary(golden string) []string {
	token := regexp.MustCompile(`\b([A-Z][A-Za-z]*)\(`)
	internalCaps := regexp.MustCompile(`^[A-Z][a-z]+[A-Z][A-Za-z]*$`)
	seen := map[string]bool{}
	for _, line := range strings.Split(golden, "\n") {
		if !strings.HasPrefix(line, "plan:") {
			continue
		}
		for _, m := range token.FindAllStringSubmatch(line, -1) {
			if internalCaps.MatchString(m[1]) {
				seen[m[1]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	// A Cascades rule named in prose is always a claim about how the plan was
	// built. Nobody writes "…Rule" casually.
	planRulePattern = regexp.MustCompile(`\b[A-Z][A-Za-z]*Rule\b`)
	// "pushdown" and its verb forms are optimization claims in prose.
	planPushdownPattern = regexp.MustCompile(`(?i)\bpush(down|es down|ed down|ing down)\b|\bpush down\b`)
	yamlCommentPattern  = regexp.MustCompile(`(?m)^[[:space:]]*#.*$`)
)

// planClaimsIn returns the distinct plan claims made in a scenario's COMMENTS.
// Only comment lines are examined: a plan assertion's own value ("FlatMap(") is
// data the gate must not read as a claim, and neither is SQL or an expected row.
func planClaimsIn(source string, vocabulary []string) []string {
	comments := strings.Join(yamlCommentPattern.FindAllString(source, -1), "\n")
	seen := map[string]bool{}
	for _, op := range vocabulary {
		if strings.Contains(comments, op) {
			seen[op] = true
		}
	}
	for _, m := range planRulePattern.FindAllString(comments, -1) {
		seen[m] = true
	}
	if planPushdownPattern.MatchString(comments) {
		seen["pushdown"] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hasPlanAssertion reports whether a scenario checks a plan at all. One
// assertion anywhere in the file discharges the gate: the gate's job is to stop
// a file from claiming a plan shape with NO plan checking whatsoever, not to
// pair every sentence with an assertion — that judgement belongs to a reader.
func hasPlanAssertion(source string) bool {
	return strings.Contains(source, "plan_contains:") || strings.Contains(source, "plan_not_contains:")
}

func TestYamsqlPlanClaimsAreAsserted(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	goldenBytes, err := os.ReadFile(filepath.Join(root, planShapeGolden))
	if err != nil {
		t.Fatalf("read %s: %v", planShapeGolden, err)
	}
	vocabulary := planClaimVocabulary(string(goldenBytes))
	if len(vocabulary) < planVocabularyMn {
		t.Fatalf("derived only %d plan operators from %s (%v) — the golden's format changed and the gate is now blind, not clean",
			len(vocabulary), planShapeGolden, vocabulary)
	}

	paths, err := filepath.Glob(filepath.Join(root, yamsqlCorpusDir, "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// A green from an empty set is the dominant false positive: if the corpus
	// were staged wrong under Bazel this loop would examine nothing and pass.
	if len(paths) < yamsqlCorpusMin {
		t.Fatalf("scanned %d scenarios under %s, expected at least %d — the corpus is not reaching this test, so a pass would mean nothing",
			len(paths), yamsqlCorpusDir, yamsqlCorpusMin)
	}

	var offenders []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		source := string(b)
		if hasPlanAssertion(source) {
			continue
		}
		if claims := planClaimsIn(source, vocabulary); len(claims) > 0 {
			offenders = append(offenders, fmt.Sprintf("  %s claims %v", filepath.Base(p), claims))
		}
	}
	if len(offenders) > 0 {
		t.Errorf("these scenarios name a plan operator, a Cascades rule, or a pushdown in their comments but assert no plan:\n%s\n\n"+
			"Rows cannot see a plan — a full scan with a residual filter and a narrowed scan return the same rows — so such a file\n"+
			"passes against a tree that does not do what it says. Add plan_contains / plan_not_contains for the claim, or, if the\n"+
			"claim turns out to be false, correct the comment to what the plan actually is (see like_prefix_pushdown.yaml).",
			strings.Join(offenders, "\n"))
	}
}

// The corpus reading above exercises only the arms the corpus happens to reach.
// Every arm is driven here instead, so an arm that is rare today cannot ship
// untested and be mistaken for a finding the first time it fires.
func TestPlanClaimDetectorPrecisionAndRecall(t *testing.T) {
	t.Parallel()
	vocabulary := []string{"FlatMap", "InMemorySort", "IndexScan", "PredicatesFilter", "NestedLoopJoin"}

	cases := []struct {
		name   string
		source string
		want   []string
	}{
		// Recall: each detector arm fires on its own shape.
		{
			name:   "operator name in a header comment",
			source: "# joins here use FlatMap, not a full scan\ntests: []\n",
			want:   []string{"FlatMap"},
		},
		{
			name:   "operator name in a per-test comment",
			source: "tests:\n  # the InMemorySort is eliminated here\n  - query: SELECT 1\n",
			want:   []string{"InMemorySort"},
		},
		{
			name:   "cascades rule identifier",
			source: "# RemoveSortRule drops the sort when the index provides the order\n",
			want:   []string{"RemoveSortRule"},
		},
		{
			name:   "rule identifier the vocabulary has never seen",
			source: "# fired by SomeBrandNewRule\n",
			want:   []string{"SomeBrandNewRule"},
		},
		{
			name:   "pushdown as a noun",
			source: "# prefix pushdown narrows the scan\n",
			want:   []string{"pushdown"},
		},
		{
			name:   "pushdown as a verb",
			source: "# the planner pushes down the equality\n",
			want:   []string{"pushdown"},
		},
		{
			name:   "push down as two words",
			source: "# the planner should push down the PK equality\n",
			want:   []string{"pushdown"},
		},
		{
			name:   "pushed down",
			source: "# dept filter pushed down below the join\n",
			want:   []string{"pushdown"},
		},
		{
			name:   "several claims in one file are all reported",
			source: "# uses FlatMap not NestedLoopJoin, via RemoveSortRule\n",
			want:   []string{"FlatMap", "NestedLoopJoin", "RemoveSortRule"},
		},

		// Precision: the shapes that must stay quiet.
		{
			name:   "generic operator words that are also English",
			source: "# the intersection of the DISTINCT path with the join evaluator;\n# a full scan projects every column and limits the result\n",
			want:   nil,
		},
		{
			name:   "plan-time error prose is not a plan-shape claim",
			source: "# rejected at PLAN time (42804); the plan-time gate fires before execution\n",
			want:   nil,
		},
		{
			name:   "an operator name outside a comment is not a claim",
			source: "tests:\n  - query: SELECT 1\n    plan_contains: \"FlatMap(\"\n",
			want:   nil,
		},
		{
			name:   "an operator name inside a SQL string is not a claim",
			source: "tests:\n  - query: SELECT 'FlatMap' FROM t\n",
			want:   nil,
		},
		{
			name:   "ordinary prose with no claim",
			source: "# COUNT(col) skips NULLs; COUNT(*) counts every row\n",
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := planClaimsIn(tc.source, vocabulary)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("planClaimsIn() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The vocabulary derivation is the part that decides whether this gate is
// usable or noise, so it is pinned independently of what the golden happens to
// contain today.
func TestPlanClaimVocabularyIsDerivedAndFiltered(t *testing.T) {
	t.Parallel()
	golden := strings.Join([]string{
		"# explain-baseline/v2",
		"=== some_file.yaml#0",
		"sql:   SELECT NestedLoopJoin FROM t",
		"plan:  Project([ID#0], InMemorySort([ID ASC], FlatMap(outer=Scan(T), inner=Fetch(IndexScan(IDX, [=])))))",
		"plan:  Limit(3, Distinct(Union(Intersection(PredicatesFilter(Scan(T), [1 preds])))))",
		"plan:  Project([SUM(V)#0], StreamingAgg(keys=[], CAST(COALESCE(V))))",
		"shape: RecordQueryFlatMapPlan",
	}, "\n")

	got := planClaimVocabulary(golden)
	// Byte order, not case-insensitive order: "InMemorySort" < "IndexScan"
	// because 'M' < 'd'.
	want := []string{"FlatMap", "InMemorySort", "IndexScan", "PredicatesFilter", "StreamingAgg"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("planClaimVocabulary() = %v, want %v", got, want)
	}

	// Spelled out because each exclusion is load-bearing for precision, and a
	// regression in the filter would show up here rather than as a wave of
	// corpus failures nobody can act on.
	for _, excluded := range []string{
		"Scan", "Project", "Limit", "Fetch", "Distinct", "Union", "Intersection", // English words
		"SUM", "CAST", "COALESCE", // ALLCAPS scalar functions on the same lines
		"NestedLoopJoin",         // present only on a `sql:` line, never rendered
		"RecordQueryFlatMapPlan", // `shape:` lines are Go type names, not rendered operators
	} {
		for _, v := range got {
			if v == excluded {
				t.Errorf("vocabulary must not include %q — it would flag ordinary prose (or is not a rendered operator)", excluded)
			}
		}
	}
}

// hasPlanAssertion is what discharges the gate, so a change that broke it would
// silently turn the gate off for the whole corpus rather than fail loudly.
func TestPlanAssertionDetection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		source string
		want   bool
	}{
		{"plan_contains", "tests:\n  - query: SELECT 1\n    plan_contains: \"Scan(T)\"\n", true},
		{"plan_not_contains only", "tests:\n  - query: SELECT 1\n    plan_not_contains: InMemorySort\n", true},
		{"rows only", "tests:\n  - query: SELECT 1\n    rows:\n      - [1]\n", false},
		{"columns and error pins are not plan assertions", "tests:\n  - query: SELECT 1\n    columns: [A]\n    error_code: \"42804\"\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasPlanAssertion(tc.source); got != tc.want {
				t.Errorf("hasPlanAssertion() = %v, want %v", got, tc.want)
			}
		})
	}
}
