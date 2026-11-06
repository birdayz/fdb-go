package recordlayer

import (
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/gomega"
)

func positionsEntry(positions ...int64) *IndexEntry {
	list := make(tuple.Tuple, len(positions))
	for i, p := range positions {
		list[i] = p
	}
	return &IndexEntry{Key: tuple.Tuple{"t", int64(1)}, Value: tuple.Tuple{list}}
}

func TestEntriesContainPhrase(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	cases := []struct {
		entries []*IndexEntry
		tokens  []string
		want    bool
	}{
		{[]*IndexEntry{positionsEntry(0, 5), positionsEntry(6)}, []string{"a", "b"}, true},
		{[]*IndexEntry{positionsEntry(0, 5), positionsEntry(7)}, []string{"a", "b"}, false},
		// A stop word widens the expected gap.
		{[]*IndexEntry{positionsEntry(3), positionsEntry(5)}, []string{"a", "", "b"}, true},
		{[]*IndexEntry{positionsEntry(3), positionsEntry(4)}, []string{"a", "", "b"}, false},
		// A token without positions is assumed present and widens the gap.
		{[]*IndexEntry{positionsEntry(1), positionsEntry(), positionsEntry(3)}, []string{"a", "b", "c"}, true},
		{[]*IndexEntry{positionsEntry(), positionsEntry()}, []string{"a", "b"}, true},
		{[]*IndexEntry{positionsEntry(0, 2, 4), positionsEntry(1, 3), positionsEntry(4)}, []string{"a", "b", "c"}, true},
		{[]*IndexEntry{positionsEntry(0, 2), positionsEntry(1, 3), positionsEntry(3)}, []string{"a", "b", "c"}, false},
	}
	for _, c := range cases {
		got, err := entriesContainPhrase(c.entries, c.tokens)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(Equal(c.want), "%v", c.tokens)
	}
}

func TestEntriesContainAllWithin(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	cases := []struct {
		entries     []*IndexEntry
		maxDistance int
		want        bool
	}{
		{[]*IndexEntry{positionsEntry(0, 10), positionsEntry(12)}, 2, true},
		{[]*IndexEntry{positionsEntry(0, 10), positionsEntry(13)}, 2, false},
		{[]*IndexEntry{positionsEntry(5), positionsEntry(1, 9), positionsEntry(3)}, 4, true},
		{[]*IndexEntry{positionsEntry(5), positionsEntry(0, 11), positionsEntry(3)}, 4, false},
		{[]*IndexEntry{positionsEntry(), positionsEntry(4)}, 0, true},
		{[]*IndexEntry{positionsEntry(), positionsEntry()}, 0, true},
	}
	for _, c := range cases {
		got, err := entriesContainAllWithin(c.entries, c.maxDistance)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(Equal(c.want))
	}
}

