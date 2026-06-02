package bench

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"testing"

	cgotuple "github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	gotuple "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// Tuple-codec byte-identity differential vs libfdb_c — RFC-060.
//
// The tuple layer encodes FDB keys; key encoding is the wire hard line (Go and C clients share
// a cluster and read/write each other's records, so the packed bytes MUST be identical). The
// pure-Go codec (pkg/fdbgo/fdb/tuple) is a near-verbatim port of Apple's Go binding whose core
// encode/decode is byte-identical by inspection, BUT it adds go-only hot-path helpers
// (PackWithPrefix/Pack1WithPrefix/Pack1ConcatWithPrefix/PackConcatWithPrefix/Packer.AppendInto/
// packerPool) absent from libfdb_c that build the actual index/record keys. Those helpers had
// ZERO cross-codec coverage. These tests prove gotuple.Pack() == cgotuple.Pack() across every
// type code and boundary, prove the go-only helpers match the canonical cgotuple.Pack(), and
// cross-check decode in both directions.
//
// cgotuple.Pack() is itself pinned to the cross-language tuples.golden vectors (its
// TestTuplePacking), so the Pack() differential is transitively anchored to golden on the
// subset golden exercises. golden's generators are narrow (Int63()/NormFloat64(), no
// versionstamps), so the explicit boundary battery — not golden — carries the proof. Unpack and
// PackWithVersionstamp have NO golden backstop (go-vs-cgo only); the helper cases are immune by
// construction (compared to golden-pinned cgo Pack(), and cgo has no such helper code).

// assertPackEqual builds the same logical tuple in both codecs and asserts byte-identical
// Pack() output. Works for NaN too — both sides see the same float value, so the encoded bytes
// match even though NaN != NaN (we never compare decoded floats by ==).
func assertPackEqual(t *testing.T, name string, g gotuple.Tuple, c cgotuple.Tuple) {
	t.Helper()
	gb := g.Pack()
	cb := c.Pack()
	if !bytes.Equal(gb, cb) {
		t.Fatalf("%s: Pack mismatch\n go=%x\ncgo=%x", name, gb, cb)
	}
}

