package yamsql

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

func tagged(kind, value string) Scalar { return Scalar{Kind: kind, Value: &value} }

func TestScalarCodec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value Scalar
		want  any
	}{
		{Scalar{Kind: "null"}, nil},
		{tagged("int64", "-9223372036854775808"), int64(math.MinInt64)},
		{tagged("int64", "9223372036854775807"), int64(math.MaxInt64)},
		{tagged("float64", "8000000000000000"), math.Copysign(0, -1)},
		{tagged("float64", "0000000000000000"), float64(0)},
		{tagged("float64", "7ff8000000000001"), math.Float64frombits(0x7ff8000000000001)},
		{tagged("float64", "7ff0000000000000"), math.Inf(1)},
		{tagged("string", "\n"), "\n"},
		{tagged("string", "\n\n"), "\n\n"},
		{tagged("string", "x\n"), "x\n"},
		{tagged("string", ""), ""},
		{tagged("string", "hello"), "hello"},
		{tagged("bool", "true"), true},
		{tagged("bool", "false"), false},
		{tagged("bytes", "00ff"), []byte{0, 255}},
		{tagged("bytes", ""), []byte{}},
	}
	for _, tc := range cases {
		t.Run(tc.value.Kind+"/"+func() string {
			if tc.value.Value == nil {
				return "null"
			}
			return *tc.value.Value
		}(), func(t *testing.T) {
			t.Parallel()
			got, err := tc.value.decode()
			if err != nil || !exactValueEqual(tc.want, got) {
				t.Fatalf("decode = %#v, %v; want %#v", got, err, tc.want)
			}
			data, err := yaml.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			var loaded Scalar
			if err := yaml.Unmarshal(data, &loaded); err != nil {
				t.Fatal(err)
			}
			round, err := loaded.decode()
			if err != nil || !exactValueEqual(tc.want, round) {
				t.Fatalf("round trip = %#v, %v", round, err)
			}
		})
	}
}

func TestScalarCodecRejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		`[]`, `{kind: null}`, `{kind: float64, value: 8000000000000000}`,
		`{kind: "null", value: ""}`, `{kind: string}`, `{kind: bool, value: "1"}`,
		`{kind: float64, value: "0"}`, `{kind: float64, value: "zzzzzzzzzzzzzzzz"}`,
		`{kind: int64, value: "9223372036854775808"}`, `{kind: int64, value: "1.0"}`,
		`{kind: bytes, value: "a"}`, `{kind: bytes, value: "zz"}`,
		`{kind: mystery, value: "x"}`, `{kind: string, value: "x", ignored: "y"}`,
		`{kind: string, kind: string, value: "x"}`, `{kind: string, value: null}`,
	} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			var s Scalar
			if err := yaml.Unmarshal([]byte(input), &s); err == nil {
				t.Fatalf("accepted %s", input)
			}
		})
	}
}

func TestExactRowsRepresentationAndMultiplicity(t *testing.T) {
	t.Parallel()
	neg := tagged("float64", "8000000000000000")
	pos := tagged("float64", "0000000000000000")
	cases := []struct {
		name            string
		want            [][]Scalar
		got             [][]any
		unordered, pass bool
	}{
		{"negative zero", [][]Scalar{{neg}}, [][]any{{math.Copysign(0, -1)}}, false, true},
		{"zero sign", [][]Scalar{{neg}}, [][]any{{float64(0)}}, false, false},
		{"carrier", [][]Scalar{{pos}}, [][]any{{int64(0)}}, false, false},
		{"float32", [][]Scalar{{pos}}, [][]any{{float32(0)}}, false, false},
		{"nonfinal zero", [][]Scalar{{neg, tagged("int64", "1")}, {pos, tagged("int64", "2")}}, [][]any{{float64(0), int64(1)}, {math.Copysign(0, -1), int64(2)}}, true, false},
		{"multiset", [][]Scalar{{neg}, {pos}, {neg}}, [][]any{{math.Copysign(0, -1)}, {math.Copysign(0, -1)}, {float64(0)}}, true, true},
		{"duplicate loss", [][]Scalar{{neg}, {pos}, {neg}}, [][]any{{math.Copysign(0, -1)}, {float64(0)}, {float64(0)}}, true, false},
		{"ordered", [][]Scalar{{neg}, {pos}}, [][]any{{float64(0)}, {math.Copysign(0, -1)}}, false, false},
		{"empty", [][]Scalar{}, nil, false, true},
		{"row count", [][]Scalar{}, [][]any{{nil}}, false, false},
		{"width", [][]Scalar{{pos}}, [][]any{{float64(0), nil}}, false, false},
		{"invalid expectation", [][]Scalar{{tagged("bad", "")}}, [][]any{{nil}}, false, false},
		{"nan payload", [][]Scalar{{tagged("float64", "7ff8000000000001")}}, [][]any{{math.Float64frombits(0x7ff8000000000002)}}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			diff := diffExactRows(tc.want, tc.got, tc.unordered)
			if (diff == "") != tc.pass {
				t.Fatalf("pass=%v diff=%s", tc.pass, diff)
			}
		})
	}
	if !strings.Contains(diffExactRows([][]Scalar{{tagged("bad", "")}}, [][]any{{nil}}, false), "unknown scalar kind") {
		t.Fatal("malformed expectation lost")
	}
}

