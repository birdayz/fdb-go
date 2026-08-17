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
	partTypes
	partMappedAlias
	partLayout
)

// part is one identifying field. Constructed only via the typed builder
// methods, so the (kind → payload) mapping is exhaustive by construction — no
// reflection, no untyped fallthrough that could silently mis-hash a new field.
// A part carries exactly ONE payload, so the payloads share two interface
// slots rather than each owning a field. One field per kind made `part` 312
// bytes — a union sized by the SUM of eighteen alternatives — and structuralKey
// embeds eight of them inline, so every key allocated 2520 bytes to carry the
// three-to-six parts a plan actually folds. That was 24% of the whole planner's
// allocation, and MORE than the pre-inline version it replaced: the inline
// array is only a win once the element is small.
//
// `ref` holds the single-object payloads (Value, Type, OrdinalLayout, *int,
// *structuralKey, and Equatable's opaque object); `list` holds the slice
// payloads. Both are read back through the typed accessors below, so the kind
// switch stays exhaustive and a mismatched read is a nil, never a wrong-typed
// value. Boxing a slice into `list` costs one 24-byte header — paid only by the
// parts that have a slice, against 224 bytes of union saved by every part.
type part struct {
	kind  partKind
	b     bool
	i     int64
	s     string
	alias values.CorrelationIdentifier
	ref   any
	list  any
	eq    func(other any) bool
}

func (p part) value() values.Value          { v, _ := p.ref.(values.Value); return v }
func (p part) typ() values.Type             { t, _ := p.ref.(values.Type); return t }
func (p part) layout() values.OrdinalLayout { l, _ := p.ref.(values.OrdinalLayout); return l }
func (p part) iptr() *int                   { i, _ := p.ref.(*int); return i }
func (p part) sub() *structuralKey          { s, _ := p.ref.(*structuralKey); return s }
func (p part) obj() any                     { return p.ref }

func (p part) ss() []string       { s, _ := p.list.([]string); return s }
func (p part) vs() []values.Value { v, _ := p.list.([]values.Value); return v }
func (p part) preds() []predicates.QueryPredicate {
	q, _ := p.list.([]predicates.QueryPredicate)
	return q
}
func (p part) types() []values.Type { t, _ := p.list.([]values.Type); return t }
func (p part) scanComps() []*predicates.ComparisonRange {
	c, _ := p.list.([]*predicates.ComparisonRange)
	return c
}
func (p part) sks() []SortKey { s, _ := p.list.([]SortKey); return s }
func (p part) eqHash() []byte { h, _ := p.list.([]byte); return h }

// structuralKey is an ordered list of a plan's identifying fields.
// structuralKey carries its first few parts INLINE, which is a pure allocation
// decision and changes no key, hash or comparison.
//
// A key is rebuilt on every dedup comparison — memo admission tests each intent
// against each existing member, and both EqualsWithoutChildren and
// HashCodeWithoutChildren build one — so the builder is one of the hottest
// allocators in planning. Growing `parts` from nil costs four reallocations for
// the six-to-eight parts a typical plan folds, on top of the struct itself.
// Backing it with an inline array makes the common key exactly ONE allocation.
// A key with more parts than the array holds simply appends onto the heap as
// before, so nothing is capped.
//
// The inline array is only a win while `part` is small, and that is not a
// stylistic preference: at 312 bytes per part this struct was 2520 bytes and
// allocating MORE than the growing-slice version it replaced. See part's own
// comment. Any new payload field belongs in `ref`/`list`, not beside them.
const structuralKeyInlineParts = 8

type structuralKey struct {
	parts  []part
	inline [structuralKeyInlineParts]part
}

func newStructuralKey() *structuralKey {
	k := &structuralKey{}
	k.parts = k.inline[:0]
	return k
}

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
	k.parts = append(k.parts, part{kind: partStrs, list: ss})
	return k
}

