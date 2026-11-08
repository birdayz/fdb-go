package otel

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestReaderConfigValidate(t *testing.T) {
	t.Parallel()
	if err := (ReaderConfig{}).Validate(); err != nil {
		t.Fatalf("zero config (the defaults) refused: %v", err)
	}
	if d := (ReaderConfig{}).withDefaults(); d.MaxCandidates != 20_000 || d.MaxBytes != 4<<20 || d.K != 20 || d.TransactionTimeout != 5*time.Second {
		t.Fatalf("defaults %+v", d)
	}
	for _, c := range []ReaderConfig{
		{K: maxReaderK},
		{MaxCandidates: 100_000_000, MaxBytes: 1},
		{TransactionTimeout: time.Second},
		{TransactionTimeout: 5 * time.Minute},
		{TransactionTags: []string{"a", "b", "c", "d", strings.Repeat("e", 16)}},
	} {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v refused: %v", c, err)
		}
	}
	for name, c := range map[string]ReaderConfig{
		"negative candidates": {MaxCandidates: -1},
		"too many candidates": {MaxCandidates: 100_000_001},
		"negative bytes":      {MaxBytes: -1},
		"negative K":          {K: -1},
		"K over 1000":         {K: maxReaderK + 1},
		"short timeout":       {TransactionTimeout: time.Second - 1},
		"long timeout":        {TransactionTimeout: 5*time.Minute + 1},
		"six tags":            {TransactionTags: []string{"a", "b", "c", "d", "e", "f"}},
		"empty tag":           {TransactionTags: []string{""}},
		"long tag":            {TransactionTags: []string{strings.Repeat("e", 17)}},
	} {
		requireTyped[*ConfigError](t, name, c.Validate())
	}
}

func TestReaderAttributeIndex(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		signal, scope, typ string
		want               []string
	}{
		{"span", "span", "", []string{spansStrAttrIndex, spansIntAttrIndex, spansDoubleAttrIndex, spansBoolAttrIndex}},
		{"span", "resource", "string", []string{spansStrAttrIndex}},
		{"span", "scope", "int", []string{spansIntAttrIndex}},
		{"span", "span", "double", []string{spansDoubleAttrIndex}},
		{"span", "span", "bool", []string{spansBoolAttrIndex}},
		{"log", "log", "", []string{logsStrAttrIndex, logsIntAttrIndex, logsDoubleAttrIndex, logsBoolAttrIndex}},
		{"log", "resource", "string", []string{logsStrAttrIndex}},
		{"log", "scope", "bool", []string{logsBoolAttrIndex}},
	} {
		got, err := attributeIndex("t", c.signal, c.scope, c.typ)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s/%s/%s: %v (%v), want %v", c.signal, c.scope, c.typ, got, err, c.want)
		}
	}
	for name, c := range map[string][3]string{
		"unknown signal":   {"metric", "span", ""},
		"log scope span":   {"span", "log", ""},
		"span scope log":   {"log", "span", ""},
		"event_name":       {"log", LogScopeIntrinsic, "string"},
		"event scope":      {"span", "event", ""},
		"bytes type":       {"span", "span", "bytes"},
		"long string type": {"span", "span", "long_string"},
	} {
		_, err := attributeIndex("t", c[0], c[1], c[2])
		requireTyped[*ConfigError](t, name, err)
	}
	if _, err := attributeIndex("t", "metric", "span", ""); err == nil || !strings.Contains(err.Error(), "signal must be span or log") {
		t.Fatalf("unknown signal: %v", err)
	}
	for _, c := range []struct{ limit, max, want int }{{0, 100, 100}, {1, 100, 1}, {100, 100, 100}} {
		if got, err := attributeLimit("t", c.limit, c.max); err != nil || got != c.want {
			t.Errorf("limit %d of %d: %d (%v)", c.limit, c.max, got, err)
		}
	}
	for _, limit := range []int{-1, 101} {
		_, err := attributeLimit("t", limit, 100)
		requireTyped[*ConfigError](t, "limit", err)
	}
}

func TestReaderAttributeText(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		fn   func(tuple.TupleElement) (string, bool)
		in   tuple.TupleElement
		want string
		ok   bool
	}{
		{stringText, "GET", "GET", true},
		{stringText, int64(1), "", false},
		{intText, int64(-404), "-404", true},
		{intText, "404", "0", false},
		{doubleText, -2.25, "-2.25", true},
		{doubleText, math.Copysign(0, -1), "-0", true},
		{doubleText, math.NaN(), "NaN", true},
		{doubleText, int64(2), "0", false},
		{boolText, true, "true", true},
		{boolText, "true", "false", false},
	} {
		if got, ok := c.fn(c.in); ok != c.ok || ok && got != c.want {
			t.Errorf("%T %v: %q %v, want %q %v", c.in, c.in, got, ok, c.want, c.ok)
		}
	}
}

// TraceOTLP and the attribute reads refuse malformed arguments and a
// closed Reader before any read (this Reader has no store).
func TestReaderArgumentRefusals(t *testing.T) {
	t.Parallel()
	r := &Reader{cfg: (ReaderConfig{}).withDefaults()}
	for name, id := range map[string]string{
		"short":     "0102",
		"uppercase": strings.Repeat("AB", 16),
		"zero":      strings.Repeat("0", 32),
	} {
		got, stats, err := r.TraceOTLP(t.Context(), id, TraceBudget{})
		requireTyped[*ConfigError](t, name, err)
		if got == nil || len(got) != 0 || stats != (TraceReadStats{}) {
			t.Fatalf("%s: %v %+v", name, got, stats)
		}
	}
	for name, b := range map[string]TraceBudget{"negative spans": {MaxSpans: -1}, "negative bytes": {MaxBytes: -1}, "negative decoded bytes": {MaxDecodedBytes: -1}} {
		_, _, err := r.TraceOTLP(t.Context(), strings.Repeat("ab", 16), b)
		requireTyped[*ConfigError](t, name, err)
	}
	_, _, err := r.AttributeKeys(t.Context(), "span", "span", "", "", MaxAttributeKeys+1)
	requireTyped[*ConfigError](t, "keys limit", err)
	_, _, err = r.AttributeValues(t.Context(), "span", Attribute{Scope: "span", Key: "k"}, "", 0)
	requireTyped[*ConfigError](t, "values without a type", err)
	_, _, err = r.AttributeValues(t.Context(), "log", Attribute{Scope: "log", Key: "k", Type: "int"}, "", MaxAttributeValues+1)
	requireTyped[*ConfigError](t, "values limit", err)
	if vals, truncated, err := r.AttributeValues(t.Context(), "span", Attribute{Scope: "span", Key: strings.Repeat("k", 1025), Type: "string"}, "", 0); err != nil || len(vals) != 0 || truncated {
		t.Fatalf("over-cap key: %v %v %v", vals, truncated, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = r.TraceOTLP(t.Context(), strings.Repeat("ab", 16), TraceBudget{})
	requireOtelKind(t, err, "operation")
	_, err = r.Query(t.Context(), Attribute{"span", "k", "string", "v"})
	requireOtelKind(t, err, "operation")
	_, _, err = r.AttributeKeys(t.Context(), "span", "span", "", "", 0)
	requireOtelKind(t, err, "operation")
}
