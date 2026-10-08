package expressions

import (
	"maps"
	"sync"
	"sync/atomic"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type correlationMemo struct {
	version      uint64
	correlations map[values.CorrelationIdentifier]struct{}
	// content identifies the correlation set: a recomputation that yields an
	// equal set keeps it, so dependents compare content rather than snapshot
	// identity and survive an ancestor-neutral change below them.
	content      uint64
	dependencies []correlationDependency
	// validated is the correlation epoch at which every dependency was last
	// seen unchanged; 0 is never.
	validated atomic.Uint64
}

// correlationEpoch counts the graph changes that can move a reference's
// correlations: member changes and forwarding. A snapshot validated at the
// current epoch is current without revisiting its dependencies, so reads
// between two changes cost one check instead of a walk of the subgraph.
var correlationEpoch atomic.Uint64

func init() { correlationEpoch.Store(1) }

func bumpCorrelationEpoch() { correlationEpoch.Add(1) }

var correlationContents atomic.Uint64

func newCorrelationContent() uint64 { return correlationContents.Add(1) }

// sameCorrelations reports whether two snapshots carry the same correlation set.
func sameCorrelations(a, b *correlationMemo) bool {
	return a == b || a != nil && b != nil && a.content == b.content
}

// validatedAt reports whether the snapshot is current for epoch.
func (m *correlationMemo) validatedAt(epoch uint64) bool {
	return len(m.dependencies) == 0 || m.validated.Load() == epoch
}

type correlationDependency struct {
	reference *Reference
	snapshot  *correlationMemo
}

type referenceCorrelationReader struct {
	memo        map[*Reference]*correlationMemo
	expressions map[RelationalExpression]*correlationMemo
	active      map[*Reference]struct{}
	// priors are member snapshots from earlier passes, borrowed read-only and
	// reused only after revalidation.
	priors  map[RelationalExpression]*correlationMemo
	publish bool
	// epoch is read before the reader validates anything, so a stamp never
	// claims a later graph than the one it saw.
	epoch uint64
}

func (reader *referenceCorrelationReader) currentEpoch() uint64 {
	if reader.epoch == 0 {
		reader.epoch = correlationEpoch.Load()
	}
	return reader.epoch
}

var correlationReadScratch = sync.Pool{
	New: func() any { return &referenceCorrelationReader{} },
}

func (reader *referenceCorrelationReader) reset() {
	clear(reader.memo)
	clear(reader.expressions)
	clear(reader.active)
	reader.priors = nil
	reader.publish = false
	reader.epoch = 0
}

// GetCorrelatedTo returns the transitive free aliases across both member lanes.
// The returned map is borrowed and immutable. Concurrent reads require a stable
// graph; descendant edits and forwarding are checked before reusing a snapshot.
func (r *Reference) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	r = canonicalReferenceReadOnly(r)
	if r == nil {
		return nil
	}
	if cached := r.correlatedToCache.Load(); cached != nil && cached.version == r.memberVersion &&
		cached.validatedAt(correlationEpoch.Load()) {
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
	return reader.memberSnapshot(expression, reader.priors[expression])
}

func (reader *referenceCorrelationReader) computeExpressionSnapshot(expression RelationalExpression) *correlationMemo {
	if reader.expressions == nil {
		reader.expressions = make(map[RelationalExpression]*correlationMemo)
	}
	computed := &correlationMemo{content: newCorrelationContent()}
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

// memberSnapshot is the member's snapshot: the one from an earlier pass while
// every child it read still has the snapshot it read, else a fresh one.
func (reader *referenceCorrelationReader) memberSnapshot(member RelationalExpression, prior *correlationMemo) *correlationMemo {
	if cached, ok := reader.expressions[member]; ok {
		return cached
	}
	if prior != nil {
		for _, dependency := range prior.dependencies {
			if !sameCorrelations(reader.reference(dependency.reference), dependency.snapshot) {
				return reader.computeExpressionSnapshot(member)
			}
		}
		if reader.expressions == nil {
			reader.expressions = make(map[RelationalExpression]*correlationMemo)
		}
		reader.expressions[member] = prior
		return prior
	}
	return reader.computeExpressionSnapshot(member)
}

// publishMembers records the snapshots this reader holds for ref's members.
func (reader *referenceCorrelationReader) publishMembers(ref *Reference) {
	var prior map[RelationalExpression]*correlationMemo
	if p := ref.memberCorrelations.Load(); p != nil {
		prior = *p
	}
	current := make(map[RelationalExpression]*correlationMemo, len(ref.members)+len(ref.finalMembers))
	for _, members := range [][]RelationalExpression{ref.members, ref.finalMembers} {
		for _, member := range members {
			if snapshot, ok := reader.expressions[member]; ok {
				current[member] = snapshot
			} else if snapshot := prior[member]; snapshot != nil {
				current[member] = snapshot
			}
		}
	}
	ref.memberCorrelations.Store(&current)
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
	epoch := reader.currentEpoch()
	cached := ref.correlatedToCache.Load()
	if cached != nil && cached.version == ref.memberVersion && cached.validatedAt(epoch) {
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
			if !sameCorrelations(reader.reference(dependency.reference), dependency.snapshot) {
				valid = false
				break
			}
		}
		if valid {
			cached.validated.Store(epoch)
			reader.memo[ref] = cached
			return cached
		}
	}

	computed := &correlationMemo{
		version:      ref.memberVersion,
		correlations: make(map[values.CorrelationIdentifier]struct{}),
	}
	// Members of one group mostly range over the same children, and within one
	// pass a reference has one snapshot, so each child is recorded once: a
	// cache hit then revalidates distinct children, not every member's.
	seen := make(map[*Reference]struct{})
	var prior map[RelationalExpression]*correlationMemo
	if p := ref.memberCorrelations.Load(); p != nil {
		prior = *p
	}
	current := make(map[RelationalExpression]*correlationMemo, len(ref.members)+len(ref.finalMembers))
	for _, members := range [][]RelationalExpression{ref.members, ref.finalMembers} {
		for _, member := range members {
			snapshot := reader.memberSnapshot(member, prior[member])
			current[member] = snapshot
			for _, dependency := range snapshot.dependencies {
				if _, dup := seen[dependency.reference]; dup {
					continue
				}
				seen[dependency.reference] = struct{}{}
				computed.dependencies = append(computed.dependencies, dependency)
			}
			for alias := range snapshot.correlations {
				computed.correlations[alias] = struct{}{}
			}
		}
	}
	if cached != nil && maps.Equal(cached.correlations, computed.correlations) {
		computed.correlations, computed.content = cached.correlations, cached.content
	} else {
		computed.content = newCorrelationContent()
	}
	computed.validated.Store(epoch)
	if reader.publish {
		ref.memberCorrelations.Store(&current)
	}
	// Concurrent readers must return the same published snapshot, otherwise
	// a parent's dependency would appear stale on an unchanged graph.
	if reader.publish && !ref.correlatedToCache.CompareAndSwap(cached, computed) {
		computed = ref.correlatedToCache.Load()
	}
	reader.memo[ref] = computed
	return computed
}
