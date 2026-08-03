package javacorpus

import (
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// TestDescendingMetadataCorpusCost measures what CQ-74 costs, instead of
// quoting it.
//
// The booking claims a number of corpus files whose `resultMetadata:` the
// driver cannot answer, and how many of those are held behind RFC-201 Phase 3
// rather than behind the metadata truncation itself. Both figures were wrong
// the first time they were written down, because they were estimated from a
// `grep` for the directive rather than from the directive's SHAPE — most files
// carrying `resultMetadata:` carry only scalar expectations, which are asserted
// today.
//
// Parse-only, so it runs without a cluster and cannot drift silently: when
// Phase 3 lands, `structHeld` drops and this test says by how much.
func TestDescendingMetadataCorpusCost(t *testing.T) {
	t.Parallel()

	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	paths, err := corpus.List()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}

	var descending, structHeld []string
	for _, path := range paths {
		if javayamsql.PolarityOf(path) == javayamsql.NegativeParse {
			// The parser must refuse these, so their directives never exist.
			continue
		}
		file, err := corpus.ParseFile(path)
		if err != nil {
			continue
		}
		if !fileHasDescendingMetadata(file) {
			continue
		}
		descending = append(descending, path)
		// `classifyDDLByDeclaration`'s own predicate over the declaration
		// scan (nil ddlError: only the struct-vs-function bucketing is
		// wanted here, not the index-cause split): a struct-declaring file
		// is claimed by the struct workstream no matter what its metadata
		// directive says.
		if classifyDDLByDeclaration(file, nil) == SkipDDLStruct {
			structHeld = append(structHeld, path)
		}
	}
	sort.Strings(descending)
	sort.Strings(structHeld)

	const (
		wantDescending = 14
		wantStructHeld = 12
	)
	if len(descending) != wantDescending || len(structHeld) != wantStructHeld {
		t.Errorf("CQ-74's corpus cost drifted: %d files carry a descending resultMetadata "+
			"(%d of them held behind Phase-3 struct DDL), pinned baseline is %d (%d).\n"+
			"descending:\n  %s\nstruct-held:\n  %s",
			len(descending), len(structHeld), wantDescending, wantStructHeld,
			strings.Join(descending, "\n  "), strings.Join(structHeld, "\n  "))
	}
}

// fileHasDescendingMetadata reports whether any query in the file (or in an
// include it pulls in) carries a resultMetadata expectation metadataDescends
// declines.
func fileHasDescendingMetadata(f *javayamsql.File) bool {
	var walk func(*javayamsql.File) bool
	walk = func(file *javayamsql.File) bool {
		for _, b := range file.Blocks {
			switch b.Kind {
			case javayamsql.BlockInclude:
				if b.Include.Resolved != nil && walk(b.Include.Resolved) {
					return true
				}
			case javayamsql.BlockTest:
				for _, tst := range b.Test.Tests {
					for _, cfg := range tst.Command.Configs {
						if cfg.Kind != javayamsql.ConfigResultMetadata {
							continue
						}
						if descends, _ := metadataDescends(cfg.Raw); descends {
							return true
						}
					}
				}
			}
		}
		return false
	}
	return walk(f)
}
