package recordlayer

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// GuardiANN subspaces (StorageAdapter.SUBSPACE_PREFIX_*).
const (
	gSubAccessInfo      = 0x00
	gSubCentroids       = 0x01
	gSubClusterMetadata = 0x02
	gSubVectorRefs      = 0x03
	gSubCollapsed       = 0x04
	gSubVectorMetadata  = 0x05
	gSubSamples         = 0x06
	gSubTasks           = 0x07
)

// guardiannListener receives task lifecycle events (OnWriteListener's
// onTaskEnqueued / onTaskExecuted); the record layer counts them.
type guardiannListener interface {
	onTaskEnqueued()
	onTaskExecuted()
}

// guardiann is one GuardiANN structure (Java Guardiann + Locator +
// Primitives) over a subspace, driven within one transaction.
type guardiann struct {
	ss        subspace.Subspace
	config    guardiannConfig
	env       *dst.Env
	listener  guardiannListener
	centroids *hnswGraph
	codec     *guardiannVectorCodec
	// poison makes the enclosing record context uncommittable; nil outside a
	// record store.
	poison func(error)
	// timer counts the vector references read (Java's OnRead.onVectorRead);
	// nil outside a record store.
	timer *StoreTimer
}

func newGuardiann(ss subspace.Subspace, config guardiannConfig, env *dst.Env, listener guardiannListener) *guardiann {
	hc := DefaultHNSWConfig(config.numDimensions)
	hc.Metric = config.metric
	hc.EfRepair = 64
	hc.M, hc.MMax, hc.MMax0 = 16, 24, 32
	storage := newHNSWStorage(ss.Sub(int64(gSubCentroids)), hc)
	storage.env = env
	return &guardiann{ss: ss, config: config, env: env, listener: listener, codec: &guardiannVectorCodec{config: config}, centroids: NewHNSWGraph(storage, hc)}
}

func (g *guardiann) sub(prefix int64) subspace.Subspace { return g.ss.Sub(prefix) }

func (g *guardiann) withAccessInfo(info *guardiannAccessInfoValue) (*guardiann, error) {
	local := *g
	var err error
	local.codec, err = newGuardiannVectorCodec(g.config, info)
	return &local, err
}

func (g *guardiann) distance(a, b gVector) (float64, error) {
	return g.codec.distance(a, b)
}

func (g *guardiann) randomUUID(random *splittableRandom) (tuple.UUID, error) {
	return guardiannRandomUUID(random, g.config.deterministicRandomness, g.env)
}

// guardiannAccessInfoValue is Java's guardiann AccessInfo.
type guardiannAccessInfoValue struct {
	rotatorSeed     int64
	negatedCentroid []float64 // nil until RaBitQ can be used
}

func (g *guardiann) fetchAccessInfo(tx fdb.ReadTransaction) (*guardiannAccessInfoValue, error) {
	b, err := tx.Get(fdb.Key(g.sub(gSubAccessInfo).Bytes())).Get()
	if err != nil || b == nil {
		return nil, err
	}
	return accessInfoFromValue(b)
}

// accessInfoFromValue is StorageAdapter.accessInfoFromTuple.
func accessInfoFromValue(b []byte) (*guardiannAccessInfoValue, error) {
	t, err := tuple.Unpack(b)
	if err != nil {
		return nil, err
	}
	seed, err := guardiannElem[int64](t, 0, "access info")
	if err != nil {
		return nil, err
	}
	info := &guardiannAccessInfoValue{rotatorSeed: seed}
	if len(t) > 1 && t[1] != nil {
		nested, err := guardiannElem[tuple.Tuple](t, 1, "access info")
		if err != nil {
			return nil, err
		}
		raw, err := guardiannElem[[]byte](nested, 0, "access info centroid")
		if err != nil {
			return nil, err
		}
		v, err := decodeGVector(raw)
		if err != nil {
			return nil, err
		}
		info.negatedCentroid = v.data
	}
	return info, nil
}

func (g *guardiann) writeAccessInfo(tx fdb.WritableTransaction, info *guardiannAccessInfoValue) {
	var centroid any
	if info.negatedCentroid != nil {
		centroid = tuple.Tuple{serializeVector(info.negatedCentroid)}
	}
	tx.Set(fdb.Key(g.sub(gSubAccessInfo).Bytes()), tuple.Tuple{info.rotatorSeed, centroid}.Pack())
}

