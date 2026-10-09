package vectorindex

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Golden values from Java 4.14.2.0 RandomHelpers.random(Tuple).
func TestSplittableRandomForKeyMatchesJava(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		pk   tuple.Tuple
		long int64
		dbl  float64
	}{
		{tuple.Tuple{int64(1)}, 353957409317239339, 0.4707595955137297},
		{tuple.Tuple{"abc", int64(42)}, 5546748198164881090, 0.7340220151691406},
	} {
		r := newSplittableRandomForKey(c.pk)
		if got := r.nextLong(); got != c.long {
			t.Errorf("%v nextLong = %d, want %d", c.pk, got, c.long)
		}
		if got := r.nextDouble(); got != c.dbl {
			t.Errorf("%v nextDouble = %v, want %v", c.pk, got, c.dbl)
		}
	}
}
