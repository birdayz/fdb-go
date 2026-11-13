package javahashmap

import (
	"math"
	"slices"
	"testing"
)

func TestOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		first, then []string
		want        []string
	}{
		{"empty", nil, nil, []string{}},
		// 'p' hashes to bucket 0 at capacity 16, bucket 16 at capacity 32.
		{"default capacity", nil, []string{"p", "a"}, []string{"p", "a"}},
		{"duplicate put", nil, []string{"p", "a", "p"}, []string{"p", "a"}},
		{
			"putAll presizes",
			[]string{"p", "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"},
			nil,
			[]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "p"},
		},
		{
			"put resizes", nil,
			[]string{"p", "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"},
			[]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "p"},
		},
		// These strings have the same Java hash and retain insertion order.
		{"collision chain", nil, []string{"BB", "Aa"}, []string{"BB", "Aa"}},
		{"UTF16", nil, []string{"\U0001F600", "\U0001D11E"}, []string{"\U0001D11E", "\U0001F600"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Order(tc.first, tc.then); !slices.Equal(got, tc.want) {
				t.Fatalf("Order(%v, %v) = %v, want %v", tc.first, tc.then, got, tc.want)
			}
		})
	}
}

func TestConstructionModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		keys, bulk, defaults []string
	}{
		{[]string{"A", "E", "B"}, []string{"A", "B", "E"}, []string{"A", "B", "E"}},
		{[]string{"H", "A", "B", "C", "D", "E"}, []string{"A", "B", "C", "D", "E", "H"}, []string{"A", "B", "C", "D", "E", "H"}},
		{[]string{"P", "A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K"}, []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "P"}, []string{"P", "A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K"}},
	} {
		if got := ExpectedSizeOrder(tc.keys); !slices.Equal(got, tc.keys) {
			t.Errorf("expected-size(%v) = %v", tc.keys, got)
		}
		if got := Order(nil, tc.keys); !slices.Equal(got, tc.defaults) {
			t.Errorf("default(%v) = %v", tc.keys, got)
		}
		if got := Order(tc.keys, nil); !slices.Equal(got, tc.bulk) {
			t.Errorf("bulk(%v) = %v, want %v", tc.keys, got, tc.bulk)
		}
	}
	for _, keys := range [][]string{nil, {"A"}, {"A", "B"}} {
		if got := ExpectedSizeOrder(keys); !slices.Equal(got, keys) {
			t.Errorf("tiny expected-size(%v) = %v", keys, got)
		}
	}
	// The expectation counts duplicates, even though insertion does not.
	if got := ExpectedSizeOrder([]string{"A", "E", "B", "A"}); !slices.Equal(got, []string{"A", "B", "E"}) {
		t.Errorf("duplicate-inclusive constructor size: %v", got)
	}
}

func TestCapacityTransitions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		size, request, capacity int
	}{
		{0, 1, 1},
		{1, 2, 2},
		{2, 3, 4},
		{3, 4, 4},
		{6, 8, 8},
		{12, 16, 16},
		{maximumCapacity, math.MaxInt32, maximumCapacity},
	} {
		if got := expectedCapacity(tc.size); got != tc.request {
			t.Errorf("expectedCapacity(%d) = %d, want %d", tc.size, got, tc.request)
		}
		if got := tableSizeFor(tc.request); got != tc.capacity {
			t.Errorf("tableSizeFor(%d) = %d, want %d", tc.request, got, tc.capacity)
		}
	}
	for _, request := range []int{1, 2, 4, 8, 16} {
		m := stringMap{threshold: request}
		m.put("A")
		want := request
		if request == 1 {
			want = 2 // first put exceeds the initial zero load threshold
		}
		if len(m.table) != want || m.threshold != want*3/4 {
			t.Fatalf("request %d: capacity=%d threshold=%d", request, len(m.table), m.threshold)
		}
		checkMap(t, &m)
	}
	m := stringMap{}
	for _, key := range []string{"P", "A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K"} {
		m.put(key)
		checkMap(t, &m)
	}
	if len(m.table) != 16 || m.size != 12 {
		t.Fatalf("12th default insertion: capacity=%d size=%d", len(m.table), m.size)
	}
	unchangedPut(t, &m, "P")
	m.put("L")
	if len(m.table) != 32 || m.threshold != 24 || m.size != 13 {
		t.Fatalf("13th default insertion: capacity=%d threshold=%d size=%d", len(m.table), m.threshold, m.size)
	}
	unchangedPut(t, &m, "K")
	checkMap(t, &m)
}