func (g *guardiann) fetchVectorMetadata(tx fdb.ReadTransaction, pk tuple.Tuple) (*guardiannVectorMetadata, error) {
	b, err := tx.Get(fdb.Key(g.sub(gSubVectorMetadata).Pack(pk))).Get()
	if err != nil || b == nil {
		return nil, err
	}
	return vectorMetadataFromValue(pk, b)
}

// vectorMetadataFromValue is StorageAdapter.vectorMetadataFromTuples.
func vectorMetadataFromValue(pk tuple.Tuple, b []byte) (*guardiannVectorMetadata, error) {
	t, err := tuple.Unpack(b)
	if err != nil {
		return nil, err
	}
	id, err := guardiannElem[tuple.UUID](t, 0, "vector metadata")
	if err != nil {
		return nil, err
	}
	md := &guardiannVectorMetadata{id: guardiannVectorID{pk: pk, uuid: id}}
	if len(t) > 1 && t[1] != nil {
		if md.additionalValues, err = guardiannElem[tuple.Tuple](t, 1, "vector metadata"); err != nil {
			return nil, err
		}
	}
	return md, nil
}

func (g *guardiann) writeVectorMetadata(tx fdb.WritableTransaction, md guardiannVectorMetadata) {
	var av any
	if md.additionalValues != nil {
		av = md.additionalValues
	}
	tx.Set(fdb.Key(g.sub(gSubVectorMetadata).Pack(md.id.pk)), tuple.Tuple{md.id.uuid, av}.Pack())
}

func (g *guardiann) deleteVectorMetadata(tx fdb.WritableTransaction, pk tuple.Tuple) {
	tx.Clear(fdb.Key(g.sub(gSubVectorMetadata).Pack(pk)))
}

func (g *guardiann) clusterMetadataKey(id tuple.UUID) fdb.Key {
	return fdb.Key(g.sub(gSubClusterMetadata).Pack(tuple.Tuple{id}))
}

func (g *guardiann) fetchClusterMetadata(tx fdb.ReadTransaction, id tuple.UUID) (*guardiannClusterMetadata, error) {
	b, err := tx.Get(g.clusterMetadataKey(id)).Get()
	if err != nil || b == nil {
		return nil, err
	}
	return clusterMetadataFromValue(id, b)
}

// clusterMetadataFromValue is StorageAdapter.clusterMetadataFromTuple. A
// four-element value comes from a GuardiANN build before the lifetime peak was
// stored; Java 4.14.2.0 cannot read it either, so it is refused by name.
func clusterMetadataFromValue(id tuple.UUID, b []byte) (*guardiannClusterMetadata, error) {
	t, err := tuple.Unpack(b)
	if err != nil {
		return nil, err
	}
	if len(t) == 4 {
		return nil, &RecordCoreError{Message: fmt.Sprintf(
			"guardiann cluster metadata of cluster %v has 4 elements, want 5: it was written by a GuardiANN build that did not store the lifetime peak; rebuild the index", id)}
	}
	var md guardiannClusterMetadata
	md.id = id
	if md.numUnderrep, err = guardiannInt(t, 0, "cluster metadata"); err != nil {
		return nil, err
	}
	if md.numReplicated, err = guardiannInt(t, 1, "cluster metadata"); err != nil {
		return nil, err
	}
	st, err := guardiannElem[tuple.Tuple](t, 2, "cluster metadata")
	if err != nil {
		return nil, err
	}
	if md.stats.n, err = guardiannElem[int64](st, 0, "cluster running stats"); err != nil {
		return nil, err
	}
	if md.stats.mean, err = guardiannElem[float64](st, 1, "cluster running stats"); err != nil {
		return nil, err
	}
	if md.stats.m2, err = guardiannElem[float64](st, 2, "cluster running stats"); err != nil {
		return nil, err
	}
	if md.stats.maxEver, err = guardiannElem[float64](st, 3, "cluster running stats"); err != nil {
		return nil, err
	}
	if md.states, err = guardiannInt(t, 3, "cluster metadata"); err != nil {
		return nil, err
	}
	if md.maxEverPrimary, err = guardiannInt(t, 4, "cluster metadata"); err != nil {
		return nil, err
	}
	return &md, nil
}

