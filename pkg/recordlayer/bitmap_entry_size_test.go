package recordlayer

import (
	"errors"
	"testing"
)

// TestBitmapValueEntrySizeIsReadAsJavaReadsIt pins the entry size to Java's
// BitmapValueIndexMaintainer constructor (BitmapValueIndexMaintainer.java:
// 100-105): Integer.parseInt, 10000 when absent, and "entry size option is too
// large" above 250000. Go read it with strconv and fell back to 10000 on every
// refusal, so "٢" was maintained at 10000 where Java maintains it at 2, and a
// size Java refuses was written. Zero and below are Go's refusal (Java fails
// at the first write).
func TestBitmapValueEntrySizeIsReadAsJavaReadsIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		value  *string
		want   int64
		class  string
		reason string
	}{
		{"absent", nil, 10000, "", ""},
		{"a plain size", ptr("64"), 64, "", ""},
		{"a plus sign", ptr("+2"), 2, "", ""},
		{"Arabic-Indic digits", ptr("\u0662"), 2, "", ""},
		{"the maximum", ptr("250000"), 250000, "", ""},
		{"past the maximum", ptr("250001"), 0, "argument", "entry size option is too large"},
		{"a space", ptr(" 2"), 0, "number", ""},
		{"empty", ptr(""), 0, "number", ""},
		{"zero", ptr("0"), 0, "argument", "entry size option must be positive"},
		{"negative", ptr("-8"), 0, "argument", "entry size option must be positive"},
	} {
		idx := NewIndex("bitmap", GroupBy(Field("price"), Field("order_id")))
		idx.Type = IndexTypeBitmapValue
		if c.value != nil {
			idx.Options[IndexOptionBitmapValueEntrySize] = *c.value
		}
		got, err := BitmapValueEntrySizeOption(idx)
		switch c.class {
		case "":
			if err != nil || got != c.want {
				t.Errorf("%s: %d, %v; want %d", c.name, got, err, c.want)
			}
		case "argument":
			var ae *RecordCoreArgumentError
			if !errors.As(err, &ae) || ae.Message != c.reason {
				t.Errorf("%s: %v (%T), want RecordCoreArgumentError %q", c.name, err, err, c.reason)
			}
		case "number":
			var ne *NumberFormatError
			if !errors.As(err, &ne) {
				t.Errorf("%s: %v (%T), want NumberFormatError", c.name, err, err)
			}
		}
	}
}

func ptr(s string) *string { return &s }
