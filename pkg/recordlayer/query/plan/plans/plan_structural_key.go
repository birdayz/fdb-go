package plans

// structuralKey centralizes the per-plan EqualsPlanWithoutChildren /
// HashCodeWithoutChildren pairs behind ONE typed builder. Each plan declares
// its identifying fields (children excluded) exactly once via structuralKey();
// the same key drives BOTH equality and hashing, so the two can never disagree
// on which fields matter — the "hand-copy reintroduces the class" risk that
// every independently-maintained equals/hash pair carries.
//
// The hash value is Go-internal (memo dedup + REWRITING-phase tie-break in
// deepHashCode); it is not wire-persisted and not the PLANNING-phase tie-break
// (stablePlanHash is independent). Equal keys MUST hash equal — guaranteed here
// because Equal and Hash walk the identical part list.

import (
	"encoding/binary"
	"hash"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type partKind uint8

// New kinds are APPENDED — the kind byte is folded into the hash, so reordering
// would change every migrated plan's hash value.
const (
	partBool partKind = iota
	partInt
	partStr
	partStrs
	partValue
	partStructVal
	partValues
	partPreds
	partType
	partScanComps
	partValuePtr
	partIntPtr
	partSortKeys
	partEquatable
	partSub
)

// part is one identifying field. Constructed only via the typed builder
// methods, so the (kind → payload) mapping is exhaustive by construction — no
// reflection, no untyped fallthrough that could silently mis-hash a new field.
type part struct {
	kind      partKind
	b         bool
	i         int64
	s         string
	ss        []string
	v         values.Value
	vs        []values.Value
	preds     []predicates.QueryPredicate
	typ       values.Type
	scanComps []*predicates.ComparisonRange
	iptr      *int
	sks       []SortKey
	obj       any
	eq        func(other any) bool
	eqHash    []byte
	sub       *structuralKey
}

// structuralKey is an ordered list of a plan's identifying fields.
type structuralKey struct{ parts []part }

func newStructuralKey() *structuralKey { return &structuralKey{} }

func (k *structuralKey) Bool(b bool) *structuralKey {
	k.parts = append(k.parts, part{kind: partBool, b: b})
	return k
}

func (k *structuralKey) Int(i int) *structuralKey {
	k.parts = append(k.parts, part{kind: partInt, i: int64(i)})
	return k
}

func (k *structuralKey) Int64(i int64) *structuralKey {
	k.parts = append(k.parts, part{kind: partInt, i: i})
	return k
}

func (k *structuralKey) Str(s string) *structuralKey {
	k.parts = append(k.parts, part{kind: partStr, s: s})
	return k
}

// Strs folds an ORDERED string slice (record-type lists, column-name lists).
// Order and length are load-bearing.
func (k *structuralKey) Strs(ss []string) *structuralKey {
	k.parts = append(k.parts, part{kind: partStrs, ss: ss})
	return k
}

// Alias folds a correlation identifier by its stable name — matching every
// hand-rolled site that wrote alias.Name() / compared alias != o.alias.
func (k *structuralKey) Alias(a values.CorrelationIdentifier) *structuralKey {
	return k.Str(a.Name())
}

func (k *structuralKey) Value(v values.Value) *structuralKey {
	k.parts = append(k.parts, part{kind: partValue, v: v})
	return k
}

// StructVal is like Value but compares by STRUCTURAL equality
// (values.ValuesStructurallyEqual) rather than semantic-under-alias-map. It
// exists so a plan whose hand-rolled equals used ValuesStructurallyEqual keeps
// that exact primitive when it migrates — structural equality is stricter
// (alias-sensitive) than semantic. The hash is the same alias-invariant
// SemanticHashCode: structural equality implies semantic equality implies equal
// SemanticHashCode, so equal⟹same-hash still holds.
func (k *structuralKey) StructVal(v values.Value) *structuralKey {
	k.parts = append(k.parts, part{kind: partStructVal, v: v})
	return k
}

func (k *structuralKey) Values(vs []values.Value) *structuralKey {
	k.parts = append(k.parts, part{kind: partValues, vs: vs})
	return k
}

func (k *structuralKey) Preds(ps []predicates.QueryPredicate) *structuralKey {
	k.parts = append(k.parts, part{kind: partPreds, preds: ps})
	return k
}

// Type folds a flowed values.Type by typeEquals — the exact primitive the
// hand-rolled scan/index-scan equals used. It is EQUALS-ONLY: the hash omits it
// (contributing only the kind byte), matching those plans' hand-rolled hashes,
// which never folded flowedType. A type mismatch still separates identities via
// Equal; two plans differing only in flowed type collide in the hash (a valid
// under-hash — equal⟹same-hash still holds).
func (k *structuralKey) Type(t values.Type) *structuralKey {
	k.parts = append(k.parts, part{kind: partType, typ: t})
	return k
}

// ScanComps folds a scan/index-scan ComparisonRange list — the SARG bounds —
// via the same scanComparisonRangesEqual / writeScanComparisonRangesHash
// primitives the hand-rolled methods used.
func (k *structuralKey) ScanComps(sc []*predicates.ComparisonRange) *structuralKey {
	k.parts = append(k.parts, part{kind: partScanComps, scanComps: sc})
	return k
}

// ValuePtr folds a Value by POINTER/interface identity (Go ==), NOT semantic
// equality — the exact primitive the hand-rolled Explode / TableFunction equals
// used (p.collectionValue == o.collectionValue). Distinct from Value (semantic):
// two structurally-equal but distinct Value instances are DIFFERENT here, so a
// migrating plan keeps them in separate memo members. Hash folds the Value's
// stable .Name() (nil → empty), matching those plans' hand-rolled hashes.
func (k *structuralKey) ValuePtr(v values.Value) *structuralKey {
	k.parts = append(k.parts, part{kind: partValuePtr, v: v})
	return k
}

// IntPtr folds an optional int (*int) by eqIntPtr — nil and non-nil are
// distinct, two non-nil compare by value. The exact primitive the hand-rolled
// vector-scan equals used for efSearch. Hash folds a nil sentinel or the int.
func (k *structuralKey) IntPtr(p *int) *structuralKey {
	k.parts = append(k.parts, part{kind: partIntPtr, iptr: p})
	return k
}

// SortKeys folds an ORDERED []SortKey via sortKeyEqual (display Field +
// Desc / NullsFirst direction + the semantic ValueExpr) — the InMemorySort
// identity. Hash folds each key's identifying fields (the same set sortKeyEqual
// compares), preserving equal⟹same-hash.
func (k *structuralKey) SortKeys(sks []SortKey) *structuralKey {
	k.parts = append(k.parts, part{kind: partSortKeys, sks: sks})
	return k
}

// Equatable is the escape hatch for a plan field whose identity primitive is the
// field type's OWN comparison — a custom .Equals() (PlanSelector, KeysSource) or
// a bespoke slice compare (InJoin inValues via inValuesEqual, InUnion inSources
// via reflect.DeepEqual) — that the typed kinds above don't model. eq captures
// THIS side's value and compares it against the other side's raw value (obj);
// hashBytes is this side's stable hash contribution, which the CALLER guarantees
// is identical for equal values (obj.String(), a dimension encoding, %#v — the
// exact bytes the hand-rolled hash folded). That caller guarantee is what keeps
// equal⟹same-hash — the one invariant the builder cannot enforce for an opaque
// value it compares only through a closure.
func (k *structuralKey) Equatable(obj any, eq func(other any) bool, hashBytes []byte) *structuralKey {
	k.parts = append(k.parts, part{kind: partEquatable, obj: obj, eq: eq, eqHash: hashBytes})
	return k
}

// Sub nests another plan's structuralKey — used where a plan's identity embeds a
// whole sub-plan compared structurally (RecordQueryAggregateIndexPlan wraps a
// RecordQueryIndexPlan via its EqualsPlanWithoutChildren, which IS
// indexPlan.structuralKey().Equal). Equal recurses into the sub-key; Hash folds
// the sub-key's parts (strengthening the hand-rolled hash, which folded only the
// index name — safe: equal⟹same-hash, fewer collisions).
func (k *structuralKey) Sub(sub *structuralKey) *structuralKey {
	k.parts = append(k.parts, part{kind: partSub, sub: sub})
	return k
}

// Equal reports element-wise structural equality, dispatching per kind through
// the same semantic comparators the hand-rolled methods used.
func (k *structuralKey) Equal(o *structuralKey) bool {
	if len(k.parts) != len(o.parts) {
		return false
	}
	for i := range k.parts {
		if !partEqual(k.parts[i], o.parts[i]) {
			return false
		}
	}
	return true
}

func partEqual(a, b part) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case partBool:
		return a.b == b.b
	case partInt:
		return a.i == b.i
	case partStr:
		return a.s == b.s
	case partStrs:
		if len(a.ss) != len(b.ss) {
			return false
		}
		for i := range a.ss {
			if a.ss[i] != b.ss[i] {
				return false
			}
		}
		return true
	case partValue:
		return semanticValueEquals(a.v, b.v)
	case partStructVal:
		return values.ValuesStructurallyEqual(a.v, b.v)
	case partValues:
		if len(a.vs) != len(b.vs) {
			return false
		}
		for i := range a.vs {
			if !semanticValueEquals(a.vs[i], b.vs[i]) {
				return false
			}
		}
		return true
	case partPreds:
		if len(a.preds) != len(b.preds) {
			return false
		}
		for i := range a.preds {
			if !predicates.PredicateEquals(a.preds[i], b.preds[i]) {
				return false
			}
		}
		return true
	case partType:
		return typeEquals(a.typ, b.typ)
	case partScanComps:
		return scanComparisonRangesEqual(a.scanComps, b.scanComps)
	case partValuePtr:
		return a.v == b.v
	case partIntPtr:
		return eqIntPtr(a.iptr, b.iptr)
	case partSortKeys:
		if len(a.sks) != len(b.sks) {
			return false
		}
		for i := range a.sks {
			if !sortKeyEqual(a.sks[i], b.sks[i]) {
				return false
			}
		}
		return true
	case partEquatable:
		return a.eq(b.obj)
	case partSub:
		return a.sub.Equal(b.sub)
	}
	return false
}