// Alias folds a correlation identifier by its stable name — matching every
// hand-rolled site that wrote alias.Name() / compared alias != o.alias.
func (k *structuralKey) Alias(a values.CorrelationIdentifier) *structuralKey {
	return k.Str(a.Name())
}

// MappedAlias folds a correlation identifier whose name is local to this
// expression's child quantifiers. Raw plan equality still compares the exact
// identifier; expression equality maps it through the memo's alpha-renaming.
// Its hash deliberately omits the spelling so alpha-equivalent plans enter the
// same bucket and let EqualUnderAliases decide.
func (k *structuralKey) MappedAlias(a values.CorrelationIdentifier) *structuralKey {
	k.parts = append(k.parts, part{kind: partMappedAlias, alias: a})
	return k
}

// Layout folds an immutable physical output layout. Raw plan equality uses
// exact source aliases; memo equality translates those aliases, while hashing
// uses the layout's alias-free digest. A nil layout represents the explicitly
// dynamic AnyRecord property and compares only with another nil layout.
func (k *structuralKey) Layout(layout values.OrdinalLayout) *structuralKey {
	k.parts = append(k.parts, part{kind: partLayout, ref: layout})
	return k
}

func (k *structuralKey) Value(v values.Value) *structuralKey {
	k.parts = append(k.parts, part{kind: partValue, ref: v})
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
	k.parts = append(k.parts, part{kind: partStructVal, ref: v})
	return k
}

func (k *structuralKey) Values(vs []values.Value) *structuralKey {
	k.parts = append(k.parts, part{kind: partValues, list: vs})
	return k
}

func (k *structuralKey) Preds(ps []predicates.QueryPredicate) *structuralKey {
	k.parts = append(k.parts, part{kind: partPreds, list: ps})
	return k
}

// Type folds a flowed values.Type by typeEquals — the exact primitive the
// hand-rolled scan/index-scan equals used. It is EQUALS-ONLY: the hash omits it
// (contributing only the kind byte), matching those plans' hand-rolled hashes,
// which never folded flowedType. A type mismatch still separates identities via
// Equal; two plans differing only in flowed type collide in the hash (a valid
// under-hash — equal⟹same-hash still holds).
func (k *structuralKey) Type(t values.Type) *structuralKey {
	k.parts = append(k.parts, part{kind: partType, ref: t})
	return k
}

// Types folds an ORDERED type slice. Physical key-component types are part of
// a scan's semantics: FLOAT and DOUBLE use different FDB tuple tags, so two
// otherwise identical comparison plans with different component types cannot
// share one memo member.
func (k *structuralKey) Types(ts []values.Type) *structuralKey {
	k.parts = append(k.parts, part{kind: partTypes, list: ts})
	return k
}

// ScanComps folds a scan/index-scan ComparisonRange list — the SARG bounds —
// via the same scanComparisonRangesEqual / writeScanComparisonRangesHash
// primitives the hand-rolled methods used.
func (k *structuralKey) ScanComps(sc []*predicates.ComparisonRange) *structuralKey {
	k.parts = append(k.parts, part{kind: partScanComps, list: sc})
	return k
}

// ValuePtr folds a Value by POINTER/interface identity (Go ==), NOT semantic
// equality — the exact primitive the hand-rolled Explode / TableFunction equals
// used (p.collectionValue == o.collectionValue). Distinct from Value (semantic):
// two structurally-equal but distinct Value instances are DIFFERENT here, so a
// migrating plan keeps them in separate memo members. Hash folds the Value's
// stable .Name() (nil → empty), matching those plans' hand-rolled hashes.
func (k *structuralKey) ValuePtr(v values.Value) *structuralKey {
	k.parts = append(k.parts, part{kind: partValuePtr, ref: v})
	return k
}

// IntPtr folds an optional int (*int) by eqIntPtr — nil and non-nil are
// distinct, two non-nil compare by value. The exact primitive the hand-rolled
// vector-scan equals used for efSearch. Hash folds a nil sentinel or the int.
func (k *structuralKey) IntPtr(p *int) *structuralKey {
	k.parts = append(k.parts, part{kind: partIntPtr, ref: p})
	return k
}

