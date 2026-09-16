package embedded

import (
	"database/sql/driver"
	"testing"
)

func BenchmarkSubstituteParams_None(b *testing.B) {
	for b.Loop() {
		_, _ = substituteParams("SELECT * FROM t", nil)
	}
}

func BenchmarkSubstituteParams_Three(b *testing.B) {
	args := []driver.NamedValue{
		{Ordinal: 1, Value: int64(42)},
		{Ordinal: 2, Value: "hello"},
		{Ordinal: 3, Value: nil},
	}
	for b.Loop() {
		_, _ = substituteParams("SELECT * FROM t WHERE id = ? AND name = ? AND deleted = ?", args)
	}
}
