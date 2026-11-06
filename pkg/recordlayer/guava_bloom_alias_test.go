package recordlayer

import (
	"bytes"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

type bloomAliasKey []byte

func (k bloomAliasKey) FDBKey() fdb.Key { return fdb.Key(k) }

func TestBloomFunnelNormalizesUnsignedAndByteKeyAliases(t *testing.T) {
	t.Parallel()
	for _, values := range [][]any{
		{int64(7), uint(7), uint64(7)},
		{uint(math.MaxUint64), uint64(math.MaxUint64)},
		{[]byte{0, 1, 255}, fdb.Key{0, 1, 255}, bloomAliasKey{0, 1, 255}},
	} {
		want, err := bloomKeyFunnel(tuple.Tuple{values[0]})
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range values[1:] {
			got, err := bloomKeyFunnel(tuple.Tuple{value})
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("alias %T: %x want %x / %v", value, got, want, err)
			}
		}
	}
	nested := tuple.Tuple{"word"}
	got, err := bloomKeyFunnel(tuple.Tuple{nested})
	if err != nil {
		t.Fatal(err)
	}
	want := tuple.Tuple{nested}.Pack()
	if !bytes.Equal(got, want) || bytes.Equal(got, nested.Pack()) {
		t.Fatalf("nested tuple was flattened: %x, nested want %x", got, want)
	}
	raw, err := bloomKeyFunnel(tuple.Tuple{fdb.Key(nested.Pack())})
	if err != nil || bytes.Equal(got, raw) {
		t.Fatalf("nested tuple confused with byte-key alias: %x / %v", raw, err)
	}
}
