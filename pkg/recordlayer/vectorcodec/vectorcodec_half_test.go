package vectorcodec

import (
	"math"
	"testing"
)

// TestHalfRoundTripExhaustive proves float32ToHalf is the exact inverse of
// halfToFloat32 across the entire 16-bit space: decode(h) re-encodes to h for
// every non-NaN half value. This pins the codec bijection — no drift, no
// double-rounding — which the SPFresh index relies on for stable centroid and
// sidecar bytes.
func TestHalfRoundTripExhaustive(t *testing.T) {
	t.Parallel()
	for i := 0; i <= 0xffff; i++ {
		h := uint16(i)
		f := halfToFloat32(h)
		if f != f { // NaN: payload truncation is lossy by design; just stay NaN
			back := float32ToHalf(f)
			if backF := halfToFloat32(back); backF == backF {
				t.Fatalf("half %#04x: NaN did not survive round-trip (got %v)", h, backF)
			}
			continue
		}
		if got := float32ToHalf(f); got != h {
			t.Fatalf("half %#04x -> float %v -> half %#04x (not a bijection)", h, f, got)
		}
	}
}

// TestFloat32ToHalfRounding pins round-to-nearest-even on values that fall
// between representable halves, plus the overflow/underflow edges.
func TestFloat32ToHalfRounding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float32
		want uint16
	}{
		{0, 0x0000},
		{float32(math.Copysign(0, -1)), 0x8000},
		{1.0, 0x3c00},
		{-2.0, 0xc000},
		{65504, 0x7bff},                       // max finite half
		{65520, 0x7c00},                       // rounds up past max -> +Inf
		{1e10, 0x7c00},                        // overflow -> +Inf
		{-1e10, 0xfc00},                       // overflow -> -Inf
		{float32(math.Inf(1)), 0x7c00},        // +Inf
		{float32(math.Inf(-1)), 0xfc00},       // -Inf
		{5.960464477539063e-08, 0x0001},       // min subnormal half
		{2.980232238769531e-08, 0x0000},       // half of min subnormal: ties-to-even -> 0
		{8.940696716308594e-08, 0x0002},       // 1.5 * min subnormal: ties-to-even -> 2
		{6.103515625e-05, 0x0400},             // min normal half
		{1.0 + 1.0/2048.0, 0x3c00},            // tie between 1.0 and 1+1/1024: even -> 1.0
		{1.0 + 3.0/2048.0, 0x3c02},            // tie: even -> 1+2/1024
		{float32(math.Float64frombits(0)), 0}, // exact zero via float64 path
	}
	for _, c := range cases {
		if got := float32ToHalf(c.in); got != c.want {
			t.Errorf("float32ToHalf(%v) = %#04x, want %#04x", c.in, got, c.want)
		}
	}
}

// TestSerializeHalfRoundTrip checks the full codec path: SerializeHalf ->
// Deserialize yields the half-rounded components with the HALF type ordinal.
func TestSerializeHalfRoundTrip(t *testing.T) {
	t.Parallel()
	in := []float64{0, 1, -1, 0.5, 3.14159, -65504, 65504, 1e-8, 123.456}
	data := SerializeHalf(in)
	if data[0] != typeHalf {
		t.Fatalf("type ordinal = %d, want %d (HALF)", data[0], typeHalf)
	}
	if len(data) != 1+2*len(in) {
		t.Fatalf("len = %d, want %d", len(data), 1+2*len(in))
	}
	out, err := Deserialize(data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		want := float64(halfToFloat32(float32ToHalf(float32(in[i]))))
		if out[i] != want {
			t.Errorf("component %d: got %v, want %v (half-rounded %v)", i, out[i], want, in[i])
		}
	}
}

// FuzzHalfRoundTrip fuzzes float32 inputs through encode->decode->encode:
// the second encode must equal the first (idempotence after one rounding).
func FuzzHalfRoundTrip(f *testing.F) {
	f.Add(float32(1.5))
	f.Add(float32(-0.00001))
	f.Add(float32(65504))
	f.Add(float32(math.Inf(1)))
	f.Fuzz(func(t *testing.T, in float32) {
		h1 := float32ToHalf(in)
		back := halfToFloat32(h1)
		if back != back { // NaN
			if in == in {
				t.Fatalf("non-NaN %v decoded to NaN (h=%#04x)", in, h1)
			}
			return
		}
		if h2 := float32ToHalf(back); h2 != h1 {
			t.Fatalf("%v: encode %#04x -> decode %v -> encode %#04x (not idempotent)", in, h1, back, h2)
		}
	})
}
