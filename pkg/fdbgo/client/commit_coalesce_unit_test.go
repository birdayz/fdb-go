package client

import (
	"bytes"
	"math/rand"
	"slices"
	"testing"
)

// TestCoalescePlainSetsSnapshot pins the plain-Set specialization independently
// of the replay oracle: byte-sorted keys, last-write wins, and no input mutation.
func TestCoalescePlainSetsSnapshot(t *testing.T) {
	t.Parallel()
	m := func(k, v string) Mutation {
		return Mutation{Type: MutSetValue, Key: []byte(k), Value: []byte(v)}
	}
	cases := []struct {
		name string
		in   []Mutation
		want []Mutation
		fast bool
	}{
		{"empty", nil, nil, true},
		{"singleton", []Mutation{m("a", "one")}, []Mutation{m("a", "one")}, true},
		{"sorted", []Mutation{m("a", "one"), m("b", "two")}, []Mutation{m("a", "one"), m("b", "two")}, true},
		{"sorted-overwrites", []Mutation{m("a", "first"), m("a", "last"), m("b", "old"), m("b", "new")}, []Mutation{m("a", "last"), m("b", "new")}, true},
		{"reverse", []Mutation{m("b", "two"), m("a", "one")}, []Mutation{m("a", "one"), m("b", "two")}, false},
		{"last-write", []Mutation{m("b", "old"), m("a", "first"), m("b", "new"), m("a", "last")}, []Mutation{m("a", "last"), m("b", "new")}, false},
		{"binary-prefixes", []Mutation{m("\xff", "ff"), m("a\x00", "nul"), m("a", "a"), m("", "empty")}, []Mutation{m("", "empty"), m("a", "a"), m("a\x00", "nul"), m("\xff", "ff")}, false},
		{"ordered-binary", []Mutation{m("", "empty"), m("a", "a"), m("a\x00", "nul"), m("\xff", "ff")}, []Mutation{m("", "empty"), m("a", "a"), m("a\x00", "nul"), m("\xff", "ff")}, true},
		{"empty-value-is-set", []Mutation{m("a", "old"), {Type: MutSetValue, Key: []byte("a")}}, []Mutation{m("a", "")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := cloneCoalesceMutationsForTest(tc.in)
			got, ok := coalescePlainSetMutations(tc.in)
			if ok != tc.fast {
				t.Fatalf("fast path=%v, want %v", ok, tc.fast)
			}
			if ok {
				assertCoalesceMutationsEqual(t, got, tc.want)
			} else if got != nil {
				t.Fatal("fallback returned a partial vector")
			}
			assertCoalesceMutationsEqual(t, tc.in, before)
			assertCoalesceMutationsEqual(t, coalesceCommitMutations(tc.in), tc.want)
		})
	}
	// A concurrently appended suffix is not part of the validated snapshot.
	backing := []Mutation{m("a", "old"), m("a", "new"), m("b", "one"), m("a", "unvalidated")}
	got := coalesceCommitMutations(backing[:3])
	assertCoalesceMutationsEqual(t, got, []Mutation{m("a", "new"), m("b", "one")})
}

func TestCoalescePlainSetsRejectsMixedSnapshots(t *testing.T) {
	t.Parallel()
	// The helper must inspect the whole snapshot before returning a result;
	// rejection leaves the existing mixed-op replay and versionstamp gate intact.
	for _, typ := range []MutationType{MutClearRange, MutAddValue, MutCompareAndClear, MutSetVersionstampedKey, MutSetVersionstampedValue} {
		for position := range 3 {
			muts := []Mutation{
				{Type: MutSetValue, Key: []byte("a"), Value: []byte("one")},
				{Type: MutSetValue, Key: []byte("b"), Value: []byte("two")},
				{Type: MutSetValue, Key: []byte("c"), Value: []byte("last")},
			}
			muts[position].Type = typ
			before := cloneCoalesceMutationsForTest(muts)
			if got, ok := coalescePlainSetMutations(muts); ok || got != nil {
				t.Fatalf("type %d at %d accepted: %v", typ, position, got)
			}
			assertCoalesceMutationsEqual(t, muts, before)
		}
	}
}

func TestCoalescePlainSetsMatchesReplay(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(391719))
	for trial := range 1000 {
		muts := make([]Mutation, rng.Intn(151))
		for i := range muts {
			key := []byte{byte(rng.Intn(32)), byte(rng.Intn(4))}
			if i%13 == 0 {
				key = key[:i%2]
			}
			value := make([]byte, rng.Intn(33))
			for j := range value {
				value[j] = byte(rng.Intn(256))
			}
			muts[i] = Mutation{Type: MutSetValue, Key: key, Value: value}
		}
		if trial%2 == 0 {
			slices.SortStableFunc(muts, func(a, b Mutation) int { return bytes.Compare(a.Key, b.Key) })
			if _, ok := coalescePlainSetMutations(muts); !ok {
				t.Fatalf("trial %d: ordered Sets rejected", trial)
			}
		}
		var replay rywCache
		for _, m := range muts {
			replay.set(m.Key, m.Value)
		}
		want := replay.materializeCommit()
		got := coalesceCommitMutations(muts)
		assertCoalesceMutationsEqual(t, got, want)
		tx := &Transaction{}
		if tx.approximateCommitSize(got, nil) != tx.approximateCommitSize(want, nil) {
			t.Fatalf("trial %d: coalesced size changed", trial)
		}
	}
}

func TestCoalescePlainSetsOwnedVectorBorrowedOperands(t *testing.T) {
	t.Parallel()
	// Structural rather than process-global allocation counting: the result
	// owns its vector but keeps the frozen operands. Reverting the dispatch to
	// replay must fail the pointer identity check because replay copies bytes.
	muts := make([]Mutation, 50)
	for i := range muts {
		muts[i] = Mutation{Type: MutSetValue, Key: []byte{byte(i)}, Value: make([]byte, 1000)}
	}
	result := coalesceCommitMutations(muts)
	if len(result) != len(muts) || &result[0] == &muts[0] {
		t.Fatal("result must have its own complete vector")
	}
	for i := range result {
		if &result[i].Key[0] != &muts[i].Key[0] || &result[i].Value[0] != &muts[i].Value[0] {
			t.Fatalf("operand %d was copied instead of borrowed from the frozen snapshot", i)
		}
	}
	result[0].Type = MutClearRange
	if muts[0].Type != MutSetValue {
		t.Fatal("changing output header mutated the snapshot")
	}
	// A hot key needs storage for one result, not for the entire input log.
	hot := make([]Mutation, 10000)
	for i := range hot {
		hot[i] = muts[0]
	}
	result = coalesceCommitMutations(hot)
	if len(result) != 1 || cap(result) != 1 {
		t.Fatalf("hot-key vector len/cap=%d/%d, want 1/1", len(result), cap(result))
	}
}

func cloneCoalesceMutationsForTest(in []Mutation) []Mutation {
	out := slices.Clone(in)
	for i := range out {
		out[i].Key = bytes.Clone(out[i].Key)
		out[i].Value = bytes.Clone(out[i].Value)
	}
	return out
}

func assertCoalesceMutationsEqual(t *testing.T, got, want []Mutation) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("mutation count=%d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Type != want[i].Type || !bytes.Equal(got[i].Key, want[i].Key) || !bytes.Equal(got[i].Value, want[i].Value) {
			t.Fatalf("mutation %d=%+v, want %+v", i, got[i], want[i])
		}
	}
}
