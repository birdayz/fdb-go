package recordlayer

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// ABSOLUTIZATION MUST NOT CHANGE WHAT A NAME BINDS TO.
//
// Protobuf resolves a relative type name by walking OUTWARD from the declaring
// scope and taking the first match, so `Inner` written inside `Host` means
// `Host.Inner` whenever `Host` declares one — even with a top-level `Inner` also
// present. Rewriting every relative name to `.pkg.` + name ignores the scope and
// silently REBINDS to a different message: measured on the previous version,
// `probe.Host.Inner` became `probe.Inner`.
//
// Nothing errors when that happens, which is why it needs a test rather than a
// panic. The rewritten name is a perfectly valid reference to a real type, so
// protodesc builds the file happily and the field simply points somewhere else.
// Records written through it would carry the wrong message's fields.
//
// Two of the three arms are the two directions this can go wrong: the rewrite
// must move a nested reference to the NESTED type, and must still reach the
// top-level type when the declaring scope has no candidate of its own.
//
// The third is the MULTI-COMPONENT axis, and it is where this resolver
// deliberately departs from the `protodesc` it feeds. For `A.B`, protobuf's
// language rule — protoc's — resolves the FIRST component outward and then
// requires the rest beneath it, so `A.B` inside `Host` means `Host.A.B` when
// `Host.A` exists, and FAILS rather than falling back to `.pkg.A.B`. protoc
// says so out loud: "is resolved to probe.Host.A.B, which is not defined. The
// innermost scope is searched first in name resolution." protodesc instead
// retries the whole reference at each level and would bind `.probe.A.B`.
//
// This follows protoc, because protoc is what compiled every descriptor that
// can reach here and Java resolves the same way — a shape protoc rejects cannot
// come out of a .proto file, so matching protodesc's laxer walk would only
// paper over a descriptor no compiler produced. The arm pins the strict answer
// so the departure is a decision on record rather than an accident.
func TestAbsolutizeFieldTypeNamesPreservesLexicalScope(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// hostDeclaresInner controls whether the shadowing nested type exists.
		hostDeclaresInner bool
		wantTypeName      string
		wantBinding       string
	}{
		{
			// The shadowing case. `.probe.Inner` exists too, so a scope-blind
			// rewrite produces a valid name for the WRONG message.
			name:              "a nested type shadows the top-level one",
			hostDeclaresInner: true,
			wantTypeName:      ".probe.Host.Inner",
			wantBinding:       "probe.Host.Inner",
		},
		{
			// The control. With nothing to shadow it, the same relative name must
			// still resolve outward to the package-level type — otherwise the fix
			// would have traded a rebinding bug for a resolution failure.
			name:              "no nested type, so the outward walk reaches the package",
			hostDeclaresInner: false,
			wantTypeName:      ".probe.Inner",
			wantBinding:       "probe.Inner",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			build := func() *descriptorpb.FileDescriptorProto {
				host := &descriptorpb.DescriptorProto{
					Name: proto.String("Host"),
					Field: []*descriptorpb.FieldDescriptorProto{{
						Name:   proto.String("f"),
						Number: proto.Int32(1),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						// RELATIVE, which is the whole subject.
						TypeName: proto.String("Inner"),
					}},
				}
				if tc.hostDeclaresInner {
					host.NestedType = []*descriptorpb.DescriptorProto{
						{Name: proto.String("Inner")},
					}
				}
				return &descriptorpb.FileDescriptorProto{
					Name:    proto.String("absolutize_scope.proto"),
					Package: proto.String("probe"),
					Syntax:  proto.String("proto2"),
					MessageType: []*descriptorpb.DescriptorProto{
						{Name: proto.String("Inner")},
						host,
					},
				}
			}

			// What protobuf itself binds the untouched relative name to. This is
			// the authority the rewrite must agree with, rather than a value
			// hard-coded here and asserted against itself.
			untouched, err := protodesc.NewFile(build(), nil)
			if err != nil {
				t.Fatalf("building the untouched descriptor: %v", err)
			}
			wantBinding := untouched.Messages().ByName("Host").Fields().ByName("f").Message().FullName()
			if string(wantBinding) != tc.wantBinding {
				t.Fatalf("protobuf binds the untouched name to %s, but this arm is written against %s — "+
					"the fixture no longer expresses the shape it is named for", wantBinding, tc.wantBinding)
			}

			rewritten := build()
			absolutizeFieldTypeNames(rewritten)

			if got := rewritten.MessageType[1].Field[0].GetTypeName(); got != tc.wantTypeName {
				t.Fatalf("absolutizeFieldTypeNames produced type_name %q, want %q", got, tc.wantTypeName)
			}

			after, err := protodesc.NewFile(rewritten, nil)
			if err != nil {
				t.Fatalf("building the absolutized descriptor: %v", err)
			}
			got := after.Messages().ByName("Host").Fields().ByName("f").Message().FullName()
			if got != wantBinding {
				t.Fatalf("absolutization REBOUND the field: %s -> %s.\n"+
					"The rewritten name is valid, so protodesc reports no error and the field simply "+
					"points at a different message — records written through it carry the wrong "+
					"message's fields.", wantBinding, got)
			}
		})
	}
}

// THE MULTI-COMPONENT AXIS, kept out of the table above because its assertion is
// about the rewritten NAME rather than about a binding that survives: the shape
// it pins is one protodesc refuses to build, by design.
//
// `A.B` written inside `Host`, where `Host.A` exists but declares no `B`, and a
// top-level `.probe.A.B` does exist. protoc resolves the first component
// outward, finds `Host.A`, requires `B` beneath it, and ERRORS -- it does not
// continue outward to `.probe.A`. protodesc retries the whole reference per
// level and would bind `.probe.A.B` instead.
//
// This resolver follows protoc, so it produces `.probe.Host.A.B`. Feeding that
// to protodesc then fails loudly, which is the correct outcome for a descriptor
// no compiler could have emitted -- and is why the arm asserts the NAME and the
// resulting error rather than a successful binding.
func TestAbsolutizeFieldTypeNamesResolvesTheFirstComponentOutward(t *testing.T) {
	t.Parallel()

	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_multicomponent.proto"),
		Package: proto.String("probe"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			// .probe.A.B exists at the top level -- the binding protodesc's
			// laxer walk would have chosen.
			{
				Name:       proto.String("A"),
				NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("B")}},
			},
			{
				Name: proto.String("Host"),
				// Host.A exists but has NO nested B, so the first component
				// matches here and the rest does not.
				NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("A")}},
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:     proto.String("f"),
					Number:   proto.Int32(1),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					TypeName: proto.String("A.B"),
				}},
			},
		},
	}

	absolutizeFieldTypeNames(fd)

	got := fd.MessageType[1].Field[0].GetTypeName()
	if got != ".probe.Host.A.B" {
		t.Fatalf("absolutizeFieldTypeNames produced %q, want %q.\n"+
			"The first component `A` resolves to Host.A in the innermost scope that declares it, "+
			"and the remainder is required beneath it -- protoc does not continue outward to "+
			"`.probe.A.B`, and neither does this. Producing `.probe.A.B` would silently bind a "+
			"reference protoc rejects.", got, ".probe.Host.A.B")
	}

	// And the consequence, asserted rather than described: protodesc refuses it.
	// That is the intended outcome -- a loud failure on a descriptor no compiler
	// emits, rather than a quiet binding to a different message.
	if _, err := protodesc.NewFile(fd, nil); err == nil {
		t.Fatal("protodesc accepted `.probe.Host.A.B`, so either Host.A gained a nested B or the " +
			"resolver stopped following protoc's first-component rule. Re-read the doc comment " +
			"before relaxing this: the departure from protodesc is deliberate")
	}
}

// THE FALLBACK ARM: a name this file does not declare.
//
// This is the half of the resolver that repaired twelve cross-engine SQL
// scenarios, and for two commits it was reached by NO test in this package --
// proven, not assumed: a panic() planted at the fallback left the whole package
// green, while the same panic on the walk branch reddened it. The scope walk had
// two arms and the fallback had none, so the arm that actually broke production
// was the untested one.
//
// The shape is the relational DDL builder's, which is the producer that emits
// relative names at all: a file with NO package, a message-scoped field, and a
// dotted name binding into a dependency. Nothing in the file declares `com`, so
// the outward walk finds no candidate and the root fallback answers. Note this
// file has NO package, which makes root and package prefix the same string -- so
// this arm pins that the fallback FIRES, not which form it takes; the form is
// TestAbsolutizeFieldTypeNamesFallsBackToRootNotToThePackage's job. That is
// exactly the rewrite the old blanket code performed, and
// getting it wrong made protodesc resolve against the enclosing message and fail
// with `cannot resolve type "T_UUID.com.apple.foundationdb.record.UUID"`.
func TestAbsolutizeFieldTypeNamesFallsBackForNamesTheFileDoesNotDeclare(t *testing.T) {
	t.Parallel()

	fd := &descriptorpb.FileDescriptorProto{
		Name:   proto.String("absolutize_fallback.proto"),
		Syntax: proto.String("proto2"),
		// NO package, which is what makes the prefix a bare "." and is the shape
		// the DDL builder emits.
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("T_UUID"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("u"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				// Dotted, and binding into a DEPENDENCY: no scope in this file
				// declares `com`, so the walk must fall through.
				TypeName: proto.String("com.apple.foundationdb.record.UUID"),
			}},
		}},
	}

	// The premise, asserted so this cannot pass because the walk happened to
	// find something. It checks BOTH scopes the walk visits — `.T_UUID.com` and
	// `.com` — and both kinds of declaration. An earlier version iterated
	// `fd.MessageType` only, so a top-level ENUM named `com`, or a nested one
	// inside T_UUID, would have moved this arm onto the walk branch with the
	// assertion still passing: the fallback and the outermost walk iteration
	// emit the identical string, so no value check can separate them and the
	// premise is the ONLY thing establishing which branch ran.
	for _, m := range fd.MessageType {
		if m.GetName() == "com" {
			t.Fatal("the fixture declares a top-level message `com`; the outward walk would resolve " +
				"it and this arm would exercise the walk rather than the fallback")
		}
		for _, e := range m.EnumType {
			if e.GetName() == "com" {
				t.Fatalf("message %q declares a nested enum `com`; the walk would resolve it at that "+
					"scope and this arm would no longer reach the fallback", m.GetName())
			}
		}
		for _, n := range m.NestedType {
			if n.GetName() == "com" {
				t.Fatalf("message %q declares a nested message `com`; same problem", m.GetName())
			}
		}
	}
	for _, e := range fd.EnumType {
		if e.GetName() == "com" {
			t.Fatal("the fixture declares a top-level enum `com`; `declared` holds enums too, so the " +
				"walk would resolve it and this arm would not reach the fallback")
		}
	}

	absolutizeFieldTypeNames(fd)

	got := fd.MessageType[0].Field[0].GetTypeName()
	if got != ".com.apple.foundationdb.record.UUID" {
		t.Fatalf("absolutizeFieldTypeNames produced %q, want %q.\n"+
			"A name no scope in this file declares must fall back to a ROOT lookup. Leaving it "+
			"RELATIVE is what broke twelve cross-engine SQL scenarios, and rewriting it to anything "+
			"else would bind a different type.", got, ".com.apple.foundationdb.record.UUID")
	}
}

// A CROSS-PACKAGE REFERENCE MUST RESOLVE INTO THE IMPORT, not under this file's
// own package.
//
// This is the regression the extendee rewrite introduced and the dependency-aware
// `declared` set repairs. A packaged file importing `other.Host` and writing
// `extendee = "other.Host"` had that name rewritten to `.probe.other.Host` --
// which exists nowhere -- so a descriptor that previously LOADED stopped loading.
// Before the extendee rewrite the name was left alone and protodesc resolved it
// correctly, so the rewrite turned a latent divergence into a fatal one.
//
// Both positions are driven, because `Extendee` and `TypeName` go through the
// same resolver and only one of them was ever exercised by a cross-package
// fixture. The arms assert the NAME rather than a binding: the point is what the
// rewrite produces, and a wrong name here is not a mis-binding but an
// unresolvable descriptor.
//
// THE DEPENDENCY IS IN A SIBLING PACKAGE, so this arm does not pin the
// dependency SEEDING: `other.Host` from package `probe` resolves at root, where
// the walk and the fallback return the same string, and dropping the dependency
// types from the symbol table leaves it green -- measured. Seeding is pinned by
// TestAbsolutizeFieldTypeNamesResolvesIntoAnAncestorPackageDependency, which
// puts the target in an ancestor package so the walk must stop short of root.
// What this arm pins is that both POSITIONS are rewritten, which is a different
// axis and the one it was written for.
func TestAbsolutizeFieldTypeNamesResolvesIntoDependencies(t *testing.T) {
	t.Parallel()

	dep := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_dep.proto"),
		Package: proto.String("other"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Host"),
			ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
				{Start: proto.Int32(1000), End: proto.Int32(2000)},
			},
		}},
	}

	fd := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("absolutize_crosspkg.proto"),
		Package:    proto.String("probe"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{"absolutize_dep.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Local"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("h"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				// Relative, binding into the imported package.
				TypeName: proto.String("other.Host"),
			}},
		}},
		Extension: []*descriptorpb.FieldDescriptorProto{{
			Name:   proto.String("x"),
			Number: proto.Int32(1001),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			// The position that broke: same shape, different field.
			Extendee: proto.String("other.Host"),
		}},
	}

	absolutizeFieldTypeNames(fd, dep)

	if got := fd.MessageType[0].Field[0].GetTypeName(); got != ".other.Host" {
		t.Errorf("field type_name = %q, want %q -- a name binding into an imported package must "+
			"not be rewritten under this file's own package", got, ".other.Host")
	}
	if got := fd.Extension[0].GetExtendee(); got != ".other.Host" {
		t.Errorf("extendee = %q, want %q -- this is the regression the extendee rewrite introduced: "+
			"`.probe.other.Host` exists nowhere, so a descriptor that used to load stops loading",
			got, ".other.Host")
	}

	// And the consequence, so the arm is about behaviour and not only strings.
	depFD, err := protodesc.NewFile(dep, nil)
	if err != nil {
		t.Fatalf("building the dependency: %v", err)
	}
	files := &protoregistry.Files{}
	if err := files.RegisterFile(depFD); err != nil {
		t.Fatalf("registering the dependency: %v", err)
	}
	if _, err := protodesc.NewFile(fd, files); err != nil {
		t.Fatalf("the absolutized descriptor does not build: %v", err)
	}
}

// ABSOLUTIZATION IS LOAD-BEARING, and this is the shape that proves it.
//
// A review argued the pass should be deleted: once descriptorResolver returns
// the NotFound sentinel protodesc's walk works, so the rewrite looks like pure
// risk. The argument is careful and the conclusion is wrong -- disabling the
// pass fails a real stored template, `struct_uuid_vector`, in the cross-engine
// byte-goldens. (The differential figures that made the pass look harmful were
// measured against a version with no symbol seeding; the seeded walk scores 0
// divergences. See absolutizeFieldTypeNames' own comment for the population.)
//
// THE MECHANISM IS A FIELD-NAME / TYPE-NAME COLLISION. The template is
//
//	CREATE TYPE AS STRUCT SV (...)  CREATE TABLE T (id BIGINT, sv SV, ...)
//
// so message `T` carries a field named `SV` whose type is ALSO `SV`. protodesc
// registers fields into the same by-name map it resolves type references
// against (desc_init.go's makeBase), so resolving relative `SV` from inside `T`
// finds `T.SV` -- the FIELD -- binds it, and dies at desc_resolve.go's `case 0`
// with "unknown kind". That error has exactly one emitter and it is NOT a
// not-found: it fires when resolution SUCCEEDS and returns something that is
// neither message nor enum.
//
// protoc accepts the same source, because its type_name lookup skips non-type
// symbols. protodesc has no such filter. That is a THIRD protodesc-vs-protoc
// divergence, distinct from the two the function's doc comment describes, and
// `declared` holding only messages and enums is exactly why this pass agrees
// with protoc where protodesc does not.
//
// An earlier revision of this file said a minimal reproduction had been
// ATTEMPTED AND FAILED, and told the reader a green unit probe proves nothing.
// That was false and it was the deterrent kind of false. The missing ingredient
// was the collision: relative name, empty package and unset kind are each
// insufficient on their own, which the control arm below pins.
func TestAbsolutizationIsRequiredWhenAFieldShadowsItsOwnType(t *testing.T) {
	t.Parallel()

	build := func(fieldName string) *descriptorpb.FileDescriptorProto {
		return &descriptorpb.FileDescriptorProto{
			Name:   proto.String("absolutize_necessity.proto"),
			Syntax: proto.String("proto2"),
			// No package: the shape the relational DDL builder emits.
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("SV")},
				{
					Name: proto.String("T"),
					Field: []*descriptorpb.FieldDescriptorProto{{
						Name:   proto.String(fieldName),
						Number: proto.Int32(1),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						// Relative, as stored.
						TypeName: proto.String("SV"),
					}},
				},
			},
		}
	}

	// THE COLLISION: the field is named `SV` and its type is `SV`.
	if _, err := protodesc.NewFile(build("SV"), nil); err == nil {
		t.Fatal("protodesc built the colliding descriptor unrewritten. If that now works the pass " +
			"may be removable -- but check the cross-engine byte-goldens before concluding it, " +
			"because struct_uuid_vector is what caught the last attempt")
	}

	rewritten := build("SV")
	absolutizeFieldTypeNames(rewritten)
	if got := rewritten.MessageType[1].Field[0].GetTypeName(); got != ".SV" {
		t.Fatalf("absolutizeFieldTypeNames produced %q, want %q -- which is what protoc writes for "+
			"this source", got, ".SV")
	}
	if _, err := protodesc.NewFile(rewritten, nil); err != nil {
		t.Fatalf("the absolutized descriptor does not build: %v", err)
	}

	// THE CONTROL, and the reason the first reproduction attempt failed: without
	// the collision the same descriptor builds fine unrewritten. Relative name,
	// empty package and unset kind are each insufficient; it is the field name
	// shadowing the type name that does it.
	if _, err := protodesc.NewFile(build("sv_lower"), nil); err != nil {
		t.Fatalf("renaming the field should remove the collision and let the UNREWRITTEN descriptor "+
			"build, but it failed: %v -- so this arm is not isolating what it claims", err)
	}
}

// A PACKAGE-QUALIFIED NAME INSIDE ITS OWN PACKAGE.
//
// `extend probe.Ext` written in package `probe`. protoc accepts it and binds
// `.probe.Ext`, because its first-component lookup consults PACKAGES as well as
// types (Java's AGGREGATES_ONLY is message|enum|package|service).
//
// This was measurably broken before the walk learned about packages. Left
// relative, protodesc bound it correctly, so the extendee rewrite -- correct in
// itself -- routed it through a fallback that prepended the file's own package
// and produced `.probe.probe.Ext`, which exists nowhere: a descriptor that
// LOADED stopped loading -- a regression the differential of the day scored
// entirely on this shape.
//
// IT PINS NEITHER THE WALK NOR THE FALLBACK, and saying so is the point of this
// paragraph, because an earlier version claimed it pinned the walk.
//
// The reference resolves at ROOT scope, where the walk returns `"." + name` and
// the fallback returns `"." + name` -- output-identical by construction. So both
// mutations leave this arm green, measured: no-op'ing the package seeding does,
// and switching the fallback back to the package prefix does. What it does pin
// is the REGRESSION it was written for -- that this shape produces a loadable
// name at all, rather than the `.probe.probe.Ext` that stopped a working
// descriptor from loading.
//
// Separating a stopped walk from the fallback needs a target in the same or an
// ancestor package; that is TestAbsolutizeFieldTypeNamesStopsAtAPackageScope and
// TestAbsolutizeFieldTypeNamesResolvesIntoAnAncestorPackageDependency.
//
// Both positions are driven because `TypeName` and `Extendee` share one resolver
// and diverged once already by being written at different times.
func TestAbsolutizeFieldTypeNamesResolvesAPackageQualifiedNameInItsOwnPackage(t *testing.T) {
	t.Parallel()

	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_pkgqual.proto"),
		Package: proto.String("probe"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Ext"),
				ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
					{Start: proto.Int32(1000), End: proto.Int32(2000)},
				},
			},
			{
				Name: proto.String("Local"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:   proto.String("e"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					// Package-qualified, from inside that same package.
					TypeName: proto.String("probe.Ext"),
				}},
			},
		},
		Extension: []*descriptorpb.FieldDescriptorProto{{
			Name:     proto.String("x"),
			Number:   proto.Int32(1001),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Extendee: proto.String("probe.Ext"),
		}},
	}

	absolutizeFieldTypeNames(fd)

	if got := fd.MessageType[1].Field[0].GetTypeName(); got != ".probe.Ext" {
		t.Errorf("field type_name = %q, want %q -- the first component `probe` is this file's own "+
			"PACKAGE, so the walk must stop at root rather than prepending the package again", got, ".probe.Ext")
	}
	if got := fd.Extension[0].GetExtendee(); got != ".probe.Ext" {
		t.Errorf("extendee = %q, want %q -- `.probe.probe.Ext` exists nowhere and makes the "+
			"descriptor fail to load", got, ".probe.Ext")
	}

	if _, err := protodesc.NewFile(fd, nil); err != nil {
		t.Fatalf("the absolutized descriptor does not build: %v", err)
	}
}

