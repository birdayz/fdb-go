package values

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// One parser reads the text of every DATE and TIMESTAMP value: four layouts,
// surrounding whitespace trimmed, converted to UTC, and a UTC year outside
// 0000-9999 refused (its canonical text would neither parse nor sort).
func TestParseTemporalText(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"2024-07-04 15:30:45":       "2024-07-04 15:30:45",
		"2024-07-04T15:30:45Z":      "2024-07-04 15:30:45",
		"2024-07-04T15:30:45+02:00": "2024-07-04 13:30:45",
		"2024-07-04T15:30:45":       "2024-07-04 15:30:45",
		"2024-07-04":                "2024-07-04 00:00:00",
		"  2024-07-04 \t":           "2024-07-04 00:00:00",
		"0000-01-01 00:00:00":       "0000-01-01 00:00:00",
		"9999-12-31 23:59:59":       "9999-12-31 23:59:59",
	} {
		got, err := ParseTemporalText(in)
		if err != nil || got.Format(timestampLayout) != want {
			t.Errorf("ParseTemporalText(%q) = %v, %v; want %s", in, got, err, want)
		}
		if err == nil && got.Location() != time.UTC {
			t.Errorf("ParseTemporalText(%q) is not UTC", in)
		}
	}
	for in, outOfDomain := range map[string]bool{
		"9999-12-31T23:00:00-05:00": true,
		"0000-01-01T00:30:00+01:00": true,
		"2024-13-01":                false,
		"15:04:05":                  false,
		"not a date":                false,
		"":                          false,
	} {
		_, err := ParseTemporalText(in)
		var e *TemporalTextError
		if !errors.As(err, &e) || e.OutOfDomain != outOfDomain {
			t.Errorf("ParseTemporalText(%q) = %v; want a TemporalTextError, out of domain %t", in, err, outOfDomain)
		}
	}
}

// The CASTs and the date-part functions read exactly the parser's texts: a
// text is a DATE by CAST exactly when it is a TIMESTAMP by CAST.
func TestTemporalTextConsumers(t *testing.T) {
	t.Parallel()
	cast := func(in any, source, target Type) (any, error) {
		return NewCastValue(&ConstantValue{Value: in, Typ: source}, target).Evaluate(nil)
	}
	for _, c := range []struct {
		in, date, timestamp string
	}{
		{"2024-01-01T23:00:00-05:00", "2024-01-02", "2024-01-02 04:00:00"},
		{" 2024-01-01 ", "2024-01-01", "2024-01-01 00:00:00"},
		{"2024-01-01T10:00:00", "2024-01-01", "2024-01-01 10:00:00"},
	} {
		if got, err := cast(c.in, NotNullString, NullableDate); err != nil || got != c.date {
			t.Errorf("CAST(%q AS DATE) = %v, %v; want %s", c.in, got, err, c.date)
		}
		if got, err := cast(c.in, NotNullString, NullableTimestamp); err != nil || got != c.timestamp {
			t.Errorf("CAST(%q AS TIMESTAMP) = %v, %v; want %s", c.in, got, err, c.timestamp)
		}
	}
	for _, target := range []Type{NullableDate, NullableTimestamp} {
		_, err := cast("9999-12-31T23:00:00-05:00", NotNullString, target)
		var invalid *InvalidCastError
		if !errors.As(err, &invalid) || !strings.Contains(invalid.Message, "0000-9999") {
			t.Errorf("out-of-domain CAST AS %v: want an InvalidCastError naming the domain, got %v", target, err)
		}
	}
	// Epoch milliseconds take the same domain.
	if got, err := cast(int64(0), TypeUnknown, NullableTimestamp); err != nil || got != "1970-01-01 00:00:00" {
		t.Errorf("CAST(0 ms AS TIMESTAMP) = %v, %v", got, err)
	}
	if _, err := cast(int64(253402300800000), TypeUnknown, NullableTimestamp); err == nil {
		t.Error("CAST of epoch milliseconds in year 10000 AS TIMESTAMP must be refused")
	}

	part := func(name string, arg any) (any, error) { return evalScalarFunction(name, []any{arg}) }
	for _, c := range []struct {
		name string
		in   string
		want int64
	}{
		{"YEAR", " 2024-01-01", 2024},
		{"HOUR", "2024-01-01T03:04:05+02:00", 1},
		{"HOUR", "15:04:05", 15},
		{"MINUTE", " 15:04:05 ", 4},
	} {
		if got, err := part(c.name, c.in); err != nil || got != c.want {
			t.Errorf("%s(%q) = %v, %v; want %d", c.name, c.in, got, err, c.want)
		}
	}
	if _, err := part("YEAR", "9999-12-31T23:00:00-05:00"); err == nil {
		t.Error("a date part of an out-of-domain text must be refused")
	}
}

// Every canonical TIMESTAMP text in the domain reads back as its instant.
func FuzzParseTemporalTextRoundTrip(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(1720108245))
	f.Add(int64(-62167219200)) // 0000-01-01 00:00:00
	f.Add(int64(253402300799)) // 9999-12-31 23:59:59
	f.Fuzz(func(t *testing.T, epochSec int64) {
		if epochSec < -62167219200 || epochSec > 253402300799 {
			return
		}
		original := time.Unix(epochSec, 0).UTC()
		text := original.Format(timestampLayout)
		parsed, err := ParseTemporalText(text)
		if err != nil || !parsed.Equal(original) {
			t.Fatalf("%v -> %q -> %v, %v", original, text, parsed, err)
		}
	})
}
