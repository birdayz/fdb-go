package plans

import (
	"encoding/binary"
	"hash"
	"hash/fnv"
)

// PlanHash computes a deterministic hash of the entire plan tree.
// The hash combines each node's HashCodeWithoutChildren with its
// structural position (depth-first traversal order). Two plans
// with the same tree shape and same node-info hash to the same key.
//
// Used as the cache key for plan caching (RFC-024).
func PlanHash(p RecordQueryPlan) uint64 {
	if p == nil {
		return 0
	}
	h := fnv.New64a()
	planHashRecursive(h, p, 0)
	return h.Sum64()
}

func planHashRecursive(h hash.Hash64, p RecordQueryPlan, depth int) {
	var depthBuf [4]byte
	binary.BigEndian.PutUint32(depthBuf[:], uint32(depth))
	_, _ = h.Write(depthBuf[:])

	var nodeBuf [8]byte
	binary.BigEndian.PutUint64(nodeBuf[:], p.HashCodeWithoutChildren())
	_, _ = h.Write(nodeBuf[:])

	for i, child := range p.GetChildren() {
		planHashRecursive(h, child, depth*31+i+1)
	}
}

// PlanHashEqual reports whether two plans have the same hash.
func PlanHashEqual(a, b RecordQueryPlan) bool {
	return PlanHash(a) == PlanHash(b)
}
