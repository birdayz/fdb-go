package semantic

import (
	"errors"
	"testing"
)

// nestedDeclineScope builds a scope with ONE source carrying a struct column,
// so `N.CO` resolves by DESCENT and `T.ID` resolves directly.
func nestedDeclineScope(t *testing.T) *Scope {
	t.Helper()
	tbl := &StaticTable{
		TableName: ParseQualifiedName("T", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "BIGINT"},
			{Id: NewUnquoted("n"), Type: "RECORD", StructFields: []Column{
				{Id: NewUnquoted("sk"), Type: "BIGINT"},
				{Id: NewUnquoted("co"), Type: "BIGINT"},
			}},
		},
	}
	s := NewScope(nil)
	if err := s.AddSource(ScopeSource{
		Table: tbl, Alias: NewUnquoted("t"), CorrelationName: "T",
	}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	return s
}

// TestResolveQualifiedColumnDeclinesADescent pins the chain-free lookup's
// refusal to answer a resolution that descended into a struct column.
//
// The refusal is the whole contract. ResolveQualifiedColumn returns
// (Column, ScopeSource) with no accessor chain, and for a descent the Column it
// COULD return is the struct ROOT — not the field the reference named. A caller
// receiving the root has been handed a wrong column with no way to tell: the
// value is well-formed, the error is nil, and the only symptom appears far
// downstream as a struct where a scalar was expected.
//
// This is the same defect class that made a qualified projection read the whole
// struct. That one was closed by routing every mint through the nested form;
// this closes the other door, so the class cannot be reopened by a future caller
// picking the shorter-named function.
func TestResolveQualifiedColumnDeclinesADescent(t *testing.T) {
	t.Parallel()
	s := nestedDeclineScope(t)

	// POSITIVE CONTROL: a DIRECT match still answers. Without this the decline
	// below is satisfied by a function that refuses everything.
	col, src, err := s.ResolveQualifiedColumn(NewUnquoted("t"), NewUnquoted("id"))
	if err != nil {
		t.Fatalf("direct T.ID: %v — the chain-free form must still answer for the "+
			"references that carry no chain, which is every reference that "+
			"addresses a source column", err)
	}
	if col.Id.Name() != "ID" || src.Alias.Name() != "T" {
		t.Fatalf("direct T.ID resolved to %q on %q, want ID on T",
			col.Id.Name(), src.Alias.Name())
	}

	// THE DECLINE: `N.CO` descends into the struct column N.
	got, gotSrc, err := s.ResolveQualifiedColumn(NewUnquoted("n"), NewUnquoted("co"))
	var nested *NestedResolutionError
	if !errors.As(err, &nested) {
		t.Fatalf("N.CO returned column %q on source %q with err %v.\n"+
			"  A descent has no answer this form can express: the Column it would "+
			"hand back is the struct ROOT N, not the member CO, and a caller cannot "+
			"detect the substitution. It must decline with NestedResolutionError "+
			"and let the caller use ResolveQualifiedColumnNested.",
			got.Id.Name(), gotSrc.Alias.Name(), err)
	}
	if nested.Qualifier.Name() != "N" || nested.Id.Name() != "CO" {
		t.Fatalf("decline names %s.%s, want N.CO", nested.Qualifier.Name(), nested.Id.Name())
	}
	// The declined result carries nothing — a caller that ignores the error must
	// not find a usable-looking struct root sitting in the Column return.
	if got.Id.Name() != "" || gotSrc.Alias.Name() != "" {
		t.Fatalf("the decline still returned column %q on source %q; it must return "+
			"zero values, because a caller that ignores the error would otherwise "+
			"read the struct root exactly as before",
			got.Id.Name(), gotSrc.Alias.Name())
	}

	// The NESTED form answers the same reference, with the chain.
	nc, _, accessors, nerr := s.ResolveQualifiedColumnNested(NewUnquoted("n"), NewUnquoted("co"))
	if nerr != nil {
		t.Fatalf("nested N.CO: %v — the decline above is only correct because this "+
			"form answers", nerr)
	}
	if nc.Id.Name() != "N" {
		t.Fatalf("nested form's root column is %q, want N", nc.Id.Name())
	}
	if len(accessors) != 1 || accessors[0].Name != "CO" || accessors[0].Ordinal != 1 {
		t.Fatalf("nested form returned chain %v, want one accessor CO at ordinal 1 "+
			"(SK is 0)", accessors)
	}
}