// Hash folds the discriminator and every part into an FNV-64a digest. A length
// tag precedes each variable-length part so [A][BC] and [AB][C] cannot collide.
func (k *structuralKey) Hash(discriminator string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(discriminator))
	k.writeParts(h)
	return h.Sum64()
}

// writeParts folds every part into w. Extracted from Hash so partSub can fold a
// nested key's parts into the SAME digest — a nested key contributes its parts,
// NOT its discriminator (equal sub-keys have equal parts, so equal⟹same-hash
// still holds).
func (k *structuralKey) writeParts(w hash.Hash64) {
	var buf [8]byte
	for _, p := range k.parts {
		w.Write([]byte{byte(p.kind)})
		switch p.kind {
		case partBool:
			if p.b {
				w.Write([]byte{1})
			} else {
				w.Write([]byte{0})
			}
		case partInt:
			binary.BigEndian.PutUint64(buf[:], uint64(p.i))
			w.Write(buf[:])
		case partStr:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.s)))
			w.Write(buf[:])
			w.Write([]byte(p.s))
		case partStrs:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.ss)))
			w.Write(buf[:])
			for _, s := range p.ss {
				binary.BigEndian.PutUint64(buf[:], uint64(len(s)))
				w.Write(buf[:])
				w.Write([]byte(s))
			}
		case partValue, partStructVal:
			writeValueHash(w, p.v)
		case partValues:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.vs)))
			w.Write(buf[:])
			for _, v := range p.vs {
				writeValueHash(w, v)
			}
		case partPreds:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.preds)))
			w.Write(buf[:])
			for _, pr := range p.preds {
				binary.BigEndian.PutUint64(buf[:], predicates.SemanticHashCode(pr))
				w.Write(buf[:])
			}
		case partType:
			// Equals-only — no payload (the kind byte above is the whole
			// contribution). Mirrors the hand-rolled scan/index hashes, which
			// fold recordTypes + scanComparisons + flags but never flowedType.
		case partScanComps:
			writeScanComparisonRangesHash(w, p.scanComps)
		case partValuePtr:
			// Pointer-identity equal ⟹ same interface value ⟹ same Name();
			// length-tagged so distinct names cannot collide across a boundary.
			var name string
			if p.v != nil {
				name = p.v.Name()
			}
			binary.BigEndian.PutUint64(buf[:], uint64(len(name)))
			w.Write(buf[:])
			w.Write([]byte(name))
		case partIntPtr:
			if p.iptr == nil {
				w.Write([]byte{0})
			} else {
				w.Write([]byte{1})
				binary.BigEndian.PutUint64(buf[:], uint64(*p.iptr))
				w.Write(buf[:])
			}
		case partSortKeys:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.sks)))
			w.Write(buf[:])
			for _, sk := range p.sks {
				// sk.Field is DISPLAY-ONLY and is deliberately NOT folded, so
				// equal sort keys hash equal (RFC-197 item 3; sortKeyEqual
				// stopped comparing it).
				if sk.Desc {
					w.Write([]byte{1})
				} else {
					w.Write([]byte{0})
				}
				if sk.NullsFirst {
					w.Write([]byte{1})
				} else {
					w.Write([]byte{0})
				}
				writeValueHash(w, sk.ValueExpr)
			}
		case partEquatable:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.eqHash)))
			w.Write(buf[:])
			w.Write(p.eqHash)
		case partSub:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.sub.parts)))
			w.Write(buf[:])
			p.sub.writeParts(w)
		}
	}
}
