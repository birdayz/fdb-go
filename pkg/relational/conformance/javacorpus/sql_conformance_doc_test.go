package javacorpus_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"fdb.dev/pkg/relational/conformance/javacorpus"
	"fdb.dev/pkg/relational/conformance/javayamsql"
)

const (
	ledgerDocBegin = "<!-- BEGIN javacorpus ledger: rendered by TestSQLConformanceStatesTheLedger -->\n"
	ledgerDocEnd   = "<!-- END javacorpus ledger -->"
)

// TestSQLConformanceStatesTheLedger prevents drift between the documented ledger,
// pinned results, vendored version and corpus plan assertions.
func TestSQLConformanceStatesTheLedger(t *testing.T) {
	t.Parallel()
	version := strings.TrimSpace(readAbove(t, "third_party/apple/fdb-record-layer/VERSION"))
	plans, err := countPlanAssertions(javayamsql.OpenCorpus)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := parseLedgerLine(pinnedLedger)
	if err != nil {
		t.Fatal(err)
	}
	want := renderLedgerStatement(version, ledger, pinnedFileTotal, plans)

	doc := readAbove(t, "SQL_CONFORMANCE.md")
	_, after, ok := strings.Cut(doc, ledgerDocBegin)
	got, _, closed := strings.Cut(after, ledgerDocEnd)
	if !ok || !closed {
		t.Fatalf("SQL_CONFORMANCE.md has no %q … %q block", strings.TrimSpace(ledgerDocBegin), ledgerDocEnd)
	}
	if got != want {
		t.Errorf("SQL_CONFORMANCE.md's Java corpus statement is stale; replace the block's body with:\n%s", want)
	}

	// The re-verified documents name the Java version they were measured on.
	for doc, heading := range map[string]string{
		"SQL_CONFORMANCE.md": "Java fdb-relational **" + version + "**",
		"DIVERGENCES.md":     "## Go-Only Extensions (re-verified on Java " + version + ")",
	} {
		if !strings.Contains(readAbove(t, doc), heading) {
			t.Errorf("%s does not say %q: re-verify it on %s", doc, heading, version)
		}
	}
}

func readAbove(t *testing.T, rel string) string {
	t.Helper()
	path, err := javacorpus.FindAbove(rel)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// planAssertions counts the corpus's explain/explainContains/planHash
// directives over the files that parse.
type planAssertions struct {
	parsedFiles, filesWithPlans int
	byKind                      map[javayamsql.ConfigKind]int
}

func countPlanAssertions(open func() (*javayamsql.Corpus, error)) (planAssertions, error) {
	corpus, err := open()
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
	if out.parsedFiles == 0 {
		return planAssertions{}, fmt.Errorf("plan-assertion census has no parsed files")
	}
	return out, nil
}

type planAssertionErrorFS struct {
	err error
}

func (f planAssertionErrorFS) Open(string) (fs.File, error) {
	return nil, f.err
}

func TestCountPlanAssertions(t *testing.T) {
	t.Parallel()
	plain := &fstest.MapFile{Data: []byte("test_block:\n  tests:\n    -\n      - query: select 1\n")}
	invalid := &fstest.MapFile{Data: []byte("test_block: [")}
	const negative = "supported-version/illegal-version-at-file.yamsql"
	openErr := errors.New("fixture open failure")
	listErr := errors.New("fixture list failure")
	for _, tc := range []struct {
		name                   string
		files                  fs.FS
		openErr                error
		wantParsed, wantPlans  int
		wantExplain            int
		wantContains, wantHash int
		wantErr                string
		wrappedErr             error
		wantParseErr           bool
	}{
		{
			name: "all directive kinds and negative filtering",
			files: fstest.MapFS{
				"explain.yamsql": {Data: []byte(`test_block:
  tests:
    -
      - query: select 1
      - explain: scan
      - explainContains: scan
    -
      - query: select 2
      - explain: scan again
`)},
				"hash.yamsql": {Data: []byte(`test_block:
  tests:
    -
      - query: select 1
      - planHash: 10
    -
      - query: select 2
      - planHash: 20
    -
      - query: select 3
      - planHash: 30
`)},
				"plain.yamsql": plain,
				negative:       invalid,
				"ignored.txt":  invalid,
			},
			wantParsed: 3, wantPlans: 2, wantExplain: 2, wantContains: 1, wantHash: 3,
		},
		{
			name:       "parsed file without plan directives",
			files:      fstest.MapFS{"plain.yamsql": plain},
			wantParsed: 1,
		},
		{
			name: "empty corpus", files: fstest.MapFS{},
			wantErr: "no parsed files",
		},
		{
			name: "all negative parse files", files: fstest.MapFS{negative: invalid},
			wantErr: "no parsed files",
		},
		{
			name: "open error", openErr: openErr,
			wantErr: openErr.Error(), wrappedErr: openErr,
		},
		{
			name: "list error", files: planAssertionErrorFS{listErr},
			wantErr: listErr.Error(), wrappedErr: listErr,
		},
		{
			name:    "parse error after a parsed file",
			files:   fstest.MapFS{"a.yamsql": plain, "bad.yamsql": invalid},
			wantErr: "bad.yamsql:", wantParseErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := countPlanAssertions(func() (*javayamsql.Corpus, error) {
				return &javayamsql.Corpus{FS: tc.files}, tc.openErr
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("countPlanAssertions() error = %v, want %q", err, tc.wantErr)
				}
				if tc.wrappedErr != nil && !errors.Is(err, tc.wrappedErr) {
					t.Errorf("countPlanAssertions() error = %v does not preserve %v", err, tc.wrappedErr)
				}
			}
			if tc.wantParseErr {
				var parseErr *javayamsql.ParseError
				if !errors.As(err, &parseErr) || parseErr.Path != "bad.yamsql" {
					t.Errorf("countPlanAssertions() error = %v, want wrapped ParseError for bad.yamsql", err)
				}
			}
			if got.parsedFiles != tc.wantParsed || got.filesWithPlans != tc.wantPlans {
				t.Errorf("file counts = (%d parsed, %d with plans), want (%d, %d)",
					got.parsedFiles, got.filesWithPlans, tc.wantParsed, tc.wantPlans)
			}
			for kind, want := range map[javayamsql.ConfigKind]int{
				javayamsql.ConfigExplain:         tc.wantExplain,
				javayamsql.ConfigExplainContains: tc.wantContains,
				javayamsql.ConfigPlanHash:        tc.wantHash,
			} {
				if got.byKind[kind] != want {
					t.Errorf("%s count = %d, want %d", kind, got.byKind[kind], want)
				}
			}
		})
	}
}