func collisionKeys() []string {
	return []string{
		"AaAaAaAa", "AaAaAaBB", "AaAaBBAa", "AaAaBBBB",
		"AaBBAaAa", "AaBBAaBB", "AaBBBBAa", "AaBBBBBB",
		"BBAaAaAa", "BBAaAaBB", "BBAaBBAa", "BBAaBBBB",
		"BBBBAaAa", "BBBBAaBB", "BBBBBBAa", "BBBBBBBB",
	}
}

func TestCollisionGrowthAndTreeOrder(t *testing.T) {
	t.Parallel()
	keys := collisionKeys()
	for _, key := range keys {
		if javaSpread(key) != javaSpread(keys[0]) {
			t.Fatalf("fixture is not a collision: %q", key)
		}
	}
	for _, request := range []int{0, 32, 64} {
		m := stringMap{threshold: request}
		for i, key := range keys {
			m.put(key)
			checkMap(t, &m)
			count := i + 1
			wantCapacity, treeAt := 64, 9
			if request == 0 {
				treeAt = 11
				if count <= 8 {
					wantCapacity = 16
				} else if count == 9 {
					wantCapacity = 32
				}
			} else if request == 32 {
				treeAt = 10
				if count <= 8 {
					wantCapacity = 32
				}
			}
			head := m.table[int(javaSpread(key))&(len(m.table)-1)]
			if len(m.table) != wantCapacity || head.tree != (count >= treeAt) {
				t.Fatalf("request=%d count=%d: capacity=%d tree=%v", request, count, len(m.table), head.tree)
			}
			unchangedPut(t, &m, key)
			unchangedPut(t, &m, head.key)
		}
	}
	// Collision growth moves P into a later bucket in both map stages.
	nineAndP := append(slices.Clone(keys[:9]), "P")
	if got := ExpectedSizeOrder(Order(nil, nineAndP)); !slices.Equal(got, nineAndP) {
		t.Fatalf("nine colliders plus P: %v", got)
	}
	// Full-chain expectations follow the insertion/root-front rules; complete
	// engine-byte parity is checked separately by the metadata conformance tests.
	want := append([]string{keys[3], keys[0], keys[1], keys[2]}, keys[4:]...)
	got := Order(nil, keys)
	if !slices.Equal(got, want) || !slices.Equal(ExpectedSizeOrder(got), want) {
		t.Fatalf("forward tree: first=%v second=%v want=%v", got, ExpectedSizeOrder(got), want)
	}
	reverse := slices.Clone(keys)
	slices.Reverse(reverse)
	want = append([]string{keys[12], keys[15], keys[14], keys[13]}, reverse[4:]...)
	if got := Order(nil, reverse); !slices.Equal(got, want) {
		t.Fatalf("mirrored tree: %v, want %v", got, want)
	}
	mirrored := stringMap{}
	for _, key := range reverse {
		mirrored.put(key)
		checkMap(t, &mirrored)
	}
}

func TestMirroredBentPaths(t *testing.T) {
	t.Parallel()
	keys := collisionKeys()
	for _, mirrored := range []bool{false, true} {
		m := stringMap{threshold: 64}
		// The first three keys form a bent insertion path during treeify;
		// the mirrored permutation exercises the opposite pair of rotations.
		for _, index := range []int{0, 2, 1, 3, 5, 4, 6, 8, 7, 9, 11, 10, 12, 14, 13, 15} {
			if mirrored {
				index = 15 - index
			}
			m.put(keys[index])
			checkMap(t, &m)
		}
		if m.size != 16 || !m.table[int(javaSpread(keys[0]))&63].tree {
			t.Fatal("bent-path fixture did not populate a tree")
		}
	}
}

func TestTreeParentChainInsertion(t *testing.T) {
	t.Parallel()
	keys := collisionKeys()
	m := stringMap{threshold: 64}
	for _, i := range []int{0, 2, 4, 6, 8, 10, 12, 14, 15} {
		m.put(keys[i])
		checkMap(t, &m)
	}
	before := m.order()
	want := slices.Insert(slices.Clone(before), slices.Index(before, keys[0])+1, keys[1])
	m.put(keys[1])
	if got := m.order(); !slices.Equal(got, want) {
		t.Fatalf("new leaf must follow its tree parent, not chain tail: %v, want %v", got, want)
	}
	checkMap(t, &m)
	unchangedPut(t, &m, m.table[int(javaSpread(keys[0]))&63].key)
	unchangedPut(t, &m, keys[0])
}

