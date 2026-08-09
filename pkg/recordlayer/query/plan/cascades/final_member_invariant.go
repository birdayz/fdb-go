package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// VerifyOneFinalPlanPerReference walks the reference graph reachable from root
// and reports every reference holding more than one PHYSICAL final expression.
//
// This is RFC-183 P5's precondition, stated as executable code rather than as
// a comment. Java dereferences a quantifier straight to its plan:
//
//	Quantifier.Physical.getRangesOverPlan() =
//	    Iterables.getOnlyElement(getRangesOver().getFinalExpressions())
//
// getOnlyElement THROWS on a group holding two. That single assertion is what
// lets a Java plan hold a Quantifier instead of a raw child pointer — i.e. it
// is what makes the nil-inner shell state unrepresentable. Go cannot adopt
// that shape until the same property holds here.
//
// planner.go's post-drain comment asserts the property already holds ("each
// Reference's FinalMembers has been pruned to exactly one physical plan by
// OptimizeGroup"). This function is how that claim gets checked instead of
// trusted.
//
// Walks the FINAL members only, and follows quantifiers from finals — the
// extraction path. Exploratory members are irrelevant here: extraction never
// reads them, and they are expected to be plural.
//
// TWO KNOWN DEFECTS, both measured, neither yet fixed — see RFC-224 and do not
// read an empty result as proof:
//
//   - The walk terminates at any reference with an EMPTY final set, which is
//     also counted as a non-violation. Measured over the 20 shapes in
//     TestOneFinalPlanPerReference: 43 visits, 21 dead ends, and on the join
//     shape 2 references visited where extraction visits 5.
//   - The property itself is Java's, not Go's. Java's Reference.pruneWith
//     leaves exactly one member; Go's OptimizeGroupTask prunes to a keep set of
//     best-plus-one-per-requested-ordering, so a group holding several physical
//     finals is correct Cascades here, not a violation.
//
// Fixing the first without settling the second would turn a vacuous green into
// a red that is also wrong. RFC-224 settles what Go should assert.
func VerifyOneFinalPlanPerReference(root *expressions.Reference) []string {
	seen := map[*expressions.Reference]bool{}
	var violations []string
	var walk func(ref *expressions.Reference, path string)
	walk = func(ref *expressions.Reference, path string) {
		if ref == nil || seen[ref] {
			return
		}
		seen[ref] = true

		finals := ref.FinalMembers()
		var phys []expressions.RelationalExpression
		for _, m := range finals {
			if _, ok := m.(physicalPlanExpression); ok {
				phys = append(phys, m)
			}
		}
		if len(phys) > 1 {
			kinds := ""
			for i, p := range phys {
				if i > 0 {
					kinds += ", "
				}
				kinds += fmt.Sprintf("%T", p)
			}
			violations = append(violations,
				fmt.Sprintf("%s: %d physical finals [%s]", path, len(phys), kinds))
		}
		// Descend through the finals' quantifiers — the edges extraction
		// actually traverses.
		for i, m := range finals {
			for j, q := range m.GetQuantifiers() {
				walk(q.GetRangesOver(), fmt.Sprintf("%s/m%d/q%d", path, i, j))
			}
		}
	}
	walk(root, "root")
	return violations
}

// SetVerifyOneFinal turns the P5 precondition check on for this planner.
func (p *Planner) SetVerifyOneFinal(on bool) { p.verifyOneFinal = on }

// OneFinalViolations returns what the last planning run found, when the check
// was enabled. Empty means every reference reachable from the root held at
// most one physical final — i.e. Java's getRangesOverPlan would be safe.
func (p *Planner) OneFinalViolations() []string { return p.oneFinalViolations }
