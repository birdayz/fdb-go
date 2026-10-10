// Portions derived from FoundationDB Record Layer (AstNormalizer.java,
// MutablePlanGenerationContext.java, PhysicalPlanEquivalence.java),
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2023 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"runtime"
	"strconv"
	"strings"
	"sync"
	"weak"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"github.com/antlr4-go/antlr/v4"
)

var queryConstantsAlias = values.NamedCorrelationIdentifier("__sql_literals")

// Execution pools stay outside cached plans; constraints protect every
// compile-time alias or literal-dependent specialization.
type queryBindings struct {
	text        string
	equivalence string
	constants   map[string]any
	literals    []queryLiteralBinding
	pins        literalPins
}

// literalPins records the runtime literals whose value planning consumed;
// a statement is planned on one goroutine.
type literalPins map[int]bool

type queryLiteralBinding struct {
	typeName string
	valueKey string
	equalTo  int
	exact    bool
}

// A plan records equalities it used, not inequalities between distinct inputs.
// Java's PhysicalPlanEquivalence tests these assumptions against the new pool.
type queryBindingConstraint struct {
	identity string
	literals []queryLiteralBinding
}

func (b queryBindings) constraint() queryBindingConstraint {
	literals := append([]queryLiteralBinding(nil), b.literals...)
	for i := range literals {
		literals[i].exact = literals[i].exact || b.pins[i]
		if !literals[i].exact {
			literals[i].valueKey = ""
		}
	}
	return queryBindingConstraint{identity: b.equivalence, literals: literals}
}

// key separates plans whose constraints differ, such as one per pinned value.
func (c queryBindingConstraint) key() string {
	var b strings.Builder
	writeLengthPrefixed(&b, c.identity)
	for _, l := range c.literals {
		writeLengthPrefixed(&b, l.typeName)
		writeLengthPrefixed(&b, l.valueKey)
		writeLengthPrefixed(&b, strconv.Itoa(l.equalTo))
	}
	return b.String()
}

func (c queryBindingConstraint) accepts(b queryBindings) bool {
	if len(c.literals) != len(b.literals) {
		return false
	}
	if len(c.literals) == 0 {
		return c.identity == b.equivalence
	}
	for i, required := range c.literals {
		actual := b.literals[i]
		if required.typeName != actual.typeName || required.exact && required.valueKey != actual.valueKey {
			return false
		}
		if required.equalTo != i && actual.valueKey != b.literals[required.equalTo].valueKey {
			return false
		}
	}
	return true
}

