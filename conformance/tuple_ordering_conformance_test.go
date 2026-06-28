//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// typedValue represents a typed value for cross-language serialization.
type typedValue struct {
	Type  string `json:"type"`
	Value any    `json:"value,omitempty"`
}

// comparisonPair represents a pair of values to compare.
type comparisonPair struct {
	A typedValue `json:"a"`
	B typedValue `json:"b"`
}

// comparisonResult is the result from Java.
type comparisonResult struct {
	Cmp int `json:"cmp"`
}

// goCompare replicates compareField logic (which is unexported).
// Uses FDB's order-preserving tuple encoding for comparison.
func goCompare(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	return signum(bytes.Compare(tuple.Tuple{a}.Pack(), tuple.Tuple{b}.Pack()))
}

func signum(x int) int {
	if x < 0 {
		return -1
	}
	if x > 0 {
		return 1
	}
	return 0
}

// toTypedValue converts a Go value to the typed JSON format for Java.
func toTypedValue(v any) typedValue {
	if v == nil {
		return typedValue{Type: "NULL"}
	}
	switch val := v.(type) {
	case int64:
		return typedValue{Type: "INT64", Value: val}
	case float64:
		if math.IsNaN(val) {
			return typedValue{Type: "FLOAT64_SPECIAL", Value: "NaN"}
		}
		if math.IsInf(val, 1) {
			return typedValue{Type: "FLOAT64_SPECIAL", Value: "Infinity"}
		}
		if math.IsInf(val, -1) {
			return typedValue{Type: "FLOAT64_SPECIAL", Value: "-Infinity"}
		}
		if val == 0 && math.Signbit(val) {
			return typedValue{Type: "FLOAT64_SPECIAL", Value: "-0.0"}
		}
		return typedValue{Type: "FLOAT64", Value: val}
	case float32:
		if math.IsNaN(float64(val)) {
			return typedValue{Type: "FLOAT32_SPECIAL", Value: "NaN"}
		}
		if math.IsInf(float64(val), 1) {
			return typedValue{Type: "FLOAT32_SPECIAL", Value: "Infinity"}
		}
		if math.IsInf(float64(val), -1) {
			return typedValue{Type: "FLOAT32_SPECIAL", Value: "-Infinity"}
		}
		if val == 0 && math.Signbit(float64(val)) {
			return typedValue{Type: "FLOAT32_SPECIAL", Value: "-0.0"}
		}
		return typedValue{Type: "FLOAT32", Value: float64(val)}
	case string:
		return typedValue{Type: "STRING", Value: val}
	case []byte:
		ints := make([]int, len(val))
		for i, b := range val {
			ints[i] = int(b)
		}
		return typedValue{Type: "BYTES", Value: ints}
	case bool:
		return typedValue{Type: "BOOL", Value: val}
	case tuple.UUID:
		return typedValue{Type: "UUID", Value: val.String()}
	case tuple.Versionstamp:
		ints := make([]int, 12)
		for i, b := range val.TransactionVersion {
			ints[i] = int(b)
		}
		ints[10] = int(val.UserVersion >> 8)
		ints[11] = int(val.UserVersion & 0xFF)
		return typedValue{Type: "VERSIONSTAMP", Value: ints}
	default:
		panic(fmt.Sprintf("unsupported type for typed value: %T", v))
	}
}

// makeUUID creates a tuple.UUID from a string.
func makeUUID(s string) tuple.UUID {
	// Parse UUID string "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" into 16 bytes.
	// Use a simple hex parser since we control the inputs.
	var uuid tuple.UUID
	hex := ""
	for _, c := range s {
		if c != '-' {
			hex += string(c)
		}
	}
	for i := 0; i < 16; i++ {
		var b byte
		for j := 0; j < 2; j++ {
			b <<= 4
			c := hex[i*2+j]
			switch {
			case c >= '0' && c <= '9':
				b |= c - '0'
			case c >= 'a' && c <= 'f':
				b |= c - 'a' + 10
			case c >= 'A' && c <= 'F':
				b |= c - 'A' + 10
			}
		}
		uuid[i] = b
	}
	return uuid
}