// SortKeys folds an ORDERED []SortKey via sortKeyEqual (display Field +
// Desc / NullsFirst direction + the semantic ValueExpr) — the InMemorySort
// identity. Hash folds each key's identifying fields (the same set sortKeyEqual
// compares), preserving equal⟹same-hash.
func (k *structuralKey) SortKeys(sks []SortKey) *structuralKey {
	k.parts = append(k.parts, part{kind: partSortKeys, list: sks})
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
	k.parts = append(k.parts, part{kind: partEquatable, ref: obj, eq: eq, list: hashBytes})
	return k
}

// Sub nests another plan's structuralKey — used where a plan's identity embeds a
// whole sub-plan compared structurally (RecordQueryAggregateIndexPlan wraps a
// RecordQueryIndexPlan via its EqualsPlanWithoutChildren, which IS
// indexPlan.structuralKey().Equal). Equal recurses into the sub-key; Hash folds
// the sub-key's parts (strengthening the hand-rolled hash, which folded only the
// index name — safe: equal⟹same-hash, fewer collisions).
func (k *structuralKey) Sub(sub *structuralKey) *structuralKey {
	k.parts = append(k.parts, part{kind: partSub, ref: sub})
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

// EqualUnderAliases is Equal at an expression/memo boundary. Only typed parts
// that can carry a child-local correlation are translated; every scalar and
// opaque part retains its raw equality contract.
func (k *structuralKey) EqualUnderAliases(o *structuralKey, aliases values.AliasMap) bool {
	if len(k.parts) != len(o.parts) {
		return false
	}
	for i := range k.parts {
		a, b := k.parts[i], o.parts[i]
		if a.kind != b.kind {
			return false
		}
		switch a.kind {
		case partMappedAlias:
			target := a.alias
			if aliases != nil {
				if mapped, ok := aliases.Target(a.alias); ok {
					target = mapped
				}
			}
			if target != b.alias {
				return false
			}
		case partValue:
			if !values.SemanticEqualsUnderAliasMap(a.value(), b.value(), aliases) {
				return false
			}
		case partValues:
			if len(a.vs()) != len(b.vs()) {
				return false
			}
			for j := range a.vs() {
				if !values.SemanticEqualsUnderAliasMap(a.vs()[j], b.vs()[j], aliases) {
					return false
				}
			}
		case partPreds:
			if len(a.preds()) != len(b.preds()) {
				return false
			}
			for j := range a.preds() {
				if !predicates.SemanticEqualsUnderAliasMap(a.preds()[j], b.preds()[j], aliases) {
					return false
				}
			}
		case partLayout:
			if a.layout() == nil || b.layout() == nil {
				if a.layout() != nil || b.layout() != nil {
					return false
				}
				continue
			}
			if !a.layout().EqualUnderAliases(b.layout(), aliases) {
				return false
			}
		case partSub:
			if !a.sub().EqualUnderAliases(b.sub(), aliases) {
				return false
			}
		default:
			if !partEqual(a, b) {
				return false
			}
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
		if len(a.ss()) != len(b.ss()) {
			return false
		}
		for i := range a.ss() {
			if a.ss()[i] != b.ss()[i] {
				return false
			}
		}
		return true
	case partValue:
		return semanticValueEquals(a.value(), b.value())
	case partStructVal:
		return values.ValuesStructurallyEqual(a.value(), b.value())
	case partValues:
		if len(a.vs()) != len(b.vs()) {
			return false
		}
		for i := range a.vs() {
			if !semanticValueEquals(a.vs()[i], b.vs()[i]) {
				return false
			}
		}
		return true
	case partPreds:
		if len(a.preds()) != len(b.preds()) {
			return false
		}
		for i := range a.preds() {
			if !predicates.PredicateEquals(a.preds()[i], b.preds()[i]) {
				return false
			}
		}
		return true
	case partType:
		return typeEquals(a.typ(), b.typ())
	case partTypes:
		if len(a.types()) != len(b.types()) {
			return false
		}
		for i := range a.types() {
			if !typeEquals(a.types()[i], b.types()[i]) {
				return false
			}
		}
		return true
	case partScanComps:
		return scanComparisonRangesEqual(a.scanComps(), b.scanComps())
	case partValuePtr:
		return a.value() == b.value()
	case partIntPtr:
		return eqIntPtr(a.iptr(), b.iptr())
	case partSortKeys:
		if len(a.sks()) != len(b.sks()) {
			return false
		}
		for i := range a.sks() {
			if !sortKeyEqual(a.sks()[i], b.sks()[i]) {
				return false
			}
		}
		return true
	case partEquatable:
		return a.eq(b.obj())
	case partSub:
		return a.sub().Equal(b.sub())
	case partMappedAlias:
		return a.alias == b.alias
	case partLayout:
		if a.layout() == nil || b.layout() == nil {
			return a.layout() == nil && b.layout() == nil
		}
		return a.layout().RawEqual(b.layout())
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
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.ss())))
			w.Write(buf[:])
			for _, s := range p.ss() {
				binary.BigEndian.PutUint64(buf[:], uint64(len(s)))
				w.Write(buf[:])
				w.Write([]byte(s))
			}
		case partValue, partStructVal:
			writeValueHash(w, p.value())
		case partValues:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.vs())))
			w.Write(buf[:])
			for _, v := range p.vs() {
				writeValueHash(w, v)
			}
		case partPreds:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.preds())))
			w.Write(buf[:])
			for _, pr := range p.preds() {
				binary.BigEndian.PutUint64(buf[:], predicates.SemanticHashCode(pr))
				w.Write(buf[:])
			}
		case partType:
			// Equals-only — no payload (the kind byte above is the whole
			// contribution). Mirrors the hand-rolled scan/index hashes, which
			// fold recordTypes + scanComparisons + flags but never flowedType.
		case partTypes:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.types())))
			w.Write(buf[:])
			for _, typ := range p.types() {
				if typ == nil {
					w.Write([]byte{0})
					continue
				}
				w.Write([]byte{1})
				typeString := typ.String()
				binary.BigEndian.PutUint64(buf[:], uint64(len(typeString)))
				w.Write(buf[:])
				w.Write([]byte(typeString))
			}
		case partScanComps:
			writeScanComparisonRangesHash(w, p.scanComps())
		case partValuePtr:
			// Pointer-identity equal ⟹ same interface value ⟹ same Name();
			// length-tagged so distinct names cannot collide across a boundary.
			var name string
			if p.value() != nil {
				name = p.value().Name()
			}
			binary.BigEndian.PutUint64(buf[:], uint64(len(name)))
			w.Write(buf[:])
			w.Write([]byte(name))
		case partIntPtr:
			if p.iptr() == nil {
				w.Write([]byte{0})
			} else {
				w.Write([]byte{1})
				binary.BigEndian.PutUint64(buf[:], uint64(*p.iptr()))
				w.Write(buf[:])
			}
		case partSortKeys:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.sks())))
			w.Write(buf[:])
			for _, sk := range p.sks() {
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
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.eqHash())))
			w.Write(buf[:])
			w.Write(p.eqHash())
		case partSub:
			binary.BigEndian.PutUint64(buf[:], uint64(len(p.sub().parts)))
			w.Write(buf[:])
			p.sub().writeParts(w)
		case partMappedAlias:
			// Alias spelling is intentionally omitted. EqualUnderAliases maps
			// this part through the memo's bijection.
		case partLayout:
			if p.layout() == nil {
				w.Write([]byte{0})
				continue
			}
			w.Write([]byte{1})
			binary.BigEndian.PutUint64(buf[:], p.layout().AliasFreeHash())
			w.Write(buf[:])
		}
	}
}
