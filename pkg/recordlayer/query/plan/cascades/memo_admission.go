package cascades

import (
	"bytes"
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// referenceMemberIntent is one member insertion in an invocation-local draft.
// It stays root-owned: expressions owns storage/equality mechanics, while root
// owns the closed logical+physical expression registry.
type referenceMemberIntent struct {
	set        expressions.ReferenceMemberSet
	expression expressions.RelationalExpression
}

type preparedReferenceBatch struct {
	reference        *expressions.Reference
	view             *expressions.ReferenceAdmissionView
	relationType     values.ExactTypeHandle
	exploratory      []expressions.RelationalExpression
	final            []expressions.RelationalExpression
	inserted         []bool
	aliasAwareDedups int
}

// prepareReferenceMemberBatch performs every fallible/method-driven operation
// before returning a batch. In particular, the complete existing+incoming
// population is closed-registry admitted and exact-result-type checked before
// the first HashCodeWithoutChildren/EqualsWithoutChildren invocation.
func prepareReferenceMemberBatch(
	reference *expressions.Reference,
	intents []referenceMemberIntent,
) (*preparedReferenceBatch, error) {
	if reference == nil {
		return nil, memoAdmissionError(values.MemoInvalidHandle, "memo.reference", "Reference is nil")
	}
	view := reference.AdmissionView()
	if view == nil {
		return nil, memoAdmissionError(values.MemoInvalidHandle, "memo.reference", "Reference cannot produce an admission view")
	}
	relationType, err := checkedStoredRelationType(view.ResultType())
	if err != nil {
		return nil, err
	}

	existingExploratory := view.Members(expressions.ReferenceExploratoryMembers)
	existingFinal := view.Members(expressions.ReferenceFinalMembers)
	all := make([]expressions.RelationalExpression, 0, len(existingExploratory)+len(existingFinal)+len(intents))
	all = append(all, existingExploratory...)
	all = append(all, existingFinal...)
	for _, intent := range intents {
		switch intent.set {
		case expressions.ReferenceExploratoryMembers, expressions.ReferenceFinalMembers:
		default:
			return nil, memoAdmissionError(values.MemoBatchConflict, "memo.memberSet", "unknown Reference member set")
		}
		all = append(all, intent.expression)
	}
	if len(all) == 0 {
		return nil, memoAdmissionError(values.MemoEmptyReference, "memo.reference", "empty Reference has no result type")
	}

	// Admission is deliberately a separate complete pass from dedup. An invalid
	// later expression therefore prevents hashing an earlier valid expression.
	admittedTypes := make([]values.ExactTypeHandle, len(all))
	for i, expression := range all {
		relationType, err := admitMemoExpression(expression)
		if err != nil {
			return nil, fmt.Errorf("memo member %d: %w", i, err)
		}
		admittedTypes[i] = relationType
	}

	if relationType == nil {
		relationType = admittedTypes[0]
	}
	want := relationType.CanonicalBytes()
	for i, admittedType := range admittedTypes {
		if !bytes.Equal(want, admittedType.CanonicalBytes()) {
			return nil, memoAdmissionError(
				values.MemoResultTypeMismatch,
				fmt.Sprintf("memo.member[%d].resultType", i),
				fmt.Sprintf("member RELATION result type %v disagrees with its Reference type %v",
					admittedType.Type(), relationType.Type()),
			)
		}
	}

	prepared := &preparedReferenceBatch{
		reference:    reference,
		view:         view,
		relationType: relationType,
		inserted:     make([]bool, len(intents)),
	}
	exploratoryScratch := append([]expressions.RelationalExpression(nil), existingExploratory...)
	finalScratch := append([]expressions.RelationalExpression(nil), existingFinal...)
	for i, intent := range intents {
		var scratch *[]expressions.RelationalExpression
		switch intent.set {
		case expressions.ReferenceExploratoryMembers:
			scratch = &exploratoryScratch
		case expressions.ReferenceFinalMembers:
			scratch = &finalScratch
		}
		duplicate, aliasAware := expressions.PreparedMemberDuplicate(*scratch, intent.expression)
		if duplicate {
			if aliasAware {
				prepared.aliasAwareDedups++
			}
			continue
		}
		prepared.inserted[i] = true
		*scratch = append(*scratch, intent.expression)
		if intent.set == expressions.ReferenceExploratoryMembers {
			prepared.exploratory = append(prepared.exploratory, intent.expression)
		} else {
			prepared.final = append(prepared.final, intent.expression)
		}
	}
	return prepared, nil
}

func (p *preparedReferenceBatch) commit() error {
	if p == nil || p.reference == nil || p.view == nil {
		return memoAdmissionError(values.MemoInvalidHandle, "memo.batch", "prepared Reference batch is nil or incomplete")
	}
	return p.reference.ApplyPreparedMemberBatch(
		p.view,
		p.relationType,
		p.exploratory,
		p.final,
		p.aliasAwareDedups,
	)
}

func checkedStoredRelationType(handle values.ExactTypeHandle) (values.ExactTypeHandle, error) {
	if handle == nil {
		return nil, nil
	}
	exact, ok := values.AsExactTypeHandle(handle)
	if !ok || exact == nil {
		return nil, memoAdmissionError(values.MemoInvalidHandle, "memo.resultType", "Reference contains a foreign exact-type handle")
	}
	inner, relation := exact.RelationInner()
	if !relation || inner == nil {
		return nil, memoAdmissionError(values.MemoMissingRelationWrapper, "memo.resultType", "Reference result type is not RELATION<T>")
	}
	if _, doubled := inner.RelationInner(); doubled {
		return nil, memoAdmissionError(values.MemoDoubleRelationWrapper, "memo.resultType", "Reference result type is RELATION<RELATION<T>>")
	}
	return exact, nil
}

// admitMemoExpression is the closed root registry required before an open
// RelationalExpression method may be invoked. Each case checks typed nil before
// the common GetResultValue path. Embedded or foreign implementations hit the
// default and have no method called at all.
func admitMemoExpression(expression expressions.RelationalExpression) (values.ExactTypeHandle, error) {
	if expression == nil {
		return nil, memoAdmissionError(values.MemoUnsupportedExpression, "memo.member", "expression is nil")
	}
	switch typed := expression.(type) {
	case *expressions.DeleteExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.ExplodeExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.FullUnorderedScanExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.GroupByExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.InsertExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.LogicalDistinctExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.LogicalFilterExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.LogicalIntersectionExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.LogicalLimitExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.LogicalProjectionExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.LogicalSortExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.LogicalTypeFilterExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.LogicalUnionExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.LogicalUniqueExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.LogicalValuesExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.MatchableSortExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.RecursiveUnionExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.SelectExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.TableFunctionExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.TempTableInsertExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.TempTableScanExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *expressions.UpdateExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *scanPlanExpression:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryAggregateIndexPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryComparatorPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryCoveringIndexPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryDefaultOnEmptyPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryDeletePlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryDistinctPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryExplodePlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryFilterPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryFirstOrDefaultPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryFlatMapPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryInJoinPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryInMemorySortPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryInUnionPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryIndexPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryInsertPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryIntersectionPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryLimitPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryLoadByKeysPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryMapPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryMergeSortUnionPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryMultiIntersectionOnValuesPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryNestedLoopJoinPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryPredicatesFilterPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryProjectionPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryRecursiveDfsJoinPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryRecursiveLevelUnionPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryScanPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryScoreForRankPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQuerySelectorPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryStreamingAggregationPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryTableFunctionPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryTempTableInsertPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryTempTableScanPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryTextIndexPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryTypeFilterPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryUnionPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryUnorderedUnionPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryUpdatePlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryValuesPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	case *plans.RecordQueryVectorIndexPlan:
		if typed == nil {
			return nil, unsupportedTypedNilExpression()
		}
	default:
		return nil, memoAdmissionError(values.MemoUnsupportedExpression, "memo.member", "expression concrete type is outside the repository manifest")
	}

	result := expression.GetResultValue()
	if result == nil {
		return nil, memoAdmissionError(values.MemoResultTypeMismatch, "memo.member.result", "expression returned a nil result Value")
	}
	objectType := result.Type()
	if _, alreadyRelation := objectType.(*values.RelationType); alreadyRelation {
		return nil, memoAdmissionError(values.MemoDoubleRelationWrapper, "memo.member.resultType", "expression result Value already has a RELATION type")
	}
	relationType, err := values.ExactRelationOf(objectType)
	if err != nil {
		return nil, fmt.Errorf("memo member result type: %w", err)
	}
	inner, ok := relationType.RelationInner()
	if !ok || inner == nil {
		return nil, memoAdmissionError(values.MemoMissingRelationWrapper, "memo.member.resultType", "failed to construct RELATION<T> result type")
	}
	if _, doubled := inner.RelationInner(); doubled {
		return nil, memoAdmissionError(values.MemoDoubleRelationWrapper, "memo.member.resultType", "expression result Value already has a RELATION type")
	}
	return relationType, nil
}

func unsupportedTypedNilExpression() error {
	return memoAdmissionError(values.MemoUnsupportedExpression, "memo.member", "repository expression is typed nil")
}

func memoAdmissionError(code values.ResolutionErrorCode, path, detail string) error {
	return &values.ResolutionError{ErrorCode: code, Path: path, Detail: detail}
}

// InitialOf, FinalOf, and FinalOfAtStage are the checked root factories. They
// return no Reference until exact admission and the complete singleton commit
// succeed.
func InitialOf(expression expressions.RelationalExpression) (*expressions.Reference, error) {
	return referenceOfAt(expression, expressions.ReferenceExploratoryMembers, expressions.StageCanonical)
}

func FinalOf(expression expressions.RelationalExpression) (*expressions.Reference, error) {
	return referenceOfAt(expression, expressions.ReferenceFinalMembers, expressions.StagePlanned)
}

func FinalOfAtStage(expression expressions.RelationalExpression, stage expressions.PlannerStage) (*expressions.Reference, error) {
	return referenceOfAt(expression, expressions.ReferenceFinalMembers, stage)
}

func referenceOfAt(
	expression expressions.RelationalExpression,
	set expressions.ReferenceMemberSet,
	stage expressions.PlannerStage,
) (*expressions.Reference, error) {
	switch stage {
	case expressions.StageInitial, expressions.StageCanonical, expressions.StagePlanned:
	default:
		return nil, memoAdmissionError(values.MemoBatchConflict, "memo.stage", "unknown planner stage")
	}
	reference := &expressions.Reference{}
	batch, err := prepareReferenceMemberBatch(reference, []referenceMemberIntent{{set: set, expression: expression}})
	if err != nil {
		return nil, err
	}
	if err := batch.commit(); err != nil {
		return nil, err
	}
	if stage != expressions.StageInitial {
		reference.AdvanceStagePreservingMembers(stage)
	}
	return reference, nil
}