// guardiannElem reads element i of a stored tuple as T. Java's Tuple getters
// throw on a missing or mistyped element; Go returns that as an error.
func guardiannElem[T any](t tuple.Tuple, i int, what string) (T, error) {
	var zero T
	if i >= len(t) {
		return zero, &RecordCoreError{Message: fmt.Sprintf("guardiann %s: element %d missing (%d elements)", what, i, len(t))}
	}
	v, ok := t[i].(T)
	if !ok {
		return zero, &RecordCoreError{Message: fmt.Sprintf("guardiann %s: element %d is %T, want %T", what, i, t[i], zero)}
	}
	return v, nil
}

// guardiannInt is Math.toIntExact(tuple.getLong(i)).
func guardiannInt(t tuple.Tuple, i int, what string) (int, error) {
	v, err := guardiannElem[int64](t, i, what)
	if err != nil {
		return 0, err
	}
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, &RecordCoreError{Message: fmt.Sprintf("guardiann %s: element %d (%d) is outside Java's int range", what, i, v)}
	}
	return int(v), nil
}

func (g *guardiann) requireClusterMetadata(tx fdb.ReadTransaction, id tuple.UUID) (guardiannClusterMetadata, error) {
	md, err := g.fetchClusterMetadata(tx, id)
	if err != nil {
		return guardiannClusterMetadata{}, err
	}
	if md == nil {
		return guardiannClusterMetadata{}, &RecordCoreError{Message: "cluster found in centroid HNSW is missing its metadata"}
	}
	return *md, nil
}

func (g *guardiann) writeClusterMetadata(tx fdb.WritableTransaction, m guardiannClusterMetadata) {
	tx.Set(g.clusterMetadataKey(m.id), clusterMetaValue(m))
}

// clusterMetaValue is StorageAdapter.valueTupleFromClusterMetadata.
func clusterMetaValue(m guardiannClusterMetadata) []byte {
	s := m.stats
	return tuple.Tuple{
		int64(m.numUnderrep), int64(m.numReplicated),
		tuple.Tuple{s.n, s.mean, s.m2, s.maxEver},
		int64(m.states), int64(m.maxEverPrimary),
	}.Pack()
}

func (g *guardiann) deleteClusterMetadata(tx fdb.WritableTransaction, id tuple.UUID) {
	tx.Clear(g.clusterMetadataKey(id))
}

// vectorRefFromValue is StorageAdapter.vectorReferenceFromTuples.
func vectorRefFromValue(pk tuple.Tuple, value []byte, decode func([]byte) (gVector, error)) (guardiannVectorRef, error) {
	t, err := tuple.Unpack(value)
	if err != nil {
		return guardiannVectorRef{}, err
	}
	const what = "vector reference"
	id, err := guardiannElem[tuple.UUID](t, 0, what)
	if err != nil {
		return guardiannVectorRef{}, err
	}
	role, err := guardiannElem[int64](t, 1, what)
	if err != nil {
		return guardiannVectorRef{}, err
	}
	collapsed, err := guardiannElem[bool](t, 2, what)
	if err != nil {
		return guardiannVectorRef{}, err
	}
	raw, err := guardiannElem[[]byte](t, 3, what)
	if err != nil {
		return guardiannVectorRef{}, err
	}
	vec, err := decode(raw)
	if err != nil {
		return guardiannVectorRef{}, err
	}
	ref := guardiannVectorRef{id: guardiannVectorID{pk: pk, uuid: id}, vector: vec, collapsed: collapsed}
	switch role {
	case roleCodePrimary:
		ref.primary = true
	case roleCodeUnderreplicated:
		ref.primary, ref.underrep = true, true
	case roleCodeReplicated:
		if ref.priority, err = guardiannElem[float64](t, 4, what); err != nil {
			return guardiannVectorRef{}, err
		}
	default:
		return ref, fmt.Errorf("unknown vector reference role code: %d", t[1])
	}
	return ref, nil
}

