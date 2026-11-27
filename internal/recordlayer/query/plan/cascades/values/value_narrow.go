// Portions derived from FoundationDB Record Layer (SemanticAnalyzer.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

import "fmt"

// NarrowValue is a value stored into a slot of a declared Target type that its
// own type does not promote to. A recursive CTE's temporary table is typed by
// the seed's row (SemanticAnalyzer.getRecursiveCteType) and Java writes later
// rows into it as they are, so the declared type can be NOT NULL where the
// written value is nullable, or of another type altogether. Evaluation fails
// for a value the slot cannot hold instead of letting it flow under a type
// that misdescribes it.
type NarrowValue struct {
	Child  Value
	Target Type
	// fits reports that Child's type is Target up to nullability, so only a
	// NULL can fail to fit.
	fits bool
}

// NewNarrowValue narrows child to target.
func NewNarrowValue(child Value, target Type) *NarrowValue {
	if child == nil || target == nil {
		panic("NewNarrowValue: nil child or target")
	}
	childType := child.Type()
	fits := childType != nil && WithNullability(childType, true).Equals(WithNullability(target, true))
	return &NarrowValue{Child: child, Target: target, fits: fits}
}

// SlotAssignmentError reports a value a narrowed slot cannot hold.
type SlotAssignmentError struct {
	// Null marks a NULL written to a NOT NULL slot.
	Null bool
	From Type
	To   Type
}

func (e *SlotAssignmentError) Error() string {
	if e.Null {
		// RecordConstructorValue.eval's Verify message.
		return "Cannot set a non-nullable field to the NULL value"
	}
	return fmt.Sprintf("%s value cannot be stored in a column of type %s", slotTypeName(e.From), slotTypeName(e.To))
}

// slotTypeName is the SQL name of a primitive type, else the type's rendering.
func slotTypeName(t Type) string {
	if name := explainTypeName(t); name != "UNKNOWN" || t == nil {
		return name
	}
	return t.String()
}

func (n *NarrowValue) Children() []Value { return []Value{n.Child} }

func (*NarrowValue) Name() string { return "narrow" }

func (n *NarrowValue) Type() Type { return n.Target }

func (n *NarrowValue) Evaluate(evalCtx any) (any, error) {
	v, err := n.Child.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if v == nil {
		if n.Target.IsNullable() {
			return nil, nil
		}
		return nil, &SlotAssignmentError{Null: true, From: n.Child.Type(), To: n.Target}
	}
	if !n.fits {
		return nil, &SlotAssignmentError{From: n.Child.Type(), To: n.Target}
	}
	return v, nil
}
