package metadata

import "math"

// javaHashMapOrder is the iteration order of a java.util.HashMap that
// received putAll(first) on an empty map and then put(k) for each of then:
// buckets by (h ^ h>>>16) & (cap-1), chains in insertion order, putAll
// presizing the table and each put past the threshold doubling it.
func javaHashMapOrder(first, then []string) []string {
	capacity := 16
	if len(first) > 0 {
		capacity = tableSizeFor(int(float32(len(first))/0.75 + 1))
	}
	var buckets [][]string
	size := 0
	rehash := func(n int) {
		old := buckets
		buckets = make([][]string, n)
		for _, chain := range old {
			for _, k := range chain {
				b := javaSpread(k) & (n - 1)
				buckets[b] = append(buckets[b], k)
			}
		}
	}
	rehash(capacity)
	put := func(k string) {
		b := javaSpread(k) & (len(buckets) - 1)
		for _, e := range buckets[b] {
			if e == k {
				return
			}
		}
		buckets[b] = append(buckets[b], k)
		size++
		if float64(size) > float64(len(buckets))*0.75 {
			rehash(len(buckets) * 2)
		}
	}
	for _, k := range first {
		put(k)
	}
	for _, k := range then {
		put(k)
	}
	out := make([]string, 0, size)
	for _, chain := range buckets {
		out = append(out, chain...)
	}
	return out
}

func tableSizeFor(c int) int {
	n := 1
	for n < c && n < math.MaxInt32/2 {
		n <<= 1
	}
	return n
}

func javaSpread(s string) int {
	var h uint32
	for _, c := range s {
		h = 31*h + uint32(c)
	}
	return int(h ^ h>>16)
}
