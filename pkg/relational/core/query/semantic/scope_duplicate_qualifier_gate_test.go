package semantic

import (
	"errors"
	"testing"
)

// This file pins WHICH arm of Scope adjudicates a reference through a DUPLICATED
// FROM alias, and what the arm's complement — the references that arm lets
// through — must resolve to.
//
// The motivation is that the arm was twice cited wrongly. Two independent
// readers recorded `ResolveColumn`'s multi-match arm (the BARE one) as the guard
// behind the leg-window readers' qualifier first-match. It is not: a qualified
// reference takes `ResolveQualifiedColumnNested`, and only THAT function's
// multi-match arm ever sees a duplicated qualifier. Deleting the bare arm leaves
// every qualified-reference test green, which is exactly why the mis-citation
// survived — the wrong claim had no failing test attached to it.
//
// A control over ABSENT code has no runtime form, so the distinction is pinned
// through a field that only one arm populates: AmbiguousColumnError.Qualifier is
// set by the qualified arm and left zero by the bare one. Asserting it makes the
// two arms tell themselves apart at runtime instead of in a comment.
//
// The SECOND half of this file matters more than the first. The arm fires on
// `len(matches) > 1` for ONE column reference, so a duplicated alias whose two
// sources carry DISJOINT columns produces one match per reference and resolves.
// That is not a hole — it is Java's per-attribute rule (SemanticAnalyzer.lookup
// walks every operator's output and counts candidates for the reference, with no
// alias-uniqueness check anywhere; the file's only DUPLICATE_ALIAS assert,
// SemanticAnalyzer.java:180, is CTE-name lookup). But it means the safety of the
// shape rests entirely on the per-attribute binding picking the RIGHT source,
// and a regression to first-match-by-alias would still return rows. Those are
// the assertions that would catch it.

// dupAliasScope builds one scope level holding TWO sources under the SAME alias
// `a`: `left` carries LID and SHARED, `right` carries RID and SHARED. So
// `a.shared` is the arm's shape and `a.lid` / `a.rid` are its complement.
func dupAliasScope(t *testing.T) *Scope {
	t.Helper()
	left := &StaticTable{
		TableName: ParseQualifiedName("zn", false),
		TableColumns: []Column{
			{Id: NewUnquoted("lid"), Type: "INT"},
			{Id: NewUnquoted("shared"), Type: "INT"},
		},
	}
	right := &StaticTable{
		TableName: ParseQualifiedName("zp", false),
		TableColumns: []Column{
			{Id: NewUnquoted("rid"), Type: "INT"},
			{Id: NewUnquoted("shared"), Type: "INT"},
		},
	}
	s := NewScope(nil)
	if err := s.AddSource(ScopeSource{Table: left, Alias: NewUnquoted("a"), CorrelationName: "Q$DUP1"}); err != nil {
		t.Fatalf("AddSource left: %v", err)
	}
	// A duplicate PLAIN alias is accepted at declaration on purpose — Java has no
	// declaration-time alias-uniqueness check and adjudicates per attribute. If
	// this ever starts failing, the guard MOVED to declaration time, which is a
	// deliberate Postgres-ward divergence from Java and needs its own ruling —
	// not a quiet re-bless here.
	if err := s.AddSource(ScopeSource{Table: right, Alias: NewUnquoted("a"), CorrelationName: "Q$DUP2"}); err != nil {
		t.Fatalf("A DUPLICATE FROM ALIAS WAS REJECTED AT DECLARATION: %v\n"+
			"  Java accepts the declaration (no alias-uniqueness check exists in\n"+
			"  SemanticAnalyzer; its only DUPLICATE_ALIAS assert is CTE-name lookup)\n"+
			"  and rejects per ATTRIBUTE at reference resolution. Fifteen\n"+
			"  dup_from_alias_* cross-engine corpus entries are live-verified\n"+
			"  against Java on that basis. Moving the rejection here is a\n"+
			"  behaviour change against the spec, not a hardening.", err)
	}
	return s
}

