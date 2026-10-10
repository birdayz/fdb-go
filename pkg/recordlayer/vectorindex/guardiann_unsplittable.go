// Portions derived from FoundationDB Record Layer (SplitMergeTask.java,
// KMeans.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"bytes"
	"fmt"
	"math"
	"sort"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// The Go rule for a split with no usable candidate (RFC-257 WS-D, declared
// (h); DIVERGENCES "GuardiANN splits what Java cannot"). Java throws where
// every candidate is INVALID (or n<k) and the collapse route does not apply
// (SplitMergeTask.java:397, KMeans.java:136), and the task fails the same way
// forever: in deferred mode the cluster strands at the hard cap, inline every
// later insert of the partition fails. Go enters its rule exactly there and
// only there: an admitted outlier peel (step 1), else a terminal reconcile of
// the target in place (step 2), which fails with ClusterUnsplittableError when
// the reconciled count is above the hard cap.

// peelWorkBound is B, the work bound of the peel's admission: a design
// constant chosen for coverage (it admits the default hard cap, n = 2000, up to
// d = 980, and the first over-max size, n = 1001, up to d = 2175).
const peelWorkBound = 1.96e7

// Causes a ClusterUnsplittableError records.
const (
	UnsplittableNoUsablePartition = "no usable partition under the rule"
	UnsplittablePeelNotAdmitted   = "a peel the rule does not admit"
)

// ClusterUnsplittableError is a GuardiANN split that found no usable partition
// and whose cluster, after its stale references are removed, still holds more
// primaries than primaryClusterHardMax. It is Go-only (Java throws
// NoSuchElementException or IllegalArgumentException there), poisons the
// transaction like any task error, and is deliberately not the insert cap's
// capacity error: its remedy is deleting vectors, not draining a merge.
type ClusterUnsplittableError struct {
	IndexName string
	Prefix    tuple.Tuple
	Cluster   tuple.UUID
	Count     int
	Limit     int
	Cause     string
}

func (e *ClusterUnsplittableError) Error() string {
	return fmt.Sprintf("vector index %q (GUARDIANN) cluster %s of partition %v cannot be split: %s, and it holds %d primaries, above primaryClusterHardMax %d",
		e.IndexName, e.Cluster, e.Prefix, e.Cause, e.Count, e.Limit)
}

// peelExit names how the peel ended, for the goldens.
type peelExit int

const (
	peelNotRun peelExit = iota
	peelSelected
	peelUndersizedChildHoldsNoMass
	peelMassBelowTwo
)

// peelAdmitted is the peel's admission: its work W = floor(log2(n - 1)) * n *
// d * f is at most B. The knob factor f is a fit's KMeans work relative to the
// default knobs' (I = 8, R = 3), floored at 1 so no knobs enlarge admission: the
// larger of its iterations, I * (R + 1) / 32, and its objective passes per
// vector, (R + 1) * (2I + 2) / 72, since every restart's seeding and final
// assignment cost passes at any I. W is a pure function of the task's input.
func peelAdmitted(n, d, iterations, restarts int) bool {
	if n < 2 {
		return false
	}
	runs, it := float64(restarts+1), float64(iterations)
	knob := math.Max(math.Max(it*runs/32, runs*(2*it+2)/72), 1)
	w := math.Floor(math.Log2(float64(n-1))) * float64(n) * float64(d) * knob
	return w <= peelWorkBound
}

// unsplittable is the rule's entry. It returns the peel's selected candidate,
// or nil after a terminal reconcile.
func (g *guardiann) unsplittable(tx fdb.WritableTransaction, random *splittableRandom, target guardiannClusterMetadata,
	centroid gVector, current guardiannCluster, c12 *repartitioningCandidate,
) (*repartitioningCandidate, error) {
	// The task RNG is split once into the peel RNG, whichever step follows, so
	// a later draw of a Go-rule task does not depend on how many refits ran.
	peel := random.split()
	cause := UnsplittableNoUsablePartition
	if !c12.nk {
		n := len(c12.primaries)
		if peelAdmitted(n, len(c12.primaries[0].vector.data), g.config.kMeansMaxIterations, g.config.kMeansMaxRestarts) {
			cand, _, err := g.peelSplit(peel, current, c12)
			if err != nil || cand != nil {
				return cand, err
			}
		} else {
			cause = UnsplittablePeelNotAdmitted
		}
	}
	return nil, g.reconcileUnsplittable(tx, random, target, centroid, cause)
}

