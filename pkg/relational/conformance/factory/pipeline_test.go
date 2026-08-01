package factory_test

import (
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/conformance/yamsql"
)

func blessedOutcome(t *testing.T, name, featureVector, planShape string) factory.Outcome {
	t.Helper()
	return factory.Outcome{
		Blessed: true,
		Header: factorycorpus.Header{
			Name:      name,
			Generator: factory.GeneratorVersion,
			// The seed is derived from the name so two outcomes in one test do
			// not collide on the (seed, query, projection) sort key the family
			// writer orders scenarios by.
			Seed:          seedFromName(name),
			QueryIndex:    0,
			Projection:    0,
			Date:          "2026-07-31",
			Blessing:      factorycorpus.BlessingMetamorphic,
			Oracles:       []string{"tlp", "second-plan"},
			FeatureVector: featureVector,
			PlanShape:     planShape,
			DedupKey:      factorycorpus.DedupKeyOf(featureVector, planShape),
		},
		Scenario: &yamsql.Scenario{
			Name:           name,
			SchemaTemplate: "CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))",
			Setup:          []string{"INSERT INTO t VALUES (1, 2)"},
			Tests: []yamsql.Test{{
				Query:     "SELECT id FROM t WHERE a = 2",
				Unordered: true,
				Columns:   []string{"ID"},
				Rows:      [][]any{{int64(1)}},
			}},
		},
	}
}

// seedFromName folds a scenario name into a stable small seed.
func seedFromName(name string) uint64 {
	var s uint64
	for _, r := range name {
		s = s*31 + uint64(r)
	}
	return s % 100000
}

// TestDedupRejectsARepeatedPoint is the dedup rule's detector.
//
// Committing 90k variants of one shape is volume without coverage (§5.4), and
// the failure mode is silent: a batch with a broken dedup key reports a big
// committed count and reads as a very productive run. Two outcomes at the same
// (feature vector, plan shape) point must produce exactly one file, and the
// second must be COUNTED as a dedup rejection rather than dropped.
func TestDedupRejectsARepeatedPoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b, err := factory.NewBatch(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	const fv = "shape=single;idx=A;proj=star;where=cmp.eq;order=none"
	const shape = "aaaaaaaaaaaaaaaa"

	first, err := b.Offer(blessedOutcome(t, "fc_dedup_a", fv, shape))
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("the first candidate at a fresh point was not committed")
	}
	second, err := b.Offer(blessedOutcome(t, "fc_dedup_b", fv, shape))
	if err != nil {
		t.Fatal(err)
	}
	if second != "" {
		t.Fatalf("a second candidate at the SAME (feature vector, plan shape) point was committed as %s; "+
			"the corpus is growing in volume without growing in coverage", second)
	}

	// A different point on either axis must still be admitted, or "dedup"
	// would just be "commit one file".
	if p, err := b.Offer(blessedOutcome(t, "fc_dedup_c", fv, "bbbbbbbbbbbbbbbb")); err != nil || p == "" {
		t.Fatalf("a candidate with a DIFFERENT plan shape was rejected (%q, %v); the same query answered by a "+
			"different plan is a different test", p, err)
	}
	if p, err := b.Offer(blessedOutcome(t, "fc_dedup_d", fv+";distinct=1", shape)); err != nil || p == "" {
		t.Fatalf("a candidate with a DIFFERENT feature vector was rejected (%q, %v)", p, err)
	}

	m, err := b.Finish(1, 1, "2026-07-31", "metamorphic")
	if err != nil {
		t.Fatal(err)
	}
	if m.DedupRejected != 1 {
		t.Errorf("dedup_rejected = %d, want 1: a rejection that is not counted is a rejection nobody can audit", m.DedupRejected)
	}
	if m.Committed != 3 {
		t.Errorf("committed = %d, want 3", m.Committed)
	}
	if m.Census.Scenarios != 3 {
		t.Errorf("census counted %d scenarios on disk, want 3", m.Census.Scenarios)
	}
}