func TestTreeSplit(t *testing.T) {
	t.Parallel()
	// All these identifiers hash to bucket 32 at capacity 64. Successive
	// doubled letters add 64 to the hash, alternating the resize partition.
	bothSix := []string{"AA", "CC", "EE", "GG", "II", "KK", "MM", "OO", "QQ", "SS", "UU", "WW"}
	for _, tc := range []struct {
		name      string
		keys      []string
		low, high int
	}{
		{"both untreeify", bothSix, 6, 6},
		{"low untreeifies", append(slices.Clone(bothSix), "cc"), 6, 7},
		{"high untreeifies", append(slices.Clone(bothSix), "YY"), 7, 6},
		{"both rebuild", append(slices.Clone(bothSix), "YY", "cc"), 7, 7},
		{"unsplit high tree", collisionKeys(), 0, 16},
		{"unsplit low tree", []string{"AA", "EE", "II", "MM", "QQ", "UU", "YY", "aa", "ee"}, 9, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := stringMap{threshold: 64}
			bucket := int(javaSpread(tc.keys[0]) & 63)
			counts := [2]int{}
			for _, key := range tc.keys {
				if int(javaSpread(key)&63) != bucket {
					t.Fatalf("split fixture misses bucket: %q", key)
				}
				counts[int(javaSpread(key)>>6)&1]++
				m.put(key)
				checkMap(t, &m)
			}
			if counts != [2]int{tc.low, tc.high} || !m.table[bucket].tree {
				t.Fatalf("fixture must start with a populated tree: counts=%v", counts)
			}
			root := m.table[bucket]
			beforeNodes := snapshot(&m)
			var partition [2][]string
			for e := root; e != nil; e = e.next {
				side := int(e.hash>>6) & 1
				partition[side] = append(partition[side], e.key)
			}
			// Distinct one-letter fillers avoid the collision bucket. The put
			// crossing size 48, rather than an explicit resize call, splits it.
			for _, c := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvw" {
				if m.size == 49 {
					break
				}
				key := string(c)
				if int(javaSpread(key)&63) == bucket {
					continue
				}
				m.put(key)
				checkMap(t, &m)
			}
			if m.size != 49 || len(m.table) != 128 {
				t.Fatalf("resize was not reached: size=%d capacity=%d", m.size, len(m.table))
			}
			for side, expected := range partition {
				head := m.table[bucket+side*64]
				var got []string
				for e := head; e != nil; e = e.next {
					got = append(got, e.key)
				}
				if len(expected) == 0 {
					if head != nil {
						t.Fatal("empty partition became populated")
					}
					continue
				}
				if head.tree != (len(expected) > 6) {
					t.Fatalf("side %d: tree=%v count=%d", side, head.tree, len(expected))
				}
				if !head.tree && !slices.Equal(got, expected) {
					t.Fatalf("untreeify must preserve stable chain: got %v want %v", got, expected)
				}
				if head.tree && tc.low != 0 && tc.high != 0 {
					// Rebuilding can only move the root; all other chain links
					// retain their stable relative order.
					rest := slices.Delete(slices.Clone(expected), slices.Index(expected, head.key), slices.Index(expected, head.key)+1)
					if !slices.Equal(got[1:], rest) {
						t.Fatalf("tree split changed nonroot order: %v from %v", got, expected)
					}
				}
			}
			if tc.low == 0 || tc.high == 0 {
				surviving := bucket
				if tc.low == 0 {
					surviving += 64
				}
				if m.table[surviving] != root {
					t.Fatal("unsplit tree root changed")
				}
				for e, state := range beforeNodes {
					if *e != state {
						t.Fatalf("unsplit tree links changed at %q", e.key)
					}
				}
				key := "ii"
				if tc.low == 0 {
					key = "I"
				}
				if int(javaSpread(key)&127) != surviving {
					t.Fatalf("post-resize insert misses surviving tree: %q", key)
				}
				m.put(key)
				checkMap(t, &m)
				unchangedPut(t, &m, key)
			} else {
				for _, key := range []string{"ee", "gg"} {
					if int(javaSpread(key)&63) != bucket {
						t.Fatalf("post-split insert misses fixture bucket: %q", key)
					}
					m.put(key)
					checkMap(t, &m)
					unchangedPut(t, &m, key)
				}
			}
		})
	}
}

