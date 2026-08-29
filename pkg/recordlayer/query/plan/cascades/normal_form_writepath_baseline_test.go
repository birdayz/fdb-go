package cascades

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// The WRITE PATH, recorded as a golden BEFORE the normalizer is rewritten, so
// "it does not move" is checkable afterwards (RFC-240 §7, §8.2).
//
// `NormalizeDNFWithoutSimplification` output becomes stored
// `RecordMetaDataProto.Predicate` bytes through
// `pkg/relational/core/query/ddl/generator_predicate.go:55`. It is already the
// exact port of Java's `normalize` (not `normalizeAndSimplify` — Java reaches
// it the same way at `MaterializedViewIndexGenerator.java:675`, which is why
// the no-absorption variant exists at all). RFC-240 re-expresses its bodies
// through a mode parameter and must not change what it returns for any input.
//
// A differential against "the current implementation" cannot verify that: the
// current implementation is deleted by the same change. A golden captured
// beforehand and committed is the form that survives.
//
// ONLY the write path is recorded here. The cost-model metric deliberately DOES
// move — it is a mis-port being corrected — and is asserted positively by name
// rather than frozen; freezing it under a new name was RFC-240 v1's rejected
// design.
//
// Regenerate with:
//
//	NORMAL_FORM_WRITEPATH_UPDATE=1 go test ./pkg/recordlayer/query/plan/cascades/ \
//	    -run TestNormalFormWritePath_IsUnchanged -count=1
//
// Regenerating records a DELIBERATE wire-visible change. Doing it to turn a red
// test green is how a golden stops meaning anything: the diff is the artifact,
// so read it.
const normalFormWritePathFile = "testdata/normal_form_writepath_baseline.txt"

// normalFormWritePathBound caps the recorded normal-form clause count so the
// golden stays readable. An unbounded first attempt produced a 2.1 MB file,
// which is a golden nobody reviews — one entry alone rendered 648 KB, because
// the eager cross product is exponential in the input.
//
// Entries above the bound are recorded by SHAPE (clause and atom counts) rather
// than dropped, so an explosive input still contributes a check: its structure
// is pinned even where its text is not.
const normalFormWritePathBound = 24

func normalFormWritePathCorpus() []predicates.QueryPredicate {
	var scripts [][]byte
	for a := 0; a < 8; a++ {
		for b := 0; b < 8; b++ {
			for c := 0; c < 5; c++ {
				scripts = append(scripts, []byte{byte(a), byte(b), byte(c)})
				scripts = append(scripts, []byte{byte(a), byte(b), byte(c), byte(a * 3), byte(b * 5)})
			}
		}
	}
	out := make([]predicates.QueryPredicate, 0, len(scripts))
	for _, s := range scripts {
		b := &predicateBuilder{script: s}
		if p := b.build(0); p != nil {
			out = append(out, p)
		}
	}
	return append(out, normalFormWritePathNamedShapes()...)
}

// normalFormWritePathNamedShapes are the shapes RFC-240 is about, written out
// rather than left to the script sweep to stumble on. The script corpus is
// broad but nothing in it says WHICH shapes matter; these do.
func normalFormWritePathNamedShapes() []predicates.QueryPredicate {
	a, b, c := normalFormWritePathLeaves()
	return []predicates.QueryPredicate{
		predicates.NewNot(predicates.NewAnd(a, b)),
		predicates.NewNot(predicates.NewOr(a, b)),
		predicates.NewAnd(predicates.NewNot(predicates.NewAnd(a, b)), c),
		predicates.NewOr(predicates.NewNot(predicates.NewAnd(a, b)), c),
		predicates.NewAnd(predicates.NewNot(predicates.NewOr(a, b)), c),
		predicates.NewNot(predicates.NewNot(predicates.NewAnd(a, b))),
		predicates.NewNot(predicates.NewAnd(a, predicates.NewOr(b, c))),
		predicates.NewAnd(predicates.NewOr(a, b), predicates.NewOr(b, c)),
		predicates.NewOr(predicates.NewAnd(a, b), predicates.NewAnd(b, c)),
		predicates.NewAnd(predicates.NewOr(a, b), c, predicates.NewOr(a, b)),
		predicates.NewOr(predicates.NewAnd(a, b), c, predicates.NewAnd(a, b)),
	}
}

func normalFormWritePathLeaves() (x, y, z predicates.QueryPredicate) {
	bld := &predicateBuilder{script: []byte{0}}
	col0 := bld.column(0)
	col1 := bld.column(1)
	return predicates.NewComparisonPredicate(col0,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))),
		predicates.NewComparisonPredicate(col1,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2))),
		predicates.NewComparisonPredicate(col0,
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)))
}

func normalFormWritePathRender(p predicates.QueryPredicate) string {
	out, changed := NormalizeDNFWithoutSimplification(p, NormalizerDefaultSizeLimit)
	if predicates.PredicateSize(out) > normalFormWritePathBound {
		return fmt.Sprintf("in=%s\n  writepath(changed=%v)=<shape size=%d>",
			p.Explain(), changed, predicates.PredicateSize(out))
	}
	return fmt.Sprintf("in=%s\n  writepath(changed=%v)=%s", p.Explain(), changed, out.Explain())
}

// TestNormalFormWritePath_IsUnchanged asserts the write path against the
// committed golden.
//
// It guards its own population first — a corpus that collapsed, or a golden
// that lost its entries, would pass while checking nothing, which is the
// dominant false positive for a golden test.
func TestNormalFormWritePath_IsUnchanged(t *testing.T) {
	t.Parallel()

	corpus := normalFormWritePathCorpus()
	if len(corpus) < 400 {
		t.Fatalf("corpus collapsed to %d entries — the golden would check almost nothing", len(corpus))
	}
	// The named shapes are the point; a sweep that stopped producing NOTs would
	// leave the golden broad and blind.
	nots := 0
	for _, p := range corpus {
		if _, isNot := p.(*predicates.NotPredicate); isNot {
			nots++
		}
	}
	if nots == 0 {
		t.Fatal("no NOT-rooted entry in the corpus — the shape RFC-240 changes is unrepresented")
	}

	lines := make([]string, 0, len(corpus))
	for _, p := range corpus {
		lines = append(lines, normalFormWritePathRender(p))
	}
	got := strings.Join(lines, "\n") + "\n"

	path := filepath.Clean(normalFormWritePathFile)
	if os.Getenv("NORMAL_FORM_WRITEPATH_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create testdata dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("write-path golden REGENERATED: %d entries (%d NOT-rooted), %d bytes — read the diff",
			len(corpus), nots, len(got))
		return
	}

	wantBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with NORMAL_FORM_WRITEPATH_UPDATE=1)", path, err)
	}
	want := string(wantBytes)
	if entries := strings.Count(want, "in="); entries != len(corpus) {
		t.Fatalf("golden has %d entries, corpus has %d — the corpus changed shape; "+
			"regenerate deliberately and read the diff", entries, len(corpus))
	}
	if got == want {
		return
	}

	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")
	for i := range gotLines {
		if i >= len(wantLines) {
			t.Fatalf("golden is shorter than the current output at line %d: %q", i, gotLines[i])
		}
		if gotLines[i] != wantLines[i] {
			t.Fatalf("the WRITE PATH moved at line %d.\n  want: %s\n  got:  %s\n"+
				"NormalizeDNFWithoutSimplification feeds stored index predicate bytes; "+
				"RFC-240 §7 freezes it.", i, wantLines[i], gotLines[i])
		}
	}
	t.Fatalf("golden differs in length: got %d lines, want %d", len(gotLines), len(wantLines))
}
