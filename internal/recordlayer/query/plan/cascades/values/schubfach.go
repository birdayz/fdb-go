package values

// Schubfach digit engine — a 1:1 port of OpenJDK's
// jdk.internal.math.DoubleToDecimal / FloatToDecimal (the JDK 19+
// Double.toString / Float.toString implementation; Giulietti, "The
// Schubfach way to render doubles"). The conformance server runs on
// remotejdk_21 (.bazelrc), so THIS algorithm — not Go's shortest-decimal
// strconv — is the digit authority. The two agree on every normal value
// (both are shortest-round-trip there), but diverge on SUBNORMALS:
// Schubfach's digit-removal step is gated on s >= 100, so the tiniest
// values keep two significand digits where Go shortens to one
// (Double.MIN_VALUE: Java "4.9E-324", Go "5e-324"; Float.MIN_VALUE:
// Java "1.4E-45", Go "1e-45").
//
// The port mirrors the Java source structure: toDecimal's normal /
// subnormal / tiny-subnormal dispatch, the regular vs irregular spacing
// split, the rop computations, and the s>=100 / s-or-t / closest-choice
// selection. The g table (128-bit approximations of the powers of 10,
// MathUtils.g) is COMPUTED at init from its definition — g = floor(β)+1
// where 10^-k = β·2^r, 2^125 ≤ β < 2^126 — and spot-verified against the
// JDK's literal constants in the tests.

import (
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"strings"
)

const (
	dblP      = 53
	dblW      = 11
	dblQMin   = (-1 << (dblW - 1)) - dblP + 3 // -1074
	dblCTiny  = 3
	dblCMin   = int64(1) << (dblP - 1)
	dblBQMask = (1 << dblW) - 1
	dblTMask  = (int64(1) << (dblP - 1)) - 1

	fltP      = 24
	fltW      = 8
	fltQMin   = (-1 << (fltW - 1)) - fltP + 3 // -149
	fltCTiny  = 8
	fltCMin   = int32(1) << (fltP - 1)
	fltBQMask = (1 << fltW) - 1
	fltTMask  = (int32(1) << (fltP - 1)) - 1

	schubKMin = -324
	schubKMax = 292

	// C_10 = floor(log10(2)·2^Q_10), A_10 = floor(log10(3/4)·2^Q_10),
	// C_2 = floor(log2(10)·2^Q_2) (MathUtils).
	schubQ10 = 41
	schubC10 = 661971961083
	schubA10 = -274743187321
	schubQ2  = 38
	schubC2  = 913124641741

	mask63 = int64(^uint64(0) >> 1) // (1<<63)-1
	mask32 = (int64(1) << 32) - 1
)

var schubPow10 = [18]uint64{
	1, 10, 100, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9,
	1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17,
}

// schubG holds the g1/g0 pairs of MathUtils.g: for each k in
// [schubKMin, schubKMax], g = floor(β)+1 split into the high 63 bits
// (g1) and low 63 bits (g0).
var schubG [(schubKMax - schubKMin + 1) * 2]int64

func init() {
	ten := big.NewInt(10)
	one := big.NewInt(1)
	for k := schubKMin; k <= schubKMax; k++ {
		r := flog2pow10(-k) - 125
		num := new(big.Int).Set(one)
		den := new(big.Int).Set(one)
		if k <= 0 {
			num.Exp(ten, big.NewInt(int64(-k)), nil)
		} else {
			den.Exp(ten, big.NewInt(int64(k)), nil)
		}
		if r <= 0 {
			num.Lsh(num, uint(-r))
		} else {
			den.Lsh(den, uint(r))
		}
		g := new(big.Int).Quo(num, den)
		g.Add(g, one)
		schubG[(k-schubKMin)*2] = int64(new(big.Int).Rsh(g, 63).Uint64())
		schubG[(k-schubKMin)*2+1] = int64(new(big.Int).And(g, big.NewInt(mask63)).Uint64())
	}
}