func FuzzScalarFloatBits(f *testing.F) {
	for _, bits := range []uint64{0, 1, 0x8000000000000000, 0x7ff8000000000001, 0x7ff0000000000000} {
		f.Add(bits)
	}
	f.Fuzz(func(t *testing.T, bits uint64) {
		value := math.Float64frombits(bits)
		// Encoding by bytes is independent of the decoder's bit assembly.
		const digits = "0123456789abcdef"
		var text [16]byte
		for i := range text {
			text[i] = digits[(bits>>uint(4*(15-i)))&15]
		}
		got, err := tagged("float64", string(text[:])).decode()
		if err != nil {
			t.Fatal(err)
		}
		decoded, ok := got.(float64)
		if !ok || math.Float64bits(decoded) != bits {
			t.Fatalf("bits %016x became %#v", bits, got)
		}
		if !exactValueEqual(value, decoded) || exactValueEqual(value, math.Float64frombits(bits^1)) {
			t.Fatal("bit comparison lost representation")
		}
	})
}

func TestScalarInvalidUTF8(t *testing.T) {
	t.Parallel()
	if _, err := tagged("string", string([]byte{0xff})).decode(); err == nil {
		t.Fatal("accepted string that YAML cannot round trip; use bytes")
	}
	if got := tagged("float64", "8000000000000000").String(); got != `float64("8000000000000000")` {
		t.Fatalf("unreadable scalar %s", got)
	}
}

func FuzzScalarRoundTripAndMultiset(f *testing.F) {
	f.Add(uint64(0), []byte("\n"))
	f.Add(uint64(0x8000000000000000), []byte{0xff, 0, 1})
	f.Add(uint64(0x7ff8000000000001), []byte("hello"))
	f.Fuzz(func(t *testing.T, bits uint64, data []byte) {
		values := []Scalar{{Kind: "null"}, tagged("int64", strconv.FormatInt(int64(bits), 10)), tagged("float64", fmt.Sprintf("%016x", bits)), tagged("bool", strconv.FormatBool(bits%2 == 0)), tagged("bytes", hex.EncodeToString(data))}
		if utf8.Valid(data) {
			values = append(values, tagged("string", string(data)))
		} else {
			if _, err := tagged("string", string(data)).decode(); err == nil {
				t.Fatal("accepted invalid UTF-8")
			}
		}
		for _, value := range values {
			original, err := value.decode()
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := yaml.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var loaded Scalar
			if err := yaml.Unmarshal(encoded, &loaded); err != nil {
				t.Fatal(err)
			}
			decoded, err := loaded.decode()
			if err != nil || !exactValueEqual(original, decoded) {
				t.Fatalf("codec round trip changed %s: %v", value, err)
			}
		}
		a := values[2]
		b := tagged("float64", fmt.Sprintf("%016x", bits^1))
		av, bv := math.Float64frombits(bits), math.Float64frombits(bits^1)
		want := [][]Scalar{{a, values[1]}, {b, values[1]}, {a, values[1]}}
		// The second column makes the float non-final; multiplicity is 2:1.
		good := [][]any{{av, int64(bits)}, {av, int64(bits)}, {bv, int64(bits)}}
		bad := [][]any{{bv, int64(bits)}, {av, int64(bits)}, {bv, int64(bits)}}
		if d := diffExactRows(want, good, true); d != "" {
			t.Fatal(d)
		}
		if d := diffExactRows(want, bad, true); d == "" {
			t.Fatal("multiset accepted wrong duplicate count")
		}
		if d := diffExactRows(want, good, false); d == "" {
			t.Fatal("ordered comparator accepted swapped rows")
		}
	})
}