// ledgerCensus is a parsed Census.Line; its class lists are largest first.
type ledgerCensus struct {
	pass, fail, skip, queries int
	fileSkips, innerSkips     []classCount
}

type classCount struct {
	class string
	n     int
}

var ledgerLine = regexp.MustCompile(`^pass=(\d+) fail=(\d+) skip=(\d+) queries=(\d+) file_skips\{([^}]*)\} inner_skips\{([^}]*)\}$`)

// parseLedgerLine reads a pinned Census.Line.
func parseLedgerLine(line string) (ledgerCensus, error) {
	m := ledgerLine.FindStringSubmatch(line)
	if m == nil {
		return ledgerCensus{}, fmt.Errorf("pinned ledger %q does not have the census line's shape", line)
	}
	var c ledgerCensus
	var err error
	for i, dst := range []*int{&c.pass, &c.fail, &c.skip, &c.queries} {
		if *dst, err = strconv.Atoi(m[i+1]); err != nil {
			return ledgerCensus{}, fmt.Errorf("pinned ledger count %q: %w", m[i+1], err)
		}
	}
	if c.fileSkips, err = parseClassCounts(m[5]); err != nil {
		return ledgerCensus{}, err
	}
	if c.innerSkips, err = parseClassCounts(m[6]); err != nil {
		return ledgerCensus{}, err
	}
	return c, nil
}

// parseClassCounts reads a census `class=n,…` list, largest count first.
func parseClassCounts(s string) ([]classCount, error) {
	if s == "" {
		return nil, nil
	}
	var out []classCount
	for _, part := range strings.Split(s, ",") {
		class, n, ok := strings.Cut(part, "=")
		count, err := strconv.Atoi(n)
		if !ok || class == "" || err != nil {
			return nil, fmt.Errorf("class count %q is not class=n", part)
		}
		out = append(out, classCount{class, count})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].n > out[j].n })
	return out, nil
}

