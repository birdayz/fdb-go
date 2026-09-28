package recordlayer

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"math"
	"sort"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// This file holds GuardiANN's value types (Java package
// com.apple.foundationdb.async.guardiann): running statistics, cluster
// metadata, vector identities and references.

// gVector is Java's Transformed<RealVector>: components plus the VectorType
// ordinal they are stored at. GuardiANN's storage transform is the identity
// unless RaBitQ has established a centroid.
type gVector struct {
	data []float64
	typ  byte
}

func (v gVector) encode() []byte { return vectorcodec.SerializeAs(v.typ, v.data) }

func decodeGVector(b []byte) (gVector, error) {
	data, err := vectorcodec.Deserialize(b)
	if err != nil {
		return gVector{}, err
	}
	return gVector{data: data, typ: b[0]}, nil
}

func (v gVector) equal(o gVector) bool {
	if v.typ != o.typ || len(v.data) != len(o.data) {
		return false
	}
	for i := range v.data {
		if math.Float64bits(v.data[i]) != math.Float64bits(o.data[i]) {
			return false
		}
	}
	return true
}

// guardiannRunningStats is Java's RunningStats (Welford accumulator plus the
// running maximum).
type guardiannRunningStats struct {
	n       int64
	mean    float64
	m2      float64
	maxEver float64
}

func runningStatsIdentity() guardiannRunningStats {
	return guardiannRunningStats{maxEver: math.Inf(-1)}
}

func runningStatsOf(x float64) guardiannRunningStats { return runningStatsIdentity().add(x) }

func (s guardiannRunningStats) add(x float64) guardiannRunningStats {
	n := s.n + 1
	delta := x - s.mean
	mean := s.mean + delta/float64(n)
	delta2 := x - mean
	return guardiannRunningStats{n: n, mean: mean, m2: s.m2 + delta*delta2, maxEver: math.Max(s.maxEver, x)}
}

func (s guardiannRunningStats) remove(x float64) (guardiannRunningStats, error) {
	if s.n == 0 {
		return s, &RecordCoreError{Message: "Cannot remove from an empty set"}
	}
	if s.n == 1 {
		return runningStatsIdentity(), nil
	}
	n := s.n - 1
	delta := x - s.mean
	mean := s.mean - delta/float64(n)
	delta2 := x - mean
	m2 := s.m2 - delta*delta2
	if m2 < 0 && m2 > -1e-12 {
		m2 = 0
	}
	return guardiannRunningStats{n: n, mean: mean, m2: m2, maxEver: s.maxEver}, nil
}

func (s guardiannRunningStats) combine(o guardiannRunningStats) guardiannRunningStats {
	if o.n == 0 {
		return s
	}
	if s.n == 0 {
		return o
	}
	n := s.n + o.n
	delta := o.mean - s.mean
	mean := s.mean + delta*float64(o.n)/float64(n)
	m2 := s.m2 + o.m2 + delta*delta*float64(s.n)*float64(o.n)/float64(n)
	return guardiannRunningStats{n: n, mean: mean, m2: m2, maxEver: math.Max(s.maxEver, o.maxEver)}
}

func (s guardiannRunningStats) meanOrNaN() float64 {
	if s.n == 0 {
		return math.NaN()
	}
	return s.mean
}

func (s guardiannRunningStats) populationStdDev() float64 {
	if s.n == 0 {
		return math.NaN()
	}
	return math.Sqrt(s.m2 / float64(s.n))
}

func (s guardiannRunningStats) maxEverOrNaN() float64 {
	if math.IsInf(s.maxEver, -1) {
		return math.NaN()
	}
	return s.maxEver
}

// Cluster states (ClusterMetadata.State), a bit set.
const (
	clusterStateSplitMerge = 1
	clusterStateReassign   = 2
	clusterStateCollapse   = 4
)

// guardiannClusterMetadata is Java's ClusterMetadata.
type guardiannClusterMetadata struct {
	id             tuple.UUID
	numUnderrep    int
	numReplicated  int
	stats          guardiannRunningStats
	states         int
	maxEverPrimary int
}

func (m guardiannClusterMetadata) numPrimary() int { return int(m.stats.n) }

func (m guardiannClusterMetadata) has(state int) bool { return m.states&state != 0 }

func (m guardiannClusterMetadata) mergeThreshold(c *guardiannConfig) int {
	return max(c.primaryClusterMin, int(math.Floor(c.mergeMaxEverFraction*float64(m.maxEverPrimary))))
}

func (m guardiannClusterMetadata) raisedMaxEver(stats guardiannRunningStats) int {
	return max(m.maxEverPrimary, int(stats.n))
}

func (m guardiannClusterMetadata) withNewVectors(underrep, replicated int, stats guardiannRunningStats, states int) guardiannClusterMetadata {
	return guardiannClusterMetadata{
		id: m.id, numUnderrep: underrep, numReplicated: replicated, stats: stats,
		states: states, maxEverPrimary: m.raisedMaxEver(stats),
	}
}