// THE FALLBACK ROOTS AT `.`, NOT AT THIS FILE'S PACKAGE.
//
// Reached only when nothing in the file or its dependencies declares the name's
// first component, so the fixture uses a name that is declared nowhere. Java
// does a ROOT lookup at this point (Descriptors.java's lookupSymbol, after the
// scope loop falls through); it never prepends the file's own package.
//
// That was a Go invention with no Java counterpart. Note the committed protoc
// differential cannot settle this: every case in it has a complete dependency
// set, so this branch is unreachable there and both spellings score 0. Java is
// the authority, and this arm is where the choice is pinned.
//
// The arm asserts the NAME rather than a successful build: the name is
// unresolvable by construction, and that is correct. protoc rejects it too. What
// must not happen is inventing a different unresolvable name that hides which
// symbol was actually missing.
func TestAbsolutizeFieldTypeNamesFallsBackToRootNotToThePackage(t *testing.T) {
	t.Parallel()

	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_fallback_root.proto"),
		Package: proto.String("probe"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Local"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("f"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				// `elsewhere` is declared by nothing here and by no dependency,
				// so the walk runs out of scopes and the fallback decides.
				TypeName: proto.String("elsewhere.Target"),
			}},
		}},
	}

	// The premise: no declaration anywhere can match the first component, in any
	// scope the walk visits. Without this the arm could be exercising the walk.
	for _, m := range fd.MessageType {
		if m.GetName() == "elsewhere" {
			t.Fatal("the fixture declares `elsewhere`; the walk would resolve it and this arm " +
				"would not reach the fallback")
		}
	}

	absolutizeFieldTypeNames(fd)

	if got := fd.MessageType[0].Field[0].GetTypeName(); got != ".elsewhere.Target" {
		t.Fatalf("fallback produced %q, want %q.\n"+
			"Prepending this file's package gives `.probe.elsewhere.Target`. Java does a ROOT "+
			"lookup here (Descriptors.java's lookupSymbol, after the scope loop falls through) "+
			"and never prepends the file's own package, so that spelling is a Go invention.",
			got, ".elsewhere.Target")
	}
}

// A TYPE FROM A GLOBALLY-REGISTERED IMPORT, which the stored dependency list
// does not carry.
//
// `defaultExcludedDependencies` strips the Apple record-layer protos --
// `tuple_fields.proto` above all -- from the dependencies written into stored
// metadata, so those imports arrive only through protoregistry.GlobalFiles. A
// descriptor in `package com.apple.foundationdb.record` referencing a bare
// `UUID` therefore has NOTHING in `deps` declaring it.
//
// That shape is Java's own (`test_records_tuple_fields.proto` writes exactly
// it), and it is the case the root fallback gets wrong: with the symbol table
// blind to global imports the walk runs out of scopes and answers `.UUID`,
// which exists nowhere. The package-prefix fallback this replaced happened to
// be right on this one shape and wrong in general; no fallback rule is a
// substitute for knowing the symbols, which is why the imports are resolved and
// collected instead of guessed at.
//
// This shape is also why the stored-metadata producer is NOT exempt: the
// `records` file the SQL layer writes declares `tuple_fields.proto` and
// `record_metadata_options.proto` as imports while the stored embedded
// dependency set carries neither, which is an incomplete dependency set by
// construction.
func TestAbsolutizeFieldTypeNamesSeesGloballyRegisteredImports(t *testing.T) {
	t.Parallel()

	const uuidPath = "tuple_fields.proto"
	if _, err := protoregistry.GlobalFiles.FindFileByPath(uuidPath); err != nil {
		t.Fatalf("%s is not in the global registry (%v), so this arm cannot exercise the "+
			"globally-resolved import path it is named for", uuidPath, err)
	}

	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_globalimport.proto"),
		Package: proto.String("com.apple.foundationdb.record"),
		Syntax:  proto.String("proto2"),
		// Imported, but deliberately NOT passed in `deps` -- which is exactly
		// what defaultExcludedDependencies does to stored metadata.
		Dependency: []string{uuidPath},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Holder"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("u"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				// Bare, resolving into the ancestor package via the import.
				TypeName: proto.String("UUID"),
			}},
		}},
	}

	absolutizeFieldTypeNames(fd)

	if got := fd.MessageType[0].Field[0].GetTypeName(); got != ".com.apple.foundationdb.record.UUID" {
		t.Fatalf("absolutizeFieldTypeNames produced %q, want %q.\n"+
			"`.UUID` is what the root fallback answers when the symbol table cannot see imports "+
			"resolved through the global registry -- and it exists nowhere, so metadata Java "+
			"accepts stops loading.", got, ".com.apple.foundationdb.record.UUID")
	}
}

// A SERVICE SHADOWS A COMPOUND NAME exactly as a message does.
//
// Java's first-part lookup uses AGGREGATES_ONLY, which is
// message|enum|package|SERVICE. With service `p.q.X` in scope and an imported
// `p.X.Y`, Java stops at `p.q.X`, requires `Y` beneath it and REJECTS. A symbol
// table seeded with messages, enums and packages but not services lets the walk
// continue outward and bind `.p.X.Y` -- silently loading a descriptor Java
// refuses, which is the wrong direction for a port whose whole point is that
// both engines read the same bytes.
func TestAbsolutizeFieldTypeNamesTreatsAServiceAsAnAggregate(t *testing.T) {
	t.Parallel()

	dep := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_svc_dep.proto"),
		Package: proto.String("p"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:       proto.String("X"),
			NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("Y")}},
		}},
	}

	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_svc.proto"),
		Package: proto.String("p.q"),
		Syntax:  proto.String("proto2"),
		// The import must be DECLARED, not merely supplied. Only a file's direct
		// (and recursively public) imports are visible to resolution, so a
		// fixture that hands over a dependency without importing it measures a
		// weaker shape than it claims -- and still passes here, because the
		// expected answer comes from the service in this file.
		Dependency: []string{dep.GetName()},
		// A SERVICE named X, in the scope the walk visits first.
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("X")}},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Local"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("f"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String("X.Y"),
			}},
		}},
	}

	absolutizeFieldTypeNames(fd, dep)

	if got := fd.MessageType[0].Field[0].GetTypeName(); got != ".p.q.X.Y" {
		t.Fatalf("absolutizeFieldTypeNames produced %q, want %q.\n"+
			"The service `p.q.X` is an AGGREGATE, so the first component resolves there and the "+
			"remainder is required beneath it -- Java rejects this descriptor. Answering `.p.X.Y` "+
			"instead loads something Java refuses.", got, ".p.q.X.Y")
	}
}

// THE COMPOUND EXTENDEE SHADOWED BY A NESTED MESSAGE -- the one shape that
// justifies rewriting `extendee` at all, and the one the first-component test
// above does not reach, because that test drives `type_name` only.
//
//	message Host { message A {} extend A.B { ... } }   // package probe
//
// protoc rejects it. protodesc left to itself binds `.probe.A.B` -- a DIFFERENT
// message -- because it retries the whole reference per scope. The rewrite
// produces `.probe.Host.A.B`, which protodesc then refuses: protoc's answer,
// symbol for symbol, and a loud failure instead of a silent wrong binding.
//
// The sibling arm's own rule is that both positions get driven because
// `TypeName` and `Extendee` share one resolver; this is that rule applied to the
// shape where the two genuinely differ.
func TestAbsolutizeFieldTypeNamesResolvesAShadowedCompoundExtendee(t *testing.T) {
	t.Parallel()

	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_extendee_shadow.proto"),
		Package: proto.String("probe"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			// .probe.A.B exists -- the binding protodesc's laxer walk prefers.
			{
				Name: proto.String("A"),
				NestedType: []*descriptorpb.DescriptorProto{{
					Name: proto.String("B"),
					ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
						{Start: proto.Int32(1000), End: proto.Int32(2000)},
					},
				}},
			},
			{
				Name: proto.String("Host"),
				// A nested MESSAGE `A`, not a field: with a field every engine
				// agrees and the shape proves nothing.
				NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("A")}},
				Extension: []*descriptorpb.FieldDescriptorProto{{
					Name:     proto.String("x"),
					Number:   proto.Int32(1001),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					Extendee: proto.String("A.B"),
				}},
			},
		},
	}

	absolutizeFieldTypeNames(fd)

	if got := fd.MessageType[1].Extension[0].GetExtendee(); got != ".probe.Host.A.B" {
		t.Fatalf("extendee = %q, want %q -- the first component `A` resolves to the nested "+
			"`Host.A` and the remainder is required beneath it. Answering `.probe.A.B` binds a "+
			"different message, which is what protodesc does unaided and what protoc refuses.",
			got, ".probe.Host.A.B")
	}

	// And the consequence: protodesc refuses the rewritten name, which is the
	// intended outcome. A silent bind to `.probe.A.B` is the failure.
	if _, err := protodesc.NewFile(fd, nil); err == nil {
		t.Fatal("protodesc accepted `.probe.Host.A.B`, so either Host.A gained a nested B or the " +
			"extendee stopped following the first-component rule")
	}
}

// THE TWO ARMS BELOW ARE THE ONES THAT CAN SEE THE SEEDING, and they exist
// because every other arm in this file cannot.
//
// The symbol table is seeded from three sources -- this file's types, its
// dependencies' types, and every package prefix -- and that seeding is the
// central production change here. Yet deleting it whole left the entire package
// GREEN: measured, by no-op'ing addPackage, by dropping the dependency collect,
// and by dropping both.
//
// The reason is a degeneracy that is easy to miss and shows up in most natural
// fixtures. When the walk stops at ROOT it returns `"." + name`, and the
// fallback returns `"." + name` too -- they are output-identical by
// construction. So any fixture whose reference resolves at root pins NEITHER,
// however carefully it is written. `other.Host` from package `probe` resolves at
// root. `probe.Ext` from package `probe` resolves at root. Both were written as
// seeding tests and neither is one.
//
// A fixture only discriminates when the walk stops SHORT of root, which requires
// the target to sit in the same package or an ancestor of it. That is not an
// exotic shape -- it is `UUID` in `com.apple.foundationdb.record` referenced
// from a descendant package, which is what Java's own
// test_records_tuple_fields.proto writes and what this port loads.
//
// Both expectations are protoc's, taken from protoc 35.1 by
// `--descriptor_set_out` rather than derived by reading the algorithm.

// A PACKAGE COMPONENT IS A SCOPE THE WALK MUST STOP AT.
//
// Ground truth, protoc 35.1:
//
//	package a.b; message X {} message Outer { optional b.X f = 1; }
//	  => f.type_name = ".a.b.X"
//
// The first component `b` resolves as the PACKAGE `.a.b` from enclosing scope
// `.a.`, and `X` is then required beneath it. Java does the same -- its
// first-part lookup is AGGREGATES_ONLY, which includes package descriptors.
//
// Without package seeding the walk falls off the end and the root fallback
// answers `.b.X`, which exists nowhere. This arm is the only thing in the
// package that separates those two answers.
func TestAbsolutizeFieldTypeNamesStopsAtAPackageScope(t *testing.T) {
	t.Parallel()

	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_pkgstop.proto"),
		Package: proto.String("a.b"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("X")},
			{
				Name: proto.String("Outer"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:   proto.String("f"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					// `b` is a component of this file's OWN package.
					TypeName: proto.String("b.X"),
				}},
			},
		},
	}

	absolutizeFieldTypeNames(fd)

	if got := fd.MessageType[1].Field[0].GetTypeName(); got != ".a.b.X" {
		t.Fatalf("absolutizeFieldTypeNames produced %q, want %q (protoc 35.1).\n"+
			"`.b.X` is the answer when packages are missing from the symbol table: the walk finds "+
			"no scope to stop at, falls off the end, and the root fallback prepends a single dot to "+
			"a name whose first component is a package. Nothing declares `.b.X`.", got, ".a.b.X")
	}

	// And the descriptor must actually build, so the arm cannot pass on a name
	// that is merely different from the broken one.
	if _, err := protodesc.NewFile(fd, nil); err != nil {
		t.Fatalf("protodesc rejected the rewritten descriptor: %v", err)
	}
}

// A DEPENDENCY IN AN ANCESTOR PACKAGE, the shape the whole dependency-seeding
// branch exists for, and which no other arm reaches.
//
// Ground truth, protoc 35.1: dep `package a.b` declares `X`; main `package
// a.b.c` imports it and writes bare `X` in both positions:
//
//	f.type_name = ".a.b.X"   e.extendee = ".a.b.X"
//
// The walk climbs from `.a.b.c.Outer.` to `.a.b.` and stops there because the
// dependency declares `.a.b.X`. Drop the dependency types from the symbol table
// and it climbs past, off the end, and the fallback answers `.X`.
//
// This is Java's `test_records_tuple_fields.proto` shape in miniature: a bare
// name whose target lives in an ancestor package. Every previous
// dependency-resolution fixture in this file put the dependency in a SIBLING
// package, where the reference resolves at root and the answer is the same
// either way.
//
// Both positions are driven, as elsewhere, because `TypeName` and `Extendee`
// share one resolver and diverged once already by being written at different
// times.
func TestAbsolutizeFieldTypeNamesResolvesIntoAnAncestorPackageDependency(t *testing.T) {
	t.Parallel()

	dep := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_ancestor_dep.proto"),
		Package: proto.String("a.b"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("X"),
			ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
				{Start: proto.Int32(1000), End: proto.Int32(2000)},
			},
		}},
	}

	fd := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("absolutize_ancestor_main.proto"),
		Package:    proto.String("a.b.c"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{dep.GetName()},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Outer"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("f"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String("X"),
			}},
		}},
		Extension: []*descriptorpb.FieldDescriptorProto{{
			Name:     proto.String("e"),
			Number:   proto.Int32(1001),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Extendee: proto.String("X"),
		}},
	}

	absolutizeFieldTypeNames(fd, dep)

	if got := fd.MessageType[0].Field[0].GetTypeName(); got != ".a.b.X" {
		t.Fatalf("type_name = %q, want %q (protoc 35.1).\n"+
			"`.X` is the answer when dependency types are missing from the symbol table: the walk "+
			"climbs past `.a.b.` because nothing tells it `X` lives there, falls off the end, and "+
			"the root fallback answers a name nothing declares.", got, ".a.b.X")
	}
	if got := fd.Extension[0].GetExtendee(); got != ".a.b.X" {
		t.Fatalf("extendee = %q, want %q (protoc 35.1) -- the same resolution, at the position "+
			"that gets forgotten", got, ".a.b.X")
	}

	// The descriptor must build against the dependency, so neither answer can
	// pass by being merely different.
	depFD, err := protodesc.NewFile(dep, nil)
	if err != nil {
		t.Fatalf("building the dependency: %v", err)
	}
	reg := &protoregistry.Files{}
	if err := reg.RegisterFile(depFD); err != nil {
		t.Fatalf("registering the dependency: %v", err)
	}
	if _, err := protodesc.NewFile(fd, reg); err != nil {
		t.Fatalf("protodesc rejected the rewritten descriptor: %v", err)
	}
}

// A FIELD DOES NOT SHADOW A TYPE, and here Go follows JAVA rather than protoc.
//
// This arm exists because the engines genuinely disagree and the port had no
// record of which one it follows -- the contract was written down as "as protoc
// would resolve it", which is false on exactly this axis.
//
//		message A { extensions 1000 to 2000; }
//		message Host { optional string A = 1; extend A { ... } }
//
//	  - protoc 35.1 REJECTS: `"A" is not a message type.` Its extendee lookup uses
//	    LOOKUP_ALL, so it stops on `Host.A` -- the FIELD -- and fails.
//	  - protobuf-java ACCEPTS, binding `.A`. Descriptors.java's lookupSymbol does
//	    its first-part lookup with AGGREGATES_ONLY, and isAggregate is
//	    Descriptor|EnumDescriptor|PackageDescriptor|ServiceDescriptor, so the field
//	    is filtered out at `.Host.A` and the walk continues to the top-level `.A`.
//	    crossLink's `extendee instanceof Descriptor` check then passes.
//
// Go must produce `.A`. Java is the spec for this port, and the two engines are
// not both satisfiable: matching protoc here would mean refusing metadata Java
// writes and accepts, which is the wire-compat direction that is never allowed.
//
// This is also the standing reason NOT to add fields to `declared` -- an
// intuitive-looking change that would move Go off Java to match protoc.
func TestExtendeeSkipsAFieldShadowAsJavaDoes(t *testing.T) {
	t.Parallel()

	fd := &descriptorpb.FileDescriptorProto{
		Name:   proto.String("absolutize_field_shadow.proto"),
		Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("A"),
				ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
					{Start: proto.Int32(1000), End: proto.Int32(2000)},
				},
			},
			{
				Name: proto.String("Host"),
				// A FIELD named A, not a nested type. With a nested type both
				// engines agree and the shape proves nothing.
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:   proto.String("A"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				}},
				Extension: []*descriptorpb.FieldDescriptorProto{{
					Name:     proto.String("e"),
					Number:   proto.Int32(1001),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					Extendee: proto.String("A"),
				}},
			},
		},
	}

	absolutizeFieldTypeNames(fd)

	if got := fd.MessageType[1].Extension[0].GetExtendee(); got != ".A" {
		t.Fatalf("extendee = %q, want %q.\n"+
			"Java's first-part lookup is AGGREGATES_ONLY, so the field `Host.A` is invisible to it "+
			"and the walk binds the top-level message. Producing anything else means fields have "+
			"entered the symbol table, which matches protoc and diverges from Java -- the wrong "+
			"engine to follow for metadata Java also reads.", got, ".A")
	}

	// And it must load: Java accepts this descriptor, so Go refusing it would be
	// the same divergence in a louder form.
	if _, err := protodesc.NewFile(fd, nil); err != nil {
		t.Fatalf("protodesc rejected a descriptor protobuf-java accepts: %v", err)
	}
}