func TestTextComparisonEval(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	text := "The quick brown fox jumps over the lazy dog"
	cases := []struct {
		c    *TextComparison
		want *bool
	}{
		{NewTextComparison(TextContainsAll, "fox dog", "", ""), boolPtr(true)},
		{NewTextComparison(TextContainsAll, "fox cat", "", ""), boolPtr(false)},
		{NewTextComparison(TextContainsAny, "cat dog", "", ""), boolPtr(true)},
		{NewTextComparison(TextContainsPhrase, "quick brown fox", "", ""), boolPtr(true)},
		{NewTextComparison(TextContainsPhrase, "quick fox", "", ""), boolPtr(false)},
		{NewTextComparisonTokens(TextContainsPhrase, []string{"quick", "", "fox"}, "", ""), boolPtr(true)},
		{NewTextComparisonTokens(TextContainsPhrase, []string{"", ""}, "", ""), nil},
		{NewTextComparisonTokens(TextContainsPrefix, []string{"jum"}, "", ""), boolPtr(true)},
		{NewTextComparisonTokens(TextContainsAnyPrefix, []string{"ca", "do"}, "", ""), boolPtr(true)},
		{NewTextComparisonTokens(TextContainsAllPrefixes, []string{"qu", "la"}, "", ""), boolPtr(true)},
		{NewTextComparisonTokens(TextContainsAllPrefixes, []string{"qu", "ca"}, "", ""), boolPtr(false)},
		// quick and fox are two positions apart.
		{NewTextWithMaxDistanceComparison("quick fox", 1, "", ""), boolPtr(false)},
		{NewTextWithMaxDistanceComparison("quick fox", 2, "", ""), boolPtr(true)},
		{NewTextComparisonTokens(TextContainsAll, []string{""}, "", ""), nil},
	}
	for _, c := range cases {
		got, err := c.c.Eval(text)
		g.Expect(err).NotTo(HaveOccurred())
		if c.want == nil {
			g.Expect(got).To(BeNil(), c.c.String())
		} else {
			g.Expect(got).NotTo(BeNil(), c.c.String())
			g.Expect(*got).To(Equal(*c.want), c.c.String())
		}
	}
	got, err := NewTextComparison(TextContainsAll, "fox", "", "").Eval(nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(BeNil())
	_, err = NewTextComparison(TextContainsAll, "fox", "", "").Eval(int64(1))
	g.Expect(err).To(HaveOccurred())
}

func TestGuavaBloomFilterSizingAndRoundTrip(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	f, err := newGuavaBloomFilter(ProbableIntersectionDefaultExpectedResults, ProbableIntersectionDefaultFalsePositivePercentage)
	g.Expect(err).NotTo(HaveOccurred())
	// Guava 33: floor(-500 ln 0.01 / ln²2) = 4792 bits in 75 words, round(-log2 0.01) = 7 hashes.
	g.Expect(f.data).To(HaveLen(75))
	g.Expect(f.numHashFunctions).To(Equal(7))
	keys := []tuple.Tuple{{int64(1)}, {"a", int64(-7)}, {[]byte{1, 2}}, {nil}, {true, 2.5}}
	for _, k := range keys {
		g.Expect(f.put(k)).To(Succeed())
	}
	b := f.writeTo()
	g.Expect(b[:6]).To(Equal([]byte{1, 7, 0, 0, 0, 75}))
	r, err := readGuavaBloomFilter(b)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.writeTo()).To(Equal(b))
	for _, k := range keys {
		ok, err := r.mightContain(k)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ok).To(BeTrue())
	}
	_, err = readGuavaBloomFilter([]byte{})
	g.Expect(err).To(HaveOccurred())
	_, err = newGuavaBloomFilter(10, 1.0)
	g.Expect(err).To(HaveOccurred())
}

func TestMurmur3EmptyIsZero(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	h1, h2 := murmur3x64128(nil)
	g.Expect(h1).To(BeZero())
	g.Expect(h2).To(BeZero())
}

func longLists(lists ...[]int64) []CursorFunction[int64] {
	fns := make([]CursorFunction[int64], len(lists))
	for i, l := range lists {
		fns[i] = func(cont []byte) RecordCursor[int64] { return FromListWithContinuation(l, cont) }
	}
	return fns
}

func int64Key(v int64) (tuple.Tuple, error) { return tuple.Tuple{v}, nil }

// drain pages a merge cursor to its end, limit values per page, and returns
// the values in order.
func drain(t *testing.T, open func(cont []byte) (RecordCursor[int64], error), limit int) []int64 {
	g := NewWithT(t)
	var out []int64
	var cont []byte
	for pages := 0; pages < 100; pages++ {
		c, err := open(cont)
		g.Expect(err).NotTo(HaveOccurred())
		c = LimitRowsCursor(c, limit)
		for {
			r, err := c.OnNext(context.Background())
			g.Expect(err).NotTo(HaveOccurred())
			if !r.HasNext() {
				b, err := r.GetContinuation().ToBytes()
				g.Expect(err).NotTo(HaveOccurred())
				if b == nil {
					return out
				}
				cont = b
				break
			}
			out = append(out, r.GetValue())
		}
	}
	t.Fatal("cursor did not finish")
	return nil
}

func TestUnionCursorsResume(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	a, b, c := []int64{1, 3, 5, 7}, []int64{2, 3, 6}, []int64{3, 8}
	for _, limit := range []int{0, 1, 2} {
		ordered := drain(t, func(cont []byte) (RecordCursor[int64], error) {
			return Union(int64Key, false, longLists(a, b, c), cont)
		}, limit)
		g.Expect(ordered).To(Equal([]int64{1, 2, 3, 5, 6, 7, 8}))
		unordered := drain(t, func(cont []byte) (RecordCursor[int64], error) {
			return UnorderedUnion(longLists(a, b, c), cont)
		}, limit)
		g.Expect(unordered).To(Equal([]int64{1, 3, 5, 7, 2, 3, 6, 3, 8}))
		probable := drain(t, func(cont []byte) (RecordCursor[int64], error) {
			return ProbableIntersection(int64Key, longLists(a, b, c), 500, 0.01, cont)
		}, limit)
		g.Expect(probable).To(Equal([]int64{3}))
	}
	_, err := Union(int64Key, false, longLists(a), nil)
	g.Expect(err).To(HaveOccurred())
	_, err = UnorderedUnion(longLists(a, b), []byte{0xff})
	g.Expect(err).To(HaveOccurred())
}
