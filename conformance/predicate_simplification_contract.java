package com.birdayz.conformance;

import com.apple.foundationdb.record.EvaluationContext;
import com.apple.foundationdb.record.query.expressions.Comparisons;
import com.apple.foundationdb.record.query.plan.cascades.AliasMap;
import com.apple.foundationdb.record.query.plan.cascades.CorrelationIdentifier;
import com.apple.foundationdb.record.query.plan.cascades.predicates.AndPredicate;
import com.apple.foundationdb.record.query.plan.cascades.predicates.ConstantPredicate;
import com.apple.foundationdb.record.query.plan.cascades.predicates.NotPredicate;
import com.apple.foundationdb.record.query.plan.cascades.predicates.OrPredicate;
import com.apple.foundationdb.record.query.plan.cascades.predicates.QueryPredicate;
import com.apple.foundationdb.record.query.plan.cascades.predicates.ValuePredicate;
import com.apple.foundationdb.record.query.plan.cascades.predicates.simplification.DefaultQueryPredicateRuleSet;
import com.apple.foundationdb.record.query.plan.cascades.predicates.simplification.QueryPredicateWithDnfRuleSet;
import com.apple.foundationdb.record.query.plan.cascades.typing.Type;
import com.apple.foundationdb.record.query.plan.cascades.values.QuantifiedObjectValue;
import com.apple.foundationdb.record.query.plan.cascades.values.simplification.Simplification;

import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

class PredicateSimplificationContract {
    private static QueryPredicate leaf(String name) {
        return new ValuePredicate(QuantifiedObjectValue.of(CorrelationIdentifier.of(name),
                Type.primitiveType(Type.TypeCode.LONG, false)),
                new Comparisons.SimpleComparison(Comparisons.Type.EQUALS, 1L));
    }

    private static QueryPredicate dnf(QueryPredicate predicate) {
        return Simplification.optimize(predicate, EvaluationContext.empty(), AliasMap.emptyMap(), Set.of(),
                QueryPredicateWithDnfRuleSet.ofSimplificationRules()).getUnconstrained();
    }

    private static QueryPredicate simplify(QueryPredicate predicate) {
        return Simplification.optimize(predicate, EvaluationContext.empty(), AliasMap.emptyMap(), Set.of(),
                DefaultQueryPredicateRuleSet.ofSimplificationRules()).getUnconstrained();
    }

    private static void expect(String name, QueryPredicate actual, QueryPredicate expected) {
        assertTree(name, actual, expected);
        System.out.println("PREDICATE-SIMPLIFICATION " + name + " " + actual);
    }

    private static void assertTree(String name, QueryPredicate actual, QueryPredicate expected) {
        if (!actual.semanticEquals(expected, AliasMap.emptyMap()) || actual.isAtomic() != expected.isAtomic()) {
            throw new AssertionError(name + ": " + actual + " (atomic=" + actual.isAtomic() + "), expected " + expected + " (atomic=" + expected.isAtomic() + ")");
        }
        final var actualChildren = actual.getChildren().iterator();
        final var expectedChildren = expected.getChildren().iterator();
        while (actualChildren.hasNext() && expectedChildren.hasNext()) {
            assertTree(name, actualChildren.next(), expectedChildren.next());
        }
        if (actualChildren.hasNext() || expectedChildren.hasNext()) {
            throw new AssertionError(name + ": different child counts");
        }
    }

    private static void fixedFactorPopulation(List<QueryPredicate> factors) {
        int unions = 0;
        int legs = 0;
        final var uniqueLegs = new HashSet<List<QueryPredicate>>();
        for (int mask = 0; mask < (1 << factors.size()) - 1; mask++) {
            final var fixed = new ArrayList<QueryPredicate>();
            final var expanded = new ArrayList<QueryPredicate>();
            for (int i = 0; i < factors.size(); i++) {
                if ((mask & (1 << i)) == 0) {
                    expanded.add(factors.get(i));
                } else {
                    fixed.add(factors.get(i).withAtomicity(true));
                }
            }
            final var normalized = dnf(AndPredicate.and(expanded));
            if (!(normalized instanceof OrPredicate) || normalized.isAtomic()) {
                continue;
            }
            unions++;
            for (final var term : ((OrPredicate)normalized).getChildren()) {
                final var predicates = new ArrayList<>(fixed);
                predicates.add(term);
                uniqueLegs.add(predicates);
                legs++;
            }
        }
        if (unions != 511 || legs != 2898 || uniqueLegs.size() != 2898) {
            throw new AssertionError("fixed-factor population lost its union choices");
        }
        System.out.println("PREDICATE-SIMPLIFICATION fixed-factor-population unions=" + unions
                + " legs=" + legs + " uniqueLegs=" + uniqueLegs.size());
    }