// primitivePackBattery returns labeled values whose Go type is IDENTICAL in both tuple packages
// (int/int64/uint64, *big.Int, float32/float64, string/[]byte, bool, nil) — so gotuple.Tuple{v}
// and cgotuple.Tuple{v} are built from the same v.
func primitivePackBattery() []struct {
	name string
	v    any
} {
	var b []struct {
		name string
		v    any
	}
	add := func(name string, v any) {
		b = append(b, struct {
			name string
			v    any
		}{name, v})
	}

	// Integers: 0, ±1, and every sizeLimits[n] size-class boundary (2^(8n)-1 and the adjacent
	// 2^(8n)), positive AND negative, for n=1..7 (n=8 overflows int64 → handled explicitly).
	add("int_0", int64(0))
	add("int_1", int64(1))
	add("int_-1", int64(-1))
	for n := uint(1); n <= 7; n++ {
		lim := int64(1)<<(8*n) - 1 // 2^(8n)-1
		add(fmt.Sprintf("int_2^%d-1", 8*n), lim)
		add(fmt.Sprintf("int_2^%d", 8*n), lim+1)
		add(fmt.Sprintf("int_-(2^%d-1)", 8*n), -lim)
		add(fmt.Sprintf("int_-2^%d", 8*n), -(lim + 1))
	}
	// 8-byte size class (the int64/uint64 extremes + the decodeInt high-bit→only-uint64 path).
	add("int_MaxInt64", int64(math.MaxInt64))
	add("int_MinInt64", int64(math.MinInt64))
	add("uint_MaxUint64", uint64(math.MaxUint64))
	add("uint_1<<63", uint64(1)<<63)     // high bit set, not max
	add("uint_1<<63+1", uint64(1)<<63+1) // high bit set
	add("int_native_42", int(42))        // the `int` type code path (→ int64)
	add("uint_native_42", uint(42))      // the `uint` type code path (→ uint64)

	// *big.Int: small (decode→int64), >8-byte pos/neg, exactly-8-byte negative (decodeBigInt
	// length=8 fallthrough), MinInt64-as-bigint (minInt64BigInt round-trip), and a negative
	// magnitude that leads with 0xff (the zero-fill loop in encodeBigInt).
	add("big_0", big.NewInt(0))
	add("big_42", big.NewInt(42))
	add("big_-42", big.NewInt(-42))
	add("big_2^100", new(big.Int).Lsh(big.NewInt(1), 100))
	add("big_-2^100", new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 100)))
	// Large magnitude (125 bytes) — exercises a length-prefix byte well beyond 0x0d and the
	// `length ^ 0xff` negative-length encoding (FDB-C++ dev nit).
	add("big_2^1000", new(big.Int).Lsh(big.NewInt(1), 1000))
	add("big_-2^1000", new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 1000)))
	add("big_MinInt64", big.NewInt(math.MinInt64))
	// 8-byte negative (magnitude in [2^56, 2^64)) routing through decodeBigInt's length=8.
	add("big_-(2^60)", new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 60)))
	// Negative magnitude leading with 0xff: -(0xff * 2^64) → transformed bytes lead with 0x00.
	add("big_-0xff*2^64", new(big.Int).Neg(new(big.Int).Mul(big.NewInt(0xff), new(big.Int).Lsh(big.NewInt(1), 64))))

	// float32: ±0 (distinct sign bit), ±Inf, fixed-bit-pattern NaN (NOT math.NaN()), smallest
	// subnormal, ±1.0 (all-bytes-flip vs sign-bit-only branch), ±MaxFloat32.
	add("f32_+0", float32(0))
	add("f32_-0", math.Float32frombits(0x80000000))
	add("f32_+Inf", math.Float32frombits(0x7f800000))
	add("f32_-Inf", math.Float32frombits(0xff800000))
	add("f32_NaN", math.Float32frombits(0x7fc00001))
	add("f32_subnormal", math.SmallestNonzeroFloat32)
	add("f32_1.0", float32(1.0))
	add("f32_-1.0", float32(-1.0))
	add("f32_max", float32(math.MaxFloat32))
	add("f32_-max", float32(-math.MaxFloat32))

	// float64: same battery.
	add("f64_+0", float64(0))
	add("f64_-0", math.Float64frombits(0x8000000000000000))
	add("f64_+Inf", math.Float64frombits(0x7ff0000000000000))
	add("f64_-Inf", math.Float64frombits(0xfff0000000000000))
	add("f64_NaN", math.Float64frombits(0x7ff8000000000001))
	add("f64_subnormal", math.SmallestNonzeroFloat64)
	add("f64_1.0", float64(1.0))
	add("f64_-1.0", float64(-1.0))
	add("f64_max", math.MaxFloat64)
	add("f64_-max", -math.MaxFloat64)

	// strings / bytes: empty, embedded null (0x00→0x00 0xFF escape), trailing null, 0xFF bytes,
	// the literal 0x00 0xFF sequence, large.
	add("str_empty", "")
	add("str_hello", "hello")
	add("str_embedded_null", "a\x00b")
	add("str_trailing_null", "a\x00")
	add("str_ff", "\xff\xff")
	add("str_00ff", "\x00\xff")
	add("bytes_empty", []byte{})
	add("bytes_embedded_null", []byte{0x00, 0xff, 0x00})
	add("bytes_ff", []byte{0xff, 0xff})
	add("bytes_00ff", []byte{0x00, 0xff})
	add("bytes_large", bytes.Repeat([]byte("abcd"), 64))

	// bool, nil.
	add("bool_true", true)
	add("bool_false", false)
	add("nil_toplevel", nil)

	return b
}

