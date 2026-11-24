// Portions derived from FoundationDB Record Layer (FunctionKeyExpression.java,
// KeyExpression.java, FieldKeyExpression.java, NestingKeyExpression.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"math"
	"sync"

	"google.golang.org/protobuf/proto"
)

// FunctionEvaluator evaluates a named function on a record.
// Arguments are the pre-evaluated argument tuples from the arguments expression.
type FunctionEvaluator func(record *FDBStoredRecord[proto.Message], msg proto.Message, arguments [][]any) ([][]any, error)

// FunctionSpec is a key function's registry entry: the Go form of what a Java
// FunctionKeyExpression subclass declares.
type FunctionSpec struct {
	// Evaluator computes the function over each evaluated argument row.
	Evaluator FunctionEvaluator
	// MinArguments and MaxArguments bound the argument expression's column
	// size, Java's getMinArguments and getMaxArguments, checked where Java's
	// FunctionKeyExpression.create checks them (FunctionKeyExpression.java:
	// 118-127).
	MinArguments, MaxArguments int
	// ColumnSize is the number of key columns the function produces, Java's
	// getColumnSize.
	ColumnSize int
	// NullIsNonUnique records that the function's null result is Java's
	// Key.Evaluated.NULL, the NullStandin.NULL a unique index ignores and
	// COUNT_NOT_NULL does not count, as CollateFunctionKeyExpression.java:169
	// and CardinalityFunctionKeyExpression.java:148 return it. Otherwise its
	// null is Key.Evaluated.scalar(null), a plain null
	// (LongArithmethicFunctionKeyExpression.java:98), which collides.
	NullIsNonUnique bool
	// longArithmetic marks the target's LongArithmethicFunctionKeyExpression
	// functions, registered by this package only: their duplicates are their
	// arguments' (LongArithmethicFunctionKeyExpression.java:104-106) and their
	// Value is the arithmetic of the same logical operator.
	longArithmetic bool
}

// IsLongArithmeticFunction reports whether name is registered as one of the
// target's long-arithmetic key functions, not an application's function of
// the same name.
func IsLongArithmeticFunction(name string) bool {
	spec, ok := LookupFunction(name)
	return ok && spec.longArithmetic
}

// globalFunctionRegistry is Java's FunctionKeyExpression.Registry: the key
// functions by name. It holds the target's core functions (the arithmetic,
// collate, order and cardinality functions) and what applications register.
// Protected by globalFunctionRegistryMu.
var (
	globalFunctionRegistryMu sync.RWMutex
	globalFunctionRegistry   = map[string]FunctionSpec{}
)

// RegisterFunctionSpec registers a key function in the global registry. Call
// it before building metadata that uses the function.
func RegisterFunctionSpec(name string, spec FunctionSpec) {
	globalFunctionRegistryMu.Lock()
	defer globalFunctionRegistryMu.Unlock()
	globalFunctionRegistry[name] = spec
}

// coreKeyFunctions names the functions this package registers at init: the
// target's core registry (the factories of
// com.apple.foundationdb.record.metadata.expressions) and its ICU module's
// collate_icu. Written only by init.
var coreKeyFunctions = map[string]bool{}

// registerCoreFunction registers one of the target's core key functions.
func registerCoreFunction(name string, spec FunctionSpec) {
	coreKeyFunctions[name] = true
	RegisterFunctionSpec(name, spec)
}

// RegisterFunction registers a key function of one column, any number of
// arguments and a plain null (see FunctionSpec).
func RegisterFunction(name string, evaluator FunctionEvaluator) {
	RegisterFunctionSpec(name, FunctionSpec{Evaluator: evaluator, MinArguments: 0, MaxArguments: math.MaxInt, ColumnSize: 1})
}

// LookupFunction is the registry entry for name, Java's
// FunctionKeyExpression.Registry.getBuilder.
func LookupFunction(name string) (FunctionSpec, bool) {
	globalFunctionRegistryMu.RLock()
	defer globalFunctionRegistryMu.RUnlock()
	spec, ok := globalFunctionRegistry[name]
	return spec, ok
}

// functionNullIsNonUnique reports whether the named function's null result is
// NullStandin.NULL.
func functionNullIsNonUnique(name string) bool {
	spec, _ := LookupFunction(name)
	return spec.NullIsNonUnique
}

// functionConstructionFault is Java's FunctionKeyExpression.create refusal
// (FunctionKeyExpression.java:112-127), a KeyExpression.InvalidExpressionException:
// a name the registry lacks, or an argument expression whose column size is
// outside the function's bounds. Nil when create would succeed.
func functionConstructionFault(name string, arguments KeyExpression) error {
	spec, ok := LookupFunction(name)
	if !ok {
		return &KeyExpressionError{Message: "Function not defined"}
	}
	if n := arguments.ColumnSize(); n < spec.MinArguments || n > spec.MaxArguments {
		return &KeyExpressionError{Message: "Invalid number of arguments provided to function"}
	}
	return nil
}

// FunctionKeyExpression evaluates a named function on records to produce index
// key values. The function is resolved from the global registry by name.
// Matches Java's FunctionKeyExpression abstract class.
type FunctionKeyExpression struct {
	name      string
	arguments KeyExpression
	// fault is Java's create refusal of this function (functionConstructionFault)
	// and faultSeq the program-order place of the throw, which Build reports as
	// a builder fault (keyConstructionFault). A function loaded from stored
	// bytes carries none: its arity is refused at load and an unknown name is
	// loaded (DIVERGENCES.md).
	fault    error
	faultSeq uint64
}

// FunctionExpr is Java's FunctionKeyExpression.create(name, arguments). Java
// throws at the call for a name the registry lacks or an argument column size
// outside the function's bounds; FunctionExpr has no error channel, so it
// records the refusal in program order and Build returns it.
func FunctionExpr(name string, arguments KeyExpression) *FunctionKeyExpression {
	f := &FunctionKeyExpression{name: name, arguments: arguments}
	if err := functionConstructionFault(name, arguments); err != nil {
		f.fault, f.faultSeq = err, nextBuildFaultSeq()
	}
	return f
}

// Evaluate resolves the named function from the registry, evaluates arguments,
// and applies the function. Matches Java's FunctionKeyExpression.evaluateMessage().
func (f *FunctionKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	spec, ok := LookupFunction(f.name)
	if !ok {
		return nil, &KeyExpressionError{Message: fmt.Sprintf("unknown function key expression: %s", f.name)}
	}

	argTuples, err := f.arguments.Evaluate(record, msg)
	if err != nil {
		return nil, fmt.Errorf("evaluating arguments for function %s: %w", f.name, err)
	}

	return spec.Evaluator(record, msg, argTuples)
}

// FieldNames returns field names from the arguments expression.
func (f *FunctionKeyExpression) FieldNames() []string {
	return f.arguments.FieldNames()
}

// ColumnSize is the function's own column size, Java's getColumnSize. A
// loaded name the registry lacks has 1 (DIVERGENCES.md).
func (f *FunctionKeyExpression) ColumnSize() int {
	if spec, ok := LookupFunction(f.name); ok {
		return spec.ColumnSize
	}
	return 1
}

// Name returns the function name.
func (f *FunctionKeyExpression) Name() string {
	return f.name
}

// Arguments returns the arguments key expression.
func (f *FunctionKeyExpression) Arguments() KeyExpression {
	return f.arguments
}