    public static void main(String[] args) {
        final var p = leaf("p");
        final var q = leaf("q");
        final var r = leaf("r");
        final var s = leaf("s");
        final var t = leaf("t");
        final var u = leaf("u");
        final var v = leaf("v");
        final var w = leaf("w");

        expect("children-before-distribution", dnf(AndPredicate.and(
                OrPredicate.or(AndPredicate.and(p, q).withAtomicity(true), AndPredicate.and(p, q, r)),
                OrPredicate.or(s, t))),
                OrPredicate.or(AndPredicate.and(p, q, s), AndPredicate.and(p, q, t)));

        expect("redistribute-after-absorption", dnf(AndPredicate.and(
                OrPredicate.or(p, q).withAtomicity(true), OrPredicate.or(r, s).withAtomicity(true), p)),
                OrPredicate.or(AndPredicate.and(r, p), AndPredicate.and(s, p)));

        final var repeated = dnf(AndPredicate.and(OrPredicate.or(p, u), OrPredicate.or(q, v), OrPredicate.or(p, w)));
        expect("last-duplicate-position", repeated,
                OrPredicate.or(AndPredicate.and(q, p), AndPredicate.and(v, p), AndPredicate.and(u, q, w), AndPredicate.and(u, v, w)));
        if (!((AndPredicate)((OrPredicate)repeated).getChildren().get(0)).getChildren().equals(List.of(q, p))) {
            throw new AssertionError("repeated product must retain the last p position: " + repeated);
        }

        final var atomicRoot = AndPredicate.and(p, AndPredicate.and(OrPredicate.or(q, r), s)).withAtomicity(true);
        expect("normalize-root-only", dnf(atomicRoot), atomicRoot);
        fixedFactorPopulation(List.of(
                OrPredicate.or(p, s, t), OrPredicate.or(q, s, t), OrPredicate.or(r, s, t),
                OrPredicate.or(p, u), OrPredicate.or(q, u), OrPredicate.or(r, u),
                OrPredicate.or(p, v), OrPredicate.or(q, v), OrPredicate.or(r, v)));

        final var atomic = AndPredicate.and(p, p).withAtomicity(true);
        expect("simplify-atomic-root", simplify(atomic), p);

        final var rebuilt = simplify(AndPredicate.and(OrPredicate.or(p, ConstantPredicate.FALSE), q).withAtomicity(true));
        expect("preserve-atomicity-when-rebuilding-children", rebuilt, AndPredicate.and(p, q).withAtomicity(true));
        if (!rebuilt.isAtomic()) {
            throw new AssertionError("rebuilding children lost atomicity");
        }
        expect("demorgan-atomic-child", simplify(NotPredicate.not(AndPredicate.and(p, q).withAtomicity(true))),
                OrPredicate.or(simplify(NotPredicate.not(p)), simplify(NotPredicate.not(q))));
        expect("default-demorgan", simplify(NotPredicate.not(OrPredicate.or(p, q))),
                AndPredicate.and(simplify(NotPredicate.not(p)), simplify(NotPredicate.not(q))));
        expect("default-nested-connective", simplify(AndPredicate.and(p, AndPredicate.and(q, r))),
                AndPredicate.and(p, AndPredicate.and(q, r)));

        for (final var kind : List.of("and", "or", "not")) {
            for (final var promotion : List.of("and_identity", "or_identity", "double_not", "nested")) {
                final var trueAndP = AndPredicate.and(p, q).withChildren(List.of(ConstantPredicate.TRUE, p));
                final QueryPredicate fixed;
                final QueryPredicate expected;
                if (kind.equals("and")) {
                    fixed = trueAndP;
                    expected = promotion.equals("double_not") ? NotPredicate.not(simplify(NotPredicate.not(p))) : p;
                } else if (kind.equals("or")) {
                    fixed = OrPredicate.or(p, AndPredicate.and(p, q).withChildren(List.of(ConstantPredicate.TRUE, q)));
                    expected = promotion.equals("double_not")
                            ? OrPredicate.or(NotPredicate.not(simplify(NotPredicate.not(p))), NotPredicate.not(simplify(NotPredicate.not(q))))
                            : OrPredicate.or(p, q).withAtomicity(true);
                } else {
                    fixed = NotPredicate.not(trueAndP);
                    expected = promotion.equals("double_not")
                            ? NotPredicate.not(NotPredicate.not(simplify(NotPredicate.not(p))))
                            : simplify(NotPredicate.not(p));
                }
                final var atomicFixed = fixed.withAtomicity(true);
                final QueryPredicate input;
                if (promotion.equals("and_identity")) {
                    input = AndPredicate.and(p, q).withChildren(List.of(ConstantPredicate.TRUE, atomicFixed));
                } else if (promotion.equals("or_identity")) {
                    input = OrPredicate.or(ConstantPredicate.FALSE, atomicFixed);
                } else if (promotion.equals("double_not")) {
                    input = NotPredicate.not(NotPredicate.not(atomicFixed));
                } else {
                    input = AndPredicate.and(p, q).withChildren(List.of(ConstantPredicate.TRUE, OrPredicate.or(ConstantPredicate.FALSE, atomicFixed)));
                }
                expect("promoted-atomic-" + kind + "-" + promotion, simplify(input), expected);
            }
        }
    }
}