// TestScope_DuplicateQualifierHitsTheQualifiedArm pins that a reference whose
// duplicated qualifier carries the column on BOTH sources is refused by
// ResolveQualifiedColumn's multi-match arm — and that the error identifies
// itself as coming from that arm.
func TestScope_DuplicateQualifierHitsTheQualifiedArm(t *testing.T) {
	t.Parallel()
	s := dupAliasScope(t)

	_, _, err := s.ResolveQualifiedColumn(NewUnquoted("a"), NewUnquoted("shared"))
	if err == nil {
		t.Fatal("A REFERENCE THROUGH A DUPLICATED FROM ALIAS RESOLVED.\n" +
			"  `a.shared` is carried by BOTH sources aliased `a`, so no honest\n" +
			"  single answer exists. This is the WHOLE defence for the shape:\n" +
			"  downstream leg-window readers select a leg by matching the\n" +
			"  qualifier TEXT first-match, and the loser of that first match is a\n" +
			"  real column of the same type. So a relaxation here does not\n" +
			"  surface as an error downstream — it surfaces as WRONG ROWS.")
	}
	var amb *AmbiguousColumnError
	if !errors.As(err, &amb) {
		t.Fatalf("wrong error type for a duplicated qualifier: got %T (%v), want *AmbiguousColumnError.\n"+
			"  A refusal for some other reason would let this test pass for the\n"+
			"  wrong reason once the ambiguity check is gone.", err, err)
	}
	// The executable form of "this came from the QUALIFIED arm". The bare arm
	// leaves Qualifier zero (pinned below); nothing else distinguishes them at
	// runtime, and the distinction is precisely what two readers got wrong.
	if amb.Qualifier.Name() != "A" {
		t.Fatalf("ambiguity was raised WITHOUT a qualifier: Qualifier=%q, Reference=%q.\n"+
			"  Only ResolveQualifiedColumnNested's multi-match arm populates\n"+
			"  Qualifier. An empty one means the refusal came from the BARE arm\n"+
			"  (ResolveColumn) instead — which is the mis-citation this file\n"+
			"  exists to prevent, and which does NOT guard qualified references.",
			amb.Qualifier.Name(), amb.Reference())
	}
	if got, want := amb.Reference(), "A.SHARED"; got != want {
		t.Fatalf("rendered reference: got %q, want %q", got, want)
	}
	if len(amb.Sources) != 2 || amb.Matches != 2 {
		t.Fatalf("expected exactly 2 candidate sources, got Matches=%d Sources=%v", amb.Matches, amb.Sources)
	}
}

// TestScope_BareArmLeavesTheQualifierEmpty is the NEGATIVE CONTROL for the
// mis-citation. It pins that the bare arm is a genuinely different arm producing
// a genuinely different error value — so "removing ResolveColumn's arm does not
// affect a qualified reference" stops being a claim in prose and becomes a fact
// with a runtime witness.
func TestScope_BareArmLeavesTheQualifierEmpty(t *testing.T) {
	t.Parallel()
	s := dupAliasScope(t)

	_, _, err := s.ResolveColumn(NewUnquoted("shared"))
	var amb *AmbiguousColumnError
	if !errors.As(err, &amb) {
		t.Fatalf("bare ambiguous reference: got %T (%v), want *AmbiguousColumnError", err, err)
	}
	if amb.Qualifier.Name() != "" {
		t.Fatalf("the BARE arm populated Qualifier (%q).\n"+
			"  The two multi-match arms are then indistinguishable at runtime, and\n"+
			"  the qualified-arm pin above can no longer prove which arm refused.",
			amb.Qualifier.Name())
	}
	if got, want := amb.Reference(), "SHARED"; got != want {
		t.Fatalf("rendered bare reference: got %q, want %q", got, want)
	}
}

// TestScope_DuplicateQualifierPerAttributeBindsTheRightSource pins the arm's
// COMPLEMENT — the references a duplicated alias lets through because only one
// of the two sources carries the column.
//
// This is where a silent wrong-column read would actually live. The arm counts
// matches per COLUMN, not per ALIAS, so `a.rid` yields exactly one match and
// resolves. It must resolve against the RIGHT source. A regression to
// first-match-by-alias would answer `a.rid` off the LEFT source (or fail to find
// it), and for a same-typed column the observable consequence is wrong rows
// rather than an error.
func TestScope_DuplicateQualifierPerAttributeBindsTheRightSource(t *testing.T) {
	t.Parallel()
	s := dupAliasScope(t)

	for _, tc := range []struct{ col, wantTable, wantCorrelation string }{
		{"lid", "ZN", "Q$DUP1"},
		{"rid", "ZP", "Q$DUP2"},
	} {
		col, src, err := s.ResolveQualifiedColumn(NewUnquoted("a"), NewUnquoted(tc.col))
		if err != nil {
			t.Fatalf("a.%s through a duplicated alias did not resolve: %v\n"+
				"  Only one of the two same-aliased sources carries it, so Java's\n"+
				"  per-attribute rule gives exactly one candidate. Refusing it is a\n"+
				"  divergence from the spec and from 16 live-verified\n"+
				"  dup_from_alias_* corpus entries.", tc.col, err)
		}
		if got := src.Table.Name().String(); got != tc.wantTable {
			t.Fatalf("a.%s RESOLVED AGAINST THE WRONG SOURCE: got table %q, want %q.\n"+
				"  Both sources answer to alias `a`, so picking by alias alone is a\n"+
				"  coin flip. Binding the wrong one returns a real column of the\n"+
				"  same type — the failure is WRONG ROWS, not an error.",
				tc.col, got, tc.wantTable)
		}
		if src.CorrelationName != tc.wantCorrelation {
			t.Fatalf("a.%s bound correlation %q, want %q — the runtime key that keeps "+
				"the two same-aliased legs apart", tc.col, src.CorrelationName, tc.wantCorrelation)
		}
		if got := col.Id.Name(); got != NewUnquoted(tc.col).Name() {
			t.Fatalf("a.%s resolved to column %q", tc.col, got)
		}
	}
}
