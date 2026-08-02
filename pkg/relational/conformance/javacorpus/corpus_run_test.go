package javacorpus_test

import (
	"context"
	"crypto/sha256"
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
			// queries= is load-bearing on a skip line: a skipped file that
			// nonetheless asserted queries got PART way, and which part is the
			// difference between "blocked at the door" and "blocked halfway".
			t.Logf("SKIP  %-70s %-30s queries=%d %s", f.Path, f.SkipClass, f.QueriesRun, fileSkipDetail(f))
		}
	}

	// A SetupNegatives entry exempts its file from the "a negative that died in
	// setup never reached its assertion" rule, so an entry that stops being
	// true silently re-opens the hole that rule closes. Assert each one is
	// still a negative whose failure really is in setup.
	for path, why := range javacorpus.SetupNegatives {
		var found *javacorpus.FileResult
		for i, f := range ledger.Files() {
			if f.Path == path {
				found = &ledger.Files()[i]
			}
		}
		switch {
		case found == nil:
			t.Errorf("SetupNegatives names %s, which the run does not contain", path)
		case javayamsql.PolarityOf(path) != javayamsql.NegativeExecution:
			t.Errorf("SetupNegatives names %s, which is not an execution-level negative", path)
		case found.SkipClass != javacorpus.SkipPolarityNegativeExecution:
			t.Errorf("SetupNegatives names %s (%s), but it is booked %s — if it no longer fails "+
				"in setup the entry is stale and must be deleted, which re-arms the setup-death check",
				path, why, found.SkipClass)
		case !strings.Contains(fileSkipDetail(*found), "setup step at line"):
			t.Errorf("SetupNegatives names %s, but its failure is no longer a setup step: %s",
				path, fileSkipDetail(*found))
		}
	}

	// A gap entry whose file stopped failing is a CLOSED gap nobody deleted,
	// and it would keep a working file booked as broken. Assert each entry
	// still matched something.
	// A matched gap's skip detail is "<booking>: <error>" — key on each
	// entry's OWN booking rather than a hardcoded "CQ-" prefix, so a gap
	// booked to an RFC follow-up (e.g. "RFC-143 §3a") is checked the same
	// way as a CQ-numbered one.
	matched := map[string]bool{}
	for _, f := range ledger.Files() {
		for _, s := range f.Skips {
			for _, g := range javacorpus.EngineGaps() {
				if f.Path == g.Path && strings.HasPrefix(s.Detail, g.Booking+": ") {
					matched[f.Path] = true
				}
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

	// A file whose only query carries NO configs must be booked `no-checks`,
	// never counted as a pass.
	//
	// scenario-tests.yamsql is that file upstream — one query, no configs, and
	// a "# TODO: add data" comment saying so. It executed and asserted
	// nothing, and it was reported as one of the passes until the noChecks
	// branch stopped incrementing QueriesRun. This is the exact shape the
	// vacuous guard exists for, and the guard was defeated by the very branch
	// that books the skip, so the census alone would not have caught it: the
	// pass count simply looked one higher than it deserved.
	const noChecksOnly = "scenario-tests.yamsql"
	found := false
	for _, f := range ledger.Files() {
		if f.Path != noChecksOnly {
			continue
		}
		found = true
		if f.Status != javacorpus.StatusSkip || f.SkipClass != javacorpus.SkipNoChecks {
			t.Errorf("%s asserts nothing (its only query carries no configs) and must be booked %q; "+
				"got status=%s class=%s queries=%d",
				noChecksOnly, javacorpus.SkipNoChecks, f.Status, f.SkipClass, f.QueriesRun)
		}
		if f.QueriesRun != 0 {
			t.Errorf("%s counted %d asserted queries; a noChecks query asserts nothing and must not "+
				"count towards QueriesRun, which is what the vacuous-pass guard tests",
				noChecksOnly, f.QueriesRun)
		}
	}
	if !found {
		t.Errorf("%s is not in the corpus run — the no-checks regression is no longer pinned", noChecksOnly)
	}

	// The counts alone cannot see a SWAP: two files exchanging classes leaves
	// every total identical. The per-file digest closes that hole.
	lines := make([]string, 0, len(ledger.Files()))
	for _, f := range ledger.Files() {
		class := string(f.SkipClass)
		if f.Status != javacorpus.StatusSkip {
			class = "-"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", f.Path, f.Status, class))
	}
	sort.Strings(lines)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(lines, "\n"))))
	if digest != pinnedAssignmentDigest {
		t.Errorf("per-file class assignment drifted (counts may be unchanged — this catches a SWAP).\n"+
			" got digest: %s\nwant digest: %s\nFull assignment follows; diff it against the previous run:\n%s",
			digest, pinnedAssignmentDigest, strings.Join(lines, "\n"))
	}

	// The negative-execution accounting, MEASURED rather than restated. The
	// manifest's entry count and the ledger's file-skip count are different
	// denominators and were fused once already; splitting the remainder into
	// "an earlier capability claimed the file" versus "the assertion itself was
	// suppressed" is what makes the difference legible.
	var booked, suppressed, claimedEarlier int
	for _, f := range ledger.Files() {
		if javayamsql.PolarityOf(f.Path) != javayamsql.NegativeExecution {
			continue
		}
		switch {
		case f.SkipClass == javacorpus.SkipPolarityNegativeExecution:
			booked++
		case f.SkipClass.SuppressesAssertion():
			suppressed++
		default:
			claimedEarlier++
		}
	}
	t.Logf("NEGATIVE-EXECUTION %d manifest entries = %d booked + %d assertion-suppressed + %d claimed-earlier",
		booked+suppressed+claimedEarlier, booked, suppressed, claimedEarlier)
	// 24 / 11 / 7, from 24 / 10 / 8. The single move is
	// `wrong-array-element-type.yamsql` going claimed-earlier → suppressed:
	// while the array-literal INSERT gap was open its setup insert died and
	// the parent's gap entry claimed the file; with the gap closed the file
	// runs through to its descending resultMetadata assertion, where the
	// CQ-74 metadata truncation declines the comparison
	// (unsupported:result-metadata-nested — a SuppressesAssertion class).
	// The earlier history (24/10/8 from 20/15/7 — all five scalar
	// `check-result-metadata/shouldFail/*` files moving suppressed → booked
	// once the directive became checked) still holds for the other 41 files.
	if booked != 24 || suppressed != 11 || claimedEarlier != 7 {
		t.Errorf("negative-execution accounting drifted: %d booked / %d suppressed / %d claimed-earlier, "+
			"pinned baseline is 24 / 11 / 7 (42 manifest entries)", booked, suppressed, claimedEarlier)
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
	javacorpus.SkipGapTableValuedFunction: "the TVF-in-FROM witness (versions-with-single-type-tests.yamsql) " +
		"declares its functions in the schema template, which now fails closed as unsupported-DDL:function " +
		"instead of silently dropping the declarations and running against a template missing them. The gap " +
		"re-arms when template function DDL lands (RFC-201 Phase 4)",
	javacorpus.SkipGapReturningDryRun: "the RETURNING-DRY-RUN witness (update-delete-returning.yamsql) declares " +
		"struct types, which now fail closed as unsupported-DDL:struct instead of being silently dropped from " +
		"the template. The gap re-arms with struct DDL (RFC-201 Phase 3)",
	javacorpus.SkipCopyBlock: "the only copy_block file is copy-basic.yamsql, skipped earlier by " +
		"required_clusters: 2 (unsupported:multi-cluster)",
	javacorpus.SkipVersionGate: "provably unreachable with one version under test: the version is the " +
		"CURRENT singleton, which sorts above every literal, so SupportedAtCurrentVersion is constantly true. " +
		"It becomes reachable the day a second version is under test",
}

// TestSkipClassesAreAllReachable keeps the vocabulary honest.
func TestSkipClassesAreAllReachable(t *testing.T) {
	t.Parallel()

	produced := producedClasses(t, pinnedLedger)

	var missing, unexpectedlyMasked []string
	for _, c := range javacorpus.AllSkipClasses() {
		produced := produced[string(c)]
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

// producedClasses parses the pinned census into the SET of class names it
// records a non-zero count for.
//
// A substring probe for `name + "="` would have answered a different question:
// every class name here is a prefix of nothing, but `engine-gap:cast-array`
// IS a prefix of `engine-gap:cast-array-literal`, so a probe would report a
// class produced because a LONGER one was. Parsing removes the whole family of
// such accidents rather than arguing that today's names happen to avoid them.
func producedClasses(t *testing.T, census string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, group := range []string{"file_skips{", "inner_skips{"} {
		i := strings.Index(census, group)
		if i < 0 {
			t.Fatalf("pinned census has no %s group", group)
		}
		rest := census[i+len(group):]
		j := strings.Index(rest, "}")
		if j < 0 {
			t.Fatalf("pinned census group %s is unterminated", group)
		}
		body := rest[:j]
		if body == "" {
			continue
		}
		for _, kv := range strings.Split(body, ",") {
			name, count, ok := strings.Cut(kv, "=")
			if !ok {
				t.Fatalf("pinned census entry %q is not name=count", kv)
			}
			if count == "0" {
				continue
			}
			out[name] = true
		}
	}
	return out
}

func containsClass(all []javacorpus.SkipClass, c javacorpus.SkipClass) bool {
	for _, x := range all {
		if x == c {
			return true
		}
	}
	return false
}
