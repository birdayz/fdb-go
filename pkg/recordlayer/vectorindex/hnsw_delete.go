// Portions derived from FoundationDB Record Layer (InliningStorageAdapter.java,
// Insert.java, NodeReferenceWithVector.java, Config.java, and others),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"bytes"
	"fmt"
	"math/bits"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Delete removes a node from the HNSW graph and repairs neighbor connections.
// Wire-compatible with Java's HNSW delete (graph repair, entry point update).
//
// Optimization: for compact layers, pipelines the initial reads across all layers
// at once, reducing sequential round-trips from O(topLvl) to O(1) for the read phase.
// For inlining layers, uses preloadLayerDispatch (already done during search/insert).
// Repairs are still sequential (each needs neighbor data from FDB).
func (g *hnswGraph) Delete(tx fdb.WritableTransaction, primaryKey tuple.Tuple) error {
	accessInfo, accessErr := g.storage.loadAccessInfo(tx)
	if accessErr != nil {
		if e := hnswFatal(accessErr); e != nil {
			return e // transient read — abort/retry, don't treat as an empty graph
		}
		return nil // genuinely empty graph, nothing to delete
	}
	// Lift pre-centroid plain vectors into the current coordinate system during repair
	// distance computations (see opXform). nil when no centroid is established.
	g.opXform = g.buildTransform(accessInfo)
	// The node occupies layers [0, topLayer(pk)] by construction (Insert writes every
	// one); Java's Delete bounds its layer scan the same way (Delete.java:186
	// primitives.topLayer(primaryKey), deleteFromLayers rangeClosed(0, topLayer)).
	// This can exceed the CURRENT entry layer (the entry may have been deleted and
	// replaced by a lower node) — bounding by the entry layer would orphan the KVs
	// above it.
	//
	// topLayer(pk, M) is always the layer set the node was inserted with, because M is
	// immutable for an existing index: the metadata-evolution validator rejects a
	// changed hnswM (validateVectorIndexOptions; Java
	// VectorIndexMaintainerFactory.validateChangedOptions disallows HNSW_M), so a
	// "deleted with a different M than inserted" scenario cannot reach this code at the
	// supported surface — in either engine.
	topLvl := topLayer(primaryKey, g.config.M)

	if err := g.prefetchDeleteLayers(tx, primaryKey, topLvl); err != nil {
		return err
	}

	// Check existence at layer 0 (always compact, now served from cache).
	_, _, err := g.storage.loadNodeLayer(tx, 0, primaryKey)
	if err != nil {
		if e := hnswFatal(err); e != nil {
			return e // transient read — abort/retry, don't treat as already-deleted
		}
		return nil // already deleted or doesn't exist
	}
	if err := g.raBitQuantizerAdmits(accessInfo); err != nil {
		return err
	}

	// Delete from each layer (0..topLayer(pk)), repairing the deleted node's neighbors
	// against a SHARED candidate set with deterministic, PK-seeded sampling — matching
	// Java Delete.deleteFromLayers/deleteFromLayer. The random is seeded once from the
	// deleted key (Primitives.random(primaryKey)) and SPLIT once per layer
	// (Delete.java:261 random.split()), so each layer's sampling is an independent
	// stream: layer N's draws don't depend on how many candidates layer N-1 had.
	random := newSplittableRandomForKey(primaryKey)
	var newEntryPK tuple.Tuple
	var newEntryVecBytes []byte
	newEntryLayer := 0
	for layer := 0; layer <= topLvl; layer++ {
		layerRandom := random.split()
		_, neighbors, loadErr := g.storage.loadNodeLayerDispatch(tx, layer, primaryKey)
		if loadErr != nil {
			if e := hnswFatal(loadErr); e != nil {
				return e // transient read error — abort and retry, don't skip the layer
			}
			continue // not present at this layer
		}
		// neighbors aliases the per-tx cache entry; copy before the repair mutates the cache.
		nbCopy := make([][]byte, len(neighbors))
		copy(nbCopy, neighbors)

		entryPK, entryVec, derr := g.deleteFromLayerRepair(tx, layer, primaryKey, nbCopy, layerRandom)
		if derr != nil {
			return derr
		}
		// Java returns one potential entry per layer (the first repair candidate) and picks
		// the first non-null from the highest layer down — i.e. the highest layer that had a
		// candidate. Overwriting each layer yields exactly that.
		if entryPK != nil {
			newEntryPK = entryPK
			newEntryVecBytes = entryVec
			newEntryLayer = layer
		}
	}

	// Update the entry point if we deleted it.
	if tupleEqual(accessInfo.pk, primaryKey) {
		if newEntryPK != nil {
			// Preserve transform state (rotatorSeed, centroid) when updating entry point.
			accessInfo.layer = newEntryLayer
			accessInfo.pk = newEntryPK
			accessInfo.vectorBytes = newEntryVecBytes
			g.storage.saveAccessInfo(tx, accessInfo)
		} else {
			g.storage.clearAccessInfo(tx)
		}
	}

	return nil
}

