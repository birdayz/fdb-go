package embedded

// White-box pins for the duplicate-FROM-alias binding-id mint
// (assignFromLegBindingIDs, the single mint authority). Required properties:
// DETERMINISTIC per query (position-keyed, never an atomic counter — two
// parses of the same SQL yield identical ids), first-occurrence-keeps-alias
// (zero change for non-duplicate queries), fold-stable `Q$DUPN` form, and
// structural carry onto the logical legs (LogicalScan/LogicalCTE.Binding) —
// carried, never re-derived.

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

func joinBindings(sq *selectQuery) []string {
	out := make([]string, len(sq.joins))
	for i, j := range sq.joins {
		out[i] = j.bindingID
	}
	return out
}

func TestMintDuplicateFromAliases(t *testing.T) {
	t.Parallel()

	check := func(sql string, want []string) {
		t.Helper()
		sq := parseSelect(t, sql)
		got := joinBindings(sq)
		if len(got) != len(want) {
			t.Fatalf("%s: joins = %v, want %v", sql, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: bindingIDs = %v, want %v", sql, got, want)
			}
		}
	}

	// Non-duplicate queries mint NOTHING (first occurrence keeps the alias —
	// zero change outside the dup class).
	check("SELECT * FROM p, q", []string{""})
	check("SELECT a.id FROM p AS a, q AS b WHERE a.id = b.id", []string{""})

	// Unaliased duplicate tables: `FROM p, q, p` — the later p duplicates the
	// primary; position-keyed (joins[1] is FROM position 2 → Q$DUP2).
	check("SELECT * FROM p, q, p", []string{"", "Q$DUP2"})

	// Aliased duplicates: both later legs duplicate the primary alias.
	check("SELECT a.id FROM p AS a, q AS a", []string{"Q$DUP1"})
	check("SELECT x.id FROM q AS x, p AS x, p AS x", []string{"Q$DUP1", "Q$DUP2"})

	// Case-insensitive alias fold: `AS a` duplicates `AS A`.
	check("SELECT * FROM p AS a, q AS A", []string{"Q$DUP1"})

	// An unaliased table duplicating an ALIAS (and vice versa) folds on the
	// effective source name.
	check("SELECT * FROM q AS p, p", []string{"Q$DUP1"})

	// Derived-table duplicate aliases.
	check("SELECT * FROM (SELECT id FROM p) AS d, (SELECT id FROM p) AS d", []string{"Q$DUP1"})

	// Mint forgery guard: a QUOTED user alias can spell a mint-shaped name —
	// the mint must dodge the FULL leg-key namespace deterministically (a
	// `$`-suffix bump), including a forged alias LATER in the FROM list than
	// the mint position.
	check(`SELECT * FROM p, p, x AS "Q$DUP1"`, []string{"Q$DUP1$", ""})
	check(`SELECT * FROM p, p, x AS "Q$DUP1", y AS "Q$DUP1$"`, []string{"Q$DUP1$$", "", ""})

	// Determinism: two parses of the same SQL mint identical ids.
	a := joinBindings(parseSelect(t, "SELECT * FROM p, q, p, p"))
	b := joinBindings(parseSelect(t, "SELECT * FROM p, q, p, p"))
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("mint not deterministic: %v vs %v", a, b)
		}
	}
	check("SELECT * FROM p, q, p, p", []string{"", "Q$DUP2", "Q$DUP3"})
}

// TestBindingCarriedOntoLogicalLegs pins the structural carry: the logical
// builder copies the minted id onto the join leg's LogicalScan / LogicalCTE —
// consumers must read the carried field, never re-derive it from the alias.
func TestBindingCarriedOntoLogicalLegs(t *testing.T) {
	t.Parallel()

	sq := parseSelect(t, "SELECT a.id FROM p AS a, q AS a")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("expected a logical plan")
	}
	var scans []*logical.LogicalScan
	var walk func(logical.LogicalOperator)
	walk = func(o logical.LogicalOperator) {
		if s, ok := o.(*logical.LogicalScan); ok {
			scans = append(scans, s)
		}
		for _, ch := range o.Children() {
			walk(ch)
		}
	}
	walk(op)
	if len(scans) != 2 {
		t.Fatalf("scans = %d, want 2", len(scans))
	}
	// Tree shape is Join(Left=primary, Right=dup); walk order gives primary
	// first. The primary keeps the alias binding; the duplicate carries the
	// minted id.
	if scans[0].Binding != "" {
		t.Fatalf("primary leg Binding = %q, want \"\" (alias binds)", scans[0].Binding)
	}
	if scans[1].Binding != "Q$DUP1" {
		t.Fatalf("duplicate leg Binding = %q, want Q$DUP1", scans[1].Binding)
	}

	// Derived-table duplicate: the CTE wrapper carries the binding.
	sq2 := parseSelect(t, "SELECT * FROM (SELECT id FROM p) AS d, (SELECT id FROM p) AS d")
	op2 := buildLogicalPlanForSelect(sq2)
	if op2 == nil {
		t.Fatal("expected a logical plan for the derived dup")
	}
	var ctes []*logical.LogicalCTE
	var walk2 func(logical.LogicalOperator)
	walk2 = func(o logical.LogicalOperator) {
		if c, ok := o.(*logical.LogicalCTE); ok {
			ctes = append(ctes, c)
		}
		for _, ch := range o.Children() {
			walk2(ch)
		}
	}
	walk2(op2)
	var dupCTE *logical.LogicalCTE
	for _, c := range ctes {
		if c.Binding != "" {
			dupCTE = c
		}
	}
	if dupCTE == nil || dupCTE.Binding != "Q$DUP1" {
		t.Fatalf("derived duplicate leg must carry Binding Q$DUP1 on its CTE wrapper (ctes: %d, got %+v)", len(ctes), dupCTE)
	}
}
