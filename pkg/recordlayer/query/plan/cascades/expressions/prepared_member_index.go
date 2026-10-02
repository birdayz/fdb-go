package expressions

// PreparedMemberIndex owns one admission lane while its graph is stable.
// Construct it only after the complete batch has passed memo admission.
type PreparedMemberIndex struct {
	equality *PreparedMemberEquality
	members  []RelationalExpression
	hashes   []uint64
	buckets  map[preparedInputKey][]int
	fallback []int
	indexed  bool
}

type preparedInputKey struct {
	hash         uint64
	arity        int
	canCorrelate bool
	final        bool
}

func (p *PreparedMemberEquality) NewMemberIndex(members []RelationalExpression, hashes []uint64) *PreparedMemberIndex {
	index := &PreparedMemberIndex{
		equality: p,
		members:  append([]RelationalExpression(nil), members...),
		hashes:   make([]uint64, len(members)),
	}
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
	candidates := p.buckets[key]
	// Preserve lane order: the first match also determines aliasAwareOnly.
	for i, j := 0, 0; i < len(candidates) || j < len(p.fallback); {
		var next int
		if j == len(p.fallback) || i < len(candidates) && candidates[i] < p.fallback[j] {
			next = candidates[i]
			i++
		} else {
			next = p.fallback[j]
			j++
		}
		if duplicate, aliasAware := p.equality.DuplicateWithHashes(p.members[next:next+1], p.hashes[next:next+1], expression); duplicate {
			return true, aliasAware
		}
	}
	return false, false
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
		p.buckets = make(map[preparedInputKey][]int)
	}
	p.buckets[key] = append(p.buckets[key], index)
}

func (p *PreparedMemberIndex) inputKey(expression RelationalExpression) (preparedInputKey, bool) {
	qs := expression.GetQuantifiers()
	if len(qs) != 1 {
		return preparedInputKey{}, false
	}
	ref := preparedQuantifierReference(qs[0])
	if ref == nil || len(ref.members)+len(ref.finalMembers) != 1 {
		return preparedInputKey{}, false
	}
	// Only singleton groups have symmetric containment. Larger groups must
	// remain candidates even when their alternatives have different keys.
	final := len(ref.finalMembers) == 1
	var member RelationalExpression
	if final {
		member = ref.finalMembers[0]
	} else {
		member = ref.members[0]
	}
	return preparedInputKey{
		hash:         p.equality.equality.hash(member),
		arity:        len(member.GetQuantifiers()),
		canCorrelate: member.CanCorrelate(),
		final:        final,
	}, true
}