// A PRIVATE TRANSITIVE IMPORT IS INVISIBLE, and supplying it anyway must not
// change resolution.
//
// This matters because `rebuildFileDescriptor` hands absolutizeFieldTypeNames
// the ENTIRE transitive closure out of stored metadata, while Java resolves
// against a narrower set: DescriptorPool's constructor takes the file's DIRECT
// dependencies and recurses only through `public_dependency`. A file reached
// solely by a private import of a private import is not in Java's pool.
//
// The fixture: `main` (package `p.q`) directly imports `dep`,
// which declares `.p.X.Y`, and `bridge`, which PRIVATELY imports `hidden`,
// which declares a service `p.q.X`:
//
//	main --> dep     (declares .p.X.Y)
//	main --> bridge  --private--> hidden (declares service p.q.X)
//
// Java cannot see `hidden`, so `X.Y` written in `main` climbs past `.p.q` and
// binds `.p.X.Y`. Seed the whole closure instead and the walk stops at the
// invisible service and answers `.p.q.X.Y`.
//
// That answer FAILS LOUDLY rather than mis-binding quietly, which is worth
// stating because the reverse is the natural guess: protodesc applies the same
// visibility rule when resolving, so the over-exposed name points into the very
// file whose invisibility caused the stop, and protodesc rejects it with
// `cannot resolve type ... is not imported`. The divergence is therefore
// metadata Java loads and Go refuses -- a parity bug, not data corruption.
//
// The PUBLIC control below is the half that makes this a measurement rather
// than an assertion: flip the same import to `public` and the service becomes
// visible, so the answer must flip too. Without it, an implementation that
// ignored dependencies altogether would pass the first arm.
func TestAbsolutizeFieldTypeNamesIgnoresAPrivateTransitiveImport(t *testing.T) {
	t.Parallel()

	build := func(publicBridge bool) (main, dep, bridge, hidden *descriptorpb.FileDescriptorProto) {
		hidden = &descriptorpb.FileDescriptorProto{
			Name:    proto.String("absolutize_hidden.proto"),
			Package: proto.String("p.q"),
			Syntax:  proto.String("proto2"),
			// The shadowing aggregate, reachable only through `bridge`.
			Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("X")}},
		}
		bridge = &descriptorpb.FileDescriptorProto{
			Name:       proto.String("absolutize_bridge.proto"),
			Package:    proto.String("p.bridge"),
			Syntax:     proto.String("proto2"),
			Dependency: []string{hidden.GetName()},
		}
		if publicBridge {
			// Re-export it: index 0 of bridge's own Dependency list.
			bridge.PublicDependency = []int32{0}
		}
		dep = &descriptorpb.FileDescriptorProto{
			Name:    proto.String("absolutize_visible_dep.proto"),
			Package: proto.String("p"),
			Syntax:  proto.String("proto2"),
			MessageType: []*descriptorpb.DescriptorProto{{
				Name:       proto.String("X"),
				NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("Y")}},
			}},
		}
		main = &descriptorpb.FileDescriptorProto{
			Name:       proto.String("absolutize_visibility_main.proto"),
			Package:    proto.String("p.q"),
			Syntax:     proto.String("proto2"),
			Dependency: []string{dep.GetName(), bridge.GetName()},
			MessageType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("Local"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:     proto.String("f"),
					Number:   proto.Int32(1),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					TypeName: proto.String("X.Y"),
				}},
			}},
		}
		return main, dep, bridge, hidden
	}

	t.Run("private import stays invisible", func(t *testing.T) {
		t.Parallel()

		main, dep, bridge, hidden := build(false)
		// The whole closure is supplied, exactly as rebuildFileDescriptor does.
		absolutizeFieldTypeNames(main, dep, bridge, hidden)

		if got := main.MessageType[0].Field[0].GetTypeName(); got != ".p.X.Y" {
			t.Fatalf("type_name = %q, want %q.\n"+
				"`.p.q.X.Y` is the answer when the walk is seeded from the whole transitive "+
				"closure: it stops at service `p.q.X`, which arrives only through a PRIVATE "+
				"import of a private import and which Java's DescriptorPool never sees. "+
				"protodesc then REFUSES the descriptor (`cannot resolve type ... is not "+
				"imported`), so this is metadata Java loads and Go rejects -- a parity bug that "+
				"fails loudly, not a silent mis-binding.",
				got, ".p.X.Y")
		}
	})

	t.Run("public re-export makes it visible", func(t *testing.T) {
		t.Parallel()

		main, dep, bridge, hidden := build(true)
		absolutizeFieldTypeNames(main, dep, bridge, hidden)

		if got := main.MessageType[0].Field[0].GetTypeName(); got != ".p.q.X.Y" {
			t.Fatalf("type_name = %q, want %q.\n"+
				"With `bridge` re-exporting `hidden` as a PUBLIC dependency, Java's pool does "+
				"include it (importPublicDependencies recurses through exactly these), so the "+
				"service shadows and the walk must stop at `.p.q.X`. Answering `.p.X.Y` here "+
				"means public_dependency is not being followed at all.",
				got, ".p.q.X.Y")
		}
	})
}

// A PRIVATE IMPORT OF A VISIBLE DEPENDENCY CONTRIBUTES ITS PACKAGE BUT NOT ITS
// TYPES -- the asymmetry that makes Java's rule two-level for packages and
// one-level for types.
//
// Mechanism, from Descriptors.java: a DescriptorPool's `descriptorsByName`
// holds its own file's symbols and, via addPackage over that pool's dependency
// set, a PackageDescriptor for each of those files. findSymbol consults this
// pool and then `dependency.pool.descriptorsByName` for each dependency -- one
// level of indirection. So a dependency D that privately imports H hands F the
// PACKAGE of H (it is in D's pool) while H's messages stay in H's own pool,
// which F never consults.
//
// The fixture:
//
//	main (package p.q) --> dep (package p, declares .p.X.Y)
//	main               --> bridge --private--> hidden (package p.q.X, message Y)
//
// `X.Y` written in `main`: the first-component lookup is AGGREGATES_ONLY, which
// includes PackageDescriptor, so Java finds the package `.p.q.X` through
// bridge's pool, requires `Y` beneath it, and REJECTS the descriptor. Miss that
// package and the walk climbs to `.p.X.Y` and ACCEPTS -- Go loading metadata
// Java refuses, which the conformance rule forbids outright.
//
// THE TWO ARMS PROBE DIFFERENT FIELDS, and that is what makes them disjoint
// rather than one arm written twice:
//
//	arm 1  `X.Y`          -> `.p.q.X.Y`     the PACKAGE must reach two levels
//	arm 2  `OnlyInHidden` -> `.OnlyInHidden` the TYPES must stop at one
//
// Arm 2's probe is a bare name declared only by a second privately-imported
// file, `hidden2`, which sits in main's OWN package. That placement is the
// whole trick, and the first attempt got it wrong: a leaked type is only
// observable if it lands in a scope the outward walk VISITS. The walk climbs
// `.p.q.Local.` -> `.p.q.` -> `.p.` -> `.` and never descends into `.p.q.X.`,
// so a probe placed in `hidden` (package p.q.X) was unreachable and stayed
// green under the very mutation it was written to catch. `p.q` is on the path.
//
// Measured, each mutation proven present and proven to build:
//
//	drop the level-2 package seeding   arm 1 RED    arm 2 green
//	also leak the private import types arm 1 green  arm 2 RED
//	seed nothing at all                arm 1 RED    arm 2 green
//
// Arm 2 staying green on the last row is correct, not vacuous: with nothing
// seeded the root fallback really is the right answer for that field.
//
// Arm 2 also asserts a NAME rather than the existence of an error. An earlier
// version required protodesc to reject the built descriptor, which is true when
// the code is correct, when the types leak, AND when nothing is seeded --
// protodesc objects to IMPORT VISIBILITY, not to emptiness beneath the package,
// so all three states produce an error and the arm discriminated nothing.
func TestAbsolutizeFieldTypeNamesSeesPackagesOfPrivateImportsButNotTheirTypes(t *testing.T) {
	t.Parallel()

	build := func() (main, dep, bridge, hidden, hidden2 *descriptorpb.FileDescriptorProto) {
		hidden = &descriptorpb.FileDescriptorProto{
			Name:    proto.String("pkgshadow_hidden.proto"),
			Package: proto.String("p.q.X"),
			Syntax:  proto.String("proto2"),
			// A MESSAGE that must stay invisible, in a package that must not.
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Y")}},
		}
		// A SECOND hidden file, in main's OWN package, declaring a name nothing
		// else declares.
		//
		// It is what makes the type-hiding arm discriminate, and the first
		// attempt at that arm got this wrong: a leaked type must land in a scope
		// the outward walk actually VISITS. The walk climbs ancestors of the
		// declaring scope -- `.p.q.Local.`, `.p.q.`, `.p.`, `.` -- and never
		// descends into `.p.q.X.`, so a type leaked from `hidden` (package
		// p.q.X) is unreachable by a bare name and the probe stayed green under
		// the very mutation it was written to catch. Package `p.q` is on the
		// walk's path; `p.q.X` is not.
		hidden2 = &descriptorpb.FileDescriptorProto{
			Name:        proto.String("pkgshadow_hidden2.proto"),
			Package:     proto.String("p.q"),
			Syntax:      proto.String("proto2"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("OnlyInHidden")}},
		}
		bridge = &descriptorpb.FileDescriptorProto{
			Name:       proto.String("pkgshadow_bridge.proto"),
			Package:    proto.String("p.bridge"),
			Syntax:     proto.String("proto2"),
			Dependency: []string{hidden.GetName(), hidden2.GetName()}, // both PRIVATE
		}
		dep = &descriptorpb.FileDescriptorProto{
			Name:    proto.String("pkgshadow_dep.proto"),
			Package: proto.String("p"),
			Syntax:  proto.String("proto2"),
			MessageType: []*descriptorpb.DescriptorProto{{
				Name:       proto.String("X"),
				NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("Y")}},
			}},
		}
		main = &descriptorpb.FileDescriptorProto{
			Name:       proto.String("pkgshadow_main.proto"),
			Package:    proto.String("p.q"),
			Syntax:     proto.String("proto2"),
			Dependency: []string{dep.GetName(), bridge.GetName()},
			MessageType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("Local"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("f"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String("X.Y"),
					},
					{
						// The type-hiding probe: a bare name only `hidden2`
						// declares.
						Name:     proto.String("g"),
						Number:   proto.Int32(2),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String("OnlyInHidden"),
					},
				},
			}},
		}
		return main, dep, bridge, hidden, hidden2
	}

	t.Run("the package shadows", func(t *testing.T) {
		t.Parallel()

		main, dep, bridge, hidden, hidden2 := build()
		absolutizeFieldTypeNames(main, dep, bridge, hidden, hidden2)

		if got := main.MessageType[0].Field[0].GetTypeName(); got != ".p.q.X.Y" {
			t.Fatalf("type_name = %q, want %q.\n"+
				"`hidden` is privately imported by `bridge`, so its PACKAGE `p.q.X` sits in "+
				"bridge's pool and Java's AGGREGATES_ONLY first-component lookup finds it there. "+
				"Answering `.p.X.Y` means the package shadow was dropped -- Go would then load a "+
				"descriptor Java rejects.", got, ".p.q.X.Y")
		}
	})

	t.Run("the types stay hidden", func(t *testing.T) {
		t.Parallel()

		// ASSERTS A NAME, NOT THE EXISTENCE OF AN ERROR, and the distinction is
		// the entire content of this arm.
		//
		// An earlier version built the descriptor and required protodesc to
		// reject it. That was vacuous: protodesc's objection here is about
		// IMPORT VISIBILITY, not about emptiness beneath the package, so it
		// errors when the code is correct, when the types leak, AND when the
		// seeding does nothing at all -- three states, one verdict. Measured.
		// Its red state was a strict subset of the sibling arm's, so it
		// discriminated nothing while its comment called it the control.
		//
		// The probe is instead a bare name only `hidden2` declares, where the two
		// implementations produce DIFFERENT strings:
		//
		//	types hidden (correct): nothing declares it, root fallback -> ".OnlyInHidden"
		//	types leaked:           ".p.q.OnlyInHidden"
		main, dep, bridge, hidden, hidden2 := build()
		absolutizeFieldTypeNames(main, dep, bridge, hidden, hidden2)

		got := main.MessageType[0].Field[1].GetTypeName()
		if got == ".p.q.OnlyInHidden" {
			t.Fatalf("type_name = %q -- `hidden2`'s TYPES were seeded alongside its package. "+
				"Java puts H's messages in H's own pool, which this file never consults; only the "+
				"PackageDescriptor reaches here through the importing dependency's pool.", got)
		}
		if got != ".OnlyInHidden" {
			t.Fatalf("type_name = %q, want %q. Nothing visible declares this name, so the walk "+
				"must run out of scopes and the root fallback must answer.", got, ".OnlyInHidden")
		}
	})
}

// THE SECOND PACKAGE LEVEL'S CLOSURE WALK MUST TAKE LINEAR STEPS -- counted,
// not timed.
//
// WHAT THIS DOES NOT COVER, written first because the previous version of this
// arm was named for a claim it did not enforce. It counts closure STEPS through
// the onVisit seam. Of the thirteen non-pristine variants replaySecondLevel
// models over this fixture, that quantity INFLATES for four, REDUCES for seven,
// and is UNCHANGED for two -- the unchanged cases being load-bearing, because
// it is why emissions must be asserted alongside rather than instead. The
// tally is pinned by TestSecondLevelShapesArePairwiseDistinct, so it cannot go
// stale here the way its two predecessors did. It is not "work" in general:
//
//   - Cost inside a single step is invisible. A change making importsOf itself
//     expensive keeps the step count at 3n+6 and passes here.
//
//   - Allocation is invisible. The traversal-only regression allocates ~430x
//     at n=800 while its step count is what this catches; the step count is the
//     cheaper signal, not the direct one.
//
//   - WORK INSIDE walkVisibleImports THAT IS NOT A CLOSURE STEP. This is the
//     sharp one, and an earlier version of this list implied the opposite by
//     carving out only work OUTSIDE the function. FOUR regions run here and
//     none of them calls visit(): the `byPath` build, the level-1 exposure
//     loop, the union build, and the level-2 emission loop. The list is
//     enumerated rather than characterised because a characterisation cannot be
//     checked, and an earlier version of it omitted `byPath` -- the largest
//     member.
//
//     Two measured exemplars, both the same "you built a map for one membership
//     test" tidy-up that reads as an improvement, both leaving visits and
//     emissions at their exact pristine values and every arm green:
//
//     replacing the `fullyExposed` map lookup with a scan of `visible`
//     -> n^2 comparisons (2500 at n=50, 160000 at n=400)
//     replacing the `byPath` map with a last-writer-wins scan of `pool`
//     -> ~10n^2 (25000 at n=50, 1600000 at n=400), because it scales with
//     |pool| = 2n rather than |visible| = n
//
//     For scale against what this arm DOES catch: at n=50 the per-file form's
//     excess over pristine is 2450 closure steps (2606-156), so the first exemplar is
//     comparable and the second an order of magnitude larger. Neither is
//     visible to either counter, and TotalAlloc is byte-identical between pristine
//     and mutant for the first (an absolute figure is omitted deliberately: it moves
//     with the measuring harness, and the load-bearing fact is the EQUALITY). That
//     is one reason an allocation assertion is not the answer.
//
//   - CODE THIS FIXTURE NEVER EXECUTES, which is a different class from the
//     three above and the easiest to miss, because the list otherwise reads as
//     a partition of the function.
//
//     NO TOTAL IS CLAIMED HERE, deliberately, and that is the finding rather
//     than a hedge. This list has been written asserting a count four times --
//     two, two again, five, six -- and been wrong every time, because each
//     version was written by describing the arms its author had just been
//     looking at. A branch census over walkVisibleImports puts the real number
//     higher than any of them. So what follows is a list of arms PROBED, by
//     planting a panic in each and running the guard, and it is explicitly not
//     a partition:
//
//     importsOf's global-registry arm         ZERO -- every path here is stored
//     the level-1 loop's onGlobal arm         ZERO -- same reason
//     importsOf's malformed-index skip        ZERO -- every index here is valid
//     the emission loop's fullyExposed skip   REACHED, once per walk, via cg_p
//     the emission loop's empty-package arm   REACHED, once per walk, via cg_n
//
//     The three not-found arms (`!ok` in the emission loop, in the union build,
//     and inside visibleFrom) are also zero here, because every path this
//     fixture names is declared. Those and the malformed-index skip are
//     exercised by TestUnionSecondLevelMatchesPerFileSecondLevel's corpus
//     instead -- the latter by the two boundary descriptors that corpus now
//     places in every graph's directs. Their counts are not quoted: an earlier
//     version quoted three, and the commit beside it changed the generator so
//     that one of the three became zero, leaving a triple that described a tree
//     already deleted.
//
//     THE LAST TWO ROWS EACH INVERTED ONCE, and that history is why this entry
//     exists. Both used to read ZERO, and both were accidents of the fixture
//     masquerading as properties of the code: each let a real production defect
//     measure byte-identical to pristine and pass this guard GREEN. cg_p closed
//     the first, cg_n the second. Measured now, deleting the skip and dropping
//     the empty-package guard both give (156, 54) at n=50 -- `unrecognised`,
//     RED. They collide with each other because each adds exactly one emission,
//     cg_p's and cg_n's respectively; both are separated from pristine, which
//     is all this guard is asked to do. If a later fixture edit returns either
//     row to ZERO, that is a coverage REGRESSION, not a return to normal.
//
//     A weaker case remains: a public-index mis-resolution that is
//     set-preserving on this graph is invisible here. Correctness on all of
//     these is TestUnionSecondLevelMatchesPerFileSecondLevel's job, not this
//     arm's. Measured consequence: replacing GlobalFiles.FindFileByPath with a
//     linear RangeFiles scan -- unbounded per-lookup cost, the textbook "you
//     don't need the index" simplification -- leaves this arm and every
//     correctness arm green. The registry path is production-hot
//     (tuple_fields.proto always arrives that way), so this is not a
//     hypothetical region.
//
//   - Work outside walkVisibleImports entirely -- the resolution walk,
//     protodesc -- is out of scope for this arm.
//
// AND NOT AN ALLOCATION COUNTER EITHER, since the obvious response to the above
// is to add one. Measured: allocation is byte-identical under that same
// map-lookup shape, so it is blind to it too; and on the traversal-only
// regression it separates 430x where the step count separates 534x. It is also
// a PROXY -- it depends on which sink the arm installs and on slices allocated
// inside importsOf, so a benign pooling change disarms it silently. Two exact
// counters plus a stated gap beats a third inexact one.
//
// An earlier version of this arm counted onPackageOnly EMISSIONS and was named
// for linear work regardless. That gap was real: a regression can traverse
// quadratically and still emit linearly, because the final walk deduplicates.
//
// The regression this guards is real and severe: the level once called
// visibleFrom once per visible file, each walk covering the remaining suffix of
// a public-import chain, so O(n^2) per file -- and rebuildFileDescriptor runs
// the whole pass once per dependency, making it O(n^3). A few hundred
// descriptors then cost seconds, which turns VALID stored metadata into a CPU
// exhaustion input on the load path.
//
// NO CLOCK IS READ, and that is the point. Three successive wall-clock
// estimators were tried against this and all three failed, in both directions:
//
//	best-of-N per size        fails CLOSED  ~23% red on unmutated code
//	minimum of paired ratios  fails OPEN    the regression passes 12-31% of
//	                                        the time, worse the busier the box
//	median of paired ratios   fails CLOSED  57/192 red at 24-way contention
//
// The min-of-ratios failure is the instructive one, because the reasoning
// behind it was wrong rather than merely unlucky: noise on a quotient's
// DENOMINATOR pushes it DOWN, and `min` is an extreme-value estimator that
// hunts exactly that. Measured, one quadratic run logged twelve pairs correctly
// reporting ~4x and one pair at 2.79 whose small half was inflated 2.2x -- and
// the guard read the minimum. Worse, the distributions overlap outright:
// pristine 0.40-2.29 against a regression that reports 1.95-2.92 whenever it
// escapes. No single threshold on a timing quotient can separate those.
//
// So the guard counts closure STEPS instead, and separates by construction.
// `main` privately imports A_0..A_{n-1}; each A_i privately imports the head of
// a public chain B_0 -> ... -> B_{n-1}; the FIRST and LAST A each import a
// unique leaf; and A_0 alone PUBLICLY re-exports cg_p, which privately imports
// cg_q. The visible set is therefore the As plus cg_p -- a strict superset of
// main's directs, which is what lets this fixture tell `visible` from
// `fd.GetDependency()` at all -- and the union of the visible files' directs is
// B_0 repeated n times, the two leaves, cg_p and cg_q: n+5 entries.
//
//	union form     dedups via `seen`, walking the B chain once   -> 3n+6 visits
//	per-file form  calls visibleFrom once per A_i, re-walking B  -> n^2+2n+6
//
// CONTENTION CANNOT MOVE EITHER COUNT, which is a stronger statement than the
// contention measurement that used to sit here. That figure -- 192/192 both
// ways at 24-way load -- was taken on the emissions/4n version of this arm and
// was carried across the change of quantity without being re-run, which is the
// stale-population pattern this file keeps having to correct.
//
// It is not needed. Neither counter reads a clock, and neither depends on map
// ITERATION order: `seen` and `fullyExposed` are consulted by lookup only, and
// the traversal order comes from slices. Both values are therefore fixed by the
// fixture, and repeated invocations return them identically -- 156/306/606 and
// n+3, measured across six separate runs. A load figure would be describing a
// sensitivity the quantity does not have.
func TestSecondPackageLevelClosureWalkTakesLinearSteps(t *testing.T) {
	t.Parallel()

	for _, n := range secondLevelSizes {
		visits, emissions := measureSecondLevel(n)
		shape, detail := classifySecondLevelCounts(n, visits, emissions)
		if shape != secondLevelPristine {
			t.Fatalf("n=%d: visits=%d emissions=%d -- %s.\n%s",
				n, visits, emissions, shape, detail)
		}
		t.Logf("n=%d visits=%d emissions=%d (%s)", n, visits, emissions, shape)
	}
}

// buildSecondLevelFixture is the graph the complexity guard and every classifier
// arm are measured over.
//
// It is package-level rather than a closure because the expectations have to be
// DERIVED from it. While it was a closure the guard that claimed to check them
// against the fixture could not reach the fixture, so it compared
// secondLevelExpected against an algebraic restatement of its own body and was
// incapable of failing for any fixture whatsoever.
func buildSecondLevelFixture(n int) (*descriptorpb.FileDescriptorProto, []*descriptorpb.FileDescriptorProto) {
	return buildSecondLevelFixtureOpts(n, true)
}