// peelSplit is step 1, the iterated outlier peel with a geometric removal
// floor. It starts from the 1->2 candidate's own INVALID assignment over the
// cleaned primaries P with the mass M = P. Each round removes the undersized
// child's members from M, tops the removals up to 2^(r+1) - 1 with the members
// of M farthest from the other child's centroid, refits KMeans k = 2 on M with
// the r-th split of the peel RNG, assigns every primary of P to the nearer
// refit centroid and scores that partition against the current one. The first
// partition that is not INVALID is selected; at most floor(log2(n - 1)) refits
// run, because the peel stops once fewer than two members remain.
func (g *guardiann) peelSplit(peel *splittableRandom, current guardiannCluster, c12 *repartitioningCandidate,
) (*repartitioningCandidate, peelExit, error) {
	primaries := c12.primaries
	n := len(primaries)
	assignment := c12.kMeans.assignment
	centroids := c12.kMeans.centroids
	inMass := make([]bool, n)
	for i := range inMass {
		inMass[i] = true
	}
	mass, removed := n, 0
	for r := 0; ; r++ {
		sizes := [2]int{}
		for _, a := range assignment {
			sizes[a]++
		}
		undersized := 0
		if sizes[1] < sizes[0] {
			undersized = 1
		}
		other := 1 - undersized
		removedNow := 0
		for i, a := range assignment {
			if inMass[i] && a == undersized {
				inMass[i] = false
				removedNow++
			}
		}
		if removedNow == 0 {
			return nil, peelUndersizedChildHoldsNoMass, nil
		}
		removed += removedNow
		mass -= removedNow
		if floor := 1<<(r+1) - 1; removed < floor {
			members, err := g.peelFarthest(primaries, inMass, centroids[other])
			if err != nil {
				return nil, peelNotRun, err
			}
			for _, m := range members[:min(floor-removed, len(members))] {
				inMass[m.index] = false
				mass--
				removed++
			}
		}
		if mass < 2 {
			return nil, peelMassBelowTwo, nil
		}
		vectors := make([]gVector, 0, mass)
		for i, in := range inMass {
			if in {
				vectors = append(vectors, primaries[i].vector)
			}
		}
		refit, err := kMeansFit(peel.split(), g.codec, vectors, 2, g.config.kMeansMaxIterations, g.config.kMeansMaxRestarts)
		if err != nil {
			return nil, peelNotRun, err
		}
		cand, result, err := g.peelCandidate(current, c12, refit.centroids)
		if err != nil {
			return nil, peelNotRun, err
		}
		if result.decision != decisionInvalidCandidate {
			return cand, peelSelected, nil
		}
		assignment, centroids = cand.kMeans.assignment, refit.centroids
	}
}

// peelMember is a member of the peel's mass and its distance from a centroid.
type peelMember struct {
	index    int
	distance float64
}

// peelFarthest is the members of the mass, farthest from c first.
func (g *guardiann) peelFarthest(primaries []guardiannVectorRef, inMass []bool, c gVector) ([]peelMember, error) {
	var members []peelMember
	for i, in := range inMass {
		if in {
			d, err := g.distance(primaries[i].vector, c)
			if err != nil {
				return nil, err
			}
			members = append(members, peelMember{i, d})
		}
	}
	sort.SliceStable(members, func(a, b int) bool { return members[a].distance > members[b].distance })
	return members, nil
}

// peelCandidate assigns every primary to the nearer refit centroid and scores
// that partition against the current one.
func (g *guardiann) peelCandidate(current guardiannCluster, c12 *repartitioningCandidate, centroids []gVector,
) (*repartitioningCandidate, evaluationResult, error) {
	primaries := c12.primaries
	next := make([]int, len(primaries))
	for i, p := range primaries {
		d0, err := g.distance(p.vector, centroids[0])
		if err != nil {
			return nil, evaluationResult{}, err
		}
		d1, err := g.distance(p.vector, centroids[1])
		if err != nil {
			return nil, evaluationResult{}, err
		}
		if d1 < d0 {
			next[i] = 1
		}
	}
	cand := &repartitioningCandidate{
		cls: c12.cls, primaries: primaries,
		kMeans: kMeansResult{centroids: centroids, assignment: next},
	}
	result, err := g.scoreCandidate([]guardiannCluster{current}, cand)
	return cand, result, err
}

