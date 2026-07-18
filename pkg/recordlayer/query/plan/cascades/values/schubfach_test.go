package values

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestSchubfach_GTableMatchesJDKConstants pins the init-computed g table
// against the literal constants at the head of OpenJDK's MathUtils.g —
// a transcription-independent check that the "g = floor(β)+1" derivation
// reproduces the JDK's table.
func TestSchubfach_GTableMatchesJDKConstants(t *testing.T) {
	t.Parallel()
	// The first rows of MathUtils.g (k = -324 .. -318).
	want := []int64{
		0x4F0C_EDC9_5A71_8DD4, 0x5B01_E8B0_9AA0_D1B5, // -324
		0x7E7B_160E_F71C_1621, 0x119C_A780_F767_B5EE, // -323
		0x652F_44D8_C5B0_11B4, 0x0E16_EC67_2C52_F7F2, // -322
		0x50F2_9D7A_37C0_0E29, 0x5812_56B8_F042_5FF5, // -321
		0x40C2_1794_F966_71BA, 0x79A8_4560_C035_1991, // -320
		0x679C_F287_F570_B5F7, 0x75DA_089A_CD21_C281, // -319
		0x52E3_F539_9126_F7F9, 0x44AE_6D48_A41B_0201, // -318
	}
	for i, w := range want {
		if schubG[i] != w {
			t.Fatalf("schubG[%d] (k=%d): got %#x, want %#x", i, schubKMin+i/2, schubG[i], w)
		}
	}
}

// TestSchubfach_GoldenCorpus verifies the port digit-for-digit against
// testdata/schubfach_golden.txt — bit patterns and their exact
// Double.toString / Float.toString renderings emitted by a JDK 21+ JVM
// (the conformance server's runtime per .bazelrc remotejdk_21). The
// corpus covers the tiny/subnormal sweep, boundary bit patterns, an
// exponent ladder, and thousands of random finite values.
func TestSchubfach_GoldenCorpus(t *testing.T) {
	t.Parallel()
	f, err := os.Open("testdata/schubfach_golden.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), " ", 3)
		if len(parts) != 3 {
			t.Fatalf("bad golden line: %q", sc.Text())
		}
		switch parts[0] {
		case "D":
			bits, err := strconv.ParseUint(parts[1], 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			v := math.Float64frombits(bits)
			if got := JavaDoubleToString(v); got != parts[2] {
				t.Errorf("double bits %s: got %q, want %q", parts[1], got, parts[2])
			}
		case "F":
			bits, err := strconv.ParseUint(parts[1], 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			v := math.Float32frombits(uint32(bits))
			if got := JavaFloatToString(v); got != parts[2] {
				t.Errorf("float bits %s: got %q, want %q", parts[1], got, parts[2])
			}
		default:
			t.Fatalf("bad golden line: %q", sc.Text())
		}
		n++
		if t.Failed() && n > 50 {
			t.Fatal("aborting after 50 mismatches")
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 6000 {
		t.Fatalf("golden corpus suspiciously small: %d lines", n)
	}
}
