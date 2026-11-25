package fleet

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// ONE RULE, ONE ERROR TYPE, AT EVERY DEPTH IT FIRES.
//
// The synthetic-metadata refusal fires in three places: the collector itself
// (which owns the invariant), and the connection and fleet paths (which refuse
// earlier, before a store is opened, and can name the schema).
//
// They must all carry the SAME typed error. When they did not, the collector's
// type was structurally UNREACHABLE through the relational paths — a test
// pinning it with errors.As would pass on the direct path and be unable to fire
// on the other two. That is a green from an empty set wearing an error type:
// the assertion runs, finds nothing to contradict it, and reports coverage.
//
// This pins the fleet path. The connection path is pinned in its own package,
// and the collector's own refusal in recordlayer's suite; the three together
// are what make "one concept, one type" checkable rather than asserted.
func TestSyntheticRefusalCarriesTheCollectorsTypedError(t *testing.T) {
	t.Parallel()

	md := syntheticMetaData(t)
	ev, refused := syntheticRefusal(md)
	if !refused {
		t.Fatalf("metadata declaring synthetic types was not refused, so the error " +
			"identity below is not being exercised at all")
	}

	var typed *recordlayer.SyntheticRecordTypesNotModeledError
	if !errors.As(ev.Err, &typed) {
		t.Fatalf("the fleet refusal does not carry *SyntheticRecordTypesNotModeledError, "+
			"so one rule has two representations and a matcher can only ever fire on "+
			"whichever path it happens to take: %v", ev.Err)
	}
	if len(typed.TypeNames) == 0 {
		t.Errorf("the typed error names no declarations, so it cannot say what was refused")
	}
}