func TestSignedHashesAndUTF16(t *testing.T) {
	t.Parallel()
	keys := collisionKeys()
	negative := javaSpread(keys[0])
	if int32(negative) >= 0 || javaSpread("I")&63 != negative&63 {
		t.Fatal("signed-hash fixture precondition failed")
	}
	if compareKey(negative, keys[0], &entry{hash: javaSpread("I"), key: "I"}) >= 0 ||
		compareKey(javaSpread("I"), "I", &entry{hash: negative, key: keys[0]}) <= 0 {
		t.Fatal("tree hash comparison must be signed")
	}
	m := stringMap{threshold: 64}
	m.put("I")
	for _, key := range keys[:8] {
		m.put(key)
		checkMap(t, &m)
	}
	rightmost := m.table[int(negative)&63]
	for rightmost.right != nil {
		rightmost = rightmost.right
	}
	if rightmost.key != "I" {
		t.Fatal("positive hash must follow negative hashes in the actual tree")
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"\U00010000", "\uE000", -1},
		{"\uE000", "\U00010000", 1},
		{"", "", 0},
		{"A", "AA", -1},
		{"AA", "A", 1},
		{"\U0001F600", "\U0001F600", 0},
		{"e\u0301", "é", -1},
	} {
		if got := compareUTF16(tc.a, tc.b); got != tc.want {
			t.Errorf("compareUTF16(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	// Arithmetic over the two surrogate units, independent of the hash helper.
	for _, tc := range []struct {
		key       string
		high, low uint32
	}{
		{"\U0001F600", 0xD83D, 0xDE00}, {"\U0001D11E", 0xD834, 0xDD1E},
	} {
		raw := 31*tc.high + tc.low
		if got := javaSpread(tc.key); got != raw^(raw>>16) {
			t.Errorf("surrogate hash %q: %08x", tc.key, got)
		}
	}
	if javaSpread("") != 0 || javaSpread("Aa") != 2112 || javaSpread("BB") != 2112 {
		t.Fatal("basic hash arithmetic changed")
	}
}

func snapshot(m *stringMap) map[*entry]entry {
	out := make(map[*entry]entry, m.size)
	for _, head := range m.table {
		for e := head; e != nil; e = e.next {
			out[e] = *e
		}
	}
	return out
}

func unchangedPut(t *testing.T, m *stringMap, key string) {
	t.Helper()
	before, nodes := m.order(), snapshot(m)
	capacity, threshold, size := len(m.table), m.threshold, m.size
	m.put(key)
	if len(m.table) != capacity || m.threshold != threshold || m.size != size || !slices.Equal(before, m.order()) {
		t.Fatalf("duplicate %q structurally changed the map", key)
	}
	for e, state := range nodes {
		if *e != state {
			t.Fatalf("duplicate %q changed links or colors at %q", key, e.key)
		}
	}
}

func checkMap(t *testing.T, m *stringMap) {
	t.Helper()
	seen := make(map[string]bool, m.size)
	for bucket, head := range m.table {
		chain := make(map[*entry]bool)
		var previous *entry
		for e := head; e != nil; e = e.next {
			if chain[e] || seen[e.key] {
				t.Fatalf("duplicate/cyclic chain node %q", e.key)
			}
			chain[e], seen[e.key] = true, true
			if int(e.hash&uint32(len(m.table)-1)) != bucket || e.hash != javaSpread(e.key) {
				t.Fatalf("wrong bucket/hash for %q", e.key)
			}
			if e.tree != head.tree {
				t.Fatal("mixed tree/plain chain")
			}
			if e.tree {
				if e.prev != previous {
					t.Fatalf("broken previous link at %q", e.key)
				}
			} else if e.prev != nil || e.parent != nil || e.left != nil || e.right != nil || e.red {
				t.Fatalf("plain node retains tree state: %q", e.key)
			}
			previous = e
		}
		if head == nil || !head.tree {
			continue
		}
		if head.parent != nil || head.red {
			t.Fatal("tree root must be black and at the chain front")
		}
		treeSeen := make(map[*entry]bool)
		var visit func(*entry, *entry, *entry, *entry) int
		visit = func(e, parent, lower, upper *entry) int {
			if e == nil {
				return 1
			}
			if treeSeen[e] || !chain[e] || e.parent != parent {
				t.Fatalf("tree membership/parent failure at %q", e.key)
			}
			treeSeen[e] = true
			if lower != nil && compareKey(e.hash, e.key, lower) <= 0 || upper != nil && compareKey(e.hash, e.key, upper) >= 0 {
				t.Fatalf("tree ordering failure at %q", e.key)
			}
			if e.red && ((e.left != nil && e.left.red) || (e.right != nil && e.right.red)) {
				t.Fatalf("red-red edge at %q", e.key)
			}
			left, right := visit(e.left, e, lower, e), visit(e.right, e, e, upper)
			if left != right {
				t.Fatalf("unequal black heights at %q: %d/%d", e.key, left, right)
			}
			if !e.red {
				left++
			}
			return left
		}
		visit(head, nil, nil, nil)
		if len(treeSeen) != len(chain) {
			t.Fatal("tree and chain memberships differ")
		}
	}
	if len(seen) != m.size {
		t.Fatalf("iteration population=%d, size=%d", len(seen), m.size)
	}
}
