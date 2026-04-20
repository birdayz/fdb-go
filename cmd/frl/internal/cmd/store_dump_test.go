package cmd

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func TestSubspaceLabelCoverage(t *testing.T) {
	t.Parallel()
	// Sanity: every public subspace ID in pkg/recordlayer/constants.go
	// should have a label entry. Missed entries render as "unknown",
	// which is a regression we want to prevent when new subspaces land.
	wants := map[int64]string{
		recordlayer.StoreInfoKey:                 "store-info",
		recordlayer.RecordKey:                    "record",
		recordlayer.IndexKey:                     "index",
		recordlayer.IndexSecondarySpaceKey:       "index-secondary",
		recordlayer.RecordCountKey:               "record-count",
		recordlayer.IndexStateSpaceKey:           "index-state",
		recordlayer.IndexRangeSpaceKey:           "index-range",
		recordlayer.IndexUniquenessViolationsKey: "uniq-violations",
		recordlayer.RecordVersionKey:             "record-version",
		recordlayer.IndexBuildSpaceKey:           "index-build",
	}
	for id, want := range wants {
		if got := subspaceLabel[id]; got != want {
			t.Errorf("subspaceLabel[%d] = %q, want %q", id, got, want)
		}
	}
}

func TestRenderKV_LabelsKnownSubspaces(t *testing.T) {
	t.Parallel()
	ss := subspace.Sub("myapp", "prod")

	cases := []struct {
		name      string
		keyTuple  tuple.Tuple
		wantLabel string
	}{
		{"store-info", tuple.Tuple{int64(0)}, "store-info"},
		{"record", tuple.Tuple{int64(1), int64(42), int64(0)}, "record"},
		{"index", tuple.Tuple{int64(2), "Order$price", int64(100), int64(42)}, "index"},
		{"unknown subspace id", tuple.Tuple{int64(99)}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kv := fdb.KeyValue{
				Key:   ss.Pack(tc.keyTuple),
				Value: []byte("abc"),
			}
			line, err := renderKV(ss, kv)
			if err != nil {
				t.Fatalf("renderKV: %v", err)
			}
			if !strings.HasPrefix(line, tc.wantLabel) {
				t.Errorf("label for %v = %q; want prefix %q", tc.keyTuple, line, tc.wantLabel)
			}
			if !strings.Contains(line, "value=3 bytes") {
				t.Errorf("value byte count missing from line: %q", line)
			}
		})
	}
}

func TestSubspaceIDByLabel_RoundTrip(t *testing.T) {
	t.Parallel()
	// Every label in subspaceLabel must round-trip back to the same ID.
	// The --subspace flag relies on this, so a typo in either direction
	// silently breaks the filter.
	for id, label := range subspaceLabel {
		got, ok := subspaceIDByLabel(label)
		if !ok {
			t.Errorf("subspaceIDByLabel(%q) = _, false; label not found", label)
			continue
		}
		if got != id {
			t.Errorf("subspaceIDByLabel(%q) = %d; want %d", label, got, id)
		}
	}
}

func TestSubspaceIDByLabel_Unknown(t *testing.T) {
	t.Parallel()
	if _, ok := subspaceIDByLabel("not-a-real-subspace"); ok {
		t.Error("subspaceIDByLabel returned true for unknown label")
	}
	// Empty string should also be rejected (the flag handler pre-filters
	// empties, but be defensive in the helper itself).
	if _, ok := subspaceIDByLabel(""); ok {
		t.Error("subspaceIDByLabel returned true for empty label")
	}
}

func TestKnownSubspaceLabels_Stable(t *testing.T) {
	t.Parallel()
	// The error message + shell completion both rely on this list being
	// deterministic across invocations. Call it twice and compare.
	a := knownSubspaceLabels()
	b := knownSubspaceLabels()
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("call[%d] = %q vs %q — not deterministic", i, a[i], b[i])
		}
	}
	if len(a) != len(subspaceLabel) {
		t.Errorf("got %d labels; want %d (every subspaceLabel entry)",
			len(a), len(subspaceLabel))
	}
}

func TestRenderKV_UnparseableKey(t *testing.T) {
	t.Parallel()
	ss := subspace.Sub("myapp")
	// A key with a prefix that doesn't match the subspace renders a
	// graceful "unpack failed" line rather than panicking.
	kv := fdb.KeyValue{Key: []byte{0xff, 0xff, 0x00}, Value: []byte{}}
	line, err := renderKV(ss, kv)
	if err != nil {
		t.Fatalf("renderKV: %v", err)
	}
	if !strings.Contains(line, "unpack failed") {
		t.Errorf("expected 'unpack failed' line, got: %q", line)
	}
}