// vectorRefValue is StorageAdapter.valueTupleFromVectorReference.
func vectorRefValue(ref guardiannVectorRef, encode func(gVector) []byte) []byte {
	raw := encode(ref.vector)
	if !ref.primary {
		return tuple.Tuple{ref.id.uuid, int64(roleCodeReplicated), ref.collapsed, raw, ref.priority}.Pack()
	}
	role := int64(roleCodePrimary)
	if ref.underrep {
		role = roleCodeUnderreplicated
	}
	return tuple.Tuple{ref.id.uuid, role, ref.collapsed, raw}.Pack()
}

func (g *guardiann) fetchVectorRefs(tx fdb.ReadTransaction, clusterID tuple.UUID, decode func([]byte) (gVector, error)) ([]guardiannVectorRef, error) {
	refs := g.sub(gSubVectorRefs)
	r, err := fdb.PrefixRange(refs.Pack(tuple.Tuple{clusterID}))
	if err != nil {
		return nil, err
	}
	kvs, err := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	out := make([]guardiannVectorRef, 0, len(kvs))
	for _, kv := range kvs {
		key, err := refs.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		pk, err := guardiannElem[tuple.Tuple](key, 1, "vector reference key")
		if err != nil {
			return nil, err
		}
		ref, err := vectorRefFromValue(pk, kv.Value, decode)
		if err != nil {
			return nil, err
		}
		g.timer.Increment(CountVectorVectorReads)
		out = append(out, ref)
	}
	return out, nil
}

func (g *guardiann) fetchVectorRef(tx fdb.ReadTransaction, clusterID tuple.UUID, pk tuple.Tuple) (*guardiannVectorRef, error) {
	b, err := tx.Get(fdb.Key(g.sub(gSubVectorRefs).Pack(tuple.Tuple{clusterID, pk}))).Get()
	if err != nil || b == nil {
		return nil, err
	}
	ref, err := vectorRefFromValue(pk, b, g.codec.decode)
	if err == nil {
		g.timer.Increment(CountVectorVectorReads)
	}
	return &ref, err
}

func (g *guardiann) writeVectorRef(tx fdb.WritableTransaction, clusterID tuple.UUID, ref guardiannVectorRef) {
	tx.Set(fdb.Key(g.sub(gSubVectorRefs).Pack(tuple.Tuple{clusterID, ref.id.pk})), vectorRefValue(ref, g.codec.encode))
}

func (g *guardiann) deleteVectorRef(tx fdb.WritableTransaction, clusterID tuple.UUID, pk tuple.Tuple) {
	tx.Clear(fdb.Key(g.sub(gSubVectorRefs).Pack(tuple.Tuple{clusterID, pk})))
}

func (g *guardiann) deleteVectorRefsForCluster(tx fdb.WritableTransaction, clusterID tuple.UUID) error {
	r, err := fdb.PrefixRange(g.sub(gSubVectorRefs).Pack(tuple.Tuple{clusterID}))
	if err != nil {
		return err
	}
	tx.ClearRange(r)
	return nil
}

func (g *guardiann) fetchCollapsedIDs(tx fdb.ReadTransaction, signature tuple.UUID) ([]guardiannVectorID, error) {
	cs := g.sub(gSubCollapsed)
	r, err := fdb.PrefixRange(cs.Pack(tuple.Tuple{signature}))
	if err != nil {
		return nil, err
	}
	kvs, err := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	out := make([]guardiannVectorID, 0, len(kvs))
	for _, kv := range kvs {
		key, err := cs.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		pk, err := guardiannElem[tuple.Tuple](key, 1, "collapsed id key")
		if err != nil {
			return nil, err
		}
		id, err := collapsedIDFromValue(pk, kv.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, *id)
	}
	return out, nil
}

func (g *guardiann) fetchCollapsedID(tx fdb.ReadTransaction, signature tuple.UUID, pk tuple.Tuple) (*guardiannVectorID, error) {
	b, err := tx.Get(fdb.Key(g.sub(gSubCollapsed).Pack(tuple.Tuple{signature, pk}))).Get()
	if err != nil || b == nil {
		return nil, err
	}
	return collapsedIDFromValue(pk, b)
}

func collapsedIDFromValue(pk tuple.Tuple, b []byte) (*guardiannVectorID, error) {
	v, err := tuple.Unpack(b)
	if err != nil {
		return nil, err
	}
	id, err := guardiannElem[tuple.UUID](v, 0, "collapsed id")
	if err != nil {
		return nil, err
	}
	return &guardiannVectorID{pk: pk, uuid: id}, nil
}

