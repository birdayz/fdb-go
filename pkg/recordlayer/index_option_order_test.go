package recordlayer

import (
	"errors"
	"slices"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

func storedOptionKeys(t *testing.T, idx *Index) []string {
	t.Helper()
	p, err := indexToProto(idx)
	if err != nil {
		t.Fatalf("indexToProto: %v", err)
	}
	var keys []string
	for _, o := range p.GetOptions() {
		keys = append(keys, o.GetKey())
	}
	return keys
}

// Java stores an index's options in insertion order (Index holds an ImmutableMap,
// Index.java:131; toProto writes it in that order, :661-663). Go's Options is a
// map, so the order lives beside it: every emission must be the SetOption order,
// never map iteration order.
func TestIndexOptionOrder_SetOptionOrderIsStored(t *testing.T) {
	t.Parallel()
	// Keys chosen so that insertion order is neither sorted nor reverse-sorted,
	// and enough of them that map iteration would reorder them in some run.
	order := []string{"unique", "permutedSize", "zeta", "alpha", "mid", "beta", "omega", "gamma"}
	for range 50 {
		idx := NewIndex("i", Field("a"))
		for _, k := range order {
			idx.SetOption(k, "v-"+k)
		}
		if got := storedOptionKeys(t, idx); !slices.Equal(got, order) {
			t.Fatalf("stored option order = %v, want insertion order %v", got, order)
		}
	}
}

func TestIndexOptionOrder_ResetKeepsPositionAndDirectWritesSortLast(t *testing.T) {
	t.Parallel()
	idx := NewIndex("i", Field("a"))
	idx.SetOption("unique", "false")
	idx.SetOption("permutedSize", "1")
	idx.SetOption("unique", "true") // a second set changes the value only
	idx.Options["zz"] = "direct"
	idx.Options["aa"] = "direct"
	want := []string{"unique", "permutedSize", "aa", "zz"}
	for range 50 {
		if got := storedOptionKeys(t, idx); !slices.Equal(got, want) {
			t.Fatalf("stored option order = %v, want %v", got, want)
		}
	}
	if idx.Options["unique"] != "true" {
		t.Fatalf("unique = %q, want the second value", idx.Options["unique"])
	}
	// A deleted key drops out of the order; set again, it keeps its first slot.
	delete(idx.Options, "unique")
	if got := storedOptionKeys(t, idx); !slices.Equal(got, []string{"permutedSize", "aa", "zz"}) {
		t.Fatalf("after delete: %v", got)
	}
	idx.SetOption("unique", "false")
	if got := storedOptionKeys(t, idx); !slices.Equal(got, want) {
		t.Fatalf("after re-set: %v, want %v", got, want)
	}
}

// The constructors that seed an option set it through SetOption, so an option
// set afterwards follows it (Java's new Index(..., options) then the builder).
func TestIndexOptionOrder_ConstructorOptionsLead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		idx  *Index
		lead string
	}{
		{"permuted max", NewPermutedMaxIndex("i", Field("a"), 1), IndexOptionPermutedSize},
		{"permuted min", NewPermutedMinIndex("i", Field("a"), 1), IndexOptionPermutedSize},
		{"vector", NewVectorIndex("i", Field("a"), 3), IndexOptionVectorNumDimensions},
	} {
		tc.idx.SetUnique()
		if got := storedOptionKeys(t, tc.idx); !slices.Equal(got, []string{tc.lead, IndexOptionUnique}) {
			t.Errorf("%s: stored option order = %v, want [%s unique]", tc.name, got, tc.lead)
		}
	}
}

// A template read back and written again keeps the stored order (Index.buildOptions
// puts the list into an ImmutableMap.Builder in list order, Index.java:253-266).
func TestIndexOptionOrder_FromProtoKeepsStoredOrder(t *testing.T) {
	t.Parallel()
	stored := []string{"zeta", "unique", "alpha", "replacedBy_1", "mid", "replacedBy_0"}
	p := &gen.Index{
		Name:           proto.String("i"),
		RootExpression: Field("a").ToKeyExpression(),
	}
	for _, k := range stored {
		p.Options = append(p.Options, &gen.Index_Option{Key: proto.String(k), Value: proto.String("v-" + k)})
	}
	for range 50 {
		idx, err := indexFromProto(p)
		if err != nil {
			t.Fatalf("indexFromProto: %v", err)
		}
		if got := storedOptionKeys(t, idx); !slices.Equal(got, stored) {
			t.Fatalf("round-tripped option order = %v, want %v", got, stored)
		}
		// Index.getReplacedByIndexNames walks the options in order (:356-364).
		if got := idx.GetReplacedByIndexNames(); !slices.Equal(got, []string{"v-replacedBy_1", "v-replacedBy_0"}) {
			t.Fatalf("replaced-by names = %v, want option order", got)
		}
	}
}

// Guava's ImmutableMap.Builder refuses a key put twice, so Java cannot load an
// index whose stored option list repeats a key; neither may Go (it used to keep
// the last value silently).
func TestIndexOptionOrder_FromProtoRefusesDuplicateKey(t *testing.T) {
	t.Parallel()
	p := &gen.Index{
		Name:           proto.String("i"),
		RootExpression: Field("a").ToKeyExpression(),
		Options: []*gen.Index_Option{
			{Key: proto.String("unique"), Value: proto.String("true")},
			{Key: proto.String("x"), Value: proto.String("1")},
			{Key: proto.String("unique"), Value: proto.String("false")},
		},
	}
	_, err := indexFromProto(p)
	var dup *DuplicateIndexOptionError
	if !errors.As(err, &dup) {
		t.Fatalf("indexFromProto = %v, want DuplicateIndexOptionError", err)
	}
	// The target's message, measured on the conformance server (Guava 33.7.1) by
	// the JVM spec "WS-J duplicate index option": it names the later entry first.
	const want = "Multiple entries with same key: unique=false and unique=true"
	if dup.Error() != want {
		t.Fatalf("message = %q, want %q", dup.Error(), want)
	}
	// Adjacent repeats, the second input of the JVM spec "WS-J duplicate index
	// option" (conformance/ws_j_index_fidelity_conformance_test.go), which pins the
	// target's message for both inputs.
	_, err = indexFromProto(&gen.Index{
		Name:           proto.String("i"),
		RootExpression: Field("a").ToKeyExpression(),
		Options: []*gen.Index_Option{
			{Key: proto.String("a"), Value: proto.String("1")},
			{Key: proto.String("a"), Value: proto.String("2")},
		},
	})
	if !errors.As(err, &dup) || dup.Error() != "Multiple entries with same key: a=2 and a=1" {
		t.Fatalf("adjacent repeat: %v, want %q", err, "Multiple entries with same key: a=2 and a=1")
	}
}