func flog10pow2(e int) int { return int(int64(e) * schubC10 >> schubQ10) }
func flog10threeQuartersPow2(e int) int {
	return int((int64(e)*schubC10 + schubA10) >> schubQ10)
}
func flog2pow10(e int) int { return int(int64(e) * schubC2 >> schubQ2) }

// mulHigh is Java's Math.multiplyHigh: the high 64 bits of the SIGNED
// 128-bit product.
func mulHigh(x, y int64) int64 {
	hi, _ := bits.Mul64(uint64(x), uint64(y))
	return int64(hi) - ((x >> 63) & y) - ((y >> 63) & x)
}

// ropDbl computes rop(cp·g·2^-127) with g = g1·2^63 + g0
// (DoubleToDecimal.rop).
func ropDbl(g1, g0, cp int64) int64 {
	x1 := mulHigh(g0, cp)
	y0 := g1 * cp
	y1 := mulHigh(g1, cp)
	z := int64(uint64(y0)>>1) + x1
	vbp := y1 + int64(uint64(z)>>63)
	return vbp | int64(uint64((z&mask63)+mask63)>>63)
}

// ropFlt computes rop(cp·g·2^-95) (FloatToDecimal.rop).
func ropFlt(g, cp int64) int32 {
	x1 := mulHigh(g, cp)
	vbp := int64(uint64(x1) >> 31)
	return int32(vbp | int64(uint64((x1&mask32)+mask32)>>32))
}

// schubLen returns len such that 10^(len-1) <= f < 10^len (f > 0).
func schubLen(f uint64) int {
	l := flog10pow2(64 - bits.LeadingZeros64(f))
	if f >= schubPow10[l] {
		l++
	}
	return l
}

// javaDoubleDigits computes the Schubfach decimal of a positive finite
// non-zero double: digits f and exponent e with v rendered as
// d = 0.f × 10^e (f carries no trailing semantics — the renderer strips
// trailing zeros).
func javaDoubleDigits(v float64) (uint64, int) {
	b := math.Float64bits(v)
	t := int64(b) & dblTMask
	bq := int(b>>(dblP-1)) & dblBQMask
	if bq != 0 {
		mq := -dblQMin + 1 - bq
		c := dblCMin | t
		if 0 < mq && mq < dblP {
			if f := c >> uint(mq); f<<uint(mq) == c {
				return uint64(f), 0 + schubLen(uint64(f))
			}
		}
		return dblToDecimal(-mq, c, 0)
	}
	if t < dblCTiny {
		return dblToDecimal(dblQMin, 10*t, -1)
	}
	return dblToDecimal(dblQMin, t, 0)
}

func dblToDecimal(q int, c int64, dk int) (uint64, int) {
	out := c & 1
	cb := c << 2
	cbr := cb + 2
	var cbl int64
	var k int
	if c != dblCMin || q == dblQMin {
		cbl = cb - 2
		k = flog10pow2(q)
	} else {
		cbl = cb - 1
		k = flog10threeQuartersPow2(q)
	}
	h := q + flog2pow10(-k) + 2
	g1 := schubG[(k-schubKMin)*2]
	g0 := schubG[(k-schubKMin)*2+1]
	vb := ropDbl(g1, g0, cb<<uint(h))
	vbl := ropDbl(g1, g0, cbl<<uint(h))
	vbr := ropDbl(g1, g0, cbr<<uint(h))
	s := vb >> 2
	if s >= 100 {
		sp10 := 10 * mulHigh(s, 115292150460684698<<4)
		tp10 := sp10 + 10
		upin := vbl+out <= sp10<<2
		wpin := tp10<<2+out <= vbr
		if upin != wpin {
			f := tp10
			if upin {
				f = sp10
			}
			return uint64(f), k + schubLen(uint64(f))
		}
	}
	t := s + 1
	uin := vbl+out <= s<<2
	win := t<<2+out <= vbr
	if uin != win {
		f := t
		if uin {
			f = s
		}
		return uint64(f), k + dk + schubLen(uint64(f))
	}
	cmp := vb - (s+t)<<1
	f := t
	if cmp < 0 || (cmp == 0 && s&1 == 0) {
		f = s
	}
	return uint64(f), k + dk + schubLen(uint64(f))
}

