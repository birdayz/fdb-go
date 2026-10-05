package values

import (
	"fmt"
	"strings"
	"time"
)

// TemporalTextError is a text that is not the text of a DATE or TIMESTAMP
// value. OutOfDomain marks a text that parses but whose UTC year is outside
// 0000-9999.
type TemporalTextError struct {
	Text        string
	OutOfDomain bool
}

func (e *TemporalTextError) Error() string {
	if e.OutOfDomain {
		return fmt.Sprintf("%q is outside the years 0000-9999 in UTC", e.Text)
	}
	return fmt.Sprintf("%q is not a date or timestamp", e.Text)
}

// temporalLayouts are the texts a DATE or TIMESTAMP value may be read from.
var temporalLayouts = []string{timestampLayout, time.RFC3339, "2006-01-02T15:04:05", dateLayout}

// ParseTemporalText reads the text of a DATE or TIMESTAMP value, in UTC. The
// year domain keeps every canonical text parseable and in instant order: an
// offset can carry a year-9999 text into year 10000 in UTC.
func ParseTemporalText(s string) (time.Time, error) {
	text := strings.TrimSpace(s)
	for _, layout := range temporalLayouts {
		t, err := time.Parse(layout, text)
		if err != nil {
			continue
		}
		t = t.UTC()
		if year := t.Year(); year < 0 || year > 9999 {
			return time.Time{}, &TemporalTextError{Text: s, OutOfDomain: true}
		}
		return t, nil
	}
	return time.Time{}, &TemporalTextError{Text: s}
}

// epochMillisTimestamp is the TIMESTAMP text of epoch milliseconds.
func epochMillisTimestamp(ms int64) (string, error) {
	t := time.UnixMilli(ms).UTC()
	if year := t.Year(); year < 0 || year > 9999 {
		return "", &TemporalTextError{Text: fmt.Sprint(ms), OutOfDomain: true}
	}
	return t.Format(timestampLayout), nil
}

// CanonicalTimestampText is the TIMESTAMP text of an instant: UTC, to the second.
func CanonicalTimestampText(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}