func (m guardiannClusterMetadata) withAdditionalVectorsAndStates(underrepAdded, replicatedAdded int, stats guardiannRunningStats, states int) guardiannClusterMetadata {
	return guardiannClusterMetadata{
		id: m.id, numUnderrep: m.numUnderrep + underrepAdded,
		numReplicated: m.numReplicated + replicatedAdded, stats: stats, states: m.states | states,
		maxEverPrimary: m.raisedMaxEver(stats),
	}
}

func (m guardiannClusterMetadata) withStates(states int) guardiannClusterMetadata {
	m.states = states
	return m
}

// guardiannVectorID is Java's VectorId: the primary key plus the uuid minted
// when the vector was inserted (a re-inserted key gets a new one).
type guardiannVectorID struct {
	pk   tuple.Tuple
	uuid tuple.UUID
}

func (v guardiannVectorID) key() string {
	return string(v.pk.Pack()) + string(v.uuid[:])
}

func (v guardiannVectorID) compare(o guardiannVectorID) int {
	if c := bytes.Compare(v.pk.Pack(), o.pk.Pack()); c != 0 {
		return c
	}
	return compareJavaUUID(v.uuid, o.uuid)
}

// compareJavaUUID is java.util.UUID.compareTo: signed most-significant long,
// then signed least-significant long.
func compareJavaUUID(a, b tuple.UUID) int {
	for _, off := range []int{0, 8} {
		x, y := int64(binary.BigEndian.Uint64(a[off:])), int64(binary.BigEndian.Uint64(b[off:]))
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Vector reference roles (VectorReference.Role).
const (
	roleCodePrimary         = 0
	roleCodeUnderreplicated = 1
	roleCodeReplicated      = 2
)

// guardiannVectorRef is Java's sealed VectorReference (PrimaryCopy /
// ReplicatedCopy).
type guardiannVectorRef struct {
	id        guardiannVectorID
	vector    gVector
	primary   bool
	underrep  bool    // PrimaryCopy only
	priority  float64 // ReplicatedCopy only
	collapsed bool
}

func (r guardiannVectorRef) replicationPriority() float64 {
	if r.primary {
		return -1
	}
	return r.priority
}

func (r guardiannVectorRef) isUnderreplicated() bool { return r.primary && r.underrep }

func (r guardiannVectorRef) replicationPriorityChanged(o guardiannVectorRef) bool {
	return math.Abs(o.replicationPriority()-r.replicationPriority()) > realVectorEPS*(1+math.Abs(r.replicationPriority()))
}

// realVectorEPS is Java's RealVector.EPS.
const realVectorEPS = 1e-12

func (r guardiannVectorRef) toPrimary() guardiannVectorRef {
	r.primary, r.underrep, r.priority = true, false, 0
	return r
}

func (r guardiannVectorRef) toPrimaryUnderreplicated() guardiannVectorRef {
	r.primary, r.underrep, r.priority = true, true, 0
	return r
}

func (r guardiannVectorRef) toReplicated(priority float64) guardiannVectorRef {
	r.primary, r.underrep, r.priority = false, false, priority
	return r
}

func (r guardiannVectorRef) toCollapsed(signature, vectorUUID tuple.UUID) guardiannVectorRef {
	r.id = guardiannVectorID{pk: tuple.Tuple{signature}, uuid: vectorUUID}
	r.collapsed = true
	return r
}

// compareByPriorityThenID is Comparator.comparing(replicationPriority).thenComparing(id).
func compareByPriorityThenID(a, b guardiannVectorRef) int {
	if c := compareFloat64Java(a.replicationPriority(), b.replicationPriority()); c != 0 {
		return c
	}
	return a.id.compare(b.id)
}

// compareFloat64Java is Double.compare.
func compareFloat64Java(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	ab, bb := int64(math.Float64bits(a)), int64(math.Float64bits(b))
	if math.IsNaN(a) {
		ab = int64(math.Float64bits(math.NaN()))
	}
	if math.IsNaN(b) {
		bb = int64(math.Float64bits(math.NaN()))
	}
	switch {
	case ab < bb:
		return -1
	case ab > bb:
		return 1
	}
	return 0
}

// guardiannClusterRef is Java's ClusterReference.
type guardiannClusterRef struct {
	clusterID tuple.UUID
	centroid  gVector
}

// guardiannClusterWithDistance is Java's ClusterMetadataWithDistance.
type guardiannClusterWithDistance struct {
	meta     guardiannClusterMetadata
	centroid gVector
	distance float64
}

func (c guardiannClusterWithDistance) equal(o guardiannClusterWithDistance) bool {
	return c.meta == o.meta && c.centroid.equal(o.centroid) && math.Float64bits(c.distance) == math.Float64bits(o.distance)
}

// guardiannCluster is Java's Cluster: metadata, centroid and references.
type guardiannCluster struct {
	meta     guardiannClusterMetadata
	centroid gVector
	refs     []guardiannVectorRef
}

// guardiannVectorMetadata is Java's VectorMetadata.
type guardiannVectorMetadata struct {
	id               guardiannVectorID
	additionalValues tuple.Tuple
}

// Random identities (RandomHelpers.randomUuid): deterministic draws come from
// the SplittableRandom, the rest are random v4 UUIDs through the DST seam.
func guardiannRandomUUID(random *splittableRandom, deterministic bool, env *dst.Env) (tuple.UUID, error) {
	if deterministic {
		return randomUUIDFrom(random), nil
	}
	var u tuple.UUID
	if _, err := env.Read(u[:]); err != nil {
		return u, err
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u, nil
}

// randomUUIDFrom is RandomHelpers.randomUuid(SplittableRandom).
func randomUUIDFrom(random *splittableRandom) tuple.UUID {
	msb := uint64(random.nextLong())&0xffffffffffff0fff | 0x0000000000004000
	lsb := uint64(random.nextLong())&0x3fffffffffffffff | 0x8000000000000000
	var u tuple.UUID
	binary.BigEndian.PutUint64(u[:8], msb)
	binary.BigEndian.PutUint64(u[8:], lsb)
	return u
}

// newSplittableRandomForUUID is RandomHelpers.random(UUID).
func newSplittableRandomForUUID(u tuple.UUID) *splittableRandom {
	return &splittableRandom{seed: seedFromBytes(u[:]), gamma: goldenGamma}
}

// nextInt is SplittableRandom.nextInt(bound).
func (r *splittableRandom) nextInt(bound int) int {
	rv := mix32(r.nextSeed())
	m := int32(bound - 1)
	if int32(bound)&m == 0 {
		return int(rv & m)
	}
	u := int32(uint32(rv) >> 1)
	for {
		rv = u % int32(bound)
		if u+m-rv >= 0 {
			return int(rv)
		}
		u = int32(uint32(mix32(r.nextSeed())) >> 1)
	}
}

// mix32 is SplittableRandom.mix32.
func mix32(z int64) int32 {
	u := uint64(z)
	u = (u ^ (u >> 33)) * 0x62a9d9ed799705f5
	return int32(((u ^ (u >> 28)) * 0xcb24d0a5c88c35b3) >> 32)
}

// guardiannTopK is Java's TopK: the k greatest items under cmp, backed by a
// binary heap whose array order toUnsortedList exposes.
type guardiannTopK struct {
	cmp   func(a, b guardiannVectorRef) int
	k     int
	items []guardiannVectorRef
}

func newTopKMax(k int, cmp func(a, b guardiannVectorRef) int) *guardiannTopK {
	return &guardiannTopK{cmp: cmp, k: k}
}

func (t *guardiannTopK) add(item guardiannVectorRef) {
	if len(t.items) < t.k {
		t.items = append(t.items, item)
		t.siftUp(len(t.items) - 1)
		return
	}
	if t.k == 0 || t.cmp(item, t.items[0]) <= 0 {
		return
	}
	// PriorityQueue.poll then offer.
	last := t.items[len(t.items)-1]
	t.items = t.items[:len(t.items)-1]
	if len(t.items) > 0 {
		t.items[0] = last
		t.siftDown(0)
	}
	t.items = append(t.items, item)
	t.siftUp(len(t.items) - 1)
}

func (t *guardiannTopK) siftUp(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if t.cmp(t.items[i], t.items[p]) >= 0 {
			return
		}
		t.items[i], t.items[p] = t.items[p], t.items[i]
		i = p
	}
}

func (t *guardiannTopK) siftDown(i int) {
	n := len(t.items)
	for {
		c := 2*i + 1
		if c >= n {
			return
		}
		if c+1 < n && t.cmp(t.items[c+1], t.items[c]) < 0 {
			c++
		}
		if t.cmp(t.items[i], t.items[c]) <= 0 {
			return
		}
		t.items[i], t.items[c] = t.items[c], t.items[i]
		i = c
	}
}

func (t *guardiannTopK) unsorted() []guardiannVectorRef {
	return append([]guardiannVectorRef(nil), t.items...)
}

// guardiannRefAndDistance is Java's VectorReferenceAndDistance.
type guardiannRefAndDistance struct {
	ref      guardiannVectorRef
	distance float64
}

func compareRefAndDistance(a, b guardiannRefAndDistance) int {
	if c := compareFloat64Java(a.distance, b.distance); c != 0 {
		return c
	}
	return a.ref.id.compare(b.ref.id)
}

// distinctTopKMin is DistinctTopK.min(distance then id, k): the k smallest
// distinct items, ascending.
func distinctTopKMin(items []guardiannRefAndDistance, k int) []guardiannRefAndDistance {
	sorted := append([]guardiannRefAndDistance(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool { return compareRefAndDistance(sorted[i], sorted[j]) < 0 })
	out := sorted[:0:0]
	for _, it := range sorted {
		if len(out) > 0 && compareRefAndDistance(out[len(out)-1], it) == 0 {
			continue
		}
		out = append(out, it)
		if len(out) == k {
			break
		}
	}
	return out
}

// javaNameUUID is UUID.nameUUIDFromBytes: an MD5, version-3 UUID.
func javaNameUUID(name []byte) tuple.UUID {
	sum := md5.Sum(name)
	sum[6] = sum[6]&0x0f | 0x30
	sum[8] = sum[8]&0x3f | 0x80
	return tuple.UUID(sum)
}
