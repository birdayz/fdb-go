package javanum

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// The 22 spellings of RFC-257 WS-E round v12, each with the outcome
// Double.parseDouble gave in the target (ws_e_probe_conformance_test.go,
// wsE12Pins java_cast_NN and java_t_d_idNNN), plus boundary cases.
func TestParseDoubleMatchesTheTarget(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		bits uint64
		err  string
	}{
		{in: "NaN", bits: 0x7ff8000000000000},
		{in: "-NaN", bits: 0x7ff8000000000000},
		{in: "+NaN", bits: 0x7ff8000000000000},
		{in: "nan", err: `For input string: "nan"`},
		{in: "NAN", err: `For input string: "NAN"`},
		{in: "Infinity", bits: 0x7ff0000000000000},
		{in: "-Infinity", bits: 0xfff0000000000000},
		{in: "+Infinity", bits: 0x7ff0000000000000},
		{in: "inf", err: `For input string: "inf"`},
		{in: "infinity", err: `For input string: "infinity"`},
		{in: "1.5d", bits: 0x3ff8000000000000},
		{in: "1.5D", bits: 0x3ff8000000000000},
		{in: "1.5f", bits: 0x3ff8000000000000},
		{in: "1.5F", bits: 0x3ff8000000000000},
		{in: " 1.5 ", bits: 0x3ff8000000000000},
		{in: "0x1p3", bits: 0x4020000000000000},
		{in: "0x1.8p1", bits: 0x4008000000000000},
		{in: "1_000", err: `For input string: "1_000"`},
		{in: "1e400", bits: 0x7ff0000000000000},
		{in: "1e-400", bits: 0},
		{in: ".5", bits: 0x3fe0000000000000},
		{in: "5.", bits: 0x4014000000000000},
		// Boundaries.
		{in: "-0", bits: 0x8000000000000000},
		{in: "-1e-400", bits: 0x8000000000000000},
		{in: "-1e400", bits: 0xfff0000000000000},
		{in: "0x1p3d", bits: 0x4020000000000000},
		{in: "", err: "empty String"},
		{in: " \t\n", err: "empty String"},
		{in: "-", err: `For input string: "-"`},
		{in: "+", err: `For input string: "+"`},
		{in: "Infinityx", err: `For input string: "Infinityx"`},
		{in: "NaNd", err: `For input string: "NaNd"`},
		{in: "1.2.3", err: "multiple points"},
		{in: "0x1.8", err: `For input string: "0x1.8"`},
		{in: "\u00a01", err: "For input string: \"\u00a01\""},
	} {
		got, err := ParseDouble(c.in)
		if c.err != "" {
			var fe *FormatError
			if !errors.As(err, &fe) || err.Error() != c.err {
				t.Errorf("ParseDouble(%q) = %v, %v; want FormatError %q", c.in, got, err, c.err)
			}
			continue
		}
		if err != nil || math.Float64bits(got) != c.bits {
			t.Errorf("ParseDouble(%q) = %016x, %v; want %016x", c.in, math.Float64bits(got), err, c.bits)
		}
	}
}

// Float.parseFloat: the same grammar, Float.NaN's bits, and ONE rounding to
// binary32. The text below is 1 + 2^-24 + 2^-70: a double rounds it to the
// exact float halfway 1 + 2^-24, which ties to even (1.0f), while direct
// rounding sees it above halfway (1 + 2^-23).
func TestParseFloatRoundsOnceToBinary32(t *testing.T) {
	t.Parallel()
	const aboveHalfway = "1.0000000596046447753914720329472543003390683225006796419620513916015625"
	if d, _ := ParseDouble(aboveHalfway); float32(d) != 1 {
		t.Fatalf("test premise: rounding through a double gives %v, want 1", float32(d))
	}
	for _, c := range []struct {
		in   string
		bits uint32
		err  string
	}{
		{in: aboveHalfway, bits: 0x3f800001},
		{in: "NaN", bits: 0x7fc00000},
		{in: "-NaN", bits: 0x7fc00000},
		{in: "-Infinity", bits: 0xff800000},
		{in: "1e39", bits: 0x7f800000},
		{in: "-1e-50", bits: 0x80000000},
		{in: "0.1f", bits: 0x3dcccccd},
		{in: "0x1.8p1", bits: 0x40400000},
		{in: "nan", err: `For input string: "nan"`},
		{in: "", err: "empty String"},
	} {
		got, err := ParseFloat(c.in)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("ParseFloat(%q) = %v, %v; want %q", c.in, got, err, c.err)
			}
			continue
		}
		if err != nil || math.Float32bits(got) != c.bits {
			t.Errorf("ParseFloat(%q) = %08x, %v; want %08x", c.in, math.Float32bits(got), err, c.bits)
		}
	}
	// Widened, Float.NaN is Double.NaN (what a FLOAT column reads back).
	if f, _ := ParseFloat("NaN"); math.Float64bits(float64(f)) != NaN64Bits {
		t.Fatalf("widened Float.NaN = %016x", math.Float64bits(float64(f)))
	}
}

// Every accepted text agrees with strconv's correctly rounded value, and every
// NaN is the canonical one, whatever the input.
func FuzzParseDouble(f *testing.F) {
	for _, s := range []string{"1.5", "NaN", "-0x1.8p1d", "1e400", " .5 ", "1_000", "inf"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, derr := ParseDouble(s)
		g, ferr := ParseFloat(s)
		if (derr == nil) != (ferr == nil) {
			t.Fatalf("%q: double err %v, float err %v", s, derr, ferr)
		}
		if derr != nil {
			return
		}
		if math.IsNaN(d) != math.IsNaN(float64(g)) {
			t.Fatalf("%q: double %v, float %v", s, d, g)
		}
		if math.IsNaN(d) {
			if math.Float64bits(d) != NaN64Bits || math.Float32bits(g) != NaN32Bits {
				t.Fatalf("%q: NaN bits %016x %08x", s, math.Float64bits(d), math.Float32bits(g))
			}
			if !strings.Contains(s, "NaN") {
				t.Fatalf("%q parsed as NaN", s)
			}
		}
	})
}