// buildSecondLevelFixtureOpts builds the same graph with the head diamond
// optional. Only TestSecondLevelDiamondIsLoadBearing passes false; it is what
// lets that arm measure what the diamond buys instead of asserting it.
func buildSecondLevelFixtureOpts(n int, diamond bool) (*descriptorpb.FileDescriptorProto, []*descriptorpb.FileDescriptorProto) {
	var pool []*descriptorpb.FileDescriptorProto
	for i := range n {
		b := &descriptorpb.FileDescriptorProto{
			Name:    proto.String(fmt.Sprintf("cg_b_%d.proto", i)),
			Package: proto.String(fmt.Sprintf("cg.b%d", i)),
			Syntax:  proto.String("proto2"),
		}
		if i+1 < n {
			b.Dependency = []string{fmt.Sprintf("cg_b_%d.proto", i+1)}
			b.PublicDependency = []int32{0} // PUBLIC: extends the closure
		}
		// A DIAMOND AT THE HEAD, which is what makes the two causes of a 2n
		// reading separable. B_0 re-exports the chain's TAIL as well as its
		// successor, so the tail is reached TWICE inside a single closure
		// walk.
		//
		// Without it the graph is a strict chain, every repeat arrival comes
		// from a repeated UNION ENTRY, and two different changes collapse to
		// the same count: moving onVisit after the dedup check (a defect --
		// the counter stops seeing repeats) and de-duplicating `union`
		// before the walk (a behaviour-preserving refactor, since
		// visibleFrom already set-dedups). A guard that cannot tell those
		// apart must either accuse or excuse, and both are wrong half the
		// time.
		//
		// A repeat produced INSIDE one walk survives union de-duplication
		// and does not survive moving the callback, so the two land on
		// different totals and the equality separates them.
		if diamond && i == 0 && n > 2 {
			b.Dependency = append(b.Dependency, fmt.Sprintf("cg_b_%d.proto", n-1))
			b.PublicDependency = append(b.PublicDependency, 1)
		}
		pool = append(pool, b)
	}
	// The unique leaves the first and last A import. Leaves, so each adds
	// exactly one arrival and one package.
	pool = append(pool,
		&descriptorpb.FileDescriptorProto{
			Name:    proto.String("cg_x.proto"),
			Package: proto.String("cg.x"),
			Syntax:  proto.String("proto2"),
		},
		&descriptorpb.FileDescriptorProto{
			Name:    proto.String("cg_y.proto"),
			Package: proto.String("cg.y"),
			Syntax:  proto.String("proto2"),
		},
		// A PUBLICLY re-exported file, and the private leaf behind it.
		//
		// Without these the fixture cannot express two real defects, because
		// two distinct things are accidentally equal on it: `visible` equals
		// `main.GetDependency()` exactly (no A has a public dependency, so the
		// level-1 closure adds nothing), and no union entry is ever itself a
		// visible file (so the emission loop's fullyExposed skip never fires).
		//
		// Both accidents let a REAL production defect land on the pristine pair
		// and pass the guard green, which is the one direction that ships bugs:
		//
		//	union built over fd.GetDependency() instead of visible
		//	`continue` -> `break` on the fullyExposed skip
		//
		// cg_p is publicly imported by A_0, so it joins `visible` without being
		// one of main's directs AND appears in the union while already fully
		// exposed. cg_q sits privately behind it, so the union differs
		// depending on which set it was built from.
		&descriptorpb.FileDescriptorProto{
			Name:             proto.String("cg_p.proto"),
			Package:          proto.String("cg.p"),
			Syntax:           proto.String("proto2"),
			Dependency:       []string{"cg_q.proto"}, // PRIVATE: reached only via cg_p
			PublicDependency: nil,
		},
		&descriptorpb.FileDescriptorProto{
			Name:    proto.String("cg_q.proto"),
			Package: proto.String("cg.q"),
			Syntax:  proto.String("proto2"),
		},
		// A leaf with NO PACKAGE, walked but never emitted.
		//
		// The emission loop guards on `pkg != ""`, and nothing exercised that
		// guard: measured, the corpus reached it 0 times in 500 graphs and this
		// fixture had no package-less file at all. Dropping the guard therefore
		// left the whole file green at the PRISTINE pair -- a fail-open in a
		// branch the walk executes on every single call.
		//
		// It contributes a union entry and a walk step but no emission, so the
		// guard reddens on emissions alone when the check is removed.
		&descriptorpb.FileDescriptorProto{
			Name:   proto.String("cg_n.proto"),
			Syntax: proto.String("proto2"),
		})

	var mainDeps []string
	for i := range n {
		a := &descriptorpb.FileDescriptorProto{
			Name:       proto.String(fmt.Sprintf("cg_a_%d.proto", i)),
			Package:    proto.String(fmt.Sprintf("cg.a%d", i)),
			Syntax:     proto.String("proto2"),
			Dependency: []string{"cg_b_0.proto"}, // PRIVATE: stops the closure
		}
		// BOTH ENDS carry a unique private import.
		//
		// One unique import separates a union truncated to the LAST visible
		// file (which loses A_0's) from a de-duplicated one. It does not
		// separate a union truncated to the FIRST -- that keeps A_0's
		// contribution and lands on the benign pair exactly. Measured: the
		// single-unique fixture reported (2n+2, n+1) for a first-iteration
		// truncation and the classifier called it behaviour-preserving.
		//
		// With a unique import at each end, every truncation loses one of
		// them and shows up in the emission count, whichever end it keeps.
		switch i {
		case 0:
			a.Dependency = append(a.Dependency, "cg_x.proto", "cg_p.proto")
			// PUBLIC, and it is the only public dependency any A has. This is
			// what makes `visible` a strict superset of main's directs.
			a.PublicDependency = []int32{2}
		case n - 1:
			a.Dependency = append(a.Dependency, "cg_y.proto")
		case 1:
			// A MIDDLE A, deliberately: the FIRST and LAST must keep carrying a
			// PACKAGED unique leaf, which is what the truncation deficit rests on.
			a.Dependency = append(a.Dependency, "cg_n.proto")
		}
		pool = append(pool, a)
		mainDeps = append(mainDeps, a.GetName())
	}
	main := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("cg_main.proto"),
		Package:    proto.String("cg.main"),
		Syntax:     proto.String("proto2"),
		Dependency: mainDeps,
	}
	return main, pool
}

// VISITS, not package emissions, and the difference is the whole point.
//
// The final walk over the union deduplicates, so a regression that computes
// a closure PER VISIBLE FILE and still feeds the union -- `union = append(
// union, visibleFrom(direct)...)` -- traverses the B chain O(n^2) times and
// yet emits only n packages. Measured: that mutation leaves an
// emission-counting guard reporting exactly 53/103/203 and passing, which
// is a gate inert on a shape inside its own claim.
//
// Counting closure steps sees both regressions, because both do the extra
// walking whatever they do with the result.
func measureSecondLevel(n int) (visits, emissions int) {
	main, pool := buildSecondLevelFixture(n)
	walkVisibleImports(main, pool,
		func(*descriptorpb.FileDescriptorProto) {},
		func(protoreflect.FileDescriptor) {},
		func(string) { emissions++ },
		func(string) { visits++ },
	)
	return visits, emissions
}

// secondLevelVariant names a way the second package level can be written: one
// is what production does, the rest are the regressions and refactors the
// classifier claims to tell apart.
type secondLevelVariant string

const (
	slPristine         secondLevelVariant = "pristine"
	slOnVisitHoist     secondLevelVariant = "onVisit hoisted out of the recursion"
	slDedupMoved       secondLevelVariant = "onVisit moved after the dedup check"
	slTruncateFirst    secondLevelVariant = "union truncated to the FIRST visible file"
	slTruncateLast     secondLevelVariant = "union truncated to the LAST visible file"
	slUnionDedup       secondLevelVariant = "union de-duplicated before the walk"
	slNoPublic         secondLevelVariant = "public-import recursion removed"
	slPerFile          secondLevelVariant = "per-file closure walk"
	slPerFileSink      secondLevelVariant = "per-file closure walk, de-duplicating at the sink"
	slTraversalOnly    secondLevelVariant = "traversal-only per-file union build"
	slEmissionHoist    secondLevelVariant = "emission loop re-run over an already-walked set"
	slEmissionRewalk   secondLevelVariant = "emission loop re-run, re-walking the union each time"
	slUnionOverDirects secondLevelVariant = "union built over the directs instead of the visible set"
	slEmitBreak        secondLevelVariant = "emission loop breaks on a fully-exposed entry"
)

// secondLevelVariants is every variant replaySecondLevel implements, in one
// place so a new one cannot be added without the distinctness check seeing it.
var secondLevelVariants = []secondLevelVariant{
	slPristine, slOnVisitHoist, slDedupMoved, slTruncateFirst, slTruncateLast,
	slUnionDedup, slNoPublic, slPerFile, slPerFileSink, slTraversalOnly,
	slEmissionHoist, slEmissionRewalk, slUnionOverDirects, slEmitBreak,
}

// replaySecondLevel walks the REAL fixture under a named variant and returns
// the counts that variant produces.
//
// WHAT THIS IS NOT, first. It is not production, and running it proves nothing
// about production. Its fidelity rests on two different things, and only one of
// them is re-checked on every run:
//
//   - CONTINUOUSLY: TestSecondLevelExpectedMatchesTheFixture requires
//     replay(slPristine) to equal what walkVisibleImports actually counts, at
//     every driven size. If the model drifts on the pristine path, that fails.
//
//   - ONCE, BY HAND: each non-pristine variant's pair was reproduced by
//     applying the corresponding edit to metadata_proto.go and reading the
//     guard -- for instance the sink-dedup form measured (2606, 53) and the
//     hoisted-onVisit form (105, 53) at n=50, both matching the model. Nothing
//     re-checks those. A production refactor that changes what the
//     corresponding edit would look like leaves them describing an edit nobody
//     would now write, and no test will say so.
//
// So this establishes that the CLASSIFIER separates these shapes ON THIS
// FIXTURE. It does not establish that production is free of them, nor that the
// variant list is complete -- a shape nobody modelled reports `unrecognised`,
// which is the intended answer rather than a covered one.
//
// What it buys is the thing the closed-form table could not: the pairs are
// MEASURED over the committed graph, so a fixture edit moves them, and the
// mutations that used to justify each row are committed as code instead of
// being deleted after one reading. The previous version wrote each pair as a
// func(n) int transcribed from a throwaway mutation, which made every row a
// fact about a fixture nobody could re-derive -- and let a fixture edit that
// preserved the pristine counts silently re-point the truncation arm at a shape
// it no longer described.
func replaySecondLevel(
	main *descriptorpb.FileDescriptorProto,
	pool []*descriptorpb.FileDescriptorProto,
	v secondLevelVariant,
) (visits, emissions int) {
	byPath := make(map[string]*descriptorpb.FileDescriptorProto, len(pool))
	for _, d := range pool {
		byPath[d.GetName()] = d
	}
	// The fixture is entirely STORED, so this mirrors only the byPath arm of
	// production's importsOf. The global-registry arm runs zero times here --
	// see the never-executed-code bullet on the guard above.
	importsOf := func(path string) (pkg string, direct, public []string, ok bool) {
		d, found := byPath[path]
		if !found {
			return "", nil, nil, false
		}
		direct = d.GetDependency()
		for _, idx := range d.GetPublicDependency() {
			if idx < 0 || int(idx) >= len(direct) {
				continue
			}
			public = append(public, direct[idx])
		}
		return d.GetPackage(), direct, public, true
	}

	visibleFrom := func(directs []string) []string {
		seen := map[string]bool{}
		var out []string
		var visit func(p string)
		visit = func(p string) {
			if v != slDedupMoved && v != slOnVisitHoist {
				visits++
			}
			if seen[p] {
				return
			}
			seen[p] = true
			if v == slDedupMoved {
				visits++
			}
			out = append(out, p)
			if v == slNoPublic {
				return
			}
			_, _, public, ok := importsOf(p)
			if !ok {
				return
			}
			for _, q := range public {
				visit(q)
			}
		}
		for _, p := range directs {
			// The hoisted form still counts BEFORE the dedup check, but once
			// per entry of `directs` rather than once per arrival, so every
			// repeat produced INSIDE a walk goes uncounted.
			if v == slOnVisitHoist {
				visits++
			}
			visit(p)
		}
		return out
	}

	visible := visibleFrom(main.GetDependency())
	fullyExposed := make(map[string]bool, len(visible))
	for _, path := range visible {
		fullyExposed[path] = true
	}

	directsOf := func(path string) []string {
		_, direct, _, ok := importsOf(path)
		if !ok {
			return nil
		}
		return direct
	}
	emit := func(q string) {
		if fullyExposed[q] {
			return
		}
		if pkg, _, _, ok := importsOf(q); ok && pkg != "" {
			emissions++
		}
	}

	switch v {
	case slPerFile:
		for _, path := range visible {
			for _, q := range visibleFrom(directsOf(path)) {
				emit(q)
			}
		}
		return visits, emissions
	case slPerFileSink:
		// No union and no final walk: the duplicate packages are absorbed by
		// the sink instead. Quadratic traversal, linear emissions -- a pair
		// that shares visits with slPerFile and emissions with slTraversalOnly
		// and is neither, which is why the quadratic arms cannot be ranges.
		emitted := map[string]bool{}
		for _, path := range visible {
			for _, q := range visibleFrom(directsOf(path)) {
				if emitted[q] {
					continue
				}
				emitted[q] = true
				emit(q)
			}
		}
		return visits, emissions
	case slTraversalOnly:
		var union []string
		for _, path := range visible {
			union = append(union, visibleFrom(directsOf(path))...)
		}
		for _, q := range visibleFrom(union) {
			emit(q)
		}
		return visits, emissions
	}

	var union []string
	switch {
	case v == slTruncateFirst && len(visible) > 0:
		union = directsOf(visible[0])
	case v == slTruncateLast && len(visible) > 0:
		union = directsOf(visible[len(visible)-1])
	case v == slUnionOverDirects:
		// The visible set is a strict SUPERSET of the directs here, because
		// A_0 re-exports cg_p publicly. Building the union over the directs
		// therefore drops cg_p's own imports.
		for _, path := range main.GetDependency() {
			union = append(union, directsOf(path)...)
		}
	default:
		for _, path := range visible {
			union = append(union, directsOf(path)...)
		}
	}
	if v == slUnionDedup {
		seen := map[string]bool{}
		var deduped []string
		for _, p := range union {
			if seen[p] {
				continue
			}
			seen[p] = true
			deduped = append(deduped, p)
		}
		union = deduped
	}

	if v == slEmissionRewalk {
		// The NATURAL spelling of the emission-hoist edit. Production reads
		// `for _, q := range visibleFrom(union)`, so wrapping THAT loop in a
		// per-visible-file one re-walks the union every time. slEmissionHoist
		// models the other spelling, which lifts the walk into a variable
		// first and is a refactor plus a bug rather than just a bug.
		for range visible {
			for _, q := range visibleFrom(union) {
				emit(q)
			}
		}
		return visits, emissions
	}
	walked := visibleFrom(union)
	if v == slEmissionHoist {
		for range visible {
			for _, q := range walked {
				emit(q)
			}
		}
		return visits, emissions
	}
	for _, q := range walked {
		// `continue` -> `break`: the loop stops at the first already-exposed
		// entry instead of skipping it, truncating the rest of the level.
		if v == slEmitBreak && fullyExposed[q] {
			break
		}
		emit(q)
	}
	return visits, emissions
}

// secondLevelUniqueLeaves counts the pool paths imported by EXACTLY ONE of
// main's direct dependencies AND not themselves visible -- the quantity
// cgUniqueLeaves names -- and returns the set of directs that carry one.
//
// The visibility exclusion is not incidental. cg_p is imported by exactly one A
// and is emphatically not a unique leaf: A_0 re-exports it PUBLICLY, so it
// joins the visible set and the emission loop skips it. Counting it would make
// cgUniqueLeaves disagree with the emissions the fixture actually produces.
//
// The carrier set exists because the COUNT cannot express PLACEMENT. Moving
// cg_x inward to A_1 and cg_y inward to A_{n-2} leaves the count at 2 and
// leaves both truncation deficits equal -- so a count check and a symmetry
// check both pass while the FIRST-and-LAST claim they rest on has become false.
func secondLevelUniqueLeaves(
	main *descriptorpb.FileDescriptorProto,
	pool []*descriptorpb.FileDescriptorProto,
) (count int, carriers map[string]bool) {
	byPath := make(map[string]*descriptorpb.FileDescriptorProto, len(pool))
	for _, d := range pool {
		byPath[d.GetName()] = d
	}
	// The visible set, by the same rule production uses: the directs, plus
	// everything reachable from them through PUBLIC imports alone.
	visible := map[string]bool{}
	var expose func(path string)
	expose = func(path string) {
		if visible[path] {
			return
		}
		visible[path] = true
		d, ok := byPath[path]
		if !ok {
			return
		}
		direct := d.GetDependency()
		for _, idx := range d.GetPublicDependency() {
			if idx < 0 || int(idx) >= len(direct) {
				continue
			}
			expose(direct[idx])
		}
	}
	for _, dep := range main.GetDependency() {
		expose(dep)
	}

	importers := map[string][]string{}
	for _, dep := range main.GetDependency() {
		d, ok := byPath[dep]
		if !ok {
			continue
		}
		for _, q := range d.GetDependency() {
			importers[q] = append(importers[q], dep)
		}
	}
	carriers = map[string]bool{}
	for path, who := range importers {
		if len(who) != 1 || visible[path] {
			continue
		}
		// A PACKAGE-LESS leaf is walked but never emitted, so it is not one of
		// the leaves this counts: it contributes nothing for a truncation to
		// lose, and including it would make the closed form disagree with the
		// emissions the fixture actually produces.
		if d, ok := byPath[path]; !ok || d.GetPackage() == "" {
			continue
		}
		count++
		carriers[who[0]] = true
	}
	return count, carriers
}

// secondLevelPackagelessLeaves counts the singly-imported non-visible leaves
// that carry NO package -- the quantity cgPackagelessLeaves names, and the only
// thing in this fixture that reaches the emission loop's `pkg != ""` guard.
func secondLevelPackagelessLeaves(
	main *descriptorpb.FileDescriptorProto,
	pool []*descriptorpb.FileDescriptorProto,
) int {
	byPath := make(map[string]*descriptorpb.FileDescriptorProto, len(pool))
	for _, d := range pool {
		byPath[d.GetName()] = d
	}
	// VISIBILITY, not just directness, and the difference is the whole probe.
	// Production skips a walked file at `fullyExposed[q]` BEFORE it evaluates
	// `pkg != ""`, and fullyExposed holds the public-import closure rather than
	// main.GetDependency(). Testing only directness would keep returning 1
	// after cg_n became a public import of its A -- at which point production
	// never reaches the guard and the assertion this feeds would stay green
	// with its only probe gone.
	visible := map[string]bool{}
	var expose func(path string)
	expose = func(path string) {
		if visible[path] {
			return
		}
		visible[path] = true
		d, ok := byPath[path]
		if !ok {
			return
		}
		deps := d.GetDependency()
		for _, idx := range d.GetPublicDependency() {
			if idx < 0 || int(idx) >= len(deps) {
				continue
			}
			expose(deps[idx])
		}
	}
	for _, dep := range main.GetDependency() {
		expose(dep)
	}
	importers := map[string]int{}
	for _, dep := range main.GetDependency() {
		d, ok := byPath[dep]
		if !ok {
			continue
		}
		for _, q := range d.GetDependency() {
			importers[q]++
		}
	}
	n := 0
	for path, c := range importers {
		if c != 1 || visible[path] {
			continue
		}
		if d, ok := byPath[path]; ok && d.GetPackage() == "" {
			n++
		}
	}
	return n
}

// THE WITNESS'S VISIBILITY RULE, DRIVEN DIRECTLY.
//
// secondLevelPackagelessLeaves counts a package-less leaf only when it is NOT
// visible, because production skips a visible file at `fullyExposed[q]` one
// line before it evaluates `pkg != ""`. That rule is the whole point of the
// helper -- an earlier version tested directness instead and would have kept
// reporting coverage after the leaf was made public, i.e. after its only route
// to the guard had gone.
//
// The committed fixture cannot show the rule doing anything: its only
// package-less file is private, and the only publicly re-exported file has a
// package. So deleting the closure walk from the helper leaves every arm in
// this file green, and this is the arm that notices.
func TestSecondLevelPackagelessWitnessRequiresInvisibility(t *testing.T) {
	t.Parallel()

	build := func(public bool) (*descriptorpb.FileDescriptorProto, []*descriptorpb.FileDescriptorProto) {
		leaf := &descriptorpb.FileDescriptorProto{
			Name:   proto.String("w_leaf.proto"),
			Syntax: proto.String("proto2"),
			// No package: this is the file whose emission production guards.
		}
		a := &descriptorpb.FileDescriptorProto{
			Name:       proto.String("w_a.proto"),
			Package:    proto.String("w.a"),
			Syntax:     proto.String("proto2"),
			Dependency: []string{"w_leaf.proto"},
		}
		if public {
			a.PublicDependency = []int32{0}
		}
		main := &descriptorpb.FileDescriptorProto{
			Name:       proto.String("w_main.proto"),
			Package:    proto.String("w.main"),
			Syntax:     proto.String("proto2"),
			Dependency: []string{"w_a.proto"},
		}
		return main, []*descriptorpb.FileDescriptorProto{a, leaf}
	}

	main, pool := build(false)
	if got := secondLevelPackagelessLeaves(main, pool); got != 1 {
		t.Fatalf("a PRIVATE package-less leaf must count: got %d, want 1. Production reaches "+
			"the empty-package guard for exactly this shape", got)
	}

	main, pool = build(true)
	if got := secondLevelPackagelessLeaves(main, pool); got != 0 {
		t.Fatalf("a PUBLICLY re-exported package-less leaf must NOT count: got %d, want 0. It "+
			"is fullyExposed, so production skips it BEFORE the empty-package guard -- counting "+
			"it would report coverage of a branch the walk never reaches", got)
	}
}