// prefetchDeleteLayers pipelines and resolves all compact-layer reads for primaryKey
// across [0, topLvl] into the per-tx storage cache.
func (g *hnswGraph) prefetchDeleteLayers(tx fdb.ReadTransaction, primaryKey tuple.Tuple, topLvl int) error {
	// Pipeline: fire all compact-layer reads at once.
	// Inlining layers use range reads via loadNodeLayerDispatch.
	type layerFuture struct {
		layer  int
		key    string
		future fdb.FutureByteSlice
	}
	var futures []layerFuture

	for layer := 0; layer <= topLvl; layer++ {
		if g.storage.isInliningLayer(layer) {
			continue // inlining layers use range reads, not single-key gets
		}
		key := g.storage.dataSubspace.Pack(tuple.Tuple{int64(layer), primaryKey})
		cacheKey := string(key)
		if _, ok := g.storage.cache[cacheKey]; ok {
			continue // already cached, no need to fetch
		}
		futures = append(futures, layerFuture{
			layer:  layer,
			key:    cacheKey,
			future: tx.Get(fdb.Key(key)),
		})
	}

	// Resolve all compact-layer futures — FDB pipelines these reads into ~1 round-trip.
	// A future error is a real I/O failure (a missing key surfaces as data==nil below,
	// not an error) and must propagate so the tx retries rather than silently skipping a
	// layer and committing an incomplete delete. Record the first error but keep draining
	// the REMAINING futures before returning: leaving futures unresolved when the retry
	// loop resets/reuses the transaction violates the FDB tx contract and leaks the
	// pure-Go reply handles/timers.
	var futureErr error
	for _, f := range futures {
		data, err := f.future.Get()
		if err != nil {
			if futureErr == nil {
				futureErr = fmt.Errorf("hnsw delete: read layer %d: %w", f.layer, err)
			}
			continue
		}
		if data == nil {
			g.storage.cache[f.key] = nil // negative cache
			continue
		}
		vecBytes, neighbors, additional, parseErr := parseNodeValue(data)
		if parseErr != nil {
			continue
		}
		g.storage.cache[f.key] = &parsedNode{vecBytes: vecBytes, neighbors: neighbors, additional: additional}
	}
	return futureErr
}

// repairCandidate is a node in the shared deletion-repair candidate set: its primary key,
// nested-span form, resolved vector bytes (for re-saving) and decoded vector (for distance),
// and its current neighbor spans (to remove the deleted node and to extend with repaired ins).
type repairCandidate struct {
	pk        tuple.Tuple
	span      []byte
	vecBytes  []byte
	vec       []float64
	neighbors [][]byte
}