// renderLedgerStatement is the SQL_CONFORMANCE.md block for one pinned ledger.
func renderLedgerStatement(version string, ledger ledgerCensus, fileTotal int, plans planAssertions) string {
	var other []classCount
	planSkips := 0
	for _, c := range ledger.innerSkips {
		switch {
		case c.class == string(javacorpus.SkipPlanAssertion):
			planSkips = c.n
		case javacorpus.SkipClass(c.class).SuppressesAssertion():
			other = append(other, c)
		}
	}
	explain, contains, hash := plans.byKind[javayamsql.ConfigExplain], plans.byKind[javayamsql.ConfigExplainContains], plans.byKind[javayamsql.ConfigPlanHash]
	var b strings.Builder
	fmt.Fprintf(&b, "Java's own yamsql corpus (fdb-record-layer %s, %d files) runs against Go in\n", version, fileTotal)
	b.WriteString("`pkg/relational/conformance/javacorpus`, pinned by `pinned_ledger_test.go`:\n\n")
	fmt.Fprintf(&b, "- **%d files pass, %d fail, %d are skipped whole**, by class: %s.\n",
		ledger.pass, ledger.fail, ledger.skip, joinClassCounts(ledger.fileSkips))
	fmt.Fprintf(&b, "- A pass means every assertion that ran held; plan assertions never run (RFC-201 ruling 2).\n"+
		"  The %d files that parse carry %d of them (`explain` %d, `explainContains` %d, `planHash` %d)\n"+
		"  in %d files; %d executions of them were skipped.\n",
		plans.parsedFiles, explain+contains+hash, explain, contains, hash, plans.filesWithPlans, planSkips)
	fmt.Fprintf(&b, "- Other assertions skipped, per execution: %s.\n", joinClassCounts(other))
	fmt.Fprintf(&b, "- %d query executions were asserted. By default a test block runs each query five times, plus a plan-cache check.\n", ledger.queries)
	return b.String()
}

func joinClassCounts(cs []classCount) string {
	if len(cs) == 0 {
		return "none"
	}
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = fmt.Sprintf("%s %d", c.class, c.n)
	}
	return strings.Join(parts, ", ")
}

func TestLedgerStatementArms(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"pass=1 fail=0 skip=0 file_skips{} inner_skips{}",
		"pass=1 fail=0 skip=1 queries=2 file_skips{fragment} inner_skips{}",
		"pass=1 fail=0 skip=1 queries=2 file_skips{fragment=x} inner_skips{}",
		"pass=1 fail=0 skip=1 queries=2 file_skips{=1} inner_skips{}",
		"pass=999999999999999999999999999999 fail=0 skip=0 queries=2 file_skips{} inner_skips{}",
		"pass=1 fail=0 skip=0 queries=2 file_skips{} inner_skips{plan-assertion}",
		"pass=1 fail=0 skip=0 queries=2 file_skips{} inner_skips{plan-assertion=x}",
	} {
		if _, err := parseLedgerLine(bad); err == nil {
			t.Errorf("parseLedgerLine(%q) accepted a malformed line", bad)
		}
	}

	// Empty lists render as none; the plan-assertion count has its own line,
	// and a class that suppresses no assertion (debugger) is left out.
	ledger, err := parseLedgerLine("pass=2 fail=0 skip=0 queries=7 file_skips{} " +
		"inner_skips{plan-assertion=5,unsupported:debugger=1}")
	if err != nil {
		t.Fatal(err)
	}
	plans := planAssertions{parsedFiles: 3, filesWithPlans: 1, byKind: map[javayamsql.ConfigKind]int{javayamsql.ConfigExplain: 2}}
	got := renderLedgerStatement("9.9.9.9", ledger, 2, plans)
	for _, want := range []string{
		"(fdb-record-layer 9.9.9.9, 2 files)",
		"**2 files pass, 0 fail, 0 are skipped whole**, by class: none.",
		"The 3 files that parse carry 2 of them (`explain` 2, `explainContains` 0, `planHash` 0)\n  in 1 files; 5 executions",
		"Other assertions skipped, per execution: none.",
		"- 7 query executions were asserted.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendering lacks %q:\n%s", want, got)
		}
	}

	ledger, err = parseLedgerLine("pass=0 fail=0 skip=3 queries=0 file_skips{fragment=1,plan-assertion=2} " +
		"inner_skips{no-checks=1,unsupported:continuation=4}")
	if err != nil {
		t.Fatal(err)
	}
	got = renderLedgerStatement("1", ledger, 3, planAssertions{byKind: map[javayamsql.ConfigKind]int{}})
	for _, want := range []string{
		"by class: plan-assertion 2, fragment 1.",
		"per execution: unsupported:continuation 4, no-checks 1.",
		"0 executions of them were skipped",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendering lacks %q:\n%s", want, got)
		}
	}
}