func TestDifferential_TuplePack(t *testing.T) {
	t.Parallel()

	for _, tc := range primitivePackBattery() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertPackEqual(t, tc.name, gotuple.Tuple{tc.v}, cgotuple.Tuple{tc.v})
		})
	}

	// UUID (identical [16]byte under both packages, via conversion).
	t.Run("UUID", func(t *testing.T) {
		t.Parallel()
		gu := gotuple.UUID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
		assertPackEqual(t, "UUID", gotuple.Tuple{gu}, cgotuple.Tuple{cgotuple.UUID(gu)})
	})

	// Complete Versionstamp (NOT incomplete — Pack() rejects incomplete; that path is #4).
	t.Run("Versionstamp_complete", func(t *testing.T) {
		t.Parallel()
		tv := [10]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09}
		gv := gotuple.Versionstamp{TransactionVersion: tv, UserVersion: 620}
		cv := cgotuple.Versionstamp{TransactionVersion: tv, UserVersion: 620}
		assertPackEqual(t, "Versionstamp_complete", gotuple.Tuple{gv}, cgotuple.Tuple{cv})
	})

	// KeyConvertible element: go encodes fdb.KeyConvertible as bytesCode; cgo lacks the
	// identical interface, so supply the equivalent []byte (both → bytesCode + "xyz").
	t.Run("KeyConvertible", func(t *testing.T) {
		t.Parallel()
		assertPackEqual(t, "KeyConvertible", gotuple.Tuple{gofdb.Key("xyz")}, cgotuple.Tuple{[]byte("xyz")})
	})

	// Nested tuples: empty nested, nested-with-nil (extra 0xFF), top-level vs nested nil, deep.
	t.Run("nested_empty", func(t *testing.T) {
		t.Parallel()
		assertPackEqual(t, "nested_empty", gotuple.Tuple{gotuple.Tuple{}}, cgotuple.Tuple{cgotuple.Tuple{}})
	})
	t.Run("nested_with_nil", func(t *testing.T) {
		t.Parallel()
		assertPackEqual(t, "nested_with_nil",
			gotuple.Tuple{gotuple.Tuple{nil, int64(1), nil}},
			cgotuple.Tuple{cgotuple.Tuple{nil, int64(1), nil}})
	})
	t.Run("nested_vs_toplevel_nil", func(t *testing.T) {
		t.Parallel()
		assertPackEqual(t, "nested_vs_toplevel_nil",
			gotuple.Tuple{nil, gotuple.Tuple{nil}, nil},
			cgotuple.Tuple{nil, cgotuple.Tuple{nil}, nil})
	})
	t.Run("nested_deep", func(t *testing.T) {
		t.Parallel()
		assertPackEqual(t, "nested_deep",
			gotuple.Tuple{gotuple.Tuple{gotuple.Tuple{int64(1), "x"}, "y"}, "z"},
			cgotuple.Tuple{cgotuple.Tuple{cgotuple.Tuple{int64(1), "x"}, "y"}, "z"})
	})

	// Kitchen-sink multi-element tuple mixing many type codes.
	t.Run("multi_element", func(t *testing.T) {
		t.Parallel()
		gu := gotuple.UUID{0x11, 0x00, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x00, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
		assertPackEqual(t, "multi_element",
			gotuple.Tuple{gu, "foobarbaz", int64(1234), nil, true, []byte{0x00, 0xff}, float64(-1.5), gotuple.Tuple{int64(7)}},
			cgotuple.Tuple{cgotuple.UUID(gu), "foobarbaz", int64(1234), nil, true, []byte{0x00, 0xff}, float64(-1.5), cgotuple.Tuple{int64(7)}})
	})
}