// findDeletionRepairCandidates builds the shared repair candidate set for a delete at one
// layer: the deleted node's existing neighbors (PRIMARY, always kept) plus their neighbors
// (SECONDARY) sampled deterministically at rate (efRepair - |primary|)/numCandidates via the
// PK-seeded random. Matches Java Delete.findDeletionRepairCandidates + Primitives.neighbors/
// findNeighborReferences (a LinkedHashSet: primary first, then distinct non-primary neighbors
// in discovery order; one random.nextDouble() per non-primary candidate, in that order).
func (g *hnswGraph) findDeletionRepairCandidates(tx fdb.ReadTransaction, layer int, deletedPK tuple.Tuple, deletedNeighbors [][]byte, random *splittableRandom) ([]repairCandidate, error) {
	deletedSpan := nestPK(deletedPK)

	// PRIMARY = the deleted node's neighbors, EXCLUDING the deleted node itself. Java's
	// shouldUsePrimaryCandidateForRepair rejects a reference to the node being deleted
	// (Primitives.findNeighborReferences seeds the set with the initial node, then the
	// predicate filters it out) — so a stale self-reference never re-enters the candidate
	// set and gets re-saved. We exclude it directly when forming the primary set.
	listed := make(map[string]bool, len(deletedNeighbors))
	var listedSpans [][]byte
	for _, s := range deletedNeighbors {
		if bytes.Equal(s, deletedSpan) || listed[string(s)] {
			continue
		}
		listed[string(s)] = true
		listedSpans = append(listedSpans, s)
	}

	// PRIMARY nodes loaded so we can walk their neighbors for the secondary set.
	// Only the existing ones are initial nodes of the second neighbors() pass
	// (the first pass's filterExisting): a stale reference is not primary.
	var primaryBatch []nodeResult
	for _, r := range g.storage.loadNodeLayerBatchDispatch(tx, layer, listedSpans) {
		if r.err != nil {
			if e := hnswFatal(r.err); e != nil {
				return nil, e
			}
			continue
		}
		primaryBatch = append(primaryBatch, r)
	}
	primarySet := make(map[string]bool, len(primaryBatch))
	primarySpans := make([][]byte, 0, len(primaryBatch))
	for _, r := range primaryBatch {
		primarySet[string(r.span)] = true
		primarySpans = append(primarySpans, r.span)
	}

	// Ordered ref set (LinkedHashSet): primary first, then distinct non-primary
	// neighbors-of-primary (excluding the deleted node).
	refs := make([][]byte, 0, len(primarySpans))
	refsSeen := make(map[string]bool, len(primarySpans))
	for _, s := range primarySpans {
		refs = append(refs, s)
		refsSeen[string(s)] = true
	}
	// The deleted node, a neighbour of the primaries, is in the set too: it
	// counts toward numberOfCandidates and is then rejected without a draw
	// (shouldUseSecondaryCandidateForRepair).
	for _, r := range primaryBatch {
		for _, nb := range r.neighbors {
			key := string(nb)
			if primarySet[key] || refsSeen[key] {
				continue
			}
			refs = append(refs, nb)
			refsSeen[key] = true
		}
	}

	numCandidates := len(refs)
	var sampleRate float64
	if numCandidates > 0 {
		sampleRate = float64(g.config.EfRepair-len(primarySpans)) / float64(numCandidates)
	}

	// Sample: primary (initials) always accepted (no random call); others accepted if
	// sampleRate >= 1 or random.nextDouble() < sampleRate — in ref order, matching Java.
	var selectedSpans [][]byte
	for _, ref := range refs {
		if primarySet[string(ref)] {
			selectedSpans = append(selectedSpans, ref)
			continue
		}
		if bytes.Equal(ref, deletedSpan) {
			continue
		}
		if sampleRate >= 1 || random.nextDouble() < sampleRate {
			selectedSpans = append(selectedSpans, ref)
		}
	}

	// Load the selected candidates; filterExisting (skip absent ones). Preserve selectedSpans
	// order so the candidate set (and the first-candidate entry choice) is deterministic.
	batch := g.storage.loadNodeLayerBatchDispatch(tx, layer, selectedSpans)
	resByKey := make(map[string]nodeResult, len(batch))
	for _, r := range batch {
		resByKey[string(r.span)] = r
	}
	var candidates []repairCandidate
	for _, span := range selectedSpans {
		r, ok := resByKey[string(span)]
		if !ok || r.err != nil {
			if ok && r.err != nil {
				if e := hnswFatal(r.err); e != nil {
					return nil, e
				}
			}
			continue // absent candidate — filtered out
		}
		pk, derr := decodeNestedPK(r.span)
		if derr != nil {
			return nil, derr
		}
		resolved := g.storage.resolveVectorBytes(layer, pk, r.vecBytes)
		vec, decErr := g.decodeStoredVector(resolved)
		if decErr != nil {
			continue
		}
		candidates = append(candidates, repairCandidate{pk: pk, span: r.span, vecBytes: resolved, vec: vec, neighbors: r.neighbors})
	}
	return candidates, nil
}

