package query

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// readSiblingsThroughRows makes every expression read a source buried under one
// of its quantifiers only through that quantifier's row, as Java's rewireQov
// makes an operator read its FROM sources through their quantifiers. Go's
// resolver binds a column of a join leg to the leg's own alias, so an operator
// over a join box, and a lateral leg, scalar subquery or EXISTS beside one,
// read a quantifier that is bound only inside the box. Rewritten, every
// correlation is bound by the expression's quantifiers, a sibling, or an
// enclosing binder.
func readSiblingsThroughRows(ref *expressions.Reference, bodies scopeBodies) (*expressions.Reference, error) {
	return (&siblingReads{done: map[*expressions.Reference]*expressions.Reference{}, bodies: bodies}).reference(ref)
}

type siblingReads struct {
	done map[*expressions.Reference]*expressions.Reference
	// bodies gains each rebuilt body, so it stays hidden above.
	bodies scopeBodies
}

func (s *siblingReads) reference(ref *expressions.Reference) (*expressions.Reference, error) {
	if ref == nil {
		return nil, nil
	}
	ref = ref.Canonical()
	if rebuilt, ok := s.done[ref]; ok {
		return rebuilt, nil
	}
	if len(ref.FinalMembers()) > 0 {
		s.done[ref] = ref
		return ref, nil
	}
	members := ref.Members()
	rebuiltMembers := make([]expressions.RelationalExpression, len(members))
	changed := false
	for i, member := range members {
		rebuilt, err := s.expression(member)
		if err != nil {
			return nil, err
		}
		rebuiltMembers[i] = rebuilt
		changed = changed || rebuilt != member
	}
	result := ref
	if changed {
		result = expressions.ExploratoryOfAtStage(rebuiltMembers[0], ref.Stage())
		for _, member := range rebuiltMembers[1:] {
			result.Insert(member)
		}
	}
	s.done[ref] = result
	return result, nil
}

func (s *siblingReads) expression(e expressions.RelationalExpression) (expressions.RelationalExpression, error) {
	original := e
	qs := e.GetQuantifiers()
	rebuilt := make([]expressions.Quantifier, len(qs))
	changed := false
	for i, q := range qs {
		ref, err := s.reference(q.GetRangesOver())
		if err != nil {
			return nil, err
		}
		rebuilt[i] = q
		if ref != q.GetRangesOver() {
			rebuilt[i] = expressions.RebuildQuantifier(q, ref)
			changed = true
		}
	}
	if changed {
		var err error
		if sel, isSelect := e.(*expressions.SelectExpression); isSelect {
			e, err = sel.WithTranslatedValues(sel.GetResultValue(), rebuilt, sel.GetPredicates())
		} else {
			e, err = e.WithQuantifiers(rebuilt)
		}
		if err != nil {
			return nil, err
		}
	}
	read, err := readBuriedThroughQuantifiers(e, s.bodies)
	if err != nil {
		return nil, err
	}
	if s.bodies[original] {
		s.bodies[read] = true
	}
	return read, nil
}

// readBuriedThroughQuantifiers rewrites e's reads of a source buried under one
// of e's quantifiers, in e's own values and in its quantifiers' graphs, into
// reads of that quantifier's row.
func readBuriedThroughQuantifiers(e expressions.RelationalExpression, bodies scopeBodies) (expressions.RelationalExpression, error) {
	qs := append([]expressions.Quantifier(nil), e.GetQuantifiers()...)
	if len(qs) == 0 {
		return e, nil
	}
	// Go names an input after its rightmost source, so one alias can name a
	// quantifier's row and a source below it, told apart only by type. The
	// quantifier gets its own name: its row reads are rebased to it, and the
	// source's reads become reads buried under it.
	renamed := map[values.CorrelationIdentifier]values.QuantifiedObjectValue{}
	for i, q := range qs {
		row, err := q.RequireFlowedObjectValue()
		if err != nil {
			return nil, err
		}
		// A passthrough below that flows the same row is the same row.
		collides := false
		for _, typ := range rowsBelow([]expressions.Quantifier{q}, bodies)[q.GetAlias()] {
			collides = collides || !values.FlowedTypeEquals(row, typ)
		}
		if !collides {
			continue
		}
		renamed[q.GetAlias()] = row
		qs[i] = q.WithAlias(values.UniqueCorrelationIdentifier())
	}
	if len(renamed) > 0 {
		var err error
		if sel, isSelect := e.(*expressions.SelectExpression); isSelect {
			e, err = sel.WithTranslatedValues(sel.GetResultValue(), qs, sel.GetPredicates())
		} else {
			e, err = e.WithQuantifiers(qs)
		}
		if err != nil {
			return nil, err
		}
	}
	free := expressions.InitialOf(e).GetCorrelatedTo()
	if len(free) == 0 {
		return e, nil
	}
	own := map[values.CorrelationIdentifier]bool{}
	for _, q := range qs {
		own[q.GetAlias()] = true
	}
	owner := map[values.CorrelationIdentifier]int{}
	buried := map[values.CorrelationIdentifier][]values.Type{}
	targets := map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{}{}
	for j, q := range qs {
		for alias, rows := range rowsBelow([]expressions.Quantifier{q}, bodies) {
			if _, read := free[alias]; !read || own[alias] {
				continue
			}
			if previous, twice := owner[alias]; twice && previous != j {
				return nil, fmt.Errorf("%v is buried under two quantifiers", alias)
			}
			owner[alias] = j
			buried[alias] = rows
			targets[alias] = map[values.CorrelationIdentifier]struct{}{q.GetAlias(): {}}
		}
	}
	if len(owner) == 0 {
		return e, nil
	}
	rewrite := func(v values.Value, active map[values.CorrelationIdentifier]struct{}) (values.Value, error) {
		for alias := range active {
			q := qs[owner[alias]]
			row, err := q.RequireFlowedObjectValue()
			if err != nil {
				return nil, err
			}
			if oldRow, wasRow := renamed[alias]; wasRow {
				if v, err = rebaseRowReads(v, oldRow, q.GetAlias()); err != nil {
					return nil, err
				}
			}
			// A buried source this part of the graph binds itself is not read
			// through the quantifier.
			shadowed := map[values.CorrelationIdentifier][]values.Type{}
			for below, rows := range buried {
				if _, live := active[below]; !live {
					shadowed[below] = rows
				}
			}
			read, ok, err := readThroughRowShadowed(v, q, row, shadowed, bodies)
			if err != nil || !ok {
				return nil, err
			}
			v = read
		}
		if readsBuried(v, active, buried) {
			return nil, nil
		}
		return v, nil
	}
	return cascades.RewriteExpressionReads(e, targets, rewrite)
}

