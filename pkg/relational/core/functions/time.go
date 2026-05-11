package functions

import "time"

// TimestampLayout is the canonical ISO 8601 format used for TIMESTAMP
// values in proto storage and SQL display.
const TimestampLayout = "2006-01-02 15:04:05"

// DateLayout is the canonical ISO 8601 format used for DATE values.
const DateLayout = "2006-01-02"

// FormatTimestamp formats a time.Time as the canonical TIMESTAMP string.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(TimestampLayout)
}

// FormatDate formats a time.Time as the canonical DATE string (date only).
func FormatDate(t time.Time) string {
	return t.UTC().Format(DateLayout)
}

// ParseTimestamp attempts to parse a string as a TIMESTAMP using
// multiple common layouts. Returns the parsed time in UTC or false.
func ParseTimestamp(s string) (time.Time, bool) {
	for _, layout := range []string{
		TimestampLayout,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999",
		DateLayout,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