// deleteFromLayerRepair deletes deletedPK from one layer and repairs its neighbors against
// the shared candidate set, then prunes and persists. Returns the first repair candidate as
// Java's potential new entry node (Delete.deleteFromLayer).
func (g *hnswGraph) deleteFromLayerRepair(tx fdb.WritableTransaction, layer int, deletedPK tuple.Tuple, deletedNeighbors [][]byte, random *splittableRandom) (tuple.Tuple, []byte, error) {
	deletedSpan := nestPK(deletedPK)
	candidates, err := g.findDeletionRepairCandidates(tx, layer, deletedPK, deletedNeighbors, random)
	if err != nil {
		return nil, nil, err
	}

	// changeSet: candidate span → its mutable neighbor spans (deleted node removed up front,
	// Java initializeCandidateChangeSetMap → DeleteNeighborsChangeSet). `changed` tracks which
	// candidates were modified so we only re-write those (Java: changeSet.hasChanges()).
	changeSet := make(map[string][][]byte, len(candidates))
	candByKey := make(map[string]repairCandidate, len(candidates))
	changed := make(map[string]bool, len(candidates))
	order := make([]string, 0, len(candidates))
	for _, c := range candidates {
		key := string(c.span)
		nbs := make([][]byte, 0, len(c.neighbors))
		removed := false
		for _, nb := range c.neighbors {
			if bytes.Equal(nb, deletedSpan) {
				removed = true
				continue
			}
			nbs = append(nbs, nb)
		}
		changeSet[key] = nbs
		candByKey[key] = c
		order = append(order, key)
		if removed {
			changed[key] = true
		}
	}

	// Repair each primary neighbor P of the deleted node against the shared candidate set:
	// select M diverse candidates for P (Java repairInsForNeighborNode → selectCandidates(M))
	// and add P to each selected candidate's neighbor list (reconnecting P into the graph).
	for _, pSpan := range deletedNeighbors {
		p, ok := candByKey[string(pSpan)]
		if !ok {
			continue // primary neighbor not a (still-existing) candidate
		}
		cands := make([]hnswCandidate, 0, len(candidates))
		for _, c := range candidates {
			if bytes.Equal(c.span, pSpan) {
				continue // not P itself
			}
			cands = append(cands, hnswCandidate{pkSpan: c.span, vector: hnswVector{data: c.vecBytes}, vec: c.vec, dist: g.pairDistance(c.vec, hnswVector{data: c.vecBytes}, p.vec, hnswVector{data: p.vecBytes})})
		}
		selected := g.selectNeighbors(cands, g.config.M)
		for _, sc := range selected {
			// InsertNeighborsChangeSet.merge is concat(filter(parent, not
			// inserted), inserted): a P already in the list moves to its end,
			// and the node is rewritten (hasChanges is true).
			scKey := string(sc.pkSpan)
			kept := changeSet[scKey][:0:0]
			for _, nb := range changeSet[scKey] {
				if !bytes.Equal(nb, pSpan) {
					kept = append(kept, nb)
				}
			}
			changeSet[scKey] = append(kept, pSpan)
			changed[scKey] = true
		}
	}

	if err := g.pruneRepairCandidates(tx, layer, order, candByKey, changeSet, changed); err != nil {
		return nil, nil, err
	}
	if err := g.persistRepairedCandidates(tx, layer, deletedPK, order, candByKey, changeSet, changed); err != nil {
		return nil, nil, err
	}

	// New entry node = the first candidate (Java returns the first of candidateReferencesMap,
	// which preserves the candidate insertion order; guaranteed to exist).
	if len(order) > 0 {
		first := candByKey[order[0]]
		data, err := g.accessVectorBytes(hnswVector{data: first.vecBytes})
		return first.pk, data, err
	}
	return nil, nil, nil
}

