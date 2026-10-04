package expressions

import (
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type correlationMemo struct {
	version      uint64
	correlations map[values.CorrelationIdentifier]struct{}
	dependencies []correlationDependency
}

type correlationDependency struct {
	reference *Reference
	snapshot  *correlationMemo
}

type referenceCorrelationReader struct {
	memo        map[*Reference]*correlationMemo
	expressions map[RelationalExpression]*correlationMemo
	active      map[*Reference]struct{}
	publish     bool
}

var correlationReadScratch = sync.Pool{
	New: func() any { return &referenceCorrelationReader{} },
}

func (reader *referenceCorrelationReader) reset() {
	clear(reader.memo)
	clear(reader.expressions)
	clear(reader.active)
	reader.publish = false
}

// GetCorrelatedTo returns the transitive free aliases across both member lanes.
// The returned map is borrowed and immutable. Concurrent reads require a stable
// graph; descendant edits and forwarding are checked before reusing a snapshot.
func (r *Reference) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	r = canonicalReferenceReadOnly(r)
	if r == nil {
		return nil
	}
	if cached := r.correlatedToCache.Load(); cached != nil && cached.version == r.memberVersion && len(cached.dependencies) == 0 {
		return cached.correlations
	}
	reader := correlationReadScratch.Get().(*referenceCorrelationReader)
	// Only traversal scratch is recycled; published snapshots never refer to
	// these maps. Clearing releases all references to the previous graph.
	defer func() {
		reader.reset()
		correlationReadScratch.Put(reader)
	}()
	reader.publish = true
	return reader.correlatedTo(r)
}

func (reader *referenceCorrelationReader) expression(expression RelationalExpression) map[values.CorrelationIdentifier]struct{} {
	return reader.expressionSnapshot(expression).correlations
}

func (reader *referenceCorrelationReader) expressionSnapshot(expression RelationalExpression) *correlationMemo {
	if cached, ok := reader.expressions[expression]; ok {
		return cached
	}
	if reader.expressions == nil {
		reader.expressions = make(map[RelationalExpression]*correlationMemo)
	}
	computed := &correlationMemo{}
	computed.correlations = expressionCorrelations(expression, func(child *Reference) map[values.CorrelationIdentifier]struct{} {
		snapshot := reader.reference(child)
		computed.dependencies = append(computed.dependencies, correlationDependency{child, snapshot})
		if snapshot == nil {
			return nil
		}
		return snapshot.correlations
	})
	reader.expressions[expression] = computed
	return computed
}

func (reader *referenceCorrelationReader) correlatedTo(ref *Reference) map[values.CorrelationIdentifier]struct{} {
	if snapshot := reader.reference(ref); snapshot != nil {
		return snapshot.correlations
	}
	return nil
}

func (reader *referenceCorrelationReader) reference(ref *Reference) *correlationMemo {
	ref = canonicalReferenceReadOnly(ref)
	if ref == nil {
		return nil
	}
	cached := ref.correlatedToCache.Load()
	if cached != nil && cached.version == ref.memberVersion && len(cached.dependencies) == 0 {
		return cached
	}
	if snapshot, ok := reader.memo[ref]; ok {
		return snapshot
	}
	if _, cycle := reader.active[ref]; cycle {
		return nil
	}
	if reader.memo == nil {
		reader.memo = make(map[*Reference]*correlationMemo)
		reader.active = make(map[*Reference]struct{})
	}
	reader.active[ref] = struct{}{}
	defer delete(reader.active, ref)

	if cached != nil && cached.version == ref.memberVersion {
		valid := true
		for _, dependency := range cached.dependencies {
			if reader.reference(dependency.reference) != dependency.snapshot {
				valid = false
				break
			}
		}
		if valid {
			reader.memo[ref] = cached
			return cached
		}
	}

	computed := &correlationMemo{
		version:      ref.memberVersion,
		correlations: make(map[values.CorrelationIdentifier]struct{}),
	}
	for _, members := range [][]RelationalExpression{ref.members, ref.finalMembers} {
		for _, member := range members {
			snapshot := reader.expressionSnapshot(member)
			computed.dependencies = append(computed.dependencies, snapshot.dependencies...)
			for alias := range snapshot.correlations {
				computed.correlations[alias] = struct{}{}
			}
		}
	}
	// Concurrent readers must return the same published snapshot, otherwise
	// a parent's dependency would appear stale on an unchanged graph.
	if reader.publish && !ref.correlatedToCache.CompareAndSwap(cached, computed) {
		computed = ref.correlatedToCache.Load()
	}
	reader.memo[ref] = computed
	return computed
}
