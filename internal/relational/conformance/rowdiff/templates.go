package rowdiff

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/core/embedded"
)

// Template is one directed seed: a deterministic case CONSTRUCTED to plan
// into a required family, asserted per-seed against the typed plan. This is
// the RFC-182 §5 hard coverage gate — a template failure means the family
// became unreachable (or the template rotted), deterministically, never a
// sampling flake. Random seeds carry no per-family gate at smoke scale.
type Template struct {
	Name           string
	RequiredFamily string
	Case           *Case
}

func leaf(col string, op predicates.ComparisonType, lit any) *BoolNode {
	return &BoolNode{Leaf: &Pred{Col: col, Op: op, Lit: lit}}
}

func and(kids ...*BoolNode) *BoolNode { return &BoolNode{And: true, Kids: kids} }

// templateTable builds the fixed table shape templates share: A,B,C BIGINT
// (B NOT NULL), S STRING, F BOOLEAN — same columns as the generator, with
// an explicit index set.
func templateTable(indexes ...IndexDef) TableDef {
	return TableDef{
		Name: "T_RD",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint, NotNull: true},
			{Name: "C", Type: ColBigint},
			{Name: "S", Type: ColString},
			{Name: "F", Type: ColBoolean},
		},
		Indexes: indexes,
	}
}

// templateRows is a small fixed dataset with duplicates on every indexed
// column so intersections and residuals select strict subsets.
func templateRows() []Row {
	rows := make([]Row, 0, 24)
	for i := 0; i < 24; i++ {
		rows = append(rows, Row{
			"ID": int64(i + 1),
			"A":  int64(i % 3),
			"B":  int64(i % 4),
			"C":  int64(i % 5),
			"S":  []string{"alpha", "beta", "gamma"}[i%3],
			"F":  i%2 == 0,
		})
	}
	return rows
}

// Templates returns the P1 directed seeds. Each produces exactly one query
// (SELECT * is the variant under which the target family historically wins;
// the runner still executes all projection variants for row-diffing).
func Templates() []Template {
	return []Template{
		{
			Name:           "intersection",
			RequiredFamily: "Intersection",
			Case: &Case{
				Table: templateTable(
					IndexDef{Name: "IDX_A", Cols: []string{"A"}},
					IndexDef{Name: "IDX_C", Cols: []string{"C"}},
				),
				Rows: templateRows(),
				Queries: []Query{{
					Where: and(
						leaf("A", predicates.ComparisonEquals, int64(1)),
						leaf("C", predicates.ComparisonEquals, int64(2)),
					),
				}},
			},
		},
		{
			// The audit bug's exact shape: both-indexed equalities PLUS an
			// unindexed residual. The compensated intersection must carry
			// the residual — and stay an Intersection.
			Name:           "intersection_residual",
			RequiredFamily: "Intersection",
			Case: &Case{
				Table: templateTable(
					IndexDef{Name: "IDX_A", Cols: []string{"A"}},
					IndexDef{Name: "IDX_C", Cols: []string{"C"}},
				),
				Rows: templateRows(),
				Queries: []Query{{
					Where: and(
						leaf("A", predicates.ComparisonEquals, int64(1)),
						leaf("C", predicates.ComparisonEquals, int64(2)),
						leaf("S", predicates.ComparisonEquals, "alpha"),
					),
				}},
			},
		},
		{
			Name:           "index_scan_residual",
			RequiredFamily: "IndexScan+residual",
			Case: &Case{
				Table: templateTable(IndexDef{Name: "IDX_A", Cols: []string{"A"}}),
				Rows:  templateRows(),
				Queries: []Query{{
					Where: and(
						leaf("A", predicates.ComparisonEquals, int64(1)),
						leaf("S", predicates.ComparisonEquals, "beta"),
					),
				}},
			},
		},
		{
			Name:           "full_scan",
			RequiredFamily: "FullScan",
			Case: &Case{
				Table: templateTable(),
				Rows:  templateRows(),
				Queries: []Query{{
					Where: leaf("B", predicates.ComparisonGreaterThan, int64(1)),
				}},
			},
		},
		{
			Name:           "sort",
			RequiredFamily: "Sort",
			Case: &Case{
				Table: templateTable(IndexDef{Name: "IDX_A", Cols: []string{"A"}}),
				Rows:  templateRows(),
				Queries: []Query{{
					Where:   leaf("A", predicates.ComparisonEquals, int64(1)),
					OrderBy: []OrderKey{{Col: "B"}, {Col: "ID"}},
				}},
			},
		},
	}
}

// CheckTemplateFamily asserts the template's single query plans into its
// required family via the typed plan tree. Returns nil on success.
func CheckTemplateFamily(tpl Template) error {
	c := tpl.Case
	if len(c.Queries) != 1 {
		return fmt.Errorf("template %s must have exactly 1 query, has %d", tpl.Name, len(c.Queries))
	}
	plan, err := embedded.PlanPhysicalForTest(c.SQL(c.Queries[0], nil), c.DDL(), nil)
	if err != nil {
		return fmt.Errorf("template %s failed to plan: %w", tpl.Name, err)
	}
	for _, fam := range classifyPlan(plan) {
		if fam == tpl.RequiredFamily {
			return nil
		}
	}
	return fmt.Errorf("template %s: required family %q not in plan %s (families %v)",
		tpl.Name, tpl.RequiredFamily, plan.Explain(), classifyPlan(plan))
}
