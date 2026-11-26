package rlcollections

import (
	"bytes"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

func TestNewRankedSetConfigDefaultsAndBounds(t *testing.T) {
	t.Parallel()
	sub := subspace.FromBytes([]byte("cfg"))
	if rs := NewRankedSet(sub, RankedSetConfig{}); rs.config.NLevels != RankedSetDefaultLevels || rs.config.HashFunction == nil {
		t.Fatalf("zero config = %+v", rs.config)
	}
	if rs := NewRankedSet(sub, RankedSetConfig{NLevels: -5}); rs.config.NLevels != RankedSetDefaultLevels {
		t.Fatalf("negative NLevels = %d", rs.config.NLevels)
	}
	if rs := NewRankedSet(sub, RankedSetConfig{NLevels: 100}); rs.config.NLevels != RankedSetMaxLevels {
		t.Fatalf("clamped NLevels = %d", rs.config.NLevels)
	}
	if rs := NewRankedSet(sub, RankedSetConfig{NLevels: 3}); rs.config.NLevels != 3 {
		t.Fatalf("explicit NLevels = %d", rs.config.NLevels)
	}
	rsDefault := NewRankedSet(sub, RankedSetConfig{})
	gotDefault, err := rsDefault.config.HashFunction([]byte("test"))
	if err != nil || gotDefault != JDKArrayHash([]byte("test")) {
		t.Fatalf("default hash = %d/%v, want %d", gotDefault, err, JDKArrayHash([]byte("test")))
	}
	rsCRC := NewRankedSet(sub, RankedSetConfig{HashFunction: PureHash(CRCHash)})
	gotCRC, err := rsCRC.config.HashFunction([]byte("test"))
	if err != nil || gotCRC != CRCHash([]byte("test")) {
		t.Fatalf("crc hash = %d/%v, want %d", gotCRC, err, CRCHash([]byte("test")))
	}
}

func TestRankedSetHashesMatchJava(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   []byte
		want int32
	}{
		{[]byte{}, 1},
		{[]byte{0}, 31},
		{[]byte{1}, 32},
		{[]byte{1, 2}, 994},
		{[]byte{0xFF}, 30},
	} {
		if got := JDKArrayHash(tc.in); got != tc.want {
			t.Fatalf("JDKArrayHash(%x) = %d, want %d", tc.in, got, tc.want)
		}
	}
	h1, h2 := CRCHash([]byte("test")), CRCHash([]byte("test"))
	if h1 != h2 || h1 == CRCHash([]byte("other")) {
		t.Fatal("CRCHash unexpected results")
	}
}

func TestRankedSetEncodeDecodeLong(t *testing.T) {
	t.Parallel()
	for _, v := range []int64{0, 1, 127, 128, 255, 256, 65535, 1<<31 - 1, 1<<63 - 1, -1, -128, -256, -1 << 31, -1 << 63} {
		encoded := rsEncodeLong(v)
		if len(encoded) != 8 {
			t.Fatalf("encoded len = %d", len(encoded))
		}
		got, err := rsDecodeLong(encoded)
		if err != nil || got != v {
			t.Fatalf("round-trip %d: got %d, err %v", v, got, err)
		}
	}
	if val, err := rsDecodeLong(nil); err != nil || val != 0 {
		t.Fatalf("nil decode: %d, %v", val, err)
	}
	for _, bad := range [][]byte{{1}, {1, 2, 3, 4, 5, 6, 7}} {
		if _, err := rsDecodeLong(bad); err == nil || !strings.Contains(err.Error(), "corrupted count value") {
			t.Fatalf("short %x: got %v", bad, err)
		}
	}
	if !bytes.Equal(rsEncodeLong(1), []byte{1, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("rsEncodeLong(1) = %x", rsEncodeLong(1))
	}
	if !bytes.Equal(rsEncodeLong(256), []byte{0, 1, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("rsEncodeLong(256) = %x", rsEncodeLong(256))
	}
}
