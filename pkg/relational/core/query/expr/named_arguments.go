package expr

import (
	"fmt"
	"strings"
	"unicode/utf16"

	"fdb.dev/pkg/relational/api"
)

// DuplicateArgumentNamesError is visitNamedOrUnnamedFunctionArgs' refusal of a
// named call that gives a name twice, nil when every name is distinct. Java
// lists each repeated name with its count in the iteration order of the
// HashMap Collectors.groupingBy builds over the unqualified Identifiers.
func DuplicateArgumentNamesError(names []string) error {
	counts := map[string]int{}
	var distinct []string
	for _, n := range names {
		if counts[n] == 0 {
			distinct = append(distinct, n)
		}
		counts[n]++
	}
	var repeated []string
	for _, n := range javaIdentifierHashMapOrder(distinct) {
		if counts[n] > 1 {
			repeated = append(repeated, fmt.Sprintf("%s=%d", n, counts[n]))
		}
	}
	if len(repeated) == 0 {
		return nil
	}
	return api.NewErrorf(api.ErrCodeSyntaxError, "argument name(s) used more than once%s", strings.Join(repeated, ","))
}

// javaIdentifierHashMapOrder is the iteration order of a default HashMap that
// received the unqualified Identifiers named by keys, in order.
func javaIdentifierHashMapOrder(keys []string) []string {
	buckets := make([][]string, 16)
	place := func(k string) {
		b := javaIdentifierSpread(k) & uint32(len(buckets)-1)
		buckets[b] = append(buckets[b], k)
	}
	for i, k := range keys {
		place(k)
		if float64(i+1) > 0.75*float64(len(buckets)) {
			old := buckets
			buckets = make([][]string, 2*len(old))
			for _, chain := range old {
				for _, e := range chain {
					place(e)
				}
			}
		}
	}
	var out []string
	for _, chain := range buckets {
		out = append(out, chain...)
	}
	return out
}

// javaIdentifierSpread is HashMap.hash of Identifier.hashCode,
// Objects.hash(name, List.of()), over String.hashCode's UTF-16 units.
func javaIdentifierSpread(name string) uint32 {
	var h uint32
	for _, u := range utf16.Encode([]rune(name)) {
		h = 31*h + uint32(u)
	}
	h = 31*(31+h) + 1
	return h ^ h>>16
}