// TestBatchSeedsDedupFromTheExistingCorpus pins that the factory is cumulative
// rather than repetitive. Without it, every nightly re-commits the shapes the
// previous night covered and a year of runs produces one night's coverage a
// hundred times over.
func TestBatchSeedsDedupFromTheExistingCorpus(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const fv = "shape=single;idx=none;proj=star;where=cmp.lt;order=none"
	const shape = "cccccccccccccccc"

	first, err := factory.NewBatch(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Offer(blessedOutcome(t, "fc_prior", fv, shape)); err != nil {
		t.Fatal(err)
	}

	// A fresh batch over the same directory — the next night's run.
	second, err := factory.NewBatch(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := second.Offer(blessedOutcome(t, "fc_tonight", fv, shape)); err != nil || p != "" {
		t.Fatalf("a later batch re-committed a point the corpus already holds (%q, %v)", p, err)
	}
}

// TestQuotaIsEnforcedAndCounted pins §5.2's per-run quota. A batch that
// silently exceeds it produces a PR nobody reviews, and a batch that silently
// stops at it without saying so looks like a corpus whose frontier has
// flattened.
func TestQuotaIsEnforcedAndCounted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b, err := factory.NewBatch(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i, shape := range []string{"1111111111111111", "2222222222222222", "3333333333333333"} {
		name := "fc_quota_" + string(rune('a'+i))
		if _, err := b.Offer(blessedOutcome(t, name, "shape=single;where=cmp.eq", shape)); err != nil {
			t.Fatal(err)
		}
	}
	m, err := b.Finish(1, 1, "2026-07-31", "metamorphic")
	if err != nil {
		t.Fatal(err)
	}
	if m.Committed != 2 {
		t.Errorf("committed %d, want 2 (the quota)", m.Committed)
	}
	if m.SkipsByReason["quota-reached"] != 1 {
		t.Errorf("quota-reached counted %d, want 1", m.SkipsByReason["quota-reached"])
	}
	// All three candidates share one feature family, so the two committed
	// scenarios land in ONE family file; the census, not the file count, is
	// what says two scenarios reached disk.
	entries, _ := filepath.Glob(filepath.Join(dir, "*.yamsql"))
	if len(entries) != 1 {
		t.Errorf("%d family files on disk, want 1", len(entries))
	}
	if m.Census.Scenarios != 2 {
		t.Errorf("census counted %d scenarios on disk, want 2", m.Census.Scenarios)
	}
}

// TestTLPPartitionViolationIsDetected is the TLP oracle's detector.
//
// The oracle's whole value is that a wrong-rows bug shows up as a partition
// that does not reconstruct. A checker that cannot see a violation is a
// checker that blesses everything, and it would look identical in every log:
// same "tlp checked" count, zero violations, a growing corpus of frozen wrong
// answers. Each case below is a distinct way a real engine bug would surface.
func TestTLPPartitionViolationIsDetected(t *testing.T) {
	t.Parallel()
	row := func(id int64) []any { return []any{id} }
	unfiltered := [][]any{row(1), row(2), row(3), row(4)}

	for _, tc := range []struct {
		name              string
		pos, neg, unknown [][]any
	}{
		{"a row is lost", [][]any{row(1)}, [][]any{row(2)}, [][]any{row(3)}},
		{"a row is duplicated across branches", [][]any{row(1), row(2)}, [][]any{row(2), row(3)}, [][]any{row(4)}},
		{"a branch invents a row", [][]any{row(1), row(9)}, [][]any{row(2), row(3)}, [][]any{row(4)}},
		{"counts match but values do not", [][]any{row(1), row(2)}, [][]any{row(3), row(9)}, nil},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if d := factory.CheckPartitionForTest(unfiltered, tc.pos, tc.neg, tc.unknown); d == "" {
				t.Fatalf("the partition checker accepted %q; a wrong-rows bug of this shape would be BLESSED "+
					"and committed as a frozen expectation", tc.name)
			}
		})
	}

	// The honest partition must pass, or the oracle rejects correct engines.
	if d := factory.CheckPartitionForTest(unfiltered, [][]any{row(1), row(2)}, [][]any{row(3)}, [][]any{row(4)}); d != "" {
		t.Fatalf("a correct partition was rejected: %s", d)
	}
	// Duplicate rows are the interesting case: set equality would accept a
	// plan that drops one of two identical rows.
	dup := [][]any{row(1), row(1), row(2)}
	if d := factory.CheckPartitionForTest(dup, [][]any{row(1)}, [][]any{row(2)}, nil); d == "" {
		t.Fatal("the checker accepted a partition that lost one of two identical rows — it is comparing sets, not multisets")
	}
	if d := factory.CheckPartitionForTest(dup, [][]any{row(1), row(1)}, [][]any{row(2)}, nil); d != "" {
		t.Fatalf("a correct partition over duplicate rows was rejected: %s", d)
	}
}