// reconcileUnsplittable is step 2: the target is reconciled in place and keeps
// its topology. Every reference (primaries and replicas) is read with each
// referenced key's current identity, stale ones are removed (collapsed
// representatives are kept, as in Java), the counts and running statistics are
// recomputed from the survivors (the stored distance maximum is never lowered),
// SPLIT_MERGE is cleared and the ordinary undersized-merge rule applies to the
// reconciled count. Above primaryClusterHardMax it fails before any write.
func (g *guardiann) reconcileUnsplittable(tx fdb.WritableTransaction, random *splittableRandom,
	target guardiannClusterMetadata, centroid gVector, cause string,
) error {
	refs, err := g.fetchVectorRefs(tx, target.id, g.codec.decode)
	if err != nil {
		return err
	}
	var stale []tuple.Tuple
	stats := runningStatsIdentity()
	underrep, replicated := 0, 0
	for _, r := range refs {
		if !r.collapsed {
			md, err := g.fetchVectorMetadata(tx, r.id.pk)
			if err != nil {
				return err
			}
			if md == nil || md.id.uuid != r.id.uuid {
				stale = append(stale, r.id.pk)
				continue
			}
		}
		if !r.primary {
			replicated++
			continue
		}
		if r.underrep {
			underrep++
		}
		d, err := g.distance(r.vector, centroid)
		if err != nil {
			return err
		}
		stats = stats.add(d)
	}
	count := int(stats.n)
	if count > g.config.primaryClusterHardMax {
		return &ClusterUnsplittableError{
			IndexName: g.indexName, Prefix: g.prefix, Cluster: target.id,
			Count: count, Limit: g.config.primaryClusterHardMax, Cause: cause,
		}
	}
	if count > 0 {
		stats.maxEver = math.Max(stats.maxEver, target.stats.maxEver)
	}
	for _, pk := range stale {
		g.deleteVectorRef(tx, target.id, pk)
	}
	reconciled := target.withNewVectors(underrep, replicated, stats, target.states&^clusterStateSplitMerge)
	merged, err := g.enqueueMergeIfUndersized(tx, random, reconciled, centroid, stats, count)
	if err != nil || merged {
		return err
	}
	g.writeClusterMetadata(tx, reconciled)
	return nil
}

// mergeEmptyCore is the Go rule for a merge whose 2->1 core holds no live
// primary (RFC-257 WS-D declared (h)). It is a normal merge whose product
// keeps the core centroid with the lowest packed UUID instead of minting one:
// every core cluster's references are pruned (a dissolved core keeps no
// replica, as assignPrimaryVectorReferences considers primaries only), the
// other core clusters' centroids, references and metadata are deleted, and the
// kept cluster is reset to identity statistics, no underreplicated or
// replicated vectors and no state, keeping its lifetime peak. The core ids are
// the cause set, so the neighbours are force-reassigned as after any merge,
// which repairs owners that lost replicas. The kept cluster then takes the
// ordinary undersized-merge rule, so while other centroids remain it merges
// into a neighbour; every empty-core merge deletes at least one centroid.
func (g *guardiann) mergeEmptyCore(tx fdb.WritableTransaction, random *splittableRandom, cls *clusterClassification) error {
	kept := cls.core[0]
	for _, c := range cls.core[1:] {
		if bytes.Compare(c.meta.id[:], kept.meta.id[:]) < 0 {
			kept = c
		}
	}
	causes := make([]tuple.UUID, 0, len(cls.core))
	for _, c := range cls.core {
		causes = append(causes, c.meta.id)
		if err := g.deleteVectorRefsForCluster(tx, c.meta.id); err != nil {
			return err
		}
		if c.meta.id == kept.meta.id {
			continue
		}
		if err := g.centroids.Delete(tx, tuple.Tuple{c.meta.id}); err != nil {
			return err
		}
		g.deleteClusterMetadata(tx, c.meta.id)
	}
	for _, c := range cls.neighboring {
		if _, err := g.updateAndEnqueueReassign(tx, random, c.meta, c.centroid, 0, 0, 0, c.meta.stats, causes); err != nil {
			return err
		}
	}
	reset := guardiannClusterMetadata{id: kept.meta.id, stats: runningStatsIdentity(), maxEverPrimary: kept.meta.maxEverPrimary}
	merged, err := g.enqueueMergeIfUndersized(tx, random, reset, kept.centroid, reset.stats, 0)
	if err != nil || merged {
		return err
	}
	g.writeClusterMetadata(tx, reset)
	return nil
}
