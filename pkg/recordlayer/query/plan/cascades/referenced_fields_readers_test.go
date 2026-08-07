package cascades

// Contains, Size and Fields live in a _test.go file ON PURPOSE, and moving them
// back into referenced_fields.go silently un-pins a load-bearing fact.
//
// THE FACT: nothing in production reads the CONTENTS of the referenced-field
// set. Measured over the whole tree — the only callers of these three are
// planner_constraint_test.go and rule_push_referenced_fields_test.go. The
// constraint's sole production consumer is the push rules, which union it and
// hand it down; the set's membership decides exactly one thing, whether
// CombineReferencedFields reports GROWTH, and growth is what re-fires
// exploration. Java is the same shape: ReferencedFieldsConstraint.java's
// combine() returns an empty Optional when the union does not grow
// (:62-66), and the only other mention of REFERENCED_FIELDS outside the push
// rules is AbstractDataAccessRule.java:122, which lists it as a rule DEPENDENCY
// and never calls getReferencedFieldValues.
//
// WHY IT MATTERS: referenced_fields.go:100 keys this set by LEAF NAME and is on
// the RFC-197 ratchet under `name-keyed`. The bucket's headline is "the original
// seven bugs" — two same-named columns treated as one, wrong rows. This site
// cannot do that, because no consumer ever asks it which columns those are. A
// name collision here merges two entries, which can only make the set stop
// growing SOONER, which can only end exploration EARLIER. The cost is a
// possibly-missed alternative plan. It is not a wrong plan.
//
// That bound is what keeps the entry an open STOP rather than an urgent defect,
// and it is the reason the measured planner-budget blowup of the value-keyed
// conversion is allowed to hold the line. An entry whose severity rests on a
// fact should not rest on a fact recorded only in prose.
//
// HOW THIS PINS IT: these are the only three readers, and in a _test.go file the
// compiler will not let production call them. A future consumer of the set's
// contents cannot appear quietly — it has to move a method out of this file
// first, and that is the moment to re-read the debt entry, because from then on
// a leaf-name collision CAN change an answer somebody looks at.
//
// If you need one of these in production, that is legitimate and the process is:
// move it, then correct the referenced_fields.go:100 debt entry, which currently
// says in as many words that the conflation is plan-direction only.

// Contains reports whether the given field name is referenced.
func (r *ReferencedFields) Contains(field string) bool {
	if r == nil {
		return false
	}
	_, ok := r.fields[field]
	return ok
}

// Size returns the number of referenced fields.
func (r *ReferencedFields) Size() int {
	if r == nil {
		return 0
	}
	return len(r.fields)
}

// Fields returns the referenced field names.
func (r *ReferencedFields) Fields() map[string]struct{} {
	if r == nil {
		return nil
	}
	return r.fields
}
