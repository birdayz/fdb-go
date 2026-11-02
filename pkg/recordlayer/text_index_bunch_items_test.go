package recordlayer

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func randomBunch(rng *rand.Rand) []bunchedEntry {
	n := 1 + rng.IntN(25)
	entries := make([]bunchedEntry, n)
	seq := int64(rng.IntN(1000))
	for i := range entries {
		seq += 1 + int64(rng.IntN(300))
		var key tuple.Tuple
		switch rng.IntN(3) {
		case 0:
			key = tuple.Tuple{"writer", seq, int64(rng.IntN(3))}
		case 1:
			key = tuple.Tuple{seq}
		default:
			key = tuple.Tuple{"w", seq, nil, []byte{0, byte(i)}}
		}
		positions := []int{}
		p := 0
		for range rng.IntN(6) {
			p += rng.IntN(400)
			positions = append(positions, p)
		}
		entries[i] = bunchedEntry{Key: key, Value: positions}
	}
	slices.SortFunc(entries, func(a, b bunchedEntry) int { return compareTuples(a.Key, b.Key) })
	return entries
}

// Put and remove keep a bunch's stored bytes: decoding it into items and
// encoding them back is the identity on what serializeEntries writes, and
// the items hold its keys' packed bytes and its lists.
func TestBunchItemsRoundTrip(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	rng := rand.New(rand.NewPCG(5, 6))
	for range 300 {
		entries := randomBunch(rng)
		data, err := s.SerializeEntries(entries)
		if err != nil {
			t.Fatal(err)
		}
		items, err := decodeBunchItems(entries[0].Key.Pack(), data, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != len(entries) {
			t.Fatalf("%d items, want %d", len(items), len(entries))
		}
		for i := range items {
			if !bytes.Equal(items[i].key, entries[i].Key.Pack()) {
				t.Fatalf("item %d key %x, want %x", i, items[i].key, entries[i].Key.Pack())
			}
			if items[i].list == nil {
				t.Fatalf("item %d: canonical list decoded", i)
			}
			positions, err := itemPositions(&items[i])
			if err != nil || !slices.Equal(positions, entries[i].Value) {
				t.Fatalf("item %d positions %v %v, want %v", i, positions, err, entries[i].Value)
			}
		}
		size, err := bunchItemsSize(items)
		if err != nil {
			t.Fatal(err)
		}
		if got := encodeBunchItems(items, size); !bytes.Equal(got, data) {
			t.Fatalf("re-encoded %x, want %x", got, data)
		}
		last := items[len(items)-1]
		appended, err := serializeBunchItem(&last)
		if err != nil {
			t.Fatal(err)
		}
		want, err := s.SerializeEntry(entries[len(entries)-1].Key, entries[len(entries)-1].Value)
		if err != nil || !bytes.Equal(appended, want) {
			t.Fatalf("appended %x %v, want %x", appended, err, want)
		}
	}
}

// A list stored with non-minimal varints is written back as Java's
// deserializeList and serializeList give it, canonical; a key is written back
// as stored, as Java's Tuple.fromBytes keeps its bytes.
func TestBunchItemsCanonicalizeListsKeepKeys(t *testing.T) {
	t.Parallel()
	// (1): [1]; key (w, 7) with a 2-byte int: size 0x80 0x03, deltas 2, 0x80 0x02.
	nonCanonicalKey := []byte{0x02, 'w', 0x00, 0x16, 0x00, 0x07}
	data := []byte{0x20, 0x01, 0x01, byte(len(nonCanonicalKey))}
	data = append(data, nonCanonicalKey...)
	data = append(data, 0x80, 0x03, 0x02, 0x80, 0x02)
	items, err := decodeBunchItems(tuple.Tuple{int64(1)}.Pack(), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if items[1].list != nil || !slices.Equal(items[1].positions, []int{2, 4}) {
		t.Fatalf("item 1 = %+v, want positions [2 4]", items[1])
	}
	got, err := serializeBunchItems(items)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x20, 0x01, 0x01, byte(len(nonCanonicalKey))}
	want = append(want, nonCanonicalKey...)
	want = append(want, 0x02, 0x02, 0x02)
	if !bytes.Equal(got, want) {
		t.Fatalf("re-encoded %x, want %x", got, want)
	}
	// The public decode agrees on the positions.
	entries, err := TextIndexBunchedSerializerInstance().DeserializeEntries(tuple.Tuple{int64(1)}, data)
	if err != nil || !slices.Equal(entries[1].Value, []int{2, 4}) {
		t.Fatalf("DeserializeEntries = %v %v", entries, err)
	}
}

// decodeBunchItems fails exactly where DeserializeKey and DeserializeEntries
// together fail, on every truncation and one-byte mutation of a bunch.
func TestBunchItemsFailWhereDeserializeFails(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	rng := rand.New(rand.NewPCG(7, 8))
	check := func(firstKey, data []byte) {
		t.Helper()
		_, itemsErr := decodeBunchItems(firstKey, data, nil)
		key, keyErr := s.DeserializeKey(firstKey, 0, len(firstKey))
		var entriesErr error
		if keyErr == nil {
			_, entriesErr = s.DeserializeEntries(key, data)
		}
		if (itemsErr == nil) != (keyErr == nil && entriesErr == nil) {
			t.Fatalf("key %x data %x: items %v, key %v, entries %v", firstKey, data, itemsErr, keyErr, entriesErr)
		}
	}
	for range 60 {
		entries := randomBunch(rng)
		data, err := s.SerializeEntries(entries)
		if err != nil {
			t.Fatal(err)
		}
		firstKey := entries[0].Key.Pack()
		for n := range len(data) {
			check(firstKey, data[:n])
			mutated := slices.Clone(data)
			mutated[n] = byte(rng.IntN(256))
			check(firstKey, mutated)
		}
		for n := range len(firstKey) {
			check(firstKey[:n], data)
		}
	}
}

// Each decoded list is its own slice: appending to one cannot overwrite the
// next, and an empty list is empty, not nil (Java's Collections.emptyList()).
func TestDeserializeEntriesListsAreIndependent(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	data, err := s.SerializeEntries([]bunchedEntry{
		{Key: tuple.Tuple{int64(1)}, Value: []int{1, 2}},
		{Key: tuple.Tuple{int64(2)}, Value: []int{}},
		{Key: tuple.Tuple{int64(3)}, Value: []int{7}},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.DeserializeEntries(tuple.Tuple{int64(1)}, data)
	if err != nil {
		t.Fatal(err)
	}
	if entries[1].Value == nil || len(entries[1].Value) != 0 {
		t.Fatalf("empty list decoded as %#v", entries[1].Value)
	}
	_ = append(entries[0].Value, 99)
	_ = append(entries[1].Value, 98)
	if !slices.Equal(entries[2].Value, []int{7}) {
		t.Fatalf("an append overwrote the next list: %v", entries[2].Value)
	}
}
