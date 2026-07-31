package javacorpus_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/javacorpus"
	"fdb.dev/pkg/relational/conformance/javayamsql"
	_ "fdb.dev/pkg/relational/sqldriver"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

var clusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		// A container failure in CI must be fatal, never a silent all-skip:
		// the ledger would otherwise report a perfectly clean run of nothing,
		// which is the exact failure mode counted skips exist to prevent.
		if os.Getenv("CI") != "" {
			fmt.Fprintf(os.Stderr, "FATAL: FDB container startup failed in CI — the java corpus would silently skip: %v\n", err)
			os.Exit(1)
		}
		os.Exit(m.Run())
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFile: %v\n", err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "fdb-javacorpus-*.cluster")
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

	os.Exit(m.Run())
}

// TestJavaCorpusRuns executes every vendored `.yamsql` file against the real
// engine and pins the resulting ledger.
//
// The pinned census is the point of the test, not decoration. A corpus run
// whose only assertion is "no file failed" is satisfiable by a runner that
// skips everything, so the pass count, the skip count and the per-class
// breakdown are all asserted: a file that stops running, a skip class that
// grows, and a gap that quietly changes shape each fail with the delta named.
func TestJavaCorpusRuns(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	files, err := corpus.List()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}

	ledger := &javacorpus.Ledger{}

	// The inner group returns only once every parallel subtest under it has
	// finished, which is what makes the census below safe to read.
	t.Run("files", func(t *testing.T) {
		for i, path := range files {
			t.Run(sanitizeName(path), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()

				res := javacorpus.Run(ctx, corpus, path, javacorpus.Config{
					ClusterFile: clusterFilePath,
					IDPrefix:    fmt.Sprintf("C%d", i),
				})
				ledger.Add(res)
				if res.Status == javacorpus.StatusFail {
					t.Errorf("%s: %v", path, res.Err)
				}
			})
		}
	})

	census := ledger.Census()
	t.Logf("LEDGER %s", census.Line())

	if total := census.Pass + census.Fail + census.Skip; total != pinnedFileTotal {
		t.Errorf("accounted for %d pass + %d fail + %d skip = %d files, corpus has %d",
			census.Pass, census.Fail, census.Skip, total, pinnedFileTotal)
	}
	for _, f := range ledger.Files() {
		if f.Status == javacorpus.StatusSkip {
			t.Logf("SKIP  %-70s %-30s %s", f.Path, f.SkipClass, fileSkipDetail(f))
		}
	}

	// A gap entry whose file stopped failing is a CLOSED gap nobody deleted,
	// and it would keep a working file booked as broken. Assert each entry
	// still matched something.
	matched := map[string]bool{}
	for _, f := range ledger.Files() {
		for _, s := range f.Skips {
			if strings.HasPrefix(s.Detail, "CQ-") {
				matched[f.Path] = true
			}
		}
	}
	for _, g := range javacorpus.EngineGaps() {
		if !matched[g.Path] {
			t.Errorf("engine gap %s (%s, %s) no longer matches: the file either passes now — "+
				"delete the entry and raise the pass count — or fails differently, which is a new bug.",
				g.Path, g.Class, g.Booking)
		}
	}

	if got := census.Line(); got != pinnedLedger {
		t.Errorf("corpus ledger drifted.\n got: %s\nwant: %s", got, pinnedLedger)
	}
}

// fileSkipDetail surfaces the cause behind a file-level skip, so the log names
// the engine's own rejection rather than only the bucket it landed in.
func fileSkipDetail(f javacorpus.FileResult) string {
	for _, s := range f.Skips {
		if s.Class == f.SkipClass && s.Detail != "" {
			return s.Detail
		}
	}
	return ""
}

func sanitizeName(p string) string {
	return strings.NewReplacer("/", "_", ".yamsql", "").Replace(p)
}

// maskedClasses are the reason classes a corpus run does NOT currently
// produce, each with the class that pre-empts it.
//
// A class nothing emits is normally a dead label, and a dead label is worse
// than no label because it reads as a covered case. These four are not dead:
// each names a real directive the corpus contains, reached only after another
// skip already claimed the file. Recording the masking relationship is what
// keeps that a stated fact rather than an unexplained zero — when the masking
// class shrinks, these should start appearing, and this table is where someone
// checks that they did.
var maskedClasses = map[javacorpus.SkipClass]string{
	javacorpus.SkipDDLFunction: "every corpus template declaring `create function` also declares an " +
		"AS-SELECT value index, so unsupported-DDL:value-index-as-select claims the file first",
	javacorpus.SkipCopyBlock: "the only copy_block file is copy-basic.yamsql, skipped earlier by " +
		"required_clusters: 2 (unsupported:multi-cluster)",
	javacorpus.SkipRandomInjection: "the !r / !a segments live in prepared.yamsql and showcasing-tests.yamsql, " +
		"claimed first by engine-gap:array-literal-values and unsupported-DDL:struct",
	javacorpus.SkipVersionGate: "provably unreachable with one version under test: the version is the " +
		"CURRENT singleton, which sorts above every literal, so SupportedAtCurrentVersion is constantly true. " +
		"It becomes reachable the day a second version is under test",
}

// TestSkipClassesAreAllReachable keeps the vocabulary honest.
func TestSkipClassesAreAllReachable(t *testing.T) {
	t.Parallel()

	var missing, unexpectedlyMasked []string
	for _, c := range javacorpus.AllSkipClasses() {
		produced := strings.Contains(pinnedLedger, string(c)+"=")
		_, masked := maskedClasses[c]
		if !produced && !masked {
			missing = append(missing, string(c))
		}
		if produced && masked {
			unexpectedlyMasked = append(unexpectedlyMasked, string(c))
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpectedlyMasked)

	if len(missing) != 0 {
		t.Errorf("skip classes declared but never produced by a corpus run: %v\n"+
			"Either the class is dead and should be deleted, or the runner stopped reaching it, "+
			"or it is newly masked and belongs in maskedClasses with the class that pre-empts it.", missing)
	}
	if len(unexpectedlyMasked) != 0 {
		t.Errorf("skip classes listed as masked but now produced: %v\n"+
			"The masking class shrank — remove them from maskedClasses.", unexpectedlyMasked)
	}
	for c := range maskedClasses {
		if !containsClass(javacorpus.AllSkipClasses(), c) {
			t.Errorf("maskedClasses names %q, which is not a declared skip class", c)
		}
	}
}

func containsClass(all []javacorpus.SkipClass, c javacorpus.SkipClass) bool {
	for _, x := range all {
		if x == c {
			return true
		}
	}
	return false
}
