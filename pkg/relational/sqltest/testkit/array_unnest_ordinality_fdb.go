package testkit

import (
	"fmt"
	"reflect"
	"strings"

	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func UnnestSprint(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case string:
		return x
	case *executor.PositionalRow:
		return unnestOrdinalRecordSprint(x)
	}
	// A SLICE slot is rendered element-wise rather than handed to fmt whole.
	//
	// fmt.Sprint flattens a composite to one blob — `[]any{"a  b"}` becomes
	// `[a  b]` — and at that point unnestCollapseSpaces cannot tell a prototext
	// separator from a string's OWN double space, so it rewrites the DATA. That
	// is reachable here: SARR and STRARR are STRING arrays, and `SELECT *` puts
	// the whole array in a slot. Rendering element-wise reproduces fmt's exact
	// slice form ("[" + elements joined by one space + "]") while routing each
	// element back through this function, so a nested string stays verbatim and
	// a nested message is still collapsed.
	//
	// A Stringer/error is excluded because fmt would call its String method
	// rather than walk it, and reproducing fmt means deferring to that too.
	switch v.(type) {
	case fmt.Stringer, error:
	default:
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			parts := make([]string, rv.Len())
			for i := range parts {
				parts[i] = UnnestSprint(rv.Index(i).Interface())
			}
			return "[" + strings.Join(parts, " ") + "]"
		}
	}
	return unnestCollapseSpaces(fmt.Sprint(v))
}

// unnestOrdinalRecordSprint is the structural counterpart of protobuf's text
// rendering for a record-valued UNNEST element. The executor deliberately
// keeps such an element as an exact PositionalRow so chained FieldValues can
// continue to read it by ordinal; test rendering must not leak that Go
// transport's pointer-bearing fmt form. It renders from the immutable
// RecordType + slots, preserving field and repeated-element order.
func unnestOrdinalRecordSprint(row *executor.PositionalRow) string {
	if row == nil {
		return "<nil>"
	}
	if row.Type == nil || len(row.Type.Fields) != len(row.Slots) {
		return fmt.Sprintf("<MALFORMED ORDINAL RECORD: type=%v slots=%d>", row.Type, len(row.Slots))
	}
	parts := make([]string, 0, len(row.Slots))
	for i, field := range row.Type.Fields {
		value := row.Slots[i]
		if value == nil {
			continue
		}
		if arrayType, ok := field.FieldType.(*values.ArrayType); ok {
			rv := reflect.ValueOf(value)
			if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
				parts = append(parts, field.Name+":"+UnnestSprint(value))
				continue
			}
			for j := 0; j < rv.Len(); j++ {
				element := rv.Index(j).Interface()
				if arrayType.ElementType != nil && arrayType.ElementType.Code() == values.TypeCodeRecord {
					parts = append(parts, field.Name+":{"+UnnestSprint(element)+"}")
				} else {
					parts = append(parts, field.Name+":"+UnnestSprint(element))
				}
			}
			continue
		}
		if field.FieldType != nil && field.FieldType.Code() == values.TypeCodeRecord {
			parts = append(parts, field.Name+":{"+UnnestSprint(value)+"}")
			continue
		}
		parts = append(parts, field.Name+":"+UnnestSprint(value))
	}
	return strings.Join(parts, " ")
}

// unnestCollapseSpaces makes the non-string rendering STABLE ACROSS BUILDS.
//
// A row slot can hold a protobuf message (an unnested struct element, a nested
// repeated field), and fmt renders those through prototext, whose whitespace is
// deliberately unstable: google.golang.org/protobuf/internal/detrand varies the
// separator width so that callers cannot depend on the exact bytes. The seed is
// derived from the binary, so the output is stable WITHIN one test binary and
// flips the moment anything changes the binary — an expectation written against
// it passes locally and fails on the next unrelated edit.
//
// That instability is why the duplicate-name star row in
// buried_chained_rotation_fdb_test.go could only be pinned by COUNT: its slots
// are messages, so no literal could survive a rebuild.
//
// WHAT IT CANNOT DISTINGUISH, stated rather than assumed. This collapse operates
// on a RENDERED string, so a double space that came from the DATA is
// indistinguishable from one prototext inserted. unnestSprint keeps that reach as
// small as it can: a top-level string slot is returned raw, and a slice slot is
// rendered element-wise so its string elements are raw too (both pinned in
// TestUnnestSprintIsStableAcrossBuilds). What remains is a string nested inside a
// value fmt renders as ONE blob — a proto message's own string field, a struct
// field. Its spacing IS collapsed. An earlier version of this note claimed no
// real text was ever touched; that was false for composites, and this is the
// honest boundary.
func unnestCollapseSpaces(s string) string {
	if !strings.Contains(s, "  ") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteByte(c)
	}
	return b.String()
}

func UnnestEqualStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