// pruneRepairCandidates prunes each candidate's change set to MMax/MMax0 once it
// holds maxConn or more neighbors (Java pruneNeighborsIfNecessary).
func (g *hnswGraph) pruneRepairCandidates(tx fdb.ReadTransaction, layer int, order []string, candByKey map[string]repairCandidate, changeSet map[string][][]byte, changed map[string]bool) error {
	// Prune each modified candidate to MMax/MMax0 (Java pruneNeighborsIfNecessary).
	maxConn := g.config.MMax
	if layer == 0 {
		maxConn = g.config.MMax0
	}
	// Java prunes every candidate's change set, changed or not, once it holds
	// mMax or more neighbours; a prune that drops one rewrites the node.
	for _, key := range order {
		if len(changeSet[key]) < maxConn {
			continue
		}
		c := candByKey[key]
		pkList := make([]tuple.Tuple, 0, len(changeSet[key]))
		for _, sp := range changeSet[key] {
			pk, derr := decodeNestedPK(sp)
			if derr != nil {
				return derr
			}
			pkList = append(pkList, pk)
		}
		prunedPKs, perr := g.pruneNeighbors(tx, c.vec, hnswVector{data: c.vecBytes}, pkList, maxConn, layer)
		if perr != nil {
			return perr
		}
		pruned := make([][]byte, len(prunedPKs))
		for i, pk := range prunedPKs {
			pruned[i] = nestPK(pk)
		}
		if len(pruned) != len(changeSet[key]) {
			changed[key] = true
		}
		changeSet[key] = pruned
	}
	return nil
}

// persistRepairedCandidates deletes deletedPK from layer and writes every modified
// repair candidate back to storage.
func (g *hnswGraph) persistRepairedCandidates(tx fdb.WritableTransaction, layer int, deletedPK tuple.Tuple, order []string, candByKey map[string]repairCandidate, changeSet map[string][][]byte, changed map[string]bool) error {
	// Delete the node, then persist every modified candidate. Each neighbor's vector is
	// threaded from the candidate set when the neighbor is itself a candidate (its
	// resolved vector is in hand); other neighbors resolve from the per-tx cache (their
	// vectors were cached by the edge reads that surfaced them).
	g.storage.deleteNodeLayerDispatch(tx, layer, deletedPK)
	for _, key := range order {
		if !changed[key] {
			continue
		}
		c := candByKey[key]
		nbPKs := make([]tuple.Tuple, 0, len(changeSet[key]))
		nbVecs := make([]hnswVector, 0, len(changeSet[key]))
		for _, sp := range changeSet[key] {
			pk, derr := decodeNestedPK(sp)
			if derr != nil {
				return derr
			}
			nbPKs = append(nbPKs, pk)
			if nb, ok := candByKey[string(sp)]; ok {
				nbVecs = append(nbVecs, hnswVector{data: nb.vecBytes})
			} else {
				nbVecs = append(nbVecs, hnswVector{})
			}
		}
		if saveErr := g.saveNodeLayer(tx, layer, c.pk, hnswVector{data: c.vecBytes}, nbPKs, nbVecs); saveErr != nil {
			return fmt.Errorf("hnsw delete: save repaired candidate at layer %d: %w", layer, saveErr)
		}
	}
	return nil
}

// goldenGamma is Java SplittableRandom's GOLDEN_GAMMA, 0x9e3779b97f4a7c15 (signed).
const goldenGamma int64 = -0x61C8864680B583EB