func normalizeQueryBindings(tree antlr.ParseTree, md *recordlayer.RecordMetaData) (queryBindings, func(), error) {
	result := queryBindings{constants: make(map[string]any), pins: literalPins{}}
	var text, equivalence strings.Builder
	byValue := make(map[string]int)
	type replacement struct {
		token antlr.Token
		value values.Value
	}
	var replaced []replacement
	var refs []*values.ConstantObjectValue
	release := func() {
		for _, ref := range refs {
			expr.ForgetSpecializable(ref)
		}
		for _, old := range replaced {
			if old.value == nil {
				expr.UnbindParameter(old.token)
			} else {
				expr.BindParameter(old.token, old.value)
			}
		}
	}
	appendText := func(s string) {
		if text.Len() != 0 {
			text.WriteByte(' ')
		}
		text.WriteString(s)
	}
	bind := func(token antlr.Token, v values.Value, runtime bool) {
		var exact strings.Builder
		writeLengthPrefixed(&exact, v.Type().String())
		writeBindingKey(&exact, constantPayload(v))
		position := len(result.literals)
		literal := queryLiteralBinding{typeName: v.Type().String(), valueKey: exact.String(), equalTo: position}
		// Simplification folds TRUE/FALSE/NULL (Java's EvaluatesToValue constraints);
		// float key proofs read signed zero and NaN.
		switch v.Type().Code() {
		case values.TypeCodeNull, values.TypeCodeBoolean, values.TypeCodeFloat, values.TypeCodeDouble:
			runtime = false
		}
		if !runtime {
			literal.exact = true
			result.literals = append(result.literals, literal)
			equivalence.WriteByte('E')
			writeLengthPrefixed(&equivalence, exact.String())
			return
		}
		if first, exists := byValue[exact.String()]; exists {
			literal.equalTo = first
		} else {
			byValue[exact.String()] = position
		}
		result.literals = append(result.literals, literal)
		result.constants[strconv.Itoa(position)] = constantPayload(v)
		id := strconv.Itoa(literal.equalTo)
		equivalence.WriteByte('R')
		writeLengthPrefixed(&equivalence, v.Type().String())
		writeLengthPrefixed(&equivalence, id)
		old, _ := expr.BoundParameter(token)
		replaced = append(replaced, replacement{token: token, value: old})
		ref := values.NewConstantObjectValue(queryConstantsAlias, id, v.Type())
		refs = append(refs, ref)
		pins := result.pins
		expr.BindSpecializableParameter(token, ref, v, func() { pins[position] = true })
	}
	var walk func(antlr.Tree, bool) error
	walk = func(node antlr.Tree, runtime bool) error {
		switch n := node.(type) {
		case *antlrgen.LimitClauseContext, *antlrgen.InListContext:
			// LIMIT is an integer in the physical operator; IN's literal
			// expansion fixes its cardinality, deduplication and tuple bounds.
			runtime = false
		case *antlrgen.ScalarFunctionCallContext:
			if n.ScalarFunctionName().GetStart().GetTokenType() == antlrgen.RelationalParserCOALESCE {
				// COALESCE participates in error-pruning rewrites; preserve the
				// literal-dependent choice and constrain its cached specialization.
				runtime = false
			}
		case *antlrgen.OrderByExpressionContext:
			if _, positional := selectListPosition(n.Expression()); positional {
				runtime = false
			}
		case *antlrgen.GroupByItemContext:
			if _, positional := selectListPosition(n.Expression()); positional {
				runtime = false
			}
		case *antlrgen.PreparedStatementParameterContext:
			if value, ok := expr.BoundParameter(n.GetStart()); ok {
				bind(n.GetStart(), value, runtime)
			}
		case antlrgen.IConstantContext:
			if runtime {
				value, err := expr.ResolveLiteral(n)
				if err != nil {
					if mapped := mapPredicateWalkError(err); mapped != nil {
						return mapped
					}
					return err
				}
				if value.Type().Code() != values.TypeCodeNull {
					bind(n.GetStart(), value, true)
					appendText("?")
					return nil
				}
			}
		}
		if terminal, ok := node.(antlr.TerminalNode); ok {
			token := terminal.GetSymbol()
			if token != nil && token.GetTokenType() != antlr.TokenEOF {
				spelling := token.GetText()
				if foldsInCacheText(token.GetTokenType()) {
					spelling = strings.ToUpper(spelling)
				}
				appendText(spelling)
			}
			return nil
		}
		for i := 0; i < node.GetChildCount(); i++ {
			if err := walk(node.GetChild(i), runtime); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(tree, !requiresLiteralIndexProof(md)); err != nil {
		release()
		return queryBindings{}, func() {}, err
	}
	result.text, result.equivalence = text.String(), equivalence.String()
	return result, release, nil
}

// These index proofs consume concrete literals. Their plans use exact binding
// constraints rather than allowing an unproven replacement on a cache hit.
func requiresLiteralIndexProof(md *recordlayer.RecordMetaData) bool {
	if md == nil {
		return false
	}
	key := weak.Make(md)
	if known, ok := literalIndexProofs.Load(key); ok {
		return known.(bool)
	}
	result := scanLiteralIndexProof(md)
	if _, loaded := literalIndexProofs.LoadOrStore(key, result); !loaded {
		runtime.AddCleanup(md, func(k weak.Pointer[recordlayer.RecordMetaData]) { literalIndexProofs.Delete(k) }, key)
	}
	return result
}

// Metadata is immutable per version, so its index scan is memoized per object.
var literalIndexProofs sync.Map // weak.Pointer[recordlayer.RecordMetaData] -> bool

func scanLiteralIndexProof(md *recordlayer.RecordMetaData) bool {
	for _, index := range md.GetAllIndexes() {
		if index.HasFilteringPredicate() || keyHasLiteral(index.RootExpression) {
			return true
		}
	}
	return false
}

func keyHasLiteral(key recordlayer.KeyExpression) bool {
	switch key := key.(type) {
	case nil, *recordlayer.FieldKeyExpression, *recordlayer.RecordTypeKeyExpression,
		*recordlayer.EmptyKeyExpression, *recordlayer.VersionKeyExpression:
		return false
	case *recordlayer.CompositeKeyExpression:
		for _, child := range key.SubKeyExpressions() {
			if keyHasLiteral(child) {
				return true
			}
		}
		return false
	case *recordlayer.NestingKeyExpression:
		return keyHasLiteral(key.Child())
	default:
		return keyProtoHasLiteral(key.ToKeyExpression())
	}
}

func keyProtoHasLiteral(key *gen.KeyExpression) bool {
	if key == nil {
		return false
	}
	if key.Value != nil {
		return true
	}
	for _, child := range key.GetThen().GetChild() {
		if keyProtoHasLiteral(child) {
			return true
		}
	}
	for _, child := range key.GetList().GetChild() {
		if keyProtoHasLiteral(child) {
			return true
		}
	}
	return keyProtoHasLiteral(key.GetNesting().GetChild()) ||
		keyProtoHasLiteral(key.GetGrouping().GetWholeKey()) ||
		keyProtoHasLiteral(key.GetSplit().GetJoined()) ||
		keyProtoHasLiteral(key.GetFunction().GetArguments()) ||
		keyProtoHasLiteral(key.GetKeyWithValue().GetInnerKey()) ||
		keyProtoHasLiteral(key.GetDimensions().GetWholeKey())
}
