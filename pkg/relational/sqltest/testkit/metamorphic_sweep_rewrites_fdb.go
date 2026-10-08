package testkit

// Equivalence sweep: pairs of query spellings that MUST agree.
//
// The twin oracle compares two access paths and is blind to a defect both share.
// The NoREC oracle compares an optimized predicate against a per-row one and
// found a translator defect the twin could not see. This sweep generalizes that
// idea to a family of rewrites, each of which the engine is free to plan
// differently but not to answer differently:
//
//	IN vs a disjunction of equalities        BETWEEN vs two comparisons
//	De Morgan                                double negation
//	commutativity of AND / OR                idempotence (p AND p, p OR p)
//	DISTINCT vs GROUP BY                     HAVING vs filtering a derived table
//	a nested filter vs a conjunction         COUNT(*) vs COUNT(1)
//	parenthesized vs bare conditions         a condition in CASE vs in WHERE
//
// Each rule is a rewrite a planner might plausibly perform internally, so a
// disagreement is either the rewrite being applied wrongly or the two spellings
// taking different code paths that disagree. The parenthesization rules are
// there because that is exactly where the searched-CASE defect lived.
//
// Every pair is run on the INDEXED schema (where rewriting has the most freedom)
// and the unindexed one (which isolates a translator defect from an access-path
// one), and the counts are compared rather than the row sets where the rewrite
// does not fix an order.

import (
	"fmt"
)

// mmHeadRows truncates a row list for a failure message.
func MmHeadRows(rows []string) []string {
	if len(rows) <= 20 {
		return rows
	}
	return append(append([]string{}, rows[:20]...), fmt.Sprintf("...(+%d more)", len(rows)-20))
}