// makeVersionstamp creates a complete Versionstamp with the given global+local bytes.
func makeVersionstamp(global [10]byte, local uint16) tuple.Versionstamp {
	return tuple.Versionstamp{TransactionVersion: global, UserVersion: local}
}

var _ = Describe("Tuple Ordering Conformance", func() {
	ctx := context.Background()

	// callJavaCompare sends a batch of comparison pairs to Java and returns results.
	callJavaCompare := func(pairs []comparisonPair) []comparisonResult {
		java := NewJavaInvoker()

		pairsJSON, err := json.Marshal(pairs)
		Expect(err).NotTo(HaveOccurred())

		var results []comparisonResult
		err = java.InvokeAs(ctx, "compareTupleOrdering", map[string]any{
			"pairsJson": string(pairsJSON),
		}, &results)
		Expect(err).NotTo(HaveOccurred())
		return results
	}

	// verifyOrdering runs Go and Java comparisons on all pairs and asserts they match.
	verifyOrdering := func(goValues []any, pairs []comparisonPair) {
		// Get Java results for all pairs
		javaResults := callJavaCompare(pairs)
		Expect(javaResults).To(HaveLen(len(pairs)))

		// Compare Go vs Java for each pair
		pairIdx := 0
		for i := 0; i < len(goValues); i++ {
			for j := 0; j < len(goValues); j++ {
				goResult := goCompare(goValues[i], goValues[j])
				javaResult := javaResults[pairIdx].Cmp

				Expect(goResult).To(Equal(javaResult),
					"Mismatch for pair (%v [%T], %v [%T]): Go=%d, Java=%d",
					goValues[i], goValues[i], goValues[j], goValues[j], goResult, javaResult)
				pairIdx++
			}
		}
	}

	// buildAllPairs creates comparison pairs for all value combinations.
	buildAllPairs := func(goValues []any) []comparisonPair {
		pairs := make([]comparisonPair, 0, len(goValues)*len(goValues))
		for _, a := range goValues {
			for _, b := range goValues {
				pairs = append(pairs, comparisonPair{
					A: toTypedValue(a),
					B: toTypedValue(b),
				})
			}
		}
		return pairs
	}

	Describe("int64 ordering", func() {
		It("matches Java for all int64 edge cases", func() {
			values := []any{
				int64(0),
				int64(1),
				int64(-1),
				int64(42),
				int64(-42),
				int64(math.MaxInt64),
				int64(math.MinInt64),
				int64(127),
				int64(-128),
				int64(255),
				int64(256),
				int64(-256),
				int64(65535),
				int64(-65536),
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("float64 ordering", func() {
		It("matches Java for all float64 edge cases", func() {
			values := []any{
				float64(0.0),
				math.Copysign(0, -1), // -0.0
				float64(1.0),
				float64(-1.0),
				float64(3.14159),
				float64(-3.14159),
				math.SmallestNonzeroFloat64,
				math.MaxFloat64,
				-math.MaxFloat64,
				math.Inf(1),
				math.Inf(-1),
				math.NaN(),
				float64(1e-300),
				float64(1e300),
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("float32 ordering", func() {
		It("matches Java for float32 edge cases", func() {
			values := []any{
				float32(0.0),
				float32(math.Copysign(0, -1)), // -0.0
				float32(1.0),
				float32(-1.0),
				float32(math.SmallestNonzeroFloat32),
				float32(math.MaxFloat32),
				float32(-math.MaxFloat32),
				float32(math.Inf(1)),
				float32(math.Inf(-1)),
				float32(math.NaN()),
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("string ordering", func() {
		It("matches Java for all string edge cases", func() {
			values := []any{
				"",
				"a",
				"b",
				"abc",
				"abd",
				"ab",
				"z",
				"hello world",
				"\x00",     // null byte
				"\x00\x00", // two null bytes
				"\xc3\xbf", // ÿ (valid UTF-8)
				"αβγ",      // Greek
				"日本語",      // CJK
				"🎉",        // emoji
				"a\x00b",   // embedded null
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("bytes ordering", func() {
		It("matches Java for all byte slice edge cases", func() {
			values := []any{
				[]byte{},
				[]byte{0},
				[]byte{1},
				[]byte{0, 0},
				[]byte{0, 1},
				[]byte{1, 0},
				[]byte{255},
				[]byte{255, 255},
				[]byte{0, 255},
				[]byte{128},
				[]byte{127},
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("bool ordering", func() {
		It("matches Java for bool values", func() {
			values := []any{
				false,
				true,
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("UUID ordering", func() {
		It("matches Java for UUID edge cases", func() {
			values := []any{
				makeUUID("00000000-0000-0000-0000-000000000000"),
				makeUUID("00000000-0000-0000-0000-000000000001"),
				makeUUID("ffffffff-ffff-ffff-ffff-ffffffffffff"),
				makeUUID("550e8400-e29b-41d4-a716-446655440000"),
				makeUUID("6ba7b810-9dad-11d1-80b4-00c04fd430c8"),
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("Versionstamp ordering", func() {
		It("matches Java for Versionstamp edge cases", func() {
			values := []any{
				makeVersionstamp([10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 0),
				makeVersionstamp([10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 1),
				makeVersionstamp([10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, 0),
				makeVersionstamp([10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, 1),
				makeVersionstamp([10]byte{255, 255, 255, 255, 255, 255, 255, 255, 255, 254}, 0xFFFF),
				makeVersionstamp([10]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 42),
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("null ordering", func() {
		It("matches Java: null sorts before all types", func() {
			values := []any{
				nil,
				int64(0),
				int64(-1),
				float64(0.0),
				"",
				[]byte{},
				false,
				true,
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("cross-type ordering", func() {
		It("matches Java for FDB type code ordering", func() {
			// FDB tuple type codes define the cross-type sort order:
			// NULL(0x00) < bytes(0x01) < string(0x02) < int(0x14) < float64(0x21) < bool(0x26/0x27) < UUID(0x30) < Versionstamp(0x33)
			values := []any{
				nil,           // NULL
				[]byte{42},    // BYTES
				"hello",       // STRING
				int64(42),     // INT64
				float64(3.14), // FLOAT64
				false,         // BOOL (false)
				true,          // BOOL (true)
				makeUUID("550e8400-e29b-41d4-a716-446655440000"),            // UUID
				makeVersionstamp([10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, 0), // VERSIONSTAMP
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("int64 boundary values", func() {
		It("matches Java at integer encoding boundaries", func() {
			// FDB uses variable-length encoding for integers,
			// with encoding boundaries at powers of 256.
			values := []any{
				int64(0),
				int64(1),
				int64(-1),
				int64(127),       // max 1-byte positive
				int64(128),       // min 2-byte positive
				int64(255),       // max unsigned byte
				int64(256),       // 2-byte boundary
				int64(32767),     // max int16
				int64(32768),     // min 3-byte positive
				int64(65535),     // max uint16
				int64(65536),     // 3-byte boundary
				int64(1<<24 - 1), // max 3-byte
				int64(1 << 24),   // 4-byte boundary
				int64(1<<32 - 1), // max uint32
				int64(1 << 32),   // 5-byte boundary
				int64(-127),
				int64(-128),
				int64(-255),
				int64(-256),
				int64(-32767),
				int64(-32768),
				int64(-65535),
				int64(-65536),
				int64(math.MaxInt64),
				int64(math.MinInt64),
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("mixed numeric types", func() {
		It("matches Java for int64 vs float64 cross-type comparison", func() {
			// int64 and float64 have different type codes in FDB encoding.
			// int64 is 0x14-range, float64 is 0x21. So int always sorts before double.
			values := []any{
				int64(0),
				int64(1),
				int64(-1),
				float64(0.0),
				float64(1.0),
				float64(-1.0),
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})

	Describe("string vs bytes", func() {
		It("matches Java for string vs bytes ordering", func() {
			// Bytes type code (0x01) < String type code (0x02),
			// so all byte values sort before all string values.
			values := []any{
				[]byte{},
				[]byte{0x61}, // 'a'
				[]byte{0x62}, // 'b'
				"",
				"a",
				"b",
			}
			pairs := buildAllPairs(values)
			verifyOrdering(values, pairs)
		})
	})
})