// secondLevelPublicShape counts, out of the built pool, the two quantities the
// public re-export contributes: how many files reach the visible set ONLY by
// being publicly re-exported by a direct, and how many non-visible files those
// re-exports privately import.
//
// It exists because cgPublicReexports and cgBehindPublic are both 1, which
// makes them interchangeable to every arithmetic check in this file: swapping
// them in secondLevelExpected leaves the closed form identical and the suite
// green. Two hand-typed constants with the same value and no witness are one
// constant wearing two names until something counts them separately.
func secondLevelPublicShape(
	main *descriptorpb.FileDescriptorProto,
	pool []*descriptorpb.FileDescriptorProto,
) (reexports, behind int) {
	byPath := make(map[string]*descriptorpb.FileDescriptorProto, len(pool))
	for _, d := range pool {
		byPath[d.GetName()] = d
	}
	direct := map[string]bool{}
	for _, dep := range main.GetDependency() {
		direct[dep] = true
	}

	visible := map[string]bool{}
	var expose func(path string)
	expose = func(path string) {
		if visible[path] {
			return
		}
		visible[path] = true
		d, ok := byPath[path]
		if !ok {
			return
		}
		deps := d.GetDependency()
		for _, idx := range d.GetPublicDependency() {
			if idx < 0 || int(idx) >= len(deps) {
				continue
			}
			expose(deps[idx])
		}
	}
	for _, dep := range main.GetDependency() {
		expose(dep)
	}

	for path := range visible {
		if direct[path] {
			continue
		}
		reexports++
		d, ok := byPath[path]
		if !ok {
			continue
		}
		for _, q := range d.GetDependency() {
			if !visible[q] {
				behind++
			}
		}
	}
	return reexports, behind
}

// secondLevelSizes is the one place the GUARD's driven sizes are written. Every
// arm that drives the fixture at those sizes reads it, so a size added for one
// cannot silently leave the others measuring a different population. Exactly
// ONE arm deliberately does not read it: `grep -n "range []int{"` over this
// file returns a single hit, TestSecondLevelPairsCollideBelowTheDrivenSizes,
// which exists precisely to drive sizes BELOW these. This list's own value is
// pinned by TestSecondLevelSizesArePinned.
var secondLevelSizes = []int{50, 100, 200}

// secondLevelShape names a recognised (visits, emissions) signature of the
// second package level.
type secondLevelShape string

const (
	secondLevelPristine    secondLevelShape = "pristine"
	secondLevelVacuous     secondLevelShape = "vacuous: the level produced nothing"
	secondLevelDedupMoved  secondLevelShape = "repeat arrivals no longer counted"
	secondLevelTruncated   secondLevelShape = "union truncated to a single visible file"
	secondLevelUnionDedup  secondLevelShape = "union de-duplicated before the walk (benign)"
	secondLevelNoPublic    secondLevelShape = "public-import recursion removed"
	secondLevelQuadWalk    secondLevelShape = "per-file closure walk"
	secondLevelQuadSink    secondLevelShape = "per-file closure walk with a de-duplicating sink"
	secondLevelQuadTravers secondLevelShape = "traversal-only per-file union build"
	secondLevelQuadEmit    secondLevelShape = "emission loop re-run over an already-walked set"
	secondLevelQuadRewalk  secondLevelShape = "emission loop re-run, re-walking the union each time"
	secondLevelDirectsOnly secondLevelShape = "second level built over the directs, not the visible set"
	secondLevelEmitBreak   secondLevelShape = "emission loop truncated at a fully-exposed entry"
	secondLevelUnknown     secondLevelShape = "unrecognised"
)

// The fixture's named quantities. Each is checked against the built pool by
// TestSecondLevelExpectedMatchesTheFixture, so none is an independent knob.
const (
	cgUniqueLeaves      = 2 // cg_x on A_0, cg_y on A_{n-1}
	cgPublicReexports   = 1 // cg_p, PUBLICLY re-exported by A_0
	cgBehindPublic      = 1 // cg_q, privately imported by cg_p
	cgPackagelessLeaves = 1 // cg_n, walked but never emitted: it has no package
)

// secondLevelExpected is the CLOSED FORM of the pristine pair, kept because it
// is the only readable statement of why the numbers are what they are.
//
// It is documentation that must stay true, not a source of truth: the arm
// below ties it to what production counts AND to what the model counts, at
// every driven size. Nothing reads it to make a decision.
func secondLevelExpected(n int) (visits, emissions int) {
	// Level 1 visits each A once and then follows A_0's PUBLIC re-export, so
	// the visible set is the As plus cg_p.
	level1 := n + cgPublicReexports
	// Level 2's union carries one entry per A -- n of them, all naming B_0 --
	// plus the unique leaves, plus cg_p from A_0's directs, plus cg_q from
	// cg_p's. Walking it follows the chain's n-1 successors and the diamond's
	// single repeat of the tail.
	union := n + cgUniqueLeaves + cgPackagelessLeaves + cgPublicReexports + cgBehindPublic
	visits = level1 + union + (n - 1) + 1
	// One package per B in the chain, plus one per unique leaf, plus cg_q.
	// TWO walked files contribute nothing and for DIFFERENT reasons: cg_p is
	// already fully exposed, and cg_n has no package at all. The second is the
	// only thing exercising the emission loop's `pkg != ""` guard.
	emissions = n + cgUniqueLeaves + cgBehindPublic
	return visits, emissions
}

// secondLevelSignatures maps each modelled variant to the shape and diagnosis a
// run measuring its pair should be given.
//
// THE PAIRS ARE NOT WRITTEN HERE. classifySecondLevelCounts obtains them by
// replaying the variant over the fixture, which is what makes "an EXACT pair,
// not a range" literally true of every arm and removes the last hand-typed pair
// from the MATCHING. Hand-typed constants remain elsewhere, and the ones
// WITHOUT an independent witness are the reason for listing them at all:
// cgUniqueLeaves is counted out of the built pool, cgPublicReexports and
// cgBehindPublic likewise, and the 4/7/2 tally and the truncation deficit are
// asserted from measured pairs. secondLevelSizes has its own arm. What is left
// WITHOUT a witness is the diamond-repeat `+1` in secondLevelExpected: an edit
// removing the diamond and dropping that `+1` together is self-consistent and
// passes every tie, and TestSecondLevelDiamondIsLoadBearing measuring what the
// removal costs is the only thing standing behind that term.
// It matters because a range predicate carrying a message that names
// one cause is how this classifier twice told a real defect it was a
// behaviour-preserving refactor, and how it then absorbed a genuinely distinct
// seventh shape -- a per-file walk de-duplicating at the SINK -- into an arm
// telling the reader to look for a union and a final walk that do not exist.
var secondLevelSignatures = []struct {
	variant secondLevelVariant
	shape   secondLevelShape
	detail  string
}{
	{slPristine, secondLevelPristine, ""},
	{
		slDedupMoved, secondLevelDedupMoved,
		"Repeated arrivals are no longer being counted -- onVisit has either moved AFTER the " +
			"dedup check or been hoisted out of the recursion into the directs loop. Both leave " +
			"the counter measuring DISTINCT files and blind to a traversal-only regression, " +
			"which is the shape it exists to catch.",
	},
	{
		slOnVisitHoist, secondLevelDedupMoved,
		"Repeated arrivals are no longer being counted -- onVisit has either moved AFTER the " +
			"dedup check or been hoisted out of the recursion into the directs loop. Both leave " +
			"the counter measuring DISTINCT files and blind to a traversal-only regression, " +
			"which is the shape it exists to catch.",
	},
	{
		slTruncateFirst, secondLevelTruncated,
		"The union is being TRUNCATED to one visible file's imports rather than accumulated, so " +
			"every other file's package contribution is dropped. The fixture puts a unique " +
			"import at each END precisely so that a truncation loses one of them whichever end " +
			"it keeps -- that missing emission is what separates this from the benign " +
			"de-duplication one visit away.",
	},
	{
		slTruncateLast, secondLevelTruncated,
		"The union is being TRUNCATED to one visible file's imports rather than accumulated, so " +
			"every other file's package contribution is dropped. The fixture puts a unique " +
			"import at each END precisely so that a truncation loses one of them whichever end " +
			"it keeps -- that missing emission is what separates this from the benign " +
			"de-duplication one visit away.",
	},
	{
		slUnionDedup, secondLevelUnionDedup,
		"That is the union de-duplicated before the walk -- BEHAVIOUR-PRESERVING, since " +
			"visibleFrom already set-dedups, and still linear. Nothing is broken; this " +
			"expectation is simply stale. Re-derive it deliberately.",
	},
	{
		slNoPublic, secondLevelNoPublic,
		"Emissions have collapsed to a CONSTANT -- one package per DISTINCT union entry and " +
			"nothing beyond. The public-import recursion in visibleFrom is gone, so the closure " +
			"never extends past a file's direct imports. Java resolves against the recursive " +
			"public closure; dropping it makes Go refuse metadata Java loads.",
	},
	{
		slPerFile, secondLevelQuadWalk,
		"Both counters are quadratic: the single union traversal has been replaced by a " +
			"per-visible-file one, which is O(n^2) here and O(n^3) across " +
			"rebuildFileDescriptor's per-dependency pass -- valid metadata becomes a CPU " +
			"exhaustion input.",
	},
	{
		slPerFileSink, secondLevelQuadSink,
		"The closure is computed PER VISIBLE FILE and the duplicate packages are absorbed by " +
			"the SINK rather than by a union, so the traversal is quadratic while emissions stay " +
			"linear -- and there is no union and no final walk to go looking for. Same " +
			"asymptotics as the per-file form: O(n^2) here, O(n^3) across " +
			"rebuildFileDescriptor's per-dependency pass.",
	},
	{
		slTraversalOnly, secondLevelQuadTravers,
		"The closure walk is quadratic while emissions stay linear: the union is being built by " +
			"walking each visible file's closure, and the final walk then de-duplicates. " +
			"Counting emissions alone cannot see this.",
	},
	{
		slEmissionHoist, secondLevelQuadEmit,
		"Emissions have gone quadratic while the closure walk has NOT -- visits are exactly " +
			"pristine. The emission loop is re-running over an already-walked set. addPackage is " +
			"idempotent, so no correctness arm can see this.",
	},
	{
		slEmissionRewalk, secondLevelQuadRewalk,
		"Emissions have gone quadratic and so has the closure walk, but not in the per-file " +
			"pattern -- the emission loop is re-running over the union and re-walking it every " +
			"time. This is the emission-hoist defect written the way it would actually be " +
			"written: production's loop reads `for _, q := range visibleFrom(union)`, so " +
			"wrapping THAT re-evaluates the walk, where lifting it into a variable first is a " +
			"refactor plus a bug. addPackage is idempotent, so no correctness arm sees the " +
			"duplicate emissions.",
	},
	{
		slUnionOverDirects, secondLevelDirectsOnly,
		"The second level's union is being accumulated over the file's DIRECT dependencies " +
			"rather than over its visible set, so whatever a direct dependency re-exports " +
			"publicly contributes nothing: the imports behind a public re-export are dropped " +
			"from the level entirely. The two sets are equal on any graph with no public " +
			"imports, which is why this needs a fixture that has one.",
	},
	{
		slEmitBreak, secondLevelEmitBreak,
		"The emission loop stops at the first already-exposed entry instead of skipping it -- a " +
			"`continue` that became a `break`. Everything after that entry in the walked order " +
			"loses its package, so the level is truncated at an arbitrary point determined by " +
			"traversal order rather than by anything semantic.",
	},
}

// secondLevelDeclaredShape is the shape a variant is INTENDED to be diagnosed
// as, read straight from the signature table.
//
// It exists so the distinctness check has something to compare that does not
// come out of the classifier. Comparing two classifier answers cannot work:
// the classifier is a pure function of the counts, so a collision hands it
// identical inputs and it returns identical outputs, and the inequality that
// was supposed to catch the collision is false precisely when the collision
// happens.
func secondLevelDeclaredShape(v secondLevelVariant) (secondLevelShape, bool) {
	for _, sig := range secondLevelSignatures {
		if sig.variant == v {
			return sig.shape, true
		}
	}
	return "", false
}

// classifySecondLevelCounts names the shape a measured pair belongs to.
//
// It is a function of the counts and n alone, so every arm can be driven
// directly. The guard enters it once per size and, on correct code, only ever
// sees `pristine` -- which is exactly why the arms need a table test rather
// than the throwaway mutations that verified their predecessors.
//
// WHAT EXACT-PAIR MATCHING DOES NOT BUY. It stops a RANGE arm from absorbing a
// shape it does not describe, which is what the three `>=` predicates it
// replaced did. It does NOT stop COINCIDENTAL equality, because shapes are not
// in bijection with pairs -- two counters cannot separate more shapes than they
// have distinct values.
//
// AND THE PRISTINE PAIR IS ONE OF THE MODELLED PAIRS, which is the case that
// matters and the one an earlier version of this comment explicitly denied. It
// claimed "the guard still goes red either way; it is the diagnosis that is
// wrong, which costs the reader time rather than correctness." A shape landing
// on PRISTINE is not misnamed, it is MISSED, and the guard goes GREEN. FOUR
// real production defects did exactly that on earlier fixtures -- building the
// union over fd.GetDependency(), `continue` -> `break` on the fullyExposed
// skip, deleting that skip outright, and dropping the emission loop's
// `pkg != ""` guard -- each measuring the pristine pair of its day and passing.
// cg_p, cg_q and cg_n were added to close those four, one fixture accident at a
// time, and each was found by attacking the SENTENCE rather than the code.
//
// The CLASS remains open. Further fixture-inert mutations reaching the pristine
// pair have been measured (a first-writer-wins byPath, a public-index bounds
// `break`, a dropped `continue` after onStored). This guard counts two numbers;
// it is not a correctness oracle, and the sentence that said otherwise was the
// most expensive kind of wrong.
//
// The misnaming case, by contrast, is cheap and survives: resetting
// visibleFrom's `seen` memo per entry of `directs` measures (n^2+2n+6, n^2+3)
// -- 2606/2503 at n=50, byte-identical to slPerFile -- and is told "the single
// union traversal has been replaced by a per-visible-file one", which is not
// what happened. The union traversal is intact; the memo is broken. That one
// has no arm naming it correctly: the TestAbsolutizeFieldTypeNames correctness
// arms all pass under it (28 `=== RUN` lines package-wide, 17 top-level funcs
// in this file), and TestUnionSecondLevelMatchesPerFileSecondLevel fires only
// through its vacuity guard, which also does not name the cause.
//
// So the honest statement of the guarantee, in both directions: an unmodelled
// shape lands on `unrecognised` unless it collides with a modelled pair; a
// collision with a non-pristine pair is a wrong diagnosis on a red run; and a
// collision with the PRISTINE pair is a green run on broken code.
func classifySecondLevelCounts(n, visits, emissions int) (secondLevelShape, string) {
	if visits == 0 || emissions == 0 {
		return secondLevelVacuous, "The level produced nothing at all, so every ceiling passes " +
			"vacuously. This is the one state the signatures below cannot describe."
	}
	main, pool := buildSecondLevelFixture(n)
	for _, sig := range secondLevelSignatures {
		if v, e := replaySecondLevel(main, pool, sig.variant); v == visits && e == emissions {
			return sig.shape, sig.detail
		}
	}
	return secondLevelUnknown, "None of the recognised signatures matches, so the traversal " +
		"has changed in some way this classifier does not know. Re-measure every shape from a " +
		"fresh run rather than adjusting one expectation."
}

// PACKAGES REACH TWO LEVELS AND STOP -- the anti-overshoot control.
//
// The sibling test pins that a private import of a VISIBLE dependency
// contributes its package. This pins the other side: a private import of a
// PRIVATE import does not. Without it, "two levels" is asserted only from
// below, and an implementation that recursed the package exposure to any depth
// would satisfy every other arm in this file while diverging from Java on any
// graph three hops deep.
//
// Java's reason is structural, not a depth limit: findSymbol reads this file's
// pool and then each DEPENDENCY's pool -- one hop of indirection. A package
// three levels out sits in a pool nobody in that chain consults.
//
//	main (p.q) --> bridge --private--> mid --private--> deep (package p.q.X)
//
// `deep`'s package is two hops from `bridge`, three from `main`. Java resolves
// `X.Y` past `.p.q` and binds `.p.X.Y`; so must this. Verified against
// protobuf-java 4.29.3 on exactly this shape.
func TestAbsolutizeFieldTypeNamesDoesNotReachPackagesThreeLevelsOut(t *testing.T) {
	t.Parallel()

	deep := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("depth3_deep.proto"),
		Package: proto.String("p.q.X"),
		Syntax:  proto.String("proto2"),
	}
	mid := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("depth3_mid.proto"),
		Package:    proto.String("p.mid"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{deep.GetName()}, // PRIVATE
	}
	bridge := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("depth3_bridge.proto"),
		Package:    proto.String("p.bridge"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{mid.GetName()}, // PRIVATE
	}
	dep := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("depth3_dep.proto"),
		Package: proto.String("p"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:       proto.String("X"),
			NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("Y")}},
		}},
	}
	main := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("depth3_main.proto"),
		Package:    proto.String("p.q"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{dep.GetName(), bridge.GetName()},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Local"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("f"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String("X.Y"),
			}},
		}},
	}

	absolutizeFieldTypeNames(main, dep, bridge, mid, deep)

	if got := main.MessageType[0].Field[0].GetTypeName(); got != ".p.X.Y" {
		t.Fatalf("type_name = %q, want %q.\n"+
			"`deep`'s package `p.q.X` is THREE hops from this file, reachable only through two "+
			"private imports. Java's findSymbol consults each dependency's pool and stops there "+
			"-- one hop -- so that package is invisible and the walk climbs to `.p.X.Y`. "+
			"Answering `.p.q.X.Y` means the package exposure is recursing to arbitrary depth "+
			"instead of stopping at two levels.", got, ".p.X.Y")
	}
}

