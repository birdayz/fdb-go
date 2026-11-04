package recordlayer

import (
	_ "embed"
	"regexp"
	"testing"
)

//go:embed index.go
var indexGoSource string

// Every declared type is deliberately classified against the predicate used by
// the SQL planner. Adding a type requires both a decision and this census entry.
func TestIndexScanCandidacy_EveryTypeClassified(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		IndexTypeValue: true, IndexTypeVersion: true, IndexTypeRank: true,
		IndexTypePermutedMin: true, IndexTypePermutedMax: true,
		IndexTypeCount: false, IndexTypeCountNotNull: false, IndexTypeCountUpdates: false,
		IndexTypeSum: false, IndexTypeMaxEverLong: false, IndexTypeMinEverLong: false,
		IndexTypeMaxEverTuple: false, IndexTypeMinEverTuple: false, IndexTypeMaxEverVersion: false,
		IndexTypeMaxEver: false, IndexTypeMinEver: false, IndexTypeBitmapValue: false,
		IndexTypeVector: false, IndexTypeVectorSPFresh: false,
		IndexTypeText: false, IndexTypeMultidimensional: false, IndexTypeTimeWindowLeaderboard: false,
	}
	re := regexp.MustCompile(`IndexType\w+[ \t]+(?:\w+[ \t]+)?=[ \t]*"([^"]+)"`)
	matches := re.FindAllStringSubmatch(indexGoSource, -1)
	if len(matches) != 22 || len(want) != len(matches) {
		t.Fatalf("constant census=%d, classifications=%d, want22", len(matches), len(want))
	}
	seen := map[string]bool{}
	for _, match := range matches {
		typ := match[1]
		expected, ok := want[typ]
		if !ok {
			t.Errorf("unclassified index type%q", typ)
			continue
		}
		seen[typ] = true
		if got := IsValueScanCandidateIndexType(typ); got != expected {
			t.Errorf("production candidacy%q=%v,want%v", typ, got, expected)
		}
		if (&Index{Type: typ}).IsAtomicMutationIndex() && IsValueScanCandidateIndexType(typ) {
			t.Errorf("atomic type%q offered VALUE scan", typ)
		}
	}
	for typ := range want {
		if !seen[typ] {
			t.Errorf("census omitted%q", typ)
		}
	}
	for _, unknown := range []string{"", "opaque_plugin", "future_builtin"} {
		if IsValueScanCandidateIndexType(unknown) {
			t.Errorf("unknown type%q failed open", unknown)
		}
	}
}
