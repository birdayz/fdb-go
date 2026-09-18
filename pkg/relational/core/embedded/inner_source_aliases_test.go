package embedded

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

// TestInnerSourceAliases_MirrorsUnnestBinder pins the scope-discrimination
// universe for correlated-scalar projections against the unnest BINDER's
// correlation rule (unnestSourceCorrelation: the AS alias, falling back to the
// AT alias only in the AT-only form). With `AS v AT c` the ordinal alias `c`
// is NOT a source correlation — a same-named OUTER alias must classify as
// outer-scoped and take the materialized path (review finding: including the
// AT alias made an outer `(c.id)` read a nonexistent inner key).
func TestInnerSourceAliases_MirrorsUnnestBinder(t *testing.T) {
	t.Parallel()

	scanX := logical.NewScan("arrt", "x")

	// AS+AT form: source correlation is V; the AT alias C is a column, not a
	// source.
	asAt := logical.NewJoin(scanX, &logical.LogicalUnnest{
		Segments: []string{"X", "ARR"}, Alias: "v", AtAlias: "c",
	}, logical.JoinInner, "")
	got := innerSourceAliases(asAt)
	if _, ok := got["X"]; !ok {
		t.Error("scan alias X missing from the inner source universe")
	}
	if _, ok := got["V"]; !ok {
		t.Error("AS alias V missing — it IS the unnest source correlation")
	}
	if _, ok := got["C"]; ok {
		t.Error("AT alias C classified as an inner SOURCE — it is a column bound through V's row; a same-named outer alias would wrongly skip materialization")
	}

	// AT-only form: the AT alias IS the source correlation (the binder's
	// fallback), so it must be in the universe.
	atOnly := logical.NewJoin(scanX, &logical.LogicalUnnest{
		Segments: []string{"X", "ARR"}, AtAlias: "c",
	}, logical.JoinInner, "")
	if _, ok := innerSourceAliases(atOnly)["C"]; !ok {
		t.Error("AT-only form: the AT alias IS the source correlation and must be in the universe")
	}

	// Unaliased scan: the table name is the alias.
	if _, ok := innerSourceAliases(logical.NewScan("orders", ""))["ORDERS"]; !ok {
		t.Error("unaliased scan must contribute its table name")
	}
}

func TestInnerSourceAliasesDerivedDefinitionIsPrivate(t *testing.T) {
	t.Parallel()
	body := logical.NewScan("orders", "HIDDEN")
	main := logical.NewScan("D", "D")
	carrier := logical.NewCTE("D", body, main, false)
	got := innerSourceAliases(carrier)
	if len(got) != 1 {
		t.Fatalf("derived source binds D only, not its hidden body: %v", got)
	}
	if _, ok := got["D"]; !ok {
		t.Fatalf("missing outward binding: %v", got)
	}
}

func TestInnerSourceAliasesBindingIdentity(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "x")
	scan.Binding = "q$scan"
	unaliased := logical.NewScan("orders", "")
	unaliased.Binding = "q$unaliased"
	for _, tc := range []struct {
		name string
		op   logical.LogicalOperator
		want string
	}{
		{"scan", scan, "Q$SCAN"},
		{"unaliased scan", unaliased, "Q$UNALIASED"},
		{"unnest AS", &logical.LogicalUnnest{Segments: []string{"X", "ARR"}, Alias: "v", Binding: "q$unnest"}, "Q$UNNEST"},
		{"unnest AS AT", &logical.LogicalUnnest{Segments: []string{"X", "ARR"}, Alias: "v", AtAlias: "c", Binding: "q$unnest"}, "Q$UNNEST"},
		{"unnest AT", &logical.LogicalUnnest{Segments: []string{"X", "ARR"}, AtAlias: "c", Binding: "q$unnest"}, "Q$UNNEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := innerSourceAliases(tc.op)
			if len(got) != 1 {
				t.Fatalf("only the actual correlation is inner-scoped, not a display alias: %v", got)
			}
			if _, ok := got[tc.want]; !ok {
				t.Fatalf("missing bound correlation %s; an inner projection would classify as outer-owned: %v", tc.want, got)
			}
		})
	}
}