// THE SAME TWO-LEVEL RULE, THROUGH THE GLOBAL REGISTRY -- the half the stored
// arms cannot see.
//
// walkVisibleImports resolves each import from one of two sources: the stored
// descriptors, or protoregistry.GlobalFiles for the ones stored metadata omits.
// Both branches must expose a privately-imported file's PACKAGE and withhold
// its TYPES, and the sibling arms drive only the stored branch -- leaking types
// through the global branch alone leaves the entire package green.
//
// That is the production-relevant half, not the exotic one:
// `defaultExcludedDependencies` strips exactly the descriptors that then arrive
// through the registry, so real stored metadata reaches this branch on every
// load.
//
// The fixture uses `descriptor.proto`, which is globally registered by the
// generated protobuf runtime and is the file the excluded-dependency path
// actually hits. `main` sits in `google.protobuf` so that a leaked type would
// land in a scope the walk VISITS -- the same placement rule the stored arms
// turn on.
func TestAbsolutizeFieldTypeNamesHidesTypesReachedThroughTheGlobalRegistry(t *testing.T) {
	t.Parallel()

	const descriptorPath = "google/protobuf/descriptor.proto"

	// The premise: the file must really be globally registered, or this arm
	// exercises the registry branch in name only.
	gfd, err := protoregistry.GlobalFiles.FindFileByPath(descriptorPath)
	if err != nil {
		t.Fatalf("%s is not in the global registry (%v), so this arm cannot reach the branch "+
			"it is named for", descriptorPath, err)
	}
	if gfd.Messages().ByName("FileDescriptorProto") == nil {
		t.Fatalf("%s no longer declares FileDescriptorProto, so the probe name is not one this "+
			"file could leak", descriptorPath)
	}
	// THE ENUM AXIS GETS ITS OWN PROBE, with its own premise guard.
	// collectFromFileDescriptor seeds top-level enums through a DIFFERENT arm
	// from messages, so a leak confined to the enum arm is invisible to a
	// message-only fixture. Against the symmetric mutation the two move in
	// lockstep and this adds nothing; it exists for the asymmetric one.
	if gfd.Enums().ByName("Edition") == nil {
		t.Fatalf("%s no longer declares the enum Edition, so the enum probe below is not a name "+
			"this file could leak -- a protobuf-runtime bump has changed what this arm means",
			descriptorPath)
	}

	// A STORED bridge that privately imports the GLOBAL file. The import is
	// private, so `descriptor.proto` is visible to `bridge` and not to `main`;
	// only its package may cross.
	bridge := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("globallevel2_bridge.proto"),
		Package:    proto.String("gl2.bridge"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{descriptorPath},
	}
	main := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("globallevel2_main.proto"),
		Package:    proto.String("google.protobuf"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{bridge.GetName()},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Local"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:   proto.String("f"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					// Declared by descriptor.proto and by nothing visible here.
					TypeName: proto.String("FileDescriptorProto"),
				},
				{
					// The ENUM arm of collectFromFileDescriptor, which seeds
					// through a different loop from messages.
					Name:     proto.String("e"),
					Number:   proto.Int32(2),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
					TypeName: proto.String("Edition"),
				},
			},
		}},
	}

	// REACHABILITY, ASSERTED IN THE SAME INVOCATION, because every assertion
	// below is satisfied by the root fallback -- which is also what an arm that
	// never reaches the global branch produces.
	//
	// That is not hypothetical: this arm shipped once with `bridge` dropped from
	// the pool by an over-broad sed. `main`'s only import was then in no pool at
	// all, the union stayed empty, `descriptor.proto` was never resolved, and
	// both fields fell back to root -- so the arm passed while measuring
	// nothing, and its own header ("leaking types through the global branch
	// alone leaves the entire package green") became a description of itself.
	//
	// The probe walks the same visible set the real call does and requires the
	// globally-registered file to actually appear in it. A dropped `bridge`, or
	// a `main` that stops importing it, now fails HERE rather than passing
	// silently downstream.
	// ONE CALL SITE TAKES THE POOL, which is what actually ties the premise to
	// the invocation. Two weaker forms were tried and both fail:
	//
	//   - an independent probe that supplies `bridge` itself proves nothing --
	//     drop it from the real call and the probe still reports reachable;
	//   - a shared `pool` variable spread into both is no better, because the
	//     spread can be dropped from one of them. Measured: removing `pool...`
	//     from the absolutize call alone leaves this arm GREEN.
	//
	// So the two are behind a single helper. There is now no way to give the
	// walk a dependency the absolutization does not get, short of editing the
	// helper itself -- which is a deliberate act rather than an over-broad sed.
	absolutizeRequiringReach(t, main, descriptorPath, bridge)

	got := main.MessageType[0].Field[0].GetTypeName()
	if got == ".google.protobuf.FileDescriptorProto" {
		t.Fatalf("type_name = %q -- the TYPES of a privately-imported, globally-registered file "+
			"were seeded. Java puts them in that file's own pool, which this file never consults; "+
			"only its PackageDescriptor crosses. The stored-descriptor arms cannot see this: "+
			"leaking here alone leaves them green.", got)
	}
	if got != ".FileDescriptorProto" {
		t.Fatalf("type_name = %q, want %q. Nothing visible to this file declares the name, so "+
			"the walk must run out of scopes and the root fallback must answer.",
			got, ".FileDescriptorProto")
	}

	// The ENUM arm, seeded by a separate loop in collectFromFileDescriptor.
	if gotEnum := main.MessageType[0].Field[1].GetTypeName(); gotEnum != ".Edition" {
		t.Fatalf("enum type_name = %q, want %q -- a top-level ENUM of a privately-imported, "+
			"globally-registered file leaked. Messages and enums are seeded through different "+
			"loops, so a leak confined to the enum arm is invisible to the message probe above.",
			gotEnum, ".Edition")
	}
}

// THE OTHER HALF OF THE SAME SENTENCE: the global branch must EXPOSE a private
// import's package, not merely withhold its types.
//
// The sibling arm above pins the withhold half and is structurally blind to
// this one -- deleting the registry resolution from `importsOf` outright leaves
// it green, measured across the whole package. The reason is analytic, not
// incidental: that fixture puts `main` in `google.protobuf`, so
// `.google.protobuf` is already in `declared` from the file's own package and
// the level-2 contribution cannot appear in any answer it asserts on. Its
// premise guard checks the same registry the code checks, so it can never fail
// while the code's lookup succeeds -- it establishes REGISTRATION, not
// REACHABILITY.
//
// This fixture moves `main` up to package `google` so the contribution becomes
// observable, and writes the probe COMPOUND:
//
//	level-2 seeded:     first component `protobuf` hits `.google.protobuf`
//	                    (seeded from descriptor.proto through bridge)
//	                    -> `.google.protobuf.FileDescriptorProto`
//	not seeded:         the walk climbs past and stops at `.protobuf`, the
//	                    package of the visible stored dep
//	                    -> `.protobuf.FileDescriptorProto`
//
// Two different strings, so this arm reddens when the registry branch stops
// returning a PACKAGE, and stays green when types leak -- disjoint from its
// sibling, one arm per obligation.
//
// WHAT IT DOES NOT FOLLOW, because "the registry branch" is one branch doing
// three separable things: it returns the package (this arm), it enumerates the
// file's DIRECT imports into the level-2 union (covered by
// TestAbsolutizeFieldTypeNamesEnumeratesImportsOfAGloballyRegisteredFile, since
// this fixture's file is a leaf), and it selects the PUBLIC subset for the
// closure walk (covered by nothing -- no file in this binary's registry
// declares a public import, so the path is unreachable from the global side).
// An earlier revision said this arm reddens "exactly when the registry branch
// is removed", which deleting the enumeration alone refutes.
func TestAbsolutizeFieldTypeNamesSeesPackagesReachedThroughTheGlobalRegistry(t *testing.T) {
	t.Parallel()

	const descriptorPath = "google/protobuf/descriptor.proto"
	if _, err := protoregistry.GlobalFiles.FindFileByPath(descriptorPath); err != nil {
		t.Fatalf("%s is not in the global registry (%v)", descriptorPath, err)
	}

	bridge := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("globalpkg_bridge.proto"),
		Package:    proto.String("gp.bridge"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{descriptorPath}, // PRIVATE
	}
	// A visible stored dependency in package `protobuf`.
	//
	// It is NOT required for discrimination, and an earlier revision claimed it
	// was: "without it both answers would be the root fallback". Measured false
	// -- without the sibling the not-seeded answer is still
	// `.protobuf.FileDescriptorProto`, reached by the root fallback rather than
	// by stopping at the sibling's package, so the two answers still differ and
	// the arm still discriminates.
	//
	// What it buys is that the not-seeded answer is a name some visible file
	// could plausibly own, rather than one only the fallback can produce -- so
	// the arm distinguishes "stopped at the wrong scope" from "fell off the end"
	// rather than conflating them. Keeping it for that reason, stated honestly,
	// instead of for the necessity it does not have.
	sibling := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("globalpkg_sibling.proto"),
		Package:     proto.String("protobuf"),
		Syntax:      proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Marker")}},
	}
	main := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("globalpkg_main.proto"),
		Package:    proto.String("google"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{bridge.GetName(), sibling.GetName()},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Local"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("f"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String("protobuf.FileDescriptorProto"),
			}},
		}},
	}

	absolutizeFieldTypeNames(main, bridge, sibling)

	got := main.MessageType[0].Field[0].GetTypeName()
	if got == ".protobuf.FileDescriptorProto" {
		t.Fatalf("type_name = %q -- the level-2 PACKAGE contribution from a globally-registered "+
			"private import is missing, so the walk climbed past `.google.protobuf` and stopped "+
			"at the visible sibling's package instead. Java exposes that package through the "+
			"importing dependency's pool.", got)
	}
	if got != ".google.protobuf.FileDescriptorProto" {
		t.Fatalf("type_name = %q, want %q", got, ".google.protobuf.FileDescriptorProto")
	}
}

// THE GLOBAL BRANCH'S IMPORT ENUMERATION, which both arms above are blind to
// because they use a LEAF.
//
// `importsOf`'s global branch does three separable things: it returns the
// file's package, it enumerates that file's DIRECT imports into the level-2
// union, and it selects the PUBLIC subset for the closure walk. Both sibling
// arms resolve `google/protobuf/descriptor.proto`, which has ZERO imports
// (measured), so for them the second and third do nothing -- deleting the
// direct enumeration outright leaves both green.
//
// This is the production shape, not a contrived one.
// `defaultExcludedDependencies` strips `record_metadata_options.proto` from
// stored metadata, so it arrives through the registry; it is globally
// registered and it HAS three imports, one of them `record_key_expression.proto`
// at package `com.apple.foundationdb.record.expressions`. A schema in
// `com.apple.foundationdb.record` that names a type from that package is
// ordinary record-layer metadata.
//
// With the enumeration working, `expressions` reaches the level-2 union and the
// walk stops at `.com.apple.foundationdb.record.expressions`. Without it, the
// walk climbs past and the root fallback answers `.expressions.KeyExpression` --
// a name nothing declares, so metadata Java accepts stops loading.
//
// NOT COVERED HERE, stated because the positive claim above is narrow: the
// PUBLIC subset of that enumeration. No file in this binary's registry declares
// a public import (measured: 0 of 22), so that path cannot be reached from the
// global side at all and no fixture here exercises it.
func TestAbsolutizeFieldTypeNamesEnumeratesImportsOfAGloballyRegisteredFile(t *testing.T) {
	t.Parallel()

	const optionsPath = "record_metadata_options.proto"
	gfd, err := protoregistry.GlobalFiles.FindFileByPath(optionsPath)
	if err != nil {
		t.Fatalf("%s is not in the global registry (%v)", optionsPath, err)
	}
	// The premise that makes this arm different from its siblings: the file must
	// actually HAVE imports, or it is another leaf and the enumeration is
	// untested again.
	if n := gfd.Imports().Len(); n == 0 {
		t.Fatalf("%s declares no imports, so this arm cannot reach the global branch's import "+
			"enumeration -- which is the only thing it exists to cover", optionsPath)
	}
	var haveExpressions bool
	for i := 0; i < gfd.Imports().Len(); i++ {
		if gfd.Imports().Get(i).Package() == "com.apple.foundationdb.record.expressions" {
			haveExpressions = true
		}
	}
	if !haveExpressions {
		t.Fatalf("%s no longer imports a file in package "+
			"com.apple.foundationdb.record.expressions, so the probe name below is not one this "+
			"graph can produce", optionsPath)
	}

	// DIRECTLY imported, so the globally-registered file is at LEVEL 1 and its
	// own imports are what feed the level-2 union. Behind a bridge it would be
	// a level-2 member instead, contributing only its own package -- which is
	// how the first attempt at this arm failed on correct code.
	main := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("globalenum_main.proto"),
		Package:    proto.String("com.apple.foundationdb.record"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{optionsPath},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Local"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("f"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String("expressions.KeyExpression"),
			}},
		}},
	}

	absolutizeFieldTypeNames(main)

	got := main.MessageType[0].Field[0].GetTypeName()
	if got == ".expressions.KeyExpression" {
		t.Fatalf("type_name = %q -- the global branch stopped enumerating a registered file's "+
			"imports, so `com.apple.foundationdb.record.expressions` never reached the level-2 "+
			"union and the root fallback answered a name nothing declares. Both sibling arms are "+
			"blind to this: they resolve a file with no imports at all.", got)
	}
	if got != ".com.apple.foundationdb.record.expressions.KeyExpression" {
		t.Fatalf("type_name = %q, want %q", got,
			".com.apple.foundationdb.record.expressions.KeyExpression")
	}
}

// THE UNION REWRITE'S EQUIVALENCE, differenced against the form it replaced.
//
// This is the probe whose result was quoted in `walkVisibleImports`' comment
// and then not committed -- the exact failure that comment's own neighbours
// spend paragraphs on. The rewrite's whole safety argument is that one
// traversal over the union of all visible files' directs yields the same SET as
// one traversal per visible file, because public closure distributes over
// union. Nothing in the tree checked that; four hand-written correctness arms
// stay green under the per-file form.
//
// SCOPE: STORED GRAPHS ONLY. The replica collects its visible set from the
// `onStored` callback, so a globally-registered level-1 file would be left out
// of its per-file loop while production enumerates that file's imports. The
// generator emits only `eq_*.proto`, none of which is registered, so the two
// agree here -- and the omission fails SAFE: a registered path would make the
// replica report FEWER packages and redden on correct code, not hide a
// divergence. The global side is covered instead by
// TestAbsolutizeFieldTypeNamesEnumeratesImportsOfAGloballyRegisteredFile.
//
// It also pins the precondition that makes the difference tolerable. The two
// forms are NOT call-for-call identical: the per-file form can reach one file
// from several parents and emit its package once per arrival. That is only safe
// because the sink is idempotent, so the assertion is on the SET and the call
// counts are merely reported.
func TestUnionSecondLevelMatchesPerFileSecondLevel(t *testing.T) {
	t.Parallel()

	// perFileSecondLevel is the form walkVisibleImports replaced, reimplemented
	// here over the same primitives so the two can be differenced. It
	// deliberately mirrors the old shape rather than sharing code with the new
	// one -- a shared helper would make the comparison vacuous.
	perFileSecondLevel := func(
		fd *descriptorpb.FileDescriptorProto,
		pool []*descriptorpb.FileDescriptorProto,
	) (map[string]bool, int) {
		got, calls := map[string]bool{}, 0

		// Level 1, and the fullyExposed set the original also skipped over.
		var visible []*descriptorpb.FileDescriptorProto
		fullyExposed := map[string]bool{}
		walkVisibleImports(fd, pool,
			func(d *descriptorpb.FileDescriptorProto) {
				visible = append(visible, d)
				fullyExposed[d.GetName()] = true
			},
			func(g protoreflect.FileDescriptor) { fullyExposed[g.Path()] = true },
			func(string) {}, nil)

		for _, v := range visible {
			// ONE CLOSURE WALK PER VISIBLE FILE -- the quadratic shape. Driving
			// it through a synthetic file whose imports are v's gives exactly
			// visibleFrom(v's directs) in the level-1 callbacks.
			sub := &descriptorpb.FileDescriptorProto{
				Name:             proto.String("synthetic_" + v.GetName()),
				Dependency:       v.GetDependency(),
				PublicDependency: v.GetPublicDependency(),
			}
			add := func(path, pkg string) {
				if fullyExposed[path] || pkg == "" {
					return
				}
				got[pkg] = true
				calls++
			}
			walkVisibleImports(sub, pool,
				func(d *descriptorpb.FileDescriptorProto) { add(d.GetName(), d.GetPackage()) },
				func(g protoreflect.FileDescriptor) { add(g.Path(), string(g.Package())) },
				func(string) {}, nil)
		}
		return got, calls
	}

	unionSecondLevel := func(
		fd *descriptorpb.FileDescriptorProto,
		pool []*descriptorpb.FileDescriptorProto,
	) (map[string]bool, int) {
		got, calls := map[string]bool{}, 0
		walkVisibleImports(fd, pool,
			func(*descriptorpb.FileDescriptorProto) {},
			func(protoreflect.FileDescriptor) {},
			func(pkg string) {
				got[pkg] = true
				calls++
			}, nil)
		return got, calls
	}

	// A deterministic pseudo-random graph generator. No rand: the seed is the
	// index, so a failure names a reproducible case.
	build := func(seed, n int) (*descriptorpb.FileDescriptorProto, []*descriptorpb.FileDescriptorProto) {
		next := seed*2654435761 + 1
		roll := func(m int) int {
			next = (next*1103515245 + 12345) & 0x7fffffff
			return next % m
		}
		pool := make([]*descriptorpb.FileDescriptorProto, 0, n)
		for i := range n {
			f := &descriptorpb.FileDescriptorProto{
				Name:    proto.String(fmt.Sprintf("eq_%d_%d.proto", seed, i)),
				Package: proto.String(fmt.Sprintf("eq%d.p%d", seed, roll(4))),
				Syntax:  proto.String("proto2"),
			}
			// Imports point at higher indices (a DAG) plus, occasionally, a
			// self- or back-import to exercise the cycle guard.
			for j := 0; j < roll(3); j++ {
				target := roll(n)
				f.Dependency = append(f.Dependency, fmt.Sprintf("eq_%d_%d.proto", seed, target))
			}
			// A public subset, sometimes with a deliberately out-of-range index.
			for k := range f.Dependency {
				if roll(3) == 0 {
					f.PublicDependency = append(f.PublicDependency, int32(k))
				}
			}
			// A dangling path nothing declares.
			if roll(7) == 0 {
				f.Dependency = append(f.Dependency, "eq_missing.proto")
			}
			// MALFORMED PUBLIC INDICES, BOTH BOUNDARIES, AND AFTER THE
			// DEPENDENCY LIST IS FINAL.
			//
			// `+3` alone never produces `idx == len(direct)`, so relaxing the
			// production bound from `>=` to `>` was an index-out-of-range with
			// nothing pinning it: measured, every arm stayed green. `-1` is the
			// same story for the `idx < 0` half.
			//
			// The ORDER is load-bearing and was wrong when this was first
			// added: emitting `len(f.Dependency)` before the dangling append
			// makes it a VALID index on any file that then gets one, so the
			// upper boundary silently stopped being generated for exactly the
			// files that took that branch.
			switch roll(11) {
			case 0:
				f.PublicDependency = append(f.PublicDependency, int32(len(f.Dependency)+3))
			case 1:
				f.PublicDependency = append(f.PublicDependency, int32(len(f.Dependency)))
			case 2:
				f.PublicDependency = append(f.PublicDependency, -1)
			}
			pool = append(pool, f)
		}
		// DETERMINISTIC, REACHABLE boundary cases -- one pair per seed, both
		// placed in main's directs so production's importsOf is called on them
		// on every single graph.
		//
		// The randomized switch above is a coverage LOTTERY: main takes 1-4 of
		// n pool files, so a boundary-bearing descriptor is usually never
		// traversed. Counting those at GENERATION time counted candidates
		// rather than executions -- a roll or seed change could leave every
		// boundary-bearing file unreachable while the counters stayed
		// non-zero and the bounds check went untested.
		//
		// THEY IMPORT NOTHING, and that is load-bearing rather than tidy. The
		// first version gave each a private import, which made these sentinels
		// contribute second-level output and duplicate arrivals of their own --
		// enough that `nonEmpty` and `callDiffs` below stayed healthy at
		// 393/500 with the randomized generation switched off entirely, against
		// 0/500 before they existed. A scaffold that keeps the corpus's own
		// health counters green is a scaffold that has disabled them.
		//
		// Being empty costs nothing: with no dependencies, index 0 IS exactly
		// len(Dependency), so the upper boundary is expressed by the smallest
		// possible descriptor.
		atFile := &descriptorpb.FileDescriptorProto{
			Name:    proto.String(fmt.Sprintf("eq_%d_bound_at.proto", seed)),
			Package: proto.String(fmt.Sprintf("eq%d.boundat", seed)),
			Syntax:  proto.String("proto2"),
		}
		atFile.PublicDependency = []int32{int32(len(atFile.Dependency))}
		belowFile := &descriptorpb.FileDescriptorProto{
			Name:             proto.String(fmt.Sprintf("eq_%d_bound_below.proto", seed)),
			Package:          proto.String(fmt.Sprintf("eq%d.boundbelow", seed)),
			Syntax:           proto.String("proto2"),
			PublicDependency: []int32{-1},
		}
		pool = append(pool, atFile, belowFile)
		main := &descriptorpb.FileDescriptorProto{
			Name:    proto.String(fmt.Sprintf("eq_%d_main.proto", seed)),
			Package: proto.String("eq.main"),
			Syntax:  proto.String("proto2"),
		}
		for j := 0; j < 1+roll(4); j++ {
			main.Dependency = append(main.Dependency,
				fmt.Sprintf("eq_%d_%d.proto", seed, roll(n)))
		}
		// Both boundary files are ALWAYS direct dependencies, so importsOf
		// evaluates their malformed indices on every graph.
		main.Dependency = append(main.Dependency, atFile.GetName(), belowFile.GetName())
		return main, pool
	}

	const graphs = 500
	nonEmpty, callDiffs := 0, 0
	atReached, belowReached := 0, 0
	for seed := range graphs {
		main, pool := build(seed, 6+seed%9)

		// Reachability of the two boundary sentinels, and their boundary
		// PROPERTY, both measured rather than assumed. Matching the name alone
		// fails open twice over: a sentinel that went dangling, or whose index
		// became valid, would leave the count at exactly `graphs` while the
		// boundary it exists for went unexercised.
		byName := make(map[string]*descriptorpb.FileDescriptorProto, len(pool))
		for _, d := range pool {
			byName[d.GetName()] = d
		}
		for _, dep := range main.GetDependency() {
			d, ok := byName[dep]
			if !ok {
				continue
			}
			switch dep {
			case fmt.Sprintf("eq_%d_bound_at.proto", seed):
				for _, idx := range d.GetPublicDependency() {
					if int(idx) == len(d.GetDependency()) {
						atReached++
					}
				}
			case fmt.Sprintf("eq_%d_bound_below.proto", seed):
				for _, idx := range d.GetPublicDependency() {
					if idx < 0 {
						belowReached++
					}
				}
			}
		}

		wantSet, wantCalls := perFileSecondLevel(main, pool)
		gotSet, gotCalls := unionSecondLevel(main, pool)

		if len(gotSet) > 0 {
			nonEmpty++
		}
		if wantCalls != gotCalls {
			callDiffs++
		}
		if len(wantSet) != len(gotSet) {
			t.Fatalf("seed %d: per-file form produced %d packages, union form %d",
				seed, len(wantSet), len(gotSet))
		}
		for p := range wantSet {
			if !gotSet[p] {
				t.Fatalf("seed %d: package %q reached by the per-file form and missed by the "+
					"union form -- the distributivity argument does not hold on this graph", seed, p)
			}
		}
	}

	// THE POPULATION, asserted rather than described: a run where every graph
	// produced an empty second level would compare nothing and still pass.
	if nonEmpty < graphs/4 {
		t.Fatalf("only %d of %d graphs produced a non-empty second-level set, so the comparison "+
			"is mostly vacuous -- the generator no longer builds graphs with private imports "+
			"that reach a second level", nonEmpty, graphs)
	}
	// AND THE CALL-COUNT DIVERGENCE MUST ACTUALLY OCCUR. It is the evidence for
	// the sink-idempotence precondition in walkVisibleImports' comment: if the
	// generator stopped producing repeated arrivals, this test would still pass
	// while quietly ceasing to demonstrate that the two forms differ at all --
	// and the precondition would rest on nothing.
	// BOTH MALFORMED-INDEX BOUNDARIES MUST BE REACHED, not merely generated.
	//
	// These two descriptors are NOT what makes production's
	// `idx < 0 || int(idx) >= len(direct)` reachable, and the earlier claim
	// that they were was measured false in both halves. The randomized switch
	// above already emits an index of exactly len(Dependency) and one of -1,
	// and already reaches them: with this pair removed from main's directs,
	// relaxing `>=` to `>` still panics and so does deleting the `< 0` half.
	// Instrumented over the corpus, the guard evaluates ~15k indices per run
	// and only a minority come from this pair. The sentence that stood here
	// described the corpus as carrying "only an out-of-range +3", which was
	// true before the randomized boundary cases were added and restated in the
	// present tense afterwards -- the same import-a-stale-measurement failure
	// the not-covered list above is repenting of, one paragraph later.
	//
	// What this pair buys is DETERMINISM, which is what it is for: coverage of
	// both boundaries becomes per-graph and structural instead of contingent on
	// a roll schedule and a seed count that nothing pins.
	//
	// WHAT THIS COUNTS IS REACHABILITY, and the chain from there to execution
	// is short but worth writing down: a path in main's directs is visited by
	// visibleFrom, which calls importsOf on it, which is where the bounds check
	// lives. So reachable-from-main entails the guard evaluating that file's
	// indices. Counting at GENERATION time entailed nothing -- main takes 1-4
	// of n pool files, so a randomly-placed boundary is usually never traversed,
	// and a roll or seed change could have left every one of them unreachable
	// while the counters stayed comfortably non-zero.
	//
	// It still does not observe production directly, and does not need to: if
	// the guard is relaxed the corpus arm dies by PANIC inside the seed loop
	// and this assertion is never evaluated. The panic is the detector; this
	// counter only keeps its precondition from evaporating.
	if atReached != graphs || belowReached != graphs {
		t.Fatalf("over %d graphs a file carrying a public index of exactly len(Dependency) was "+
			"reachable from main in %d, and one carrying -1 in %d; both must be every graph or "+
			"production's bounds check is unpinned on that side",
			graphs, atReached, belowReached)
	}
	if callDiffs == 0 {
		t.Fatalf("no graph produced a call-count difference between the two forms, so this test " +
			"no longer evidences the divergence that makes sink idempotence load-bearing -- the " +
			"generator has stopped building graphs that reach one file from several parents")
	}
	t.Logf("%d graphs, %d with a non-empty second level, 0 set mismatches, %d differing in call count",
		graphs, nonEmpty, callDiffs)
}

