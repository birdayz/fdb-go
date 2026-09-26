// Package protoscope lets protobuf-go build a descriptor from a file that
// protobuf-java accepts and protoc refuses: one whose enums share a value
// name.
//
// protobuf-java scopes an enum value under its enum type (its full name is
// the enum's full name plus the value), so two enums of one scope may both
// declare Y. protoc and protobuf-go scope a value BESIDE its enum, in the
// enum's enclosing scope, so the two Ys collide and protodesc.NewFile refuses
// the file ("descriptor Y already declared"). Java's relational DDL stores
// exactly such files (`create type as enum e1('X','Y') create type as enum
// e2('Y','Z')`), so Go could neither open a store Java created that way nor
// create one.
//
// The fix is confined to the IN-MEMORY descriptor. ScopeEnumValuesAsJava
// rewrites a clone the caller builds from: each enum whose values collide in
// its scope moves into a synthetic message of its own, and every reference to
// it follows. The stored bytes never pass through it (the record layer
// re-emits the retained source proto, the relational builder emits its own
// proto), and JavaFullName answers an enum's full name as Java reads it, so
// nothing a query or an error names shows the synthetic scope.
//
// Where it applies: files Java synthesizes (the relational records file) and
// files Go synthesizes (the planner's type file). A file protoc compiled
// cannot carry such a collision, so a dependency file never needs it.
package protoscope

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// scopePrefix names the synthetic messages. A message name cannot begin with
// it in a file this package did not rewrite unless its author chose it, and
// the index after it keeps two scopes of one parent apart.
const scopePrefix = "__fdbgo_enum_scope__"

// ScopeEnumValuesAsJava rewrites fdp IN PLACE, so a caller passes the clone
// it builds the in-memory descriptor from. Type names must already be
// absolute (the callers absolutize first). A file with no colliding enum
// value is left exactly as it was, and a nil file is the caller's to refuse:
// a metadata with no records file reaches here before its build fails.
func ScopeEnumValuesAsJava(fdp *descriptorpb.FileDescriptorProto) {
	if fdp == nil {
		return
	}
	prefix := ""
	if p := fdp.GetPackage(); p != "" {
		prefix = "." + p
	}
	renames := map[string]string{}
	var others []string
	for _, e := range fdp.GetExtension() {
		others = append(others, e.GetName())
	}
	for _, s := range fdp.GetService() {
		others = append(others, s.GetName())
	}
	fdp.MessageType, fdp.EnumType = scope(prefix, fdp.MessageType, fdp.EnumType, others, renames)
	for _, m := range fdp.MessageType {
		scopeMessage(prefix, m, renames)
	}
	if len(renames) == 0 {
		return
	}
	rename := func(f *descriptorpb.FieldDescriptorProto) {
		if to, ok := renames[f.GetTypeName()]; ok {
			f.TypeName = &to
		}
	}
	var visit func(m *descriptorpb.DescriptorProto)
	visit = func(m *descriptorpb.DescriptorProto) {
		for _, f := range m.Field {
			rename(f)
		}
		for _, f := range m.Extension {
			rename(f)
		}
		for _, n := range m.NestedType {
			visit(n)
		}
	}
	for _, m := range fdp.MessageType {
		visit(m)
	}
	for _, f := range fdp.Extension {
		rename(f)
	}
}

func scopeMessage(parent string, m *descriptorpb.DescriptorProto, renames map[string]string) {
	full := parent + "." + m.GetName()
	var others []string
	for _, f := range m.Field {
		others = append(others, f.GetName())
	}
	for _, o := range m.OneofDecl {
		others = append(others, o.GetName())
	}
	for _, e := range m.Extension {
		others = append(others, e.GetName())
	}
	m.NestedType, m.EnumType = scope(full, m.NestedType, m.EnumType, others, renames)
	for _, n := range m.NestedType {
		scopeMessage(full, n, renames)
	}
}

// scope moves each enum of one scope whose value names collide there (with
// another enum's value, or with any other name the scope declares) into a
// synthetic message of its own, recording the enum's old and new absolute
// names.
func scope(parent string, msgs []*descriptorpb.DescriptorProto, enums []*descriptorpb.EnumDescriptorProto,
	others []string, renames map[string]string,
) ([]*descriptorpb.DescriptorProto, []*descriptorpb.EnumDescriptorProto) {
	count := map[string]int{}
	for _, m := range msgs {
		count[m.GetName()]++
	}
	for _, o := range others {
		count[o]++
	}
	for _, e := range enums {
		count[e.GetName()]++
		for _, v := range e.Value {
			count[v.GetName()]++
		}
	}
	kept := enums[:0:0]
	for i, e := range enums {
		collides := false
		for _, v := range e.Value {
			if count[v.GetName()] > 1 {
				collides = true
				break
			}
		}
		if !collides {
			kept = append(kept, e)
			continue
		}
		holder := fmt.Sprintf("%s%d_%s", scopePrefix, i, e.GetName())
		msgs = append(msgs, &descriptorpb.DescriptorProto{Name: &holder, EnumType: []*descriptorpb.EnumDescriptorProto{e}})
		renames[parent+"."+e.GetName()] = parent + "." + holder + "." + e.GetName()
	}
	return msgs, kept
}

// JavaFullName is an enum's full name as protobuf-java reads it: its full
// name without the synthetic scope ScopeEnumValuesAsJava may have given it.
func JavaFullName(ed protoreflect.EnumDescriptor) protoreflect.FullName {
	if p, ok := ed.Parent().(protoreflect.MessageDescriptor); ok && strings.HasPrefix(string(p.Name()), scopePrefix) {
		return p.Parent().FullName().Append(ed.Name())
	}
	return ed.FullName()
}