// readsBuried reports whether v still reads one of active's aliases as the
// buried row it names.
func readsBuried(v values.Value, active map[values.CorrelationIdentifier]struct{}, buried map[values.CorrelationIdentifier][]values.Type) bool {
	found := false
	values.WalkValue(v, func(n values.Value) bool {
		if qov, isQOV := values.AsQuantifiedObjectValue(n); isQOV {
			if _, live := active[qov.Correlation()]; live {
				if _, reads := sourceRowOf(qov, buried[qov.Correlation()]); reads {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// buriedReadViolations lists, for each expression in ref's graph, the aliases
// it reads freely that are bound only below one of its quantifiers.
func buriedReadViolations(ref *expressions.Reference, bodies scopeBodies) []string {
	var out []string
	seen := map[*expressions.Reference]bool{}
	var walk func(r *expressions.Reference)
	walk = func(r *expressions.Reference) {
		if r == nil || seen[r] {
			return
		}
		seen[r] = true
		for _, e := range r.AllMembers() {
			free := expressions.InitialOf(e).GetCorrelatedTo()
			own := map[values.CorrelationIdentifier]bool{}
			for _, q := range e.GetQuantifiers() {
				own[q.GetAlias()] = true
			}
			for _, q := range e.GetQuantifiers() {
				below := rowsBelow([]expressions.Quantifier{q}, bodies)
				for alias := range below {
					if _, read := free[alias]; read && !own[alias] {
						out = append(out, fmt.Sprintf("%T reads %v below %v: rv=%s", e, alias, q.GetAlias(), values.ExplainValue(e.GetResultValue())))
					}
				}
				// A read of q's own alias with a buried leaf's row is the
				// leaf, not q's row.
				if row, err := q.RequireFlowedObjectValue(); err == nil {
					for _, v := range nodeValues(e) {
						values.WalkValue(v, func(n values.Value) bool {
							if qov, isQOV := values.AsQuantifiedObjectValue(n); isQOV && qov.Correlation() == q.GetAlias() {
								if _, ownRow := sourceRowOf(qov, []values.Type{row.FlowedType()}); !ownRow {
									if _, leaf := sourceRowOf(qov, below[q.GetAlias()]); leaf {
										out = append(out, fmt.Sprintf("%T reads leaf %v under its own name", e, q.GetAlias()))
									}
								}
							}
							return true
						})
					}
				}
				walk(q.GetRangesOver())
			}
		}
	}
	walk(ref)
	return out
}

func nodeValues(e expressions.RelationalExpression) []values.Value {
	var out []values.Value
	switch x := e.(type) {
	case *expressions.SelectExpression:
		out = append(out, x.GetResultValue())
		for _, p := range x.GetPredicates() {
			predicates.TransformEmbeddedValues(p, func(v values.Value) values.Value { out = append(out, v); return v })
		}
	case *expressions.LogicalFilterExpression:
		for _, p := range x.GetPredicates() {
			predicates.TransformEmbeddedValues(p, func(v values.Value) values.Value { out = append(out, v); return v })
		}
	case *expressions.LogicalSortExpression:
		for _, k := range x.GetSortKeys() {
			out = append(out, k.Value)
		}
	case *expressions.GroupByExpression:
		out = append(out, x.GetGroupingKeys()...)
		for _, a := range x.GetAggregates() {
			out = append(out, a.Operand)
		}
	}
	return out
}
