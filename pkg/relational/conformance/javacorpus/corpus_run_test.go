package javacorpus_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/keystoretest"
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
// javaWorkingDir builds the directory Java's yaml-tests run in, holding the
// one file the corpus names by a relative path: serialization-options.yamsql's
// ENCRYPTION_KEY_STORE, `src/test/resources/serialization-keys.p12`. Java's
// file holds two 32-byte AES keys, `key-1` and `key-2`, under the password
// `YAML+SQL`; no key store file may be committed (cmd/secretscan), so an
// equivalent is written here. The file's records are written and read in this
// run, so only the aliases, the password and the keys' being distinct matter.
func javaWorkingDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resources := filepath.Join(dir, "src", "test", "resources")
	if err := os.MkdirAll(resources, 0o700); err != nil {
		t.Fatal(err)
	}
	key := func(first byte) []byte {
		k := make([]byte, 32)
		for i := range k {
			k[i] = first + byte(i)
		}
		return k
	}
	err := keystoretest.WritePKCS12(filepath.Join(resources, "serialization-keys.p12"), "YAML+SQL", "YAML+SQL",
		[]keystoretest.Entry{{Alias: "key-1", Key: key(0x10)}, {Alias: "key-2", Key: key(0x80)}})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

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

	workingDir := javaWorkingDir(t)
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
					WorkingDir:  workingDir,
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
	// Bookings are structured: their text may itself contain a colon.

	for _, g := range javacorpus.EngineGaps() {
		if !gapBookingMatched(g, ledger.Files()) {
			t.Errorf("engine gap %s (%s, %s) no longer matches: the file either passes now — "+
				"delete the entry and raise the pass count — or fails differently, which is a new bug.",
				g.Path, g.Class, g.Booking)
		}
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
	// Transaction setups run since RFC-257, so the five transaction-setup
	// negatives reach their own assertion (suppressed -> booked); the driver's
	// nested result metadata (CQ-74 closed) does the same for the six
	// check-result-metadata negatives on struct and array columns.
	if booked != 37 || suppressed != 5 || claimedEarlier != 0 {
		t.Errorf("negative-execution accounting drifted: %d booked / %d suppressed / %d claimed-earlier, "+
			"pinned baseline is 37 / 5 / 0 (42 manifest entries)", booked, suppressed, claimedEarlier)
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
// than no label because it reads as a covered case. These three are not dead:
// each names a real directive the corpus contains, reached only after another
// skip already claimed the file. Recording the masking relationship is what
// keeps that a stated fact rather than an unexplained zero — when the masking
// class shrinks, these should start appearing, and this table is where someone
// checks that they did.
var maskedClasses = map[javacorpus.SkipClass]string{
	javacorpus.SkipDDLStruct: "EMPTIED by RFC-204 Phase 1: CREATE TYPE AS STRUCT registers, struct " +
		"columns declare, and every former carrier re-booked to a narrower named class " +
		"(engine-gap:struct-dml, unsupported-DDL:struct-index, function/view causes) or passes. The class " +
		"stays declared as the declaration-scan fallback for a struct-declaring template failing DDL for " +
		"a cause the message rules do not name",
	javacorpus.SkipDDLStructIndex: "EMPTIED by RFC-257 WS-J step 7c: the index generator reads the " +
		"translated graph, and the class's three carriers build (aggregate-index-tests, subquery-tests, " +
		"documentation-queries/subqueries-documentation-queries). The class stays declared as the " +
		"message rule's bucket for a struct-declaring template whose index definition fails",
	javacorpus.SkipGapStructDML: "EMPTIED by RFC-204 Phase 2: struct literals write and read back, and " +
		"every carrier passes or moved on to the since-closed engine-gap:struct-query. Declared for a re-armed struct-DML " +
		"regression, which gaps.go would book here",
	javacorpus.SkipDDLOther: "EMPTIED by RFC-257: views, SQL functions, stored queries and sliding-window " +
		"vector indexes build, so no template fails DDL for an unnamed cause. The class stays declared " +
		"as the classifier's fallback bucket",
	javacorpus.SkipDDLFunction: "EMPTIED by RFC-257: SQL, macro and temporary functions all build. " +
		"Declared as the classifier's bucket for a template whose function declaration fails",
	javacorpus.SkipResultMetadataNested: "EMPTIED by CQ-74's close: every query's result set reports its " +
		"column DataTypes (api.WithResultSetMetaDataObserver), so the descending directives are compared. " +
		"Declared for a result set that reports no metadata",
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

func gapBookingMatched(g javacorpus.EngineGap, files []javacorpus.FileResult) bool {
	for _, f := range files {
		if f.Path != g.Path {
			continue
		}
		for _, s := range f.Skips {
			if s.Class == g.Class && s.GapBooking == g.Booking {
				return true
			}
		}
	}
	return false
}

func TestGapBookingWithColon(t *testing.T) {
	t.Parallel()
	g := javacorpus.EngineGap{Path: "uuid-prepared.yamsql", Class: javacorpus.SkipConformanceGoAccepts, Booking: "RFC-257: UUID sort"}
	files := []javacorpus.FileResult{{Path: g.Path, Skips: []javacorpus.Skip{{Class: g.Class, GapBooking: g.Booking, Detail: g.Booking + ": measured rejection"}}}}
	if !gapBookingMatched(g, files) {
		t.Fatal("a colon in the booking must not hide a matched gap")
	}
	g.Booking = "RFC-257: other gap"
	if gapBookingMatched(g, files) {
		t.Fatal("matched another booking")
	}
	g.Booking = "RFC-257"
	if gapBookingMatched(g, files) {
		t.Fatal("matched a booking prefix instead of the full booking")
	}
}
