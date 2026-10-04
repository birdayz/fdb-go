package functions

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestResolveTargetTablePath pins Java's SemanticAnalyzer.getTable qualifier
// rules for a statement's target: the qualifier is the schema TEMPLATE's name,
// compared exactly (both sides arrive normalized), and the refusals carry
// Java's SQLSTATE and wording.
func TestResolveTargetTablePath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		path []string
		want string
		code api.ErrorCode
		msg  string
	}{
		{"unqualified", []string{"ORDERS"}, "ORDERS", "", ""},
		{"a quoted dot is one name", []string{"q.q"}, "q.q", "", ""},
		{"the template qualifies", []string{"T", "q.q"}, "q.q", "", ""},
		{"the table keeps its case", []string{"T", "MyTable"}, "MyTable", "", ""},
		{"exact, not folded", []string{"t", "ORDERS"}, "", api.ErrCodeUndefinedDatabase, "Unknown schema template t"},
		{"another qualifier", []string{"S", "ORDERS"}, "", api.ErrCodeUndefinedDatabase, "Unknown schema template S"},
		{"the table's own name", []string{"q", "q"}, "", api.ErrCodeUndefinedDatabase, "Unknown schema template q"},
		{"two qualifiers", []string{"T", "q", "q"}, "", api.ErrCodeInternalError, "Unknown table T.q.q"},
		{"nil", nil, "", api.ErrCodeInternalError, "empty"},
		{"empty", []string{}, "", api.ErrCodeInternalError, "empty"},
		{"empty name", []string{""}, "", api.ErrCodeInternalError, "empty segment"},
		{"empty qualifier", []string{"", "q"}, "", api.ErrCodeInternalError, "empty segment"},
		{"empty qualified name", []string{"T", ""}, "", api.ErrCodeInternalError, "empty segment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTargetTablePath(tc.path, "T")
			if tc.code != "" {
				var coded *api.Error
				if !errors.As(err, &coded) || coded.Code != tc.code || !strings.Contains(coded.Message, tc.msg) {
					t.Fatalf("path %q: got %q / %v, want %s %q", tc.path, got, err, tc.code, tc.msg)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("path %q: got %q / %v, want %q", tc.path, got, err, tc.want)
			}
		})
	}
}

// TestResolveSourceTablePath pins the qualifier half of Java's
// SemanticAnalyzer.tableExists for a FROM source: it names a table only when
// unqualified or qualified by the template's exact name, and otherwise
// reports false (the caller's refusal is Java's "Unknown reference <path>").
func TestResolveSourceTablePath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		path      []string
		want      string
		ok        bool
		malformed bool
	}{
		{"unqualified", []string{"ORDERS"}, "ORDERS", true, false},
		{"the template qualifies", []string{"T", "ORDERS"}, "ORDERS", true, false},
		{"exact, not folded", []string{"t", "ORDERS"}, "", false, false},
		{"another qualifier", []string{"S", "ORDERS"}, "", false, false},
		{"two qualifiers", []string{"T", "A", "ORDERS"}, "", false, false},
		{"nil", nil, "", false, true},
		{"empty segment", []string{"T", ""}, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := ResolveSourceTablePath(tc.path, "T")
			var coded *api.Error
			if malformed := errors.As(err, &coded) && coded.Code == api.ErrCodeInternalError; malformed != tc.malformed || (err != nil && !malformed) {
				t.Fatalf("path %q: error %v, want malformed=%v", tc.path, err, tc.malformed)
			}
			if got != tc.want || ok != tc.ok {
				t.Fatalf("path %q: got %q, %v, want %q, %v", tc.path, got, ok, tc.want, tc.ok)
			}
		})
	}
	// The dotted form splits every dot: it cannot hold a quoted dot, which is
	// why parse-derived callers pass segments.
	if got, ok := ResolveQualifiedTableName("T.ORDERS", "T"); got != "ORDERS" || !ok {
		t.Errorf("dotted template-qualified: %q, %v", got, ok)
	}
	if got, ok := ResolveQualifiedTableName("", "T"); got != "" || ok {
		t.Errorf("empty dotted name: %q, %v", got, ok)
	}
	err := UnknownSourceReferenceError([]string{"S", "w"})
	var coded *api.Error
	if !errors.As(err, &coded) || coded.Code != api.ErrCodeUndefinedColumn || coded.Message != "Unknown reference S.w" {
		t.Errorf("UnknownSourceReferenceError: %v", err)
	}
}