// absolutizeRequiringReach runs absolutizeFieldTypeNames after proving that the
// closure walk over the SAME inputs actually reaches `required`.
//
// It exists for the WITHHOLD-half arm specifically -- the one asserting that a
// privately-imported registered file's types stay hidden. Its expected answers
// are root-fallback names, which is also what an arm that never reaches the
// global branch produces, so it keeps passing when disarmed. That is how it
// shipped disarmed once.
//
// The EXPOSE-half arm needs no such help and does not use this: its probe is
// compound, so seeded and unseeded give different strings and it arms itself.
// Enumerated rather than characterised, because "the arms that probe the global
// branch all assert root-fallback names" was the earlier wording and is false
// of one of the two.
//
// WHAT THIS DOES NOT TIE. absolutizeFieldTypeNames runs its OWN
// walkVisibleImports, so this helper walks once and the call under test walks
// again: the two share an ARGUMENT, not an EXECUTION. Dropping `pool...` from
// the absolutize line INSIDE this helper leaves every arm green -- measured.
// What it does catch is the regression it was written for, a dependency
// dropped at the caller, because then neither walk receives it.
func absolutizeRequiringReach(
	t *testing.T,
	fd *descriptorpb.FileDescriptorProto,
	required string,
	pool ...*descriptorpb.FileDescriptorProto,
) {
	t.Helper()

	reached := false
	walkVisibleImports(fd, pool,
		func(*descriptorpb.FileDescriptorProto) {},
		func(protoreflect.FileDescriptor) {},
		func(string) {},
		func(path string) {
			if path == required {
				reached = true
			}
		})
	if !reached {
		t.Fatalf("the closure walk over %s's %d supplied dependencies never reached %s, so every "+
			"assertion that follows would be satisfied by the root fallback and the arm would "+
			"pass without exercising the branch it is named for. Check that the bridging "+
			"descriptor is still supplied and that %s still imports it.",
			fd.GetName(), len(pool), required, fd.GetName())
	}

	absolutizeFieldTypeNames(fd, pool...)
}

// EVERY CLASSIFIER ARM, DRIVEN DIRECTLY.
//
// The guard above enters classifySecondLevelCounts once per size and, on
// correct code, only ever receives the pristine pair -- so every other arm is
// unreachable in a normal run and was, until this table, supported by nothing
// but throwaway mutations and prose. That is the same never-asserted-on gap
// that let two earlier versions of this classifier ship telling a real defect
// it was benign.
//
// Each variant's PAIR is measured by replaying it over the committed fixture.
// Each variant's expected SHAPE is written out below instead, and the
// duplication with secondLevelSignatures is the entire point of the arm.
//
// A previous commit deleted this map as "two hand-written statements of one
// mapping" and drove the subtests from the signature table itself. That made
// the comparison circular: classifySecondLevelCounts RETURNS sig.shape, so
// checking it against sig.shape cannot see a wrong variant -> shape
// ASSIGNMENT. Measured consequence -- declaring slEmissionRewalk as
// secondLevelPristine left every test in this file green, and would then have
// let the complexity guard receive a QUADRATIC pair ((n+1)(2n+6), (n+1)(n+3)) and
// report `pristine`. That is a fail-open on the one arm whose whole job is to
// go red.
//
// So the map below is an ORACLE, not a copy. When it and the signature table
// disagree, this arm is supposed to fail; reconciling them is the work, and
// deleting one of them is how the check disappears.
func TestSecondLevelClassifierNamesEveryShape(t *testing.T) {
	t.Parallel()

	want := map[secondLevelVariant]secondLevelShape{
		slPristine:         secondLevelPristine,
		slOnVisitHoist:     secondLevelDedupMoved,
		slDedupMoved:       secondLevelDedupMoved,
		slTruncateFirst:    secondLevelTruncated,
		slTruncateLast:     secondLevelTruncated,
		slUnionDedup:       secondLevelUnionDedup,
		slNoPublic:         secondLevelNoPublic,
		slPerFile:          secondLevelQuadWalk,
		slPerFileSink:      secondLevelQuadSink,
		slTraversalOnly:    secondLevelQuadTravers,
		slEmissionHoist:    secondLevelQuadEmit,
		slEmissionRewalk:   secondLevelQuadRewalk,
		slUnionOverDirects: secondLevelDirectsOnly,
		slEmitBreak:        secondLevelEmitBreak,
	}

	// THE POPULATION, checked before any verdict, and checked as a SET in BOTH
	// directions. Counting is not enough: replacing one entry of
	// secondLevelVariants with a DUPLICATE of another keeps every length equal
	// and every lookup successful, keeps the 4/7/2 tally unchanged -- and
	// silently drops an arm that is then never exercised anywhere in this file.
	// A cardinality check cannot see a set that lost a member and gained a
	// repeat.
	seenVariant := map[secondLevelVariant]bool{}
	for _, v := range secondLevelVariants {
		if seenVariant[v] {
			t.Fatalf("secondLevelVariants lists %q twice. A repeat keeps every length equal while "+
				"some other variant has silently left the population and is now driven by "+
				"nothing", v)
		}
		seenVariant[v] = true
		if _, ok := want[v]; !ok {
			t.Fatalf("variant %q is modelled by replaySecondLevel and listed in "+
				"secondLevelVariants, but this oracle does not say what it should be called, so "+
				"nothing asserts its classification", v)
		}
		if _, ok := secondLevelDeclaredShape(v); !ok {
			t.Fatalf("variant %q has no signature, so the classifier can never name it", v)
		}
	}
	for v := range want {
		if !seenVariant[v] {
			t.Fatalf("the oracle names %q, which is not in secondLevelVariants, so no arm drives "+
				"it and the distinctness check never sees it", v)
		}
	}
	seenSig := map[secondLevelVariant]bool{}
	for _, sig := range secondLevelSignatures {
		if seenSig[sig.variant] {
			t.Fatalf("secondLevelSignatures carries two rows for %q; classify returns the first "+
				"and the second is unreachable", sig.variant)
		}
		seenSig[sig.variant] = true
		if !seenVariant[sig.variant] {
			t.Fatalf("secondLevelSignatures names %q, which is not in secondLevelVariants, so no "+
				"arm drives it", sig.variant)
		}
	}
	if len(want) != len(secondLevelVariants) || len(seenSig) != len(secondLevelVariants) {
		t.Fatalf("%d oracle entries and %d distinct signatures against %d variants",
			len(want), len(seenSig), len(secondLevelVariants))
	}

	// ONLY THE PRISTINE VARIANT MAY CLAIM THE PRISTINE SHAPE, in either table.
	// The guard's sole assertion is `shape == secondLevelPristine`, so any
	// other variant wearing that shape converts the guard from a gate into a
	// rubber stamp for exactly the pair that variant models.
	for _, v := range secondLevelVariants {
		if v == slPristine {
			continue
		}
		if want[v] == secondLevelPristine {
			t.Fatalf("the oracle declares %q as %q; the guard passes on that shape, so a run "+
				"measuring this variant's pair would be waved through", v, secondLevelPristine)
		}
		if s, _ := secondLevelDeclaredShape(v); s == secondLevelPristine {
			t.Fatalf("secondLevelSignatures declares %q as %q; the guard passes on that shape, so "+
				"a run measuring this variant's pair would be waved through", v, secondLevelPristine)
		}
	}

	for _, v := range secondLevelVariants {
		t.Run(string(v), func(t *testing.T) {
			t.Parallel()
			for _, n := range secondLevelSizes {
				main, pool := buildSecondLevelFixture(n)
				gv, ge := replaySecondLevel(main, pool, v)
				got, detail := classifySecondLevelCounts(n, gv, ge)
				if got != want[v] {
					t.Fatalf("n=%d: replaying %q over the fixture gives (visits=%d, emissions=%d), "+
						"classified %q, oracle says %q. Either a signature is assigned the wrong "+
						"shape, or this variant's pair collides with an earlier signature's.\n"+
						"detail: %s", n, v, gv, ge, got, want[v], detail)
				}
				if got != secondLevelPristine && detail == "" {
					t.Fatalf("n=%d: shape %q carries no explanation, so a failing run would name "+
						"the shape and say nothing about it", n, got)
				}
				t.Logf("n=%d %s -> (%d, %d) %s", n, v, gv, ge, got)
			}
		})
	}
}

// THE TWO SHAPES NO VARIANT PRODUCES, driven by construction because there is
// nothing to replay.
func TestSecondLevelClassifierNamesTheUnreplayableShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		visits func(n int) int
		emits  func(n int) int
		want   secondLevelShape
	}{
		{"nothing emitted at all", func(n int) int { return 3*n + 2 }, func(int) int { return 0 }, secondLevelVacuous},
		{"no steps at all", func(int) int { return 0 }, func(int) int { return 0 }, secondLevelVacuous},
		{"a signature nothing recognises", func(n int) int { return 3*n - 7 }, func(n int) int { return n - 3 }, secondLevelUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, n := range secondLevelSizes {
				v, e := tc.visits(n), tc.emits(n)
				got, detail := classifySecondLevelCounts(n, v, e)
				if got != tc.want {
					t.Fatalf("n=%d: classify(visits=%d, emissions=%d) = %q, want %q.\ndetail: %s",
						n, v, e, got, tc.want, detail)
				}
				if detail == "" {
					t.Fatalf("n=%d: shape %q carries no explanation", n, got)
				}
			}
		})
	}
}

// NO TWO SHAPES SHARE A PAIR, at the sizes actually driven.
//
// WHAT THIS DOES NOT ADD, first, because an earlier version of this comment got
// it backwards and offered the mistake as the arm's whole justification. This
// is NOT the only thing that catches a collision, and it is false that "a
// collision makes both rows agree with each other rather than fail":
// classifySecondLevelCounts returns the FIRST signature whose pair matches, so
// on a collision the LATER variant's row in TestSecondLevelClassifierNamesEveryShape
// is handed the earlier one's shape and fails. Measured twice, with a
// benign-meets-defect collision planted in the fixture: that row failed both
// times. Detection is already there.
//
// What this arm adds is DIAGNOSIS and SYMMETRY. The table's failure names one
// row and the shape it got; this names both variants, the pair they share and
// what each was declared to be, and it checks every unordered pair so the
// report does not depend on signature order. It also carries the two things
// nothing else does: the vacuity check on the comparison population, and the
// inflate/reduce/unchanged tally the guard's doc block states.
//
// AGAINST THE DECLARED SHAPE, NEVER THE CLASSIFIER'S ANSWER. classify is a pure
// function of (n, visits, emissions), so two variants that collide are handed
// IDENTICAL arguments and necessarily get identical answers. The first version
// of this test compared those answers to each other -- `pairOf[a] == pairOf[b]
// && shapeOf[a] != shapeOf[b]` -- whose second half is false exactly when the
// first is true. It could not fail for any fixture, any variant set or any
// classifier, and it was written in the commit that removed a different guard
// for that same defect. A pair whose two sides share a derivation cannot be
// checked by comparing them to each other.
//
// Scoped to secondLevelSizes rather than stated generally, and the small-n
// behaviour is PINNED by TestSecondLevelPairsCollideBelowTheDrivenSizes instead
// of described here -- the described version named an n=2 collision that does
// not exist. An earlier version still claimed three sizes made a collision
// impossible "by luck", which is a proof three points cannot give.
func TestSecondLevelShapesArePairwiseDistinct(t *testing.T) {
	t.Parallel()

	type pair struct{ visits, emissions int }

	for _, n := range secondLevelSizes {
		main, pool := buildSecondLevelFixture(n)

		pairOf := map[secondLevelVariant]pair{}
		declared := map[secondLevelVariant]secondLevelShape{}
		for _, v := range secondLevelVariants {
			gv, ge := replaySecondLevel(main, pool, v)
			pairOf[v] = pair{gv, ge}
			s, ok := secondLevelDeclaredShape(v)
			if !ok {
				t.Fatalf("n=%d: variant %q has no signature, so nothing declares what it should "+
					"be diagnosed as and the comparison below has nothing to compare", n, v)
			}
			declared[v] = s
		}

		// THE NON-VACUOUS COMPARISONS, counted before the verdict. If every
		// variant declared the same shape, the loop below would run to
		// completion having tested nothing -- the empty-set green this file
		// keeps having to design out. The expected count is DERIVED from the
		// declared grouping so adding a variant does not strand it.
		byShape := map[secondLevelShape]int{}
		for _, v := range secondLevelVariants {
			byShape[declared[v]]++
		}
		total := len(secondLevelVariants) * (len(secondLevelVariants) - 1) / 2
		same := 0
		for _, k := range byShape {
			same += k * (k - 1) / 2
		}
		wantCross := total - same
		if wantCross == 0 {
			t.Fatalf("n=%d: every variant declares the same shape, so no comparison below can "+
				"fail and this arm proves nothing", n)
		}

		cross := 0
		for i, a := range secondLevelVariants {
			for _, b := range secondLevelVariants[i+1:] {
				if declared[a] == declared[b] {
					continue
				}
				cross++
				if pairOf[a] == pairOf[b] {
					t.Fatalf("n=%d: %q and %q both measure (visits=%d, emissions=%d) but are "+
						"DECLARED %q and %q. classifySecondLevelCounts returns the FIRST "+
						"signature matching a pair, so one of these will be diagnosed as the "+
						"other and its message will name a cause that is not present",
						n, a, b, pairOf[a].visits, pairOf[a].emissions, declared[a], declared[b])
				}
			}
		}
		if cross != wantCross {
			t.Fatalf("n=%d: compared %d differently-declared pairs, expected %d from the declared "+
				"grouping. Both sides derive from the same declared map, so this cannot fail on \n"+
				"DATA -- it guards the skip predicate in the loop above against an edit that \n"+
				"quietly stops comparing things", n, cross, wantCross)
		}

		// THE DOC BLOCK'S TALLY, pinned here so it cannot go stale the way its
		// two predecessors did. Both of those were rewritten by an author
		// re-counting from memory rather than from the fixture, and both were
		// wrong in the commit that claimed to have re-measured everything.
		base := pairOf[slPristine].visits
		inflate, reduce, unchanged := 0, 0, 0
		for _, v := range secondLevelVariants {
			if v == slPristine {
				continue
			}
			switch {
			case pairOf[v].visits > base:
				inflate++
			case pairOf[v].visits < base:
				reduce++
			default:
				unchanged++
			}
		}
		if inflate != 4 || reduce != 7 || unchanged != 2 {
			t.Fatalf("n=%d: of the %d non-pristine variants the step count inflates for %d, "+
				"reduces for %d and is unchanged for %d; the guard's doc block says 4/7/2 and "+
				"must be re-counted", n, len(secondLevelVariants)-1, inflate, reduce, unchanged)
		}
	}
}

// COLLISIONS EXIST BELOW THE DRIVEN SIZES, which is why the arm above is scoped
// rather than stated generally.
//
// Pinned rather than described, because the described version was wrong twice
// over: it named slNoPublic meeting slTruncateFirst at n=2, and they measure
// (6, 3) and (5, 3). This asserts the property that actually matters -- that
// distinctness FAILS down there -- and logs the colliding groups it found, so
// there is no transcribed number left to rot.
//
// The n=1 case is the stark one. The fixture's diamond is gated on n > 2 and
// the chain has no successors, so almost every variant degenerates onto one
// pair, slPristine included: a quadratic per-file walk classifies as `pristine`
// at n=1.
//
// The direction of this guard is deliberate and inverts if the fixture changes.
// It fails when NOTHING collides, because that would mean the scoping caveat on
// the arm above had become unnecessary and should be dropped -- not that
// something broke.
func TestSecondLevelPairsCollideBelowTheDrivenSizes(t *testing.T) {
	t.Parallel()

	type pair struct{ visits, emissions int }

	for _, n := range []int{1, 2} {
		main, pool := buildSecondLevelFixture(n)

		byPair := map[pair][]secondLevelVariant{}
		for _, v := range secondLevelVariants {
			gv, ge := replaySecondLevel(main, pool, v)
			byPair[pair{gv, ge}] = append(byPair[pair{gv, ge}], v)
		}

		crossShape := 0
		for p, vs := range byPair {
			shapes := map[secondLevelShape]bool{}
			for _, v := range vs {
				s, ok := secondLevelDeclaredShape(v)
				if !ok {
					t.Fatalf("n=%d: variant %q has no signature, so this arm cannot tell whether "+
						"its collisions cross a shape boundary", n, v)
				}
				shapes[s] = true
			}
			if len(shapes) > 1 {
				crossShape++
				t.Logf("n=%d: (%d, %d) shared by %d variants declaring %d distinct shapes: %v",
					n, p.visits, p.emissions, len(vs), len(shapes), vs)
			}
		}
		if crossShape == 0 {
			t.Fatalf("n=%d: no two differently-declared variants collide AT THIS SIZE. That is a "+
				"statement about n=%d alone -- other sizes here may still collide -- so replace "+
				"this size with one that does. Only if NO small size collides any more is the "+
				"distinctness arm's scoping caveat what should go", n, n)
		}
	}
}

