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
