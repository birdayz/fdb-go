package expressions

import "slices"

// PreparedMemberIndex owns one admission lane while its graph is stable.
// Construct it only after the complete batch has passed memo admission.
type PreparedMemberIndex struct {
	equality *PreparedMemberEquality
	members  []RelationalExpression
	hashes   []uint64
	buckets  map[preparedInputKey]*preparedMemberBucket
	fallback []int
	indexed  bool
}

type preparedMemberBucket struct {
	members  []int
	refined  map[preparedInputKey][]int
	fallback []int
}

type preparedInputKey struct {
	hash         uint64
	children     uint64
	arity        int
	canCorrelate bool
	final        bool
}

type preparedInputSignature struct {
	key      preparedInputKey
	complete bool
}

// The index retains members and a complete hashes slice without copying; the
// caller must not modify either afterwards.
func (p *PreparedMemberEquality) NewMemberIndex(members []RelationalExpression, hashes []uint64) *PreparedMemberIndex {
	index := &PreparedMemberIndex{
		equality: p,
		members:  slices.Clip(members),
	}
	if len(hashes) == len(members) {
		index.hashes = slices.Clip(hashes)
		return index
	}
	index.hashes = make([]uint64, len(members))
	for i, member := range members {
		if i < len(hashes) {
			index.hashes[i] = hashes[i]
		} else {
			index.hashes[i] = p.equality.hash(member)
		}
	}
	return index
}

func (p *PreparedMemberIndex) Duplicate(expression RelationalExpression) (bool, bool) {
	// Tiny lanes cost less to scan than to allocate signature buckets for.
	if len(p.members) < 8 {
		return p.equality.DuplicateWithHashes(p.members, p.hashes, expression)
	}
	key, keyed := p.inputKey(expression)
	if !keyed {
		return p.equality.DuplicateWithHashes(p.members, p.hashes, expression)
	}
	if !p.indexed {
		for i, member := range p.members {
			p.indexMember(i, member)
		}
		p.indexed = true
	}
	lists := [3][]int{nil, nil, p.fallback}
	if bucket := p.buckets[key]; bucket != nil {
		lists[0] = bucket.members
		if len(bucket.members) >= 8 {
			if signature, complete := p.inputSignature(expression); complete {
				if bucket.refined == nil {
					bucket.refined = make(map[preparedInputKey][]int)
					for _, index := range bucket.members {
						p.indexSignature(bucket, index, p.members[index])
					}
				}
				lists[0], lists[1] = bucket.refined[signature], bucket.fallback
			}
		}
	}
	// Preserve lane order across exact signatures and both fallback populations.
	for {
		next, selected := -1, -1
		for i, list := range lists {
			if len(list) > 0 && (next < 0 || list[0] < next) {
				next, selected = list[0], i
			}
		}
		if selected < 0 {
			return false, false
		}
		lists[selected] = lists[selected][1:]
		if duplicate, aliasAware := p.equality.DuplicateWithHashes(p.members[next:next+1], p.hashes[next:next+1], expression); duplicate {
			return true, aliasAware
		}
	}
}

func (p *PreparedMemberIndex) Add(expression RelationalExpression) {
	p.members = append(p.members, expression)
	p.hashes = append(p.hashes, p.equality.equality.hash(expression))
	if p.indexed {
		p.indexMember(len(p.members)-1, expression)
	}
}

func (p *PreparedMemberIndex) indexMember(index int, expression RelationalExpression) {
	key, keyed := p.inputKey(expression)
	if !keyed {
		p.fallback = append(p.fallback, index)
		return
	}
	if p.buckets == nil {
		p.buckets = make(map[preparedInputKey]*preparedMemberBucket)
	}
	bucket := p.buckets[key]
	if bucket == nil {
		bucket = &preparedMemberBucket{}
		p.buckets[key] = bucket
	}
	bucket.members = append(bucket.members, index)
	if bucket.refined != nil {
		p.indexSignature(bucket, index, expression)
	}
}

func (p *PreparedMemberIndex) indexSignature(bucket *preparedMemberBucket, index int, expression RelationalExpression) {
	signature, complete := p.inputSignature(expression)
	if !complete {
		bucket.fallback = append(bucket.fallback, index)
		return
	}
	bucket.refined[signature] = append(bucket.refined[signature], index)
}

func (p *PreparedMemberIndex) inputKey(expression RelationalExpression) (preparedInputKey, bool) {
	qs := expression.GetQuantifiers()
	if len(qs) != 1 {
		return preparedInputKey{}, false
	}
	key, _, complete := p.equality.inputNodeKey(qs[0].rangesOver)
	return key, complete
}

func (p *PreparedMemberIndex) inputSignature(expression RelationalExpression) (preparedInputKey, bool) {
	qs := expression.GetQuantifiers()
	if len(qs) != 1 {
		return preparedInputKey{}, false
	}
	ref := preparedQuantifierReference(qs[0])
	if signature, seen := p.equality.inputs[ref]; seen {
		return signature.key, signature.complete
	}
	key, member, complete := p.equality.inputNodeKey(ref)
	if !complete {
		return preparedInputKey{}, false
	}
	// Read immediate child node signatures, not whole alternative populations:
	// containment is directional below any non-singleton input.
	for _, quantifier := range member.GetQuantifiers() {
		child, _, childComplete := p.equality.inputNodeKey(quantifier.rangesOver)
		if !childComplete {
			complete = false
			break
		}
		// Java's semantic hash ignores quantifier order. Collisions still pass
		// through the full comparator, including attributes and alias bindings.
		key.children += child.fingerprint()
	}
	if p.equality.inputs == nil {
		p.equality.inputs = make(map[*Reference]preparedInputSignature)
	}
	p.equality.inputs[ref] = preparedInputSignature{key: key, complete: complete}
	return key, complete
}

func (p *PreparedMemberEquality) inputNodeKey(ref *Reference) (preparedInputKey, RelationalExpression, bool) {
	ref = canonicalReferenceReadOnly(ref)
	if ref == nil || len(ref.members)+len(ref.finalMembers) != 1 {
		return preparedInputKey{}, nil, false
	}
	final := len(ref.finalMembers) == 1
	var member RelationalExpression
	if final {
		member = ref.finalMembers[0]
	} else {
		member = ref.members[0]
	}
	return preparedInputKey{
		hash:         p.equality.hash(member),
		arity:        len(member.GetQuantifiers()),
		canCorrelate: member.CanCorrelate(),
		final:        final,
	}, member, true
}

func (k preparedInputKey) fingerprint() uint64 {
	hash := (k.hash*31+k.children)*31 + uint64(k.arity)
	hash *= 31
	if k.canCorrelate {
		hash++
	}
	hash *= 31
	if k.final {
		hash++
	}
	return hash
}