func (g *guardiann) writeCollapsedID(tx fdb.WritableTransaction, signature tuple.UUID, id guardiannVectorID) {
	tx.Set(fdb.Key(g.sub(gSubCollapsed).Pack(tuple.Tuple{signature, id.pk})), tuple.Tuple{id.uuid}.Pack())
}

func (g *guardiann) deleteCollapsedID(tx fdb.WritableTransaction, signature tuple.UUID, pk tuple.Tuple) {
	tx.Clear(fdb.Key(g.sub(gSubCollapsed).Pack(tuple.Tuple{signature, pk})))
}

// signatureUUID is StorageAdapter.signatureUuid: the first 16 bytes of the
// SHA-256 of the vector's raw data, stamped as a version-8 IETF UUID.
func signatureUUID(v gVector) tuple.UUID {
	sum := sha256.Sum256(v.encode())
	hi := binary.BigEndian.Uint64(sum[0:8])&0xffffffffffff0fff | 0x0000000000008000
	lo := binary.BigEndian.Uint64(sum[8:16])&0x3fffffffffffffff | 0x8000000000000000
	var u tuple.UUID
	binary.BigEndian.PutUint64(u[:8], hi)
	binary.BigEndian.PutUint64(u[8:], lo)
	return u
}

// replicationPriority is StorageAdapter.replicationPriority.
func (c *guardiannConfig) replicationPriority(distance, distanceToPrimaryCentroid float64, num int, mean, stdDev float64) float64 {
	const eps = 1.0e-12
	r := distanceToPrimaryCentroid / (distance + eps)
	z := 0.0
	if c.replicationZScoreWeight != 0 && num >= c.replicationStatsMinSampleSize {
		z = math.Max(0, (distanceToPrimaryCentroid-mean)/(stdDev+eps))
	}
	return c.replicationDistanceRatioWeight*r + c.replicationZScoreWeight*z
}

// isOccluded is StorageAdapter.isOccluded: a candidate is skipped when an
// already selected replication cluster's centroid lies closer to it than the
// vector does.
func (g *guardiann) isOccluded(candidate guardiannClusterWithDistance, selected []guardiannClusterWithDistance) (bool, error) {
	for _, s := range selected {
		distance, err := g.distance(candidate.centroid, s.centroid)
		if err != nil {
			return false, err
		}
		if candidate.distance > distance {
			return true, nil
		}
	}
	return false, nil
}

// clusterRefTuple is StorageAdapter.valueTupleFromClusterReference.
func clusterRefTuple(r guardiannClusterRef, encode func(gVector) []byte) tuple.Tuple {
	return tuple.Tuple{r.clusterID, encode(r.centroid)}
}

func clusterRefFromTuple(t tuple.Tuple, decode func([]byte) (gVector, error)) (guardiannClusterRef, error) {
	id, err := guardiannElem[tuple.UUID](t, 0, "cluster reference")
	if err != nil {
		return guardiannClusterRef{}, err
	}
	raw, err := guardiannElem[[]byte](t, 1, "cluster reference")
	if err != nil {
		return guardiannClusterRef{}, err
	}
	v, err := decode(raw)
	return guardiannClusterRef{clusterID: id, centroid: v}, err
}

func uuidSetTuple(ids []tuple.UUID) tuple.Tuple {
	t := make(tuple.Tuple, len(ids))
	for i, id := range ids {
		t[i] = id
	}
	return t
}

func uuidSetFromTuple(t tuple.Tuple) ([]tuple.UUID, error) {
	out := make([]tuple.UUID, len(t))
	for i := range t {
		id, err := guardiannElem[tuple.UUID](t, i, "uuid set")
		if err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}

// appendUniqueUUID keeps an ImmutableSet's insertion order.
func appendUniqueUUID(set []tuple.UUID, id tuple.UUID) []tuple.UUID {
	for _, e := range set {
		if e == id {
			return set
		}
	}
	return append(set, id)
}

func containsUUID(set []tuple.UUID, id tuple.UUID) bool {
	for _, e := range set {
		if e == id {
			return true
		}
	}
	return false
}