// TestDifferential_TupleUnpackCross proves decode parity in BOTH directions: a key packed by one
// codec unpacks to the same logical tuple under the other. go-vs-cgo only (no golden backstop
// for decode — stated in RFC-060). NaN is compared by bits, not ==.
func TestDifferential_TupleUnpackCross(t *testing.T) {
	t.Parallel()

	for _, tc := range primitivePackBattery() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// go packs → cgo unpacks; cgo packs → go unpacks. Both must succeed and the two
			// decoded forms must be byte-identical when re-packed (normalization-agnostic).
			goPacked := gotuple.Tuple{tc.v}.Pack()
			cgoPacked := cgotuple.Tuple{tc.v}.Pack()

			cgoFromGo, err := cgotuple.Unpack(goPacked)
			if err != nil {
				t.Fatalf("%s: cgo.Unpack(go.Pack) error: %v", tc.name, err)
			}
			goFromCgo, err := gotuple.Unpack(cgoPacked)
			if err != nil {
				t.Fatalf("%s: go.Unpack(cgo.Pack) error: %v", tc.name, err)
			}
			// Re-pack the decoded tuples and compare bytes (avoids element-wise == on NaN and
			// the int64/uint64/[]byte normalizations, which both codecs apply identically).
			if !bytes.Equal(cgoFromGo.Pack(), goPacked) {
				t.Fatalf("%s: cgo.Unpack(go.Pack).Pack != go.Pack\n got=%x\nwant=%x", tc.name, cgoFromGo.Pack(), goPacked)
			}
			if !bytes.Equal(goFromCgo.Pack(), cgoPacked) {
				t.Fatalf("%s: go.Unpack(cgo.Pack).Pack != cgo.Pack\n got=%x\nwant=%x", tc.name, goFromCgo.Pack(), cgoPacked)
			}
		})
	}
}

// TestDifferential_TuplePackHelpers is the core of RFC-060: the go-only hot-path helpers
// (absent from libfdb_c) must produce bytes identical to the canonical cgotuple.Pack() with the
// prefix prepended. These build the actual index/record keys on the wire.
func TestDifferential_TuplePackHelpers(t *testing.T) {
	t.Parallel()

	prefixes := [][]byte{
		nil,
		{},
		[]byte("p"),
		[]byte("a_long_subspace_prefix/with/slashes"),
		{0x00, 0xff, 0x00}, // prefix containing the escape bytes
	}
	// A representative element set (the full battery is covered by TestDifferential_TuplePack;
	// here we vary the HELPER + prefix, not re-enumerate every type code).
	elems := []any{int64(-(1 << 40)), uint64(math.MaxUint64), "a\x00b", []byte{0xff, 0x00}, float64(-1.5), true, nil}

	for pi, prefix := range prefixes {
		for ei, elem := range elems {
			pi, ei, prefix, elem := pi, ei, prefix, elem
			name := fmt.Sprintf("Pack1WithPrefix_p%d_e%d", pi, ei)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				got := gotuple.Pack1WithPrefix(prefix, elem)
				want := append(append([]byte{}, prefix...), cgotuple.Tuple{elem}.Pack()...)
				if !bytes.Equal(got, want) {
					t.Fatalf("%s: got=%x want=%x", name, got, want)
				}
			})
		}
		// PackWithPrefix over a multi-element tuple.
		name := fmt.Sprintf("PackWithPrefix_p%d", pi)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := gotuple.Tuple{int64(7), "x", []byte{0x00}}.PackWithPrefix(prefix)
			want := append(append([]byte{}, prefix...), cgotuple.Tuple{int64(7), "x", []byte{0x00}}.Pack()...)
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: got=%x want=%x", name, got, want)
			}
		})
		// Pack1ConcatWithPrefix: prefix || pack(elem) || pack(suffix...). Equals prefix ||
		// cgo.Pack(elem) || cgo.Pack(suffix) because top-level tuple encoding is concatenative.
		nameC := fmt.Sprintf("Pack1ConcatWithPrefix_p%d", pi)
		t.Run(nameC, func(t *testing.T) {
			t.Parallel()
			suffix := gotuple.Tuple{int64(2), int64(3)}
			got := gotuple.Pack1ConcatWithPrefix(prefix, int64(1), suffix)
			want := append(append([]byte{}, prefix...), cgotuple.Tuple{int64(1)}.Pack()...)
			want = append(want, cgotuple.Tuple{int64(2), int64(3)}.Pack()...)
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: got=%x want=%x", nameC, got, want)
			}
		})
		// PackConcatWithPrefix: prefix || join(pack(each tuple)).
		nameP := fmt.Sprintf("PackConcatWithPrefix_p%d", pi)
		t.Run(nameP, func(t *testing.T) {
			t.Parallel()
			t1 := gotuple.Tuple{int64(1), "a"}
			t2 := gotuple.Tuple{int64(2), "b"}
			got := gotuple.PackConcatWithPrefix(prefix, t1, t2)
			want := append(append([]byte{}, prefix...), cgotuple.Tuple{int64(1), "a"}.Pack()...)
			want = append(want, cgotuple.Tuple{int64(2), "b"}.Pack()...)
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: got=%x want=%x", nameP, got, want)
			}
		})
	}

	// Packer.AppendInto: both the in-place (spare cap) and grow/realloc (cap forces a new
	// backing array) paths must append bytes identical to prefix || cgo.Pack().
	t.Run("AppendInto_inplace_and_realloc", func(t *testing.T) {
		t.Parallel()
		check := func(label string, buf []byte) {
			start := len(buf)
			pk := gotuple.GetPacker()
			pk.EncodeElement(int64(99))
			pk.EncodeElement("tail")
			seg := pk.AppendInto(&buf, []byte("PFX"))
			gotuple.PutPacker(pk)
			want := append([]byte("PFX"), cgotuple.Tuple{int64(99), "tail"}.Pack()...)
			if !bytes.Equal(seg, want) {
				t.Fatalf("%s seg: got=%x want=%x", label, seg, want)
			}
			if !bytes.Equal(buf[start:], want) {
				t.Fatalf("%s buf tail: got=%x want=%x", label, buf[start:], want)
			}
		}
		check("spare_cap", make([]byte, 3, 256)) // plenty of cap → in-place
		check("force_realloc", make([]byte, 3))  // cap==len → grow path
	})

	// packerPool buffer-reset correctness: pack a non-trivial tuple through a pooled helper, let
	// it return to the pool, then pack a different tuple through the same pool — the second
	// result must equal the canonical cgo Pack() with no leftover bytes from the first. This
	// pins the buf[:0] reset on the hot path (a broken reset would prepend the first pack's
	// bytes to the second). Note: versionstampPos cannot be left stale via the pooled helpers —
	// they call encodeTuple(versionstamps=false), which rejects an incomplete versionstamp, and
	// a complete versionstamp never sets versionstampPos; the field is also reset to -1 on each
	// Get. So this is a general residue check, not a versionstampPos-staleness probe.
	t.Run("pool_reuse_buffer_reset", func(t *testing.T) {
		t.Parallel()
		tv := [10]byte{0x09, 0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, 0x00}
		gvs := gotuple.Versionstamp{TransactionVersion: tv, UserVersion: 7}
		// First pack (a complete versionstamp + string — a non-trivial buffer) through the pool.
		_ = gotuple.Tuple{gvs, "first"}.PackWithPrefix([]byte("A"))
		// Second pack (different, plain) through the same pool — must carry no residue.
		got := gotuple.Tuple{int64(123), "second"}.PackWithPrefix([]byte("B"))
		want := append([]byte("B"), cgotuple.Tuple{int64(123), "second"}.Pack()...)
		if !bytes.Equal(got, want) {
			t.Fatalf("pool reuse residue: got=%x want=%x", got, want)
		}
	})
}