// TestTLPCannotSeeABranchMisassignment pins what the partition property does
// NOT detect, so nobody reasons about the corpus as if it did.
//
// A row moved from the FALSE branch to the UNKNOWN branch keeps the union
// exact: it appears once on each side, and every count matches. TLP is a
// statement about the union of the three branches and carries no information
// about WHICH branch a row landed in, so a bug that mis-evaluates a predicate
// as UNKNOWN where it should be FALSE is invisible to it. That is inherent to
// the oracle, not a defect in this checker — the same blind spot exists in
// every TLP implementation.
//
// It is also exactly why RFC-201's blessing rule demands BOTH metamorphic
// oracles rather than TLP alone. The second-plan oracle sees this class
// directly: `WHERE (p)` answered by an index scan and by a full scan must
// return the same rows, and a mis-evaluated predicate changes one of them.
//
// If this test ever goes RED, the partition checker has GAINED the ability to
// see branch assignment — which would be a real strengthening, and the
// blessing rule's dependence on the second-plan oracle should be revisited
// rather than left as folklore.
func TestTLPCannotSeeABranchMisassignment(t *testing.T) {
	t.Parallel()
	row := func(id int64) []any { return []any{id} }
	unfiltered := [][]any{row(1), row(2), row(3), row(4)}
	// Row 2 belongs in NOT-p; the engine put it in p-is-null.
	misassigned := factory.CheckPartitionForTest(unfiltered,
		[][]any{row(1)}, [][]any{row(3)}, [][]any{row(2), row(4)})
	correct := factory.CheckPartitionForTest(unfiltered,
		[][]any{row(1)}, [][]any{row(2), row(3)}, [][]any{row(4)})
	if misassigned != correct {
		t.Fatalf("the partition checker now distinguishes a branch misassignment (%q vs %q). That is a "+
			"STRENGTHENING, not a failure — update this pin, and revisit whether metamorphic blessing still "+
			"needs the second-plan oracle to cover this class.", misassigned, correct)
	}
	if misassigned != "" {
		t.Fatalf("both partitions were rejected (%q); the correct one must pass", misassigned)
	}
}

// TestFindingsArePersisted pins that an oracle disagreement leaves a
// reproducible record on disk. A finding that only exists in a log line is a
// finding nobody triages.
func TestFindingsArePersisted(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "findings")
	f := &factory.Finding{
		Oracle: "tlp", Seed: 99, Candidate: "fc_0000000099_q0_p0",
		Detail: "partition size 3 != unfiltered 4",
		DDL:    "CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))",
		Setup:  "INSERT INTO t VALUES (1)",
		Queries: []string{
			"SELECT id FROM t",
			"SELECT id FROM t WHERE (a > 1)",
		},
	}
	if err := factory.WriteFindings(dir, []*factory.Finding{f}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d finding files, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"fc_0000000099_q0_p0", "CREATE TABLE", "INSERT INTO", "partition size"} {
		if !contains(string(data), must) {
			t.Errorf("persisted finding does not carry %q; it cannot be reproduced from this record", must)
		}
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
