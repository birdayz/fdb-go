package com.birdayz.conformance;

import com.apple.foundationdb.record.EvaluationContext;
import com.apple.foundationdb.record.query.expressions.Comparisons;
import com.apple.foundationdb.record.query.plan.cascades.CorrelationIdentifier;
import com.apple.foundationdb.record.query.plan.cascades.ValueEquivalence;
import com.apple.foundationdb.record.query.plan.cascades.predicates.ConstantPredicate;
import com.apple.foundationdb.record.query.plan.cascades.predicates.OrPredicate;
import com.apple.foundationdb.record.query.plan.cascades.predicates.Placeholder;
import com.apple.foundationdb.record.query.plan.cascades.predicates.QueryPredicate;
import com.apple.foundationdb.record.query.plan.cascades.predicates.ValuePredicate;
import com.apple.foundationdb.record.query.plan.cascades.values.LiteralValue;

import java.util.Collections;
import java.util.IdentityHashMap;
import java.util.List;

class PredicateMappingContract {
    private static QueryPredicate disjunction() {
        return OrPredicate.or(List.of(
                new ValuePredicate(LiteralValue.ofScalar(1L), new Comparisons.SimpleComparison(Comparisons.Type.EQUALS, 2L)),
                new ValuePredicate(LiteralValue.ofScalar(2L), new Comparisons.SimpleComparison(Comparisons.Type.LESS_THAN, 1L)),
                new ValuePredicate(LiteralValue.ofScalar(2L), new Comparisons.SimpleComparison(Comparisons.Type.GREATER_THAN, 4L))));
    }

    public static void main(String[] args) {
        final var x = Placeholder.newInstanceWithoutRanges(LiteralValue.ofScalar(1L), CorrelationIdentifier.of("x"));
        final var y = Placeholder.newInstanceWithoutRanges(LiteralValue.ofScalar(2L), CorrelationIdentifier.of("y"));
        for (boolean atomic : new boolean[]{false, true}) {
            final var query = disjunction().withAtomicity(atomic);
            final var hints = query.findImpliedMappings(ValueEquivalence.empty(), query, List.of(x, y), EvaluationContext.empty());
            if (hints.size() != 1 || hints.iterator().next().getParameterAliasOptional().isPresent()) {
                throw new AssertionError("duplicate OR hint keys or fabricated scan bound: " + hints.size());
            }
            final var withRegular = query.findImpliedMappings(ValueEquivalence.empty(), query,
                    List.of(x, y, new ConstantPredicate(true)), EvaluationContext.empty());
            if (withRegular.size() != 2) {
                throw new AssertionError("mapping kind must distinguish the regular TRUE mapping from the OR hint");
            }
            System.out.println("PREDICATE-MAPPING atomic=" + atomic + " hints=" + hints.size() + " kinds=" + withRegular.size());
        }

        final var query = disjunction();
        final var first = disjunction();
        final var second = disjunction();
        final var mappings = query.findImpliedMappings(ValueEquivalence.empty(), query, List.of(first, second), EvaluationContext.empty());
        if (mappings.size() != 1 || mappings.iterator().next().getCandidatePredicate() != first) {
            throw new AssertionError("semantic MappingKey deduplication must retain the first candidate");
        }
        final var remaining = Collections.newSetFromMap(new IdentityHashMap<QueryPredicate, Boolean>());
        remaining.add(first);
        remaining.add(second);
        mappings.forEach(mapping -> remaining.remove(mapping.getCandidatePredicate()));
        remaining.removeIf(QueryPredicate::isTautology);
        if (remaining.size() != 1 || !remaining.contains(second)) {
            throw new AssertionError("Select's identity-based coverage gate must reject the unmapped candidate twin");
        }
        System.out.println("PREDICATE-MAPPING opaque-mappings=1 uncovered-identities=1");
    }
}