// TestDifferential_TuplePackVersionstamp proves gotuple.PackWithVersionstamp ==
// cgotuple.PackWithVersionstamp byte-for-byte, including the 4-byte little-endian offset suffix,
// for incomplete-versionstamp tuples at several positions. Both clients are pinned at API 730
// (≥520), so only the 4-byte suffix is reachable; the API<520 2-byte branch is out of
// differential scope (covered by go-internal versionstamp tests). No golden backstop for this
// path (go-vs-cgo only — RFC-060).
func TestDifferential_TuplePackVersionstamp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mkGo   func() gotuple.Tuple
		mkCgo  func() cgotuple.Tuple
		prefix []byte
	}{
		{
			"incomplete_at_0",
			func() gotuple.Tuple { return gotuple.Tuple{gotuple.IncompleteVersionstamp(0)} },
			func() cgotuple.Tuple { return cgotuple.Tuple{cgotuple.IncompleteVersionstamp(0)} },
			nil,
		},
		{
			"incomplete_uv_620",
			func() gotuple.Tuple { return gotuple.Tuple{gotuple.IncompleteVersionstamp(620)} },
			func() cgotuple.Tuple { return cgotuple.Tuple{cgotuple.IncompleteVersionstamp(620)} },
			nil,
		},
		{
			"incomplete_after_elems",
			func() gotuple.Tuple {
				return gotuple.Tuple{"k", int64(42), gotuple.IncompleteVersionstamp(3)}
			},
			func() cgotuple.Tuple {
				return cgotuple.Tuple{"k", int64(42), cgotuple.IncompleteVersionstamp(3)}
			},
			nil,
		},
		{
			"incomplete_with_prefix",
			func() gotuple.Tuple { return gotuple.Tuple{"x", gotuple.IncompleteVersionstamp(9)} },
			func() cgotuple.Tuple { return cgotuple.Tuple{"x", cgotuple.IncompleteVersionstamp(9)} },
			[]byte("subspace/"),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gb, gerr := tc.mkGo().PackWithVersionstamp(tc.prefix)
			cb, cerr := tc.mkCgo().PackWithVersionstamp(tc.prefix)
			// Every case here is a well-formed tuple with exactly one incomplete
			// versionstamp, so BOTH codecs MUST succeed. Requiring success (rather than
			// tolerating a both-error return) removes a vacuous-pass path: a (gerr!=nil →
			// return) guard would silently skip the byte assertion if PackWithVersionstamp
			// or GetAPIVersion ever errored (FDB-C++ dev review).
			if gerr != nil || cerr != nil {
				t.Fatalf("%s: PackWithVersionstamp must succeed for a well-formed one-incomplete-versionstamp tuple; go=%v cgo=%v", tc.name, gerr, cerr)
			}
			if !bytes.Equal(gb, cb) {
				t.Fatalf("%s: PackWithVersionstamp mismatch\n go=%x\ncgo=%x", tc.name, gb, cb)
			}
		})
	}
}

