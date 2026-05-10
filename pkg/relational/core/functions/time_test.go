package functions

import (
	"testing"
	"time"
)

func TestFormatTimestamp(t *testing.T) {
	t.Parallel()
	ts := time.Date(2024, 7, 4, 15, 30, 45, 0, time.UTC)
	got := FormatTimestamp(ts)
	if got != "2024-07-04 15:30:45" {
		t.Errorf("FormatTimestamp = %q, want %q", got, "2024-07-04 15:30:45")
	}
}

func TestFormatTimestamp_NonUTC(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("EST", -5*3600)
	ts := time.Date(2024, 7, 4, 20, 0, 0, 0, loc) // 20:00 EST = 01:00 UTC next day
	got := FormatTimestamp(ts)
	if got != "2024-07-05 01:00:00" {
		t.Errorf("FormatTimestamp (non-UTC) = %q, want %q", got, "2024-07-05 01:00:00")
	}
}

func TestFormatDate(t *testing.T) {
	t.Parallel()
	ts := time.Date(2024, 12, 25, 23, 59, 59, 0, time.UTC)
	got := FormatDate(ts)
	if got != "2024-12-25" {
		t.Errorf("FormatDate = %q, want %q", got, "2024-12-25")
	}
}

func TestParseTimestamp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  time.Time
		ok    bool
	}{
		{"standard", "2024-07-04 15:30:45", time.Date(2024, 7, 4, 15, 30, 45, 0, time.UTC), true},
		{"date_only", "2024-07-04", time.Date(2024, 7, 4, 0, 0, 0, 0, time.UTC), true},
		{"iso_t_separator", "2024-07-04T15:30:45", time.Date(2024, 7, 4, 15, 30, 45, 0, time.UTC), true},
		{"iso_with_tz", "2024-07-04T15:30:45Z", time.Date(2024, 7, 4, 15, 30, 45, 0, time.UTC), true},
		{"midnight", "2024-01-01 00:00:00", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"invalid", "not-a-date", time.Time{}, false},
		{"empty", "", time.Time{}, false},
		{"partial", "2024-07", time.Time{}, false},
		{"time_only", "15:30:45", time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseTimestamp(tt.input)
			if ok != tt.ok {
				t.Fatalf("ParseTimestamp(%q) ok = %v, want %v", tt.input, ok, tt.ok)
			}
			if ok && !got.Equal(tt.want) {
				t.Errorf("ParseTimestamp(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseTimestamp_RoundTrip(t *testing.T) {
	t.Parallel()
	original := time.Date(2024, 3, 15, 10, 30, 0, 0, time.UTC)
	formatted := FormatTimestamp(original)
	parsed, ok := ParseTimestamp(formatted)
	if !ok {
		t.Fatalf("ParseTimestamp(FormatTimestamp(%v)) failed", original)
	}
	if !parsed.Equal(original) {
		t.Errorf("round-trip: got %v, want %v", parsed, original)
	}
}

func TestFormatDate_RoundTrip(t *testing.T) {
	t.Parallel()
	original := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	formatted := FormatDate(original)
	parsed, ok := ParseTimestamp(formatted)
	if !ok {
		t.Fatalf("ParseTimestamp(FormatDate(%v)) failed", original)
	}
	if parsed.Year() != original.Year() || parsed.Month() != original.Month() || parsed.Day() != original.Day() {
		t.Errorf("round-trip: got %v, want same date as %v", parsed, original)
	}
}

func FuzzParseTimestamp(f *testing.F) {
	f.Add("2024-07-04 15:30:45")
	f.Add("2024-07-04")
	f.Add("2024-07-04T15:30:45Z")
	f.Add("2024-07-04T15:30:45")
	f.Add("")
	f.Add("not-a-date")
	f.Add("9999-12-31 23:59:59")
	f.Add("1970-01-01 00:00:00")
	f.Add("2024-02-29")
	f.Add("2024-13-01 00:00:00")

	f.Fuzz(func(t *testing.T, s string) {
		_, _ = ParseTimestamp(s)
	})
}

func FuzzFormatParseRoundTrip(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(1720108245))
	f.Add(int64(-1))
	f.Add(int64(253402300799)) // 9999-12-31 23:59:59

	f.Fuzz(func(t *testing.T, epochSec int64) {
		if epochSec < -62135596800 || epochSec > 253402300799 {
			return
		}
		original := time.Unix(epochSec, 0).UTC()
		formatted := FormatTimestamp(original)
		parsed, ok := ParseTimestamp(formatted)
		if !ok {
			t.Fatalf("ParseTimestamp(FormatTimestamp(%v)) failed; formatted=%q", original, formatted)
		}
		if !parsed.Equal(original) {
			t.Fatalf("round-trip mismatch: %v → %q → %v", original, formatted, parsed)
		}
	})
}
