package factory_test

import (
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/pkg/relational/conformance/factory"
)

// TestOfferRefusesToClobberACommittedScenario is the corpus's write-protection
// detector.
//
// The two keys in this system are INDEPENDENT and that is the whole hazard. A
// file's NAME is (seed, query index, projection); its DEDUP KEY is (feature
// vector, plan shape). Change the planner and the plan shape moves, so the
// dedup key moves, so the dedup lookup misses — while the name is byte-identical
// to a file already committed. Without a guard the batch writes straight over a
// committed expectation, and then every instrument agrees nothing happened: the
// census is recomputed from the overwritten directory, and the ratchet measures
// that same directory. There is no downstream check that can catch this, because
// every downstream check reads the file that replaced the evidence.
//
// The refusal has no benign case to carve out, and the test pins that too: a
// re-run against an UNCHANGED engine re-derives the same dedup key and is
// rejected by dedup before the writer is reached, so the second half here
// asserts that the ordinary re-run does not raise the alarm at all.
func TestOfferRefusesToClobberACommittedScenario(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	b, err := factory.NewBatch(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	const fv = "shape=single;idx=A;proj=star;where=cmp.eq;order=none"
	path, err := b.Offer(blessedOutcome(t, "fc_collide", fv, "1111111111111111"))
	if err != nil || path == "" {
		t.Fatalf("first write: %q, %v", path, err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A LATER batch over the same directory. The plan shape has moved, so the
	// dedup key is new and the dedup check cannot see the collision — exactly
	// what a planner change produces.
	next, err := factory.NewBatch(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	shifted := blessedOutcome(t, "fc_collide", fv, "2222222222222222")
	shifted.Scenario.Tests[0].Rows = [][]any{{int64(99)}} // different content
	got, err := next.Offer(shifted)
	if err != nil {
		t.Fatalf("Offer returned an error rather than a counted refusal: %v", err)
	}
	if got != "" {
		t.Fatalf("Offer reported %q as written; a blessed candidate whose NAME was already taken must not "+
			"be committed", got)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("the committed scenario at %s was OVERWRITTEN. The census and the ratchet both measure this "+
			"directory, so nothing downstream can notice; the frozen expectation is simply gone.", path)
	}

	m, err := next.Finish(1, 1, "2026-07-31", "metamorphic")
	if err != nil {
		t.Fatal(err)
	}
	if m.NameCollisions != 1 {
		t.Errorf("name_collisions = %d, want 1: a refused clobber that is not counted is a refused clobber "+
			"nobody investigates, and the cause is a planner change worth knowing about", m.NameCollisions)
	}
	if m.SkipsByReason["name-collision"] != 1 {
		t.Errorf("skips_by_reason[name-collision] = %d, want 1", m.SkipsByReason["name-collision"])
	}
	if len(m.SkipSamples["name-collision"]) == 0 {
		t.Error("the collision was counted with no example; a count nobody can triage is a count nobody acts on")
	}
	if m.Committed != 0 {
		t.Errorf("committed = %d, want 0: nothing new reached disk", m.Committed)
	}
	if n, _ := filepath.Glob(filepath.Join(dir, "*.yamsql")); len(n) != 1 {
		t.Errorf("%d family files on disk, want 1", len(n))
	}
}

// TestAnUnchangedReRunRaisesNoCollisionAlarm keeps the alarm above meaningful.
//
// Re-running the same seeds against an UNCHANGED engine re-derives the same
// feature vector and the same plan shape, so the dedup check rejects the
// candidate before the writer is ever reached. That is the routine nightly
// path, and if it raised name_collisions the number would be large every
// morning and read by nobody.
//
// It is also why the refusal above needs no identical-content branch: the dedup
// key is rendered INTO the file, so "same name, same bytes, different key" is
// not a state that exists.
func TestAnUnchangedReRunRaisesNoCollisionAlarm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const fv = "shape=single;idx=none;proj=star;where=cmp.lt;order=none"
	const shape = "3333333333333333"

	first, err := factory.NewBatch(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := first.Offer(blessedOutcome(t, "fc_reemit", fv, shape)); err != nil || p == "" {
		t.Fatalf("first write: %q, %v", p, err)
	}

	next, err := factory.NewBatch(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := next.Offer(blessedOutcome(t, "fc_reemit", fv, shape)); err != nil || p != "" {
		t.Fatalf("the re-run wrote %q (%v); dedup should have rejected it", p, err)
	}
	m, err := next.Finish(1, 1, "2026-07-31", "metamorphic")
	if err != nil {
		t.Fatal(err)
	}
	if m.NameCollisions != 0 {
		t.Errorf("name_collisions = %d on an unchanged re-run, want 0: the alarm must fire on a moved dedup "+
			"key, not on the nightly's ordinary behaviour", m.NameCollisions)
	}
	if m.DedupRejected != 1 {
		t.Errorf("dedup_rejected = %d, want 1: the re-run must be stopped by dedup, which is what makes the "+
			"name guard a last line rather than the first", m.DedupRejected)
	}
}