// FuzzDifferential_TuplePack fuzzes a random element stream, builds the same logical tuple in
// both codecs, and asserts Pack() byte-equality. Target: 0 mismatches over a long run.
func FuzzDifferential_TuplePack(f *testing.F) {
	f.Add([]byte{0x00, 0x01, 0x02, 0x03})
	f.Add([]byte{0xff, 0x80, 0x40, 0x20, 0x10})
	f.Fuzz(func(t *testing.T, seed []byte) {
		g, c := buildFuzzTuples(seed)
		gb := g.Pack()
		cb := c.Pack()
		if !bytes.Equal(gb, cb) {
			t.Fatalf("fuzz Pack mismatch for seed %x\n go=%x\ncgo=%x", seed, gb, cb)
		}
	})
}

// buildFuzzTuples derives a parallel pair of tuples from a byte seed: each seed byte's low bits
// pick a type code, consuming following bytes as the value. Identical logic for both codecs.
func buildFuzzTuples(seed []byte) (gotuple.Tuple, cgotuple.Tuple) {
	var g gotuple.Tuple
	var c cgotuple.Tuple
	i := 0
	for i < len(seed) {
		b := seed[i]
		i++
		switch b % 9 {
		case 0:
			g = append(g, nil)
			c = append(c, nil)
		case 1:
			g = append(g, int64(b))
			c = append(c, int64(b))
		case 2:
			g = append(g, -int64(b))
			c = append(c, -int64(b))
		case 3:
			g = append(g, uint64(b)<<56)
			c = append(c, uint64(b)<<56)
		case 4:
			s := string([]byte{b, 0x00, b})
			g = append(g, s)
			c = append(c, s)
		case 5:
			bs := []byte{b, 0xff, b}
			g = append(g, bs)
			c = append(c, bs)
		case 6:
			g = append(g, b%2 == 0)
			c = append(c, b%2 == 0)
		case 7:
			g = append(g, math.Float64frombits(uint64(b)<<48))
			c = append(c, math.Float64frombits(uint64(b)<<48))
		case 8:
			g = append(g, gotuple.Tuple{int64(b), nil})
			c = append(c, cgotuple.Tuple{int64(b), nil})
		}
	}
	return g, c
}
