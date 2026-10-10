// Portions derived from FoundationDB Record Layer (
// UserDefinedMacroFunction.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

import (
	"fmt"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// MacroFunction is Java's UserDefinedMacroFunction: a body Value over one
// quantified object per parameter, expanded at each call by substituting the
// arguments for the parameters.
type MacroFunction struct {
	Name   string
	Params []QuantifiedObjectValue
	// ParamTypes are the declared types, which keep a record's storage name
	// the quantified objects' exact types do not.
	ParamTypes []Type
	ParamNames []string
	// Defaults holds each parameter's default, nil where there is none.
	Defaults []Value
	Body     Value
}

// ToProto is UserDefinedMacroFunction.toProto: one serialization context over
// the parameters, their defaults and the body, in that order.
func (m *MacroFunction) ToProto() (*gen.PUserDefinedFunction, error) {
	c := NewSerializationContext()
	p := &gen.PUserDefinedMacroFunction{FunctionName: proto.String(m.Name)}
	for i, param := range m.Params {
		pt, err := c.TypeToProto(m.ParamTypes[i])
		if err != nil {
			return nil, err
		}
		p.Arguments = append(p.Arguments, &gen.PValue{SpecificValue: &gen.PValue_QuantifiedObjectValue{
			QuantifiedObjectValue: &gen.PQuantifiedObjectValue{Alias: proto.String(param.Correlation().Name()), ResultType: pt},
		}})
		p.ArgumentNames = append(p.ArgumentNames, m.ParamNames[i])
		def := &gen.PUserDefinedFunctionArgumentDefaultValue{IsProvided: proto.Bool(m.Defaults[i] != nil)}
		if m.Defaults[i] != nil {
			if def.Value, err = c.ValueToProto(m.Defaults[i]); err != nil {
				return nil, err
			}
		}
		p.DefaultArgumentValues = append(p.DefaultArgumentValues, def)
	}
	body, err := c.ValueToProto(m.Body)
	if err != nil {
		return nil, err
	}
	p.Body = body
	return &gen.PUserDefinedFunction{SpecificFunction: &gen.PUserDefinedFunction_UserDefinedMacroFunction{UserDefinedMacroFunction: p}}, nil
}

// MacroFunctionFromProto is UserDefinedMacroFunction.fromProto.
func MacroFunctionFromProto(p *gen.PUserDefinedMacroFunction) (*MacroFunction, error) {
	c := NewSerializationContext()
	m := &MacroFunction{Name: p.GetFunctionName(), ParamNames: p.GetArgumentNames()}
	for _, a := range p.GetArguments() {
		v, err := c.ValueFromProto(a)
		if err != nil {
			return nil, err
		}
		q, ok := AsQuantifiedObjectValue(v)
		if !ok {
			return nil, fmt.Errorf("macro %s: parameter is not a quantified object", m.Name)
		}
		m.Params = append(m.Params, q)
		m.ParamTypes = append(m.ParamTypes, c.declared(q.FlowedType()))
	}
	m.Defaults = make([]Value, len(m.Params))
	for i, d := range p.GetDefaultArgumentValues() {
		if i >= len(m.Defaults) || !d.GetIsProvided() {
			continue
		}
		v, err := c.ValueFromProto(d.GetValue())
		if err != nil {
			return nil, err
		}
		m.Defaults[i] = v
	}
	if len(m.ParamNames) == 0 {
		m.ParamNames = make([]string, len(m.Params))
	}
	body, err := c.ValueFromProto(p.GetBody())
	if err != nil {
		return nil, err
	}
	m.Body = body
	return m, nil
}

// HasNamedParameters is CatalogedFunction.hasNamedParameters: a function read
// from metadata that stored no parameter names takes positional calls only.
func (m *MacroFunction) HasNamedParameters() bool {
	for _, n := range m.ParamNames {
		if n == "" {
			return false
		}
	}
	return len(m.ParamNames) > 0
}

// ParamIndex is the position of the parameter named name, or -1.
func (m *MacroFunction) ParamIndex(name string) int {
	for i, n := range m.ParamNames {
		if n == name {
			return i
		}
	}
	return -1
}

// Expand is encapsulateFromArgumentValues: the body with each parameter's
// quantified object replaced by its argument.
func (m *MacroFunction) Expand(args []Value) (Value, error) {
	b := NewTranslationMapBuilder()
	for i, param := range m.Params {
		arg := args[i]
		b = b.When(param.Correlation()).Then(func(CorrelationIdentifier, Value) Value { return arg })
	}
	return TranslateCorrelationsChecked(m.Body, b.Build())
}