// mix64 is Java SplittableRandom.mix64 (Stafford variant 13 of the SplitMix64 mixer).
func mix64(z int64) int64 {
	u := uint64(z)
	u = (u ^ (u >> 30)) * 0xbf58476d1ce4e5b9
	u = (u ^ (u >> 27)) * 0x94d049bb133111eb
	return int64(u ^ (u >> 31))
}

// mixGamma is Java SplittableRandom.mixGamma: the MurmurHash3 64-bit mixer, forced
// odd, with the low-transition correction — produces the gamma for a split() child.
func mixGamma(z int64) int64 {
	u := uint64(z)
	u = (u ^ (u >> 33)) * 0xff51afd7ed558ccd
	u = (u ^ (u >> 33)) * 0xc4ceb9fe1a85ec53
	u = (u ^ (u >> 33)) | 1
	if bits.OnesCount64(u^(u>>1)) < 24 {
		u ^= 0xaaaaaaaaaaaaaaaa
	}
	return int64(u)
}

// splitMixLong applies the SplitMix64 hash function: mix64 of the GOLDEN_GAMMA-advanced
// input — equivalently, the first nextLong() of new SplittableRandom(x).
func splitMixLong(x int64) int64 {
	return mix64(x + goldenGamma)
}

// splitMixDouble converts a SplitMix64 hash to a double in [0, 1).
func splitMixDouble(x int64) float64 {
	return float64(uint64(splitMixLong(x))>>11) * (1.0 / float64(int64(1)<<53))
}

// splittableRandom is a stateful port of java.util.SplittableRandom (seeded form,
// fixed GOLDEN_GAMMA). HNSW delete uses it for deterministic, PK-seeded sampling of
// repair candidates — matching Java's Primitives.random(primaryKey) =
// new SplittableRandom(splitMixLong(primaryKey.hashCode())). splitMixLong(x) already
// computes mix64(x + GOLDEN_GAMMA) — exactly SplittableRandom.nextLong() for state x —
// so nextLong returns splitMixLong(seed) and then advances seed by GOLDEN_GAMMA.
type splittableRandom struct {
	seed  int64
	gamma int64
}

// newSplittableRandomForKey is Java's RandomHelpers.random(primaryKey): a
// SplittableRandom (GOLDEN_GAMMA) seeded by folding the packed key through
// splitMixLong byte by byte.
func newSplittableRandomForKey(primaryKey tuple.Tuple) *splittableRandom {
	return &splittableRandom{seed: seedFromBytes(primaryKey.Pack()), gamma: goldenGamma}
}

// seedFromBytes is RandomHelpers.seedFromBytes.
func seedFromBytes(b []byte) int64 {
	var seed int64
	for _, c := range b {
		seed = splitMixLong(seed ^ int64(c))
	}
	return seed
}

// nextSeed advances and returns the seed (Java SplittableRandom.nextSeed).
func (r *splittableRandom) nextSeed() int64 {
	r.seed += r.gamma
	return r.seed
}

func (r *splittableRandom) nextLong() int64 {
	return mix64(r.nextSeed())
}

// nextDouble returns a double in [0, 1), matching SplittableRandom.nextDouble() =
// (nextLong() >>> 11) * 0x1.0p-53.
func (r *splittableRandom) nextDouble() float64 {
	return float64(uint64(r.nextLong())>>11) * (1.0 / float64(int64(1)<<53))
}

// split returns a child RNG whose stream is statistically independent of the
// parent's, matching Java SplittableRandom.split(): seed = mix64(nextSeed()),
// gamma = mixGamma(nextSeed()) — two parent seed advances per split. HNSW delete
// splits once per layer (Java Delete.deleteFromLayers) so each layer's candidate
// sampling is an independent stream regardless of how many draws other layers made.
func (r *splittableRandom) split() *splittableRandom {
	return &splittableRandom{seed: mix64(r.nextSeed()), gamma: mixGamma(r.nextSeed())}
}