// THE DIAMOND IS LOAD-BEARING, and this arm measures what it buys rather than
// asserting it in prose.
//
// It exists because the prose version was wrong within its own commit. A
// paragraph at the fixture ties recorded that
// removing the diamond and dropping the matching `+1` from the closed form
// slipped past every arm but the classifier table -- "the ensemble holds there
// by ONE arm". That measurement was taken against the PREVIOUS revision, and
// the same commit that wrote it down had already made it false: the
// declared-shape fix turned the distinctness arm into a second catcher. A
// measurement carried across a change of the instrument it describes is not a
// measurement, and prose is where that goes unnoticed.
//
// What the diamond does: B_0 re-exports the chain TAIL as well as its
// successor, so the tail arrives TWICE inside a single closure walk. That
// repeat survives union de-duplication and does not survive moving the onVisit
// callback, which is the only thing separating a defect (repeat arrivals no
// longer counted) from a behaviour-preserving refactor (union de-duplicated
// before the walk). Remove it and those declared-distinct shapes land on one
// pair.
//
// Direction, since it inverts: this arm fails when the diamond stops mattering.
// If a future fixture separates those shapes some other way, this is the arm to
// re-derive, not to delete.
func TestSecondLevelDiamondIsLoadBearing(t *testing.T) {
	t.Parallel()

	type pair struct{ visits, emissions int }

	// THE NAMED COLLISION, not "some collision". Accepting any cross-shape
	// collision as evidence lets the diamond's actual contribution disappear
	// while an unrelated pair happens to collide, and the arm still passes
	// while its own comment has become false.
	//
	// What the diamond separates is specifically the benign union
	// de-duplication from onVisit moving after the dedup check: those two
	// differ only by the repeat the diamond produces INSIDE a single walk,
	// which survives de-duplicating the union and does not survive moving the
	// callback.
	//
	// slOnVisitHoist is NOT in that pair, and finding out why was this arm
	// earning its keep. It has a SECOND, independent separator: hoisted
	// counting counts once per entry of `directs` and so misses the level-1
	// recursion into A_0's public re-export entirely. That is a property of
	// cg_p rather than of the diamond, so it is asserted separately below --
	// removing the public re-export must show up here, not silently.
	const benign, defect, secondlySeparated = slUnionDedup, slDedupMoved, slOnVisitHoist

	measure := func(main *descriptorpb.FileDescriptorProto, pool []*descriptorpb.FileDescriptorProto, v secondLevelVariant) pair {
		gv, ge := replaySecondLevel(main, pool, v)
		return pair{gv, ge}
	}

	for _, n := range secondLevelSizes {
		onMain, onPool := buildSecondLevelFixtureOpts(n, true)
		offMain, offPool := buildSecondLevelFixtureOpts(n, false)

		if got, other := measure(onMain, onPool, benign), measure(onMain, onPool, defect); got == other {
			t.Fatalf("n=%d: WITH the diamond, %q and %q both measure (visits=%d, emissions=%d). "+
				"The diamond is supposed to be exactly what separates them, so either it is no "+
				"longer doing so or the fixture has changed underneath this arm",
				n, benign, defect, got.visits, got.emissions)
		}

		base, without := measure(offMain, offPool, benign), measure(offMain, offPool, defect)
		if base != without {
			t.Fatalf("n=%d: WITHOUT the diamond, %q measures (visits=%d, emissions=%d) and %q "+
				"measures (visits=%d, emissions=%d) -- they no longer collide, so something "+
				"OTHER than the diamond is now separating them. Find out what, and say so here; "+
				"do not delete this arm",
				n, benign, base.visits, base.emissions, defect, without.visits, without.emissions)
		}

		// The public re-export's independent contribution, pinned so that
		// removing it reddens something.
		if hoisted := measure(offMain, offPool, secondlySeparated); hoisted == base {
			t.Fatalf("n=%d: WITHOUT the diamond, %q now collides with %q at (visits=%d, "+
				"emissions=%d). It used to be separated by A_0's PUBLIC re-export, which hoisted "+
				"counting misses at level 1 -- so that re-export has gone, or stopped mattering",
				n, secondlySeparated, benign, base.visits, base.emissions)
		}

		sBenign, ok1 := secondLevelDeclaredShape(benign)
		sDefect, ok2 := secondLevelDeclaredShape(defect)
		if !ok1 || !ok2 {
			t.Fatalf("n=%d: %q or %q has no signature", n, benign, defect)
		}
		if sBenign == sDefect {
			t.Fatalf("n=%d: %q and %q now declare the same shape (%q), so their collision without "+
				"the diamond would be harmless and this arm is measuring nothing",
				n, benign, defect, sBenign)
		}
		t.Logf("n=%d: without the diamond %q and %q both measure (%d, %d) across declared shapes "+
			"%q and %q; %q stays separate at (%d, %d)",
			n, benign, defect, base.visits, base.emissions, sBenign, sDefect,
			secondlySeparated, measure(offMain, offPool, secondlySeparated).visits,
			measure(offMain, offPool, secondlySeparated).emissions)
	}
}

// THE DRIVEN POPULATION IS ITSELF PINNED.
//
// secondLevelSizes had no arm asserting its VALUE -- every occurrence was a
// `range` or a comment -- so collapsing it to one entry left this whole file
// green. Measured: `[]int{7}` builds, runs the whole file without a failure, and
// silently reduces the evidence to a single point. This file's premise is that
// one point cannot tell a formula in n from a constant.
//
// The floor on n is not cosmetic either. The fixture's diamond is gated on
// n > 2, and TestSecondLevelPairsCollideBelowTheDrivenSizes measures
// declared-distinct shapes landing on one pair at n <= 2 -- so driving such a
// size here would redden the distinctness arm for a property of the fixture
// rather than a regression, which is the most expensive kind of false alarm.
func TestSecondLevelSizesArePinned(t *testing.T) {
	t.Parallel()

	if len(secondLevelSizes) < 3 {
		t.Fatalf("secondLevelSizes has %d entries (%v); fewer than three points cannot separate "+
			"a formula in n from a constant, which is the premise every arm in this file rests "+
			"on", len(secondLevelSizes), secondLevelSizes)
	}
	seen := map[int]bool{}
	for _, n := range secondLevelSizes {
		if seen[n] {
			t.Fatalf("secondLevelSizes repeats %d (%v); a repeated size adds a run, not a point",
				n, secondLevelSizes)
		}
		seen[n] = true
		if n <= 2 {
			t.Fatalf("secondLevelSizes contains %d (%v), at or below the range where "+
				"declared-distinct shapes provably collide on one pair. Driving it would redden "+
				"the distinctness arm for a property of the fixture rather than a regression",
				n, secondLevelSizes)
		}
	}
}

// THE DIAGNOSES THEMSELVES, which nothing else looks at.
//
// The classifier arm asserts a signature's SHAPE and that its detail is
// non-empty; it never compares the detail. So a detail pasted onto the wrong
// row -- the likeliest edit here, since several are near-identical prose -- is
// invisible, and the detail is the entire value of a failing run: the shape
// names what happened, the detail says what to go and look at.
//
// Two properties, both derivable, so neither adds a copy to keep in sync:
// variants that declare the SAME shape must carry the SAME detail (they are one
// diagnosis reached two ways), and variants declaring DIFFERENT shapes must
// carry different details (otherwise one of the two is describing the other's
// cause).
func TestSecondLevelDetailsMatchTheirShapes(t *testing.T) {
	t.Parallel()

	detailFor := map[secondLevelShape]string{}
	sourceFor := map[secondLevelShape]secondLevelVariant{}
	for _, sig := range secondLevelSignatures {
		if sig.shape != secondLevelPristine && sig.detail == "" {
			t.Fatalf("%q declares shape %q with an empty diagnosis, so a run that measures its "+
				"pair would name the shape and say nothing about it", sig.variant, sig.shape)
		}
		prev, seen := detailFor[sig.shape]
		if seen && prev != sig.detail {
			t.Fatalf("%q and %q both declare shape %q but carry DIFFERENT diagnoses. One shape is "+
				"one conclusion; if these two really need different text they need different "+
				"shapes", sourceFor[sig.shape], sig.variant, sig.shape)
		}
		detailFor[sig.shape] = sig.detail
		sourceFor[sig.shape] = sig.variant
	}

	byDetail := map[string]secondLevelShape{}
	for shape, detail := range detailFor {
		if detail == "" {
			continue
		}
		if other, dup := byDetail[detail]; dup {
			t.Fatalf("shapes %q and %q carry the SAME diagnosis, so one of them is describing the "+
				"other's cause and a failing run points the reader at the wrong code",
				other, shape)
		}
		byDetail[detail] = shape
	}

	// EACH DIAGNOSIS BOUND TO ITS OWN SHAPE, which uniqueness alone cannot do.
	//
	// The two comparisons above are about CARDINALITY: same shape means one
	// diagnosis, different shapes mean different diagnoses. Both survive a
	// straight SWAP of two shapes' texts -- every detail stays non-empty and
	// every detail stays unique, while a failing run sends the reader to the
	// opposite cause.
	//
	// So each shape names a phrase its diagnosis must contain, keyed by shape
	// rather than by variant. These are deliberately the phrase that identifies
	// the CAUSE, not a word from the shape's own name, so the check is not
	// satisfied by echoing the label back.
	mustMention := map[secondLevelShape]string{
		secondLevelDedupMoved:  "dedup check",
		secondLevelTruncated:   "TRUNCATED",
		secondLevelUnionDedup:  "BEHAVIOUR-PRESERVING",
		secondLevelNoPublic:    "public-import recursion in visibleFrom is gone",
		secondLevelQuadWalk:    "single union traversal has been replaced",
		secondLevelQuadSink:    "absorbed by the SINK",
		secondLevelQuadTravers: "final walk then de-duplicates",
		secondLevelQuadEmit:    "already-walked set",
		secondLevelQuadRewalk:  "re-walking it every",
		secondLevelDirectsOnly: "public re-export are dropped",
		secondLevelEmitBreak:   "stops at the first already-exposed",
	}
	for shape, detail := range detailFor {
		if shape == secondLevelPristine {
			continue
		}
		phrase, ok := mustMention[shape]
		if !ok {
			t.Fatalf("shape %q has no required phrase, so its diagnosis is bound to it by nothing "+
				"and could be swapped with another shape's undetected", shape)
		}
		if !strings.Contains(detail, phrase) {
			t.Fatalf("the diagnosis carried for shape %q does not mention %q. Either the texts "+
				"have been swapped between shapes -- which uniqueness alone cannot see -- or the "+
				"diagnosis was reworded and this binding needs re-deriving from the new text",
				shape, phrase)
		}
	}
	// And the binding must not be satisfiable by more than one shape's text,
	// or a swap could still pass.
	for shape, phrase := range mustMention {
		hits := 0
		for other, detail := range detailFor {
			if other != secondLevelPristine && strings.Contains(detail, phrase) {
				hits++
			}
		}
		if hits != 1 {
			t.Fatalf("the phrase required of shape %q appears in %d diagnoses, not exactly one; "+
				"a phrase two shapes share cannot tell them apart after a swap", shape, hits)
		}
	}
	// The population, so a signature table emptied by an edit cannot pass this
	// arm by having nothing to compare.
	if len(detailFor) < 2 {
		t.Fatalf("only %d distinct shapes carry diagnoses; with fewer than two there is nothing "+
			"for either comparison above to separate", len(detailFor))
	}
}

// THE EXPECTED PAIR COMES FROM THE FIXTURE, and this is the arm that says so.
//
// The previous version did not, and its name was the whole of its claim. It
// asserted secondLevelExpected(n) == 3n+cgUniqueLeaves against a function
// DEFINED as n + (n+cgUniqueLeaves) + (n-1) + 1 -- the same expression on both
// sides. It called neither the fixture builder nor walkVisibleImports and could
// not fail for any fixture whatsoever. Three separate counterexamples reached
// it: moving cg_y onto a middle A, moving BOTH leaves onto A_0, and adding a
// third leaf on A_1. The first two left this test, the guard and every
// classifier row green while a union truncated to the FIRST visible file was
// diagnosed "behaviour-preserving" -- the exact defect the fixture edit before
// it had been made to fix.
//
// Six ties now, and the last is the one that matters:
//
//   - the closed form equals what PRODUCTION counts;
//   - the closed form equals what the MODEL counts, which is the only point at
//     which replaySecondLevel is tied to walkVisibleImports at all;
//   - cgUniqueLeaves equals the number of singly-imported non-visible leaves in
//     the built pool, so the constant cannot drift from the graph in either
//     direction;
//   - cgPackagelessLeaves equals the number of package-less, singly-imported,
//     NON-VISIBLE leaves in that pool -- the only route this fixture has to
//     the emission loop's empty-package guard, and checked with the same
//     visibility rule production applies before reaching it;
//   - cgPublicReexports and cgBehindPublic are counted SEPARATELY out of that
//     same pool. secondLevelExpected adds them together in the union term, so
//     that term alone cannot tell them apart; only counting them apart can.
//     While both are 1 they stay numerically interchangeable and a swap of the
//     two NAMES remains uncatchable -- what this ties is a fixture change
//     moving either count;
//   - each truncation direction loses the SAME number of emissions, derived as
//     (cgUniqueLeaves-1) + cgBehindPublic, and the FIRST and LAST directs are
//     each confirmed to carry a unique non-visible leaf. The count cannot say
//     that and neither can symmetry alone: moving both leaves inward keeps the
//     count and keeps the deficits equal, so the endpoints are inspected
//     directly.
//
// WHAT THE SIX TIES DO NOT CATCH. An edit that removes the diamond AND drops
// the matching `+1` from secondLevelExpected passes all six, and passes the
// guard at 155/305/605, because it is self-consistent: the closed form is
// documentation rather than an independent witness, and an author who edits
// both has desynchronised nothing. What it silently spends is the diamond's
// separating power.
//
// THREE other arms catch it -- the classifier table, the distinctness arm and
// TestSecondLevelDiamondIsLoadBearing -- and this count is re-measured rather
// than carried, because this paragraph has now been wrong twice. Its first
// version said the ensemble held "by ONE arm", a figure taken from a mutation
// run against the PREVIOUS revision and copied in by the very commit that
// falsified it. Its second said TWO, correct when written and made stale by
// the fixture change one commit later. The property itself is pinned by
// TestSecondLevelDiamondIsLoadBearing, which measures what removing the diamond
// costs instead of describing it; this paragraph is the pointer, and every
// number in it is a fact about a fixture that has moved twice.
func TestSecondLevelExpectedMatchesTheFixture(t *testing.T) {
	t.Parallel()

	for _, n := range secondLevelSizes {
		wantV, wantE := secondLevelExpected(n)
		main, pool := buildSecondLevelFixture(n)

		// The package-less leaf, counted out of the pool BEFORE the derived
		// comparisons, because it is a structural fact about the fixture
		// rather than a consequence of one.
		//
		// The count requires package-less AND singly-imported AND NOT VISIBLE,
		// which is what production requires before it ever evaluates
		// `pkg != ""`: a publicly re-exported file is fullyExposed and skipped
		// one line earlier. Counting on directness alone -- the first version
		// of this -- would keep returning 1 after the leaf was made public,
		// i.e. after its only route to the guard had disappeared.
		//
		// cg_n is the only package-less file in the pool, so with it excluded
		// nothing reaches that guard at all, and dropping the guard leaves the
		// whole file green at the PRISTINE pair. Measured, before cg_n existed.
		if pl := secondLevelPackagelessLeaves(main, pool); pl != cgPackagelessLeaves {
			t.Fatalf("n=%d: the built pool has %d package-less singly-imported non-visible "+
				"leaf/leaves; cgPackagelessLeaves says %d. With none, the emission loop's "+
				"empty-package guard is unreached and removing it is a green fail-open",
				n, pl, cgPackagelessLeaves)
		}
		if gotV, gotE := measureSecondLevel(n); gotV != wantV || gotE != wantE {
			t.Fatalf("n=%d: production walked (visits=%d, emissions=%d); secondLevelExpected says "+
				"(%d, %d). The fixture and the closed form have diverged -- re-derive the closed "+
				"form from the fixture rather than adjusting it to match",
				n, gotV, gotE, wantV, wantE)
		}

		if v, e := replaySecondLevel(main, pool, slPristine); v != wantV || e != wantE {
			t.Fatalf("n=%d: the model's pristine replay is (%d, %d) while production and the "+
				"closed form agree on (%d, %d). This is the ONLY point replaySecondLevel is tied "+
				"to walkVisibleImports, so every classifier row is now measuring a walk "+
				"production does not do", n, v, e, wantV, wantE)
		}

		// The public re-export's two quantities, counted SEPARATELY out of the
		// pool. secondLevelExpected adds cgPublicReexports and cgBehindPublic
		// together in the union term, so that term alone cannot tell them apart;
		// emissions uses cgBehindPublic on its own, which is the asymmetry this
		// ties each to a counted quantity. While both are 1 they remain
		// numerically interchangeable and nothing here can separate them -- what
		// this catches is a FIXTURE change that moves either count.
		if re, behind := secondLevelPublicShape(main, pool); re != cgPublicReexports || behind != cgBehindPublic {
			t.Fatalf("n=%d: the built pool has %d publicly re-exported non-direct file(s) and %d "+
				"non-visible file(s) behind them; cgPublicReexports says %d and cgBehindPublic "+
				"says %d. These two are equal in value and would otherwise be interchangeable",
				n, re, behind, cgPublicReexports, cgBehindPublic)
		}
		got, carriers := secondLevelUniqueLeaves(main, pool)
		if got != cgUniqueLeaves {
			t.Fatalf("n=%d: the built pool has %d leaves imported by exactly one A; "+
				"cgUniqueLeaves says %d", n, got, cgUniqueLeaves)
		}

		// PLACEMENT, checked directly against the built pool rather than
		// inferred from any count.
		//
		// The truncation arm's diagnosis claims a unique leaf sits on the FIRST
		// visible file and another on the LAST. A count cannot say that, and
		// neither can a symmetry check: moving cg_x inward to A_1 and cg_y
		// inward to A_{n-2} keeps the count at cgUniqueLeaves AND keeps both
		// truncation deficits equal, so both pass while the claim is false.
		// Symmetric drift is the case an equality test is structurally blind
		// to, which is why the endpoints are inspected instead.
		deps := main.GetDependency()
		if len(deps) < 2 {
			t.Fatalf("n=%d: the fixture has %d directs; the endpoint claim needs at least two",
				n, len(deps))
		}
		for label, end := range map[string]string{"FIRST": deps[0], "LAST": deps[len(deps)-1]} {
			if !carriers[end] {
				t.Fatalf("n=%d: the %s visible file (%s) imports no unique non-visible leaf. The "+
					"truncation diagnosis says a truncation loses one whichever end it keeps, and "+
					"that is now false for this end -- a leaf has drifted inward", n, label, end)
			}
		}

		// And the cost of a truncation, DERIVED. Keeping one end retains that
		// end's leaf and loses the other (cgUniqueLeaves-1), and loses whatever
		// sits behind the public re-export, since a single file's directs
		// cannot reach it (cgBehindPublic).
		wantDeficit := (cgUniqueLeaves - 1) + cgBehindPublic
		for _, dir := range []secondLevelVariant{slTruncateFirst, slTruncateLast} {
			_, e := replaySecondLevel(main, pool, dir)
			if got := wantE - e; got != wantDeficit {
				t.Fatalf("n=%d: %s emits %d against pristine's %d, a deficit of %d; the fixture's "+
					"structure predicts %d = (cgUniqueLeaves-1) + cgBehindPublic. Re-derive from "+
					"the fixture rather than adjusting this expectation",
					n, dir, e, wantE, got, wantDeficit)
			}
		}
	}
}
