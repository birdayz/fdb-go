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
