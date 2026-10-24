package javacorpus_test

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/javacorpus"
	"fdb.dev/pkg/relational/conformance/javayamsql"
)

const (
	ledgerDocBegin = "<!-- BEGIN javacorpus ledger: rendered by TestSQLConformanceStatesTheLedger -->\n"
	ledgerDocEnd   = "<!-- END javacorpus ledger -->"
)

// TestSQLConformanceStatesTheLedger keeps SQL_CONFORMANCE.md's account of the
// Java corpus run equal to pinnedLedger, the vendored VERSION and the corpus's
// plan assertions; on drift it prints the block to paste.
func TestSQLConformanceStatesTheLedger(t *testing.T) {
	t.Parallel()
	versionPath, err := javacorpus.FindAbove("third_party/apple/fdb-record-layer/VERSION")
	if err != nil {
		t.Fatal(err)
	}
	version, err := os.ReadFile(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := countPlanAssertions()
	if err != nil {
		t.Fatal(err)
	}
	want, err := renderLedgerStatement(strings.TrimSpace(string(version)), pinnedLedger, pinnedFileTotal, plans)
	if err != nil {
		t.Fatal(err)
	}

	docPath, err := javacorpus.FindAbove("SQL_CONFORMANCE.md")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(doc), ledgerDocBegin)
	got, _, closed := strings.Cut(after, ledgerDocEnd)
	if !ok || !closed {
		t.Fatalf("SQL_CONFORMANCE.md has no %q … %q block", strings.TrimSpace(ledgerDocBegin), ledgerDocEnd)
	}
	if got != want {
		t.Errorf("SQL_CONFORMANCE.md's Java corpus statement is stale; replace the block's body with:\n%s", want)
	}
}

// planAssertions counts the corpus's explain/explainContains/planHash
// directives over the files that parse.
type planAssertions struct {
	parsedFiles, filesWithPlans int
	byKind                      map[javayamsql.ConfigKind]int
}

func countPlanAssertions() (planAssertions, error) {
	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		return planAssertions{}, err
	}
	files, err := corpus.List()
	if err != nil {
		return planAssertions{}, err
	}
	kinds := []javayamsql.ConfigKind{javayamsql.ConfigExplain, javayamsql.ConfigExplainContains, javayamsql.ConfigPlanHash}
	out := planAssertions{byKind: map[javayamsql.ConfigKind]int{}}
	for _, path := range files {
		if javayamsql.PolarityOf(path) == javayamsql.NegativeParse {
			continue
		}
		f, err := corpus.ParseFile(path)
		if err != nil {
			return planAssertions{}, fmt.Errorf("%s: %w", path, err)
		}
		census := javayamsql.NewCensus()
		census.Add(f)
		out.parsedFiles++
		n := 0
		for _, k := range kinds {
			out.byKind[k] += census.Configs[k]
			n += census.Configs[k]
		}
		if n > 0 {
			out.filesWithPlans++
		}
	}
	return out, nil
}

var ledgerLine = regexp.MustCompile(`^pass=(\d+) fail=(\d+) skip=(\d+) queries=(\d+) file_skips\{([^}]*)\} inner_skips\{([^}]*)\}$`)

// renderLedgerStatement is the SQL_CONFORMANCE.md block for one pinned ledger.
func renderLedgerStatement(version, ledger string, fileTotal int, plans planAssertions) (string, error) {
	m := ledgerLine.FindStringSubmatch(ledger)
	if m == nil {
		return "", fmt.Errorf("pinned ledger %q does not have the census line's shape", ledger)
	}
	fileSkips, err := parseClassCounts(m[5])
	if err != nil {
		return "", err
	}
	innerSkips, err := parseClassCounts(m[6])
	if err != nil {
		return "", err
	}
	var other []classCount
	for _, c := range innerSkips {
		if javacorpus.SkipClass(c.class).SuppressesAssertion() && c.class != string(javacorpus.SkipPlanAssertion) {
			other = append(other, c)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Java's own yamsql corpus (fdb-record-layer %s, %d files) runs against Go in\n", version, fileTotal)
	b.WriteString("`pkg/relational/conformance/javacorpus`, pinned by `pinned_ledger_test.go`:\n\n")
	fmt.Fprintf(&b, "- **%s files pass, %s fail, %s are skipped whole**, by class: %s.\n", m[1], m[2], m[3], joinClassCounts(fileSkips))
	fmt.Fprintf(&b, "- A pass means every assertion that ran held; plan assertions never run (RFC-201 ruling 2).\n"+
		"  The %d files that parse carry %d of them (`explain` %d, `explainContains` %d, `planHash` %d)\n"+
		"  in %d files; %d executions of them were skipped.\n",
		plans.parsedFiles,
		plans.byKind[javayamsql.ConfigExplain]+plans.byKind[javayamsql.ConfigExplainContains]+plans.byKind[javayamsql.ConfigPlanHash],
		plans.byKind[javayamsql.ConfigExplain], plans.byKind[javayamsql.ConfigExplainContains], plans.byKind[javayamsql.ConfigPlanHash],
		plans.filesWithPlans, countOf(innerSkips, string(javacorpus.SkipPlanAssertion)))
	fmt.Fprintf(&b, "- Other assertions skipped, per execution: %s.\n", joinClassCounts(other))
	fmt.Fprintf(&b, "- %s query executions were asserted. By default a test block runs each query five times, plus a plan-cache check.\n", m[4])
	return b.String(), nil
}

type classCount struct {
	class string
	n     int
}

// parseClassCounts reads a census `class=n,…` list, largest count first.
func parseClassCounts(s string) ([]classCount, error) {
	var out []classCount
	for _, part := range strings.Split(s, ",") {
		class, n, ok := strings.Cut(part, "=")
		count, err := strconv.Atoi(n)
		if !ok || err != nil {
			return nil, fmt.Errorf("class count %q is not class=n", part)
		}
		out = append(out, classCount{class, count})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].n > out[j].n })
	return out, nil
}

func joinClassCounts(cs []classCount) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = fmt.Sprintf("%s %d", c.class, c.n)
	}
	return strings.Join(parts, ", ")
}

func countOf(cs []classCount, class string) int {
	for _, c := range cs {
		if c.class == class {
			return c.n
		}
	}
	return 0
}