// javaFloatDigits is the float32 twin (FloatToDecimal).
func javaFloatDigits(v float32) (uint64, int) {
	b := math.Float32bits(v)
	t := int32(b) & fltTMask
	bq := int(b>>(fltP-1)) & fltBQMask
	if bq != 0 {
		mq := -fltQMin + 1 - bq
		c := fltCMin | t
		if 0 < mq && mq < fltP {
			if f := c >> uint(mq); f<<uint(mq) == c {
				return uint64(f), 0 + schubLen(uint64(f))
			}
		}
		return fltToDecimal(-mq, c, 0)
	}
	if t < fltCTiny {
		return fltToDecimal(fltQMin, 10*t, -1)
	}
	return fltToDecimal(fltQMin, t, 0)
}

func fltToDecimal(q int, c int32, dk int) (uint64, int) {
	out := int32(c) & 1
	cb := int64(c) << 2
	cbr := cb + 2
	var cbl int64
	var k int
	if c != fltCMin || q == fltQMin {
		cbl = cb - 2
		k = flog10pow2(q)
	} else {
		cbl = cb - 1
		k = flog10threeQuartersPow2(q)
	}
	h := q + flog2pow10(-k) + 33
	// g is the high 64 bits of the 128-bit g, plus one (the appendix's
	// float-sized approximation): g1(k) + 1.
	g := schubG[(k-schubKMin)*2] + 1
	vb := ropFlt(g, cb<<uint(h))
	vbl := ropFlt(g, cbl<<uint(h))
	vbr := ropFlt(g, cbr<<uint(h))
	s := vb >> 2
	if s >= 100 {
		sp10 := 10 * int32(int64(s)*1717986919>>34)
		tp10 := sp10 + 10
		upin := vbl+out <= sp10<<2
		wpin := tp10<<2+out <= vbr
		if upin != wpin {
			f := tp10
			if upin {
				f = sp10
			}
			return uint64(f), k + schubLen(uint64(f))
		}
	}
	t := s + 1
	uin := vbl+out <= s<<2
	win := t<<2+out <= vbr
	if uin != win {
		f := t
		if uin {
			f = s
		}
		return uint64(f), k + dk + schubLen(uint64(f))
	}
	cmp := vb - (s+t)<<1
	f := t
	if cmp < 0 || (cmp == 0 && s&1 == 0) {
		f = s
	}
	return uint64(f), k + dk + schubLen(uint64(f))
}

// javaRenderDigits renders the Schubfach decimal 0.f × 10^e in Java's
// Double.toString layout: plain decimal for 0 < e <= 7 (".0"-completed)
// and -3 < e <= 0 (leading zeros), computerized scientific notation
// otherwise (one digit before the point, ".0"-completed mantissa,
// upper-case E, no plus sign, no zero-padded exponent — the printed
// exponent is e-1).
func javaRenderDigits(neg bool, f uint64, e int) string {
	ds := strings.TrimRight(strconv.FormatUint(f, 10), "0")
	if ds == "" {
		ds = "0"
	}
	var sb strings.Builder
	if neg {
		sb.WriteByte('-')
	}
	switch {
	case 0 < e && e <= 7:
		if len(ds) <= e {
			sb.WriteString(ds)
			for i := len(ds); i < e; i++ {
				sb.WriteByte('0')
			}
			sb.WriteString(".0")
		} else {
			sb.WriteString(ds[:e])
			sb.WriteByte('.')
			sb.WriteString(ds[e:])
		}
	case -3 < e && e <= 0:
		sb.WriteString("0.")
		for i := 0; i < -e; i++ {
			sb.WriteByte('0')
		}
		sb.WriteString(ds)
	default:
		sb.WriteString(ds[:1])
		sb.WriteByte('.')
		if len(ds) > 1 {
			sb.WriteString(ds[1:])
		} else {
			sb.WriteByte('0')
		}
		sb.WriteByte('E')
		sb.WriteString(strconv.Itoa(e - 1))
	}
	return sb.String()
}
