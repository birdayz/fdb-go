package recordlayer

import (
	"fmt"
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
// the onVisit seam. That is the quantity three of the five known shapes inflate and two REDUCE,
// but it is not "work" in general:
//
//   - Cost inside a single step is invisible. A change making importsOf itself
//     expensive keeps the step count at 3n and passes here.
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
//     excess over pristine is 2450 closure steps, so the first exemplar is
//     comparable and the second an order of magnitude larger. Neither is
//     visible to either counter, and TotalAlloc is byte-identical between pristine
//     and mutant for the first (an absolute figure is omitted deliberately: it moves
//     with the measuring harness, and the load-bearing fact is the EQUALITY). That
//     is one reason an allocation assertion is not the answer.
//
//   - CODE THIS FIXTURE NEVER EXECUTES, which is a different class from the
//     three above and the easiest to miss, because the list otherwise reads as
//     a partition of the function. Every path here is a STORED descriptor: the
//     global-registry arm of importsOf and the onGlobal arm of the level-1 loop
//     run ZERO times at every n. Measured consequence: replacing
//     GlobalFiles.FindFileByPath with a linear RangeFiles scan -- unbounded
//     per-lookup cost, the textbook "you don't need the index" simplification
//     -- leaves this arm and every correctness arm green. The registry path is
//     production-hot (tuple_fields.proto always arrives that way), so this is
//     not a hypothetical region.
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
// So the guard counts closure STEPS instead, and separates by construction. `main`
// privately imports A_0..A_{n-1}; each A_i privately imports the head of a
// public chain B_0 -> ... -> B_{n-1}. The As have no public dependencies, so
// the visible set is exactly the As, and the union of their directs is B_0
// repeated n times:
//
//	union form     dedups via `seen`, walking the B chain once     -> 3n+1 visits
//	per-file form  calls visibleFrom per A_i, re-walking B each  -> n^2 calls
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
// fixture, and repeated invocations return them identically -- 150/300/600 and
// n, measured across six separate runs. A load figure would be describing a
// sensitivity the quantity does not have.
func TestSecondPackageLevelClosureWalkTakesLinearSteps(t *testing.T) {
	t.Parallel()

	build := func(n int) (*descriptorpb.FileDescriptorProto, []*descriptorpb.FileDescriptorProto) {
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
			if i == 0 && n > 2 {
				b.Dependency = append(b.Dependency, fmt.Sprintf("cg_b_%d.proto", n-1))
				b.PublicDependency = append(b.PublicDependency, 1)
			}
			pool = append(pool, b)
		}
		// The unique leaf A_0 imports. A leaf, so it adds exactly one arrival.
		pool = append(pool, &descriptorpb.FileDescriptorProto{
			Name:    proto.String("cg_x.proto"),
			Package: proto.String("cg.x"),
			Syntax:  proto.String("proto2"),
		})

		var mainDeps []string
		for i := range n {
			a := &descriptorpb.FileDescriptorProto{
				Name:       proto.String(fmt.Sprintf("cg_a_%d.proto", i)),
				Package:    proto.String(fmt.Sprintf("cg.a%d", i)),
				Syntax:     proto.String("proto2"),
				Dependency: []string{"cg_b_0.proto"}, // PRIVATE: stops the closure
			}
			// A_0 ALONE carries an extra private import, which is what
			// separates a de-duplicated union from a TRUNCATED one.
			//
			// Both reduce the arrival count to roughly 2n, and one is benign
			// while the other silently drops every visible file's contribution
			// but the last. With every A importing the same head they are
			// indistinguishable; giving the FIRST a unique import means a
			// truncation that keeps only the last file's directs loses it,
			// while de-duplication keeps it.
			if i == 0 {
				a.Dependency = append(a.Dependency, "cg_x.proto")
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
	// emission-counting guard reporting exactly 50/100/200 and passing, which
	// is a gate inert on a shape inside its own claim.
	//
	// Counting closure steps sees both regressions, because both do the extra
	// walking whatever they do with the result.
	count := func(n int) (visits, emissions int) {
		main, pool := build(n)
		walkVisibleImports(main, pool,
			func(*descriptorpb.FileDescriptorProto) {},
			func(protoreflect.FileDescriptor) {},
			func(string) { emissions++ },
			func(string) { visits++ },
		)
		return visits, emissions
	}

	for _, n := range []int{50, 100, 200} {
		visits, emissions := count(n)
		// VACUITY ONLY. These guard the one case the classification below cannot
		// describe -- a level that produced nothing at all, which satisfies any
		// upper bound and makes every ceiling meaningless.
		//
		// They deliberately do NOT approximate the expected value. An earlier
		// version used `< n+1`, which caught a TRUNCATION defect first and
		// reported it as "the level is not being reached" -- a wrong diagnosis
		// for a level that is reached and then silently drops most of its
		// input. Anything non-zero belongs to the classification, which knows
		// the shapes by name.
		if emissions == 0 {
			t.Fatalf("n=%d: the second package level emitted nothing at all, so every ceiling "+
				"below passes vacuously -- the level is not being reached", n)
		}
		if visits == 0 {
			t.Fatalf("n=%d: the closure walk took no steps at all, so its ceiling passes "+
				"vacuously -- the walk is not running", n)
		}
		// AN EXACT EQUALITY, not a range, because the quantity is deterministic
		// and a range is too coarse to see the one thing that makes the
		// instrumentation trustworthy.
		//
		// onVisit is called BEFORE the dedup check, so a repeated arrival at an
		// already-seen file counts. That placement is what lets the counter see
		// a traversal-quadratic regression at all -- and moving it BELOW the
		// check leaves this fixture at exactly 2n, which sits comfortably inside
		// any n..6n band. The band would have reported the instrumentation
		// working while it had been silently narrowed to distinct arrivals.
		//
		// So the arm pins the value. Pristine is 3n: one arrival per A, one per
		// B along the chain, plus the diamond's second arrival at the tail --
		// and n-1 REPEATED arrivals at B_0 from A_1..A_{n-1}, which is the
		// pre-dedup behaviour under test.
		//
		// Every total below was RE-MEASURED after the diamond was added. It
		// shifted all of them: the per-file form went n^2+n -> n^2+2n and the
		// traversal-only form 2n^2+2n-1 -> 2n^2+3n, because the extra edge adds
		// one arrival to every closure. Changing a fixture invalidates every
		// number derived from it, and these are the numbers a failure prints.
		// FIVE SHAPES, SEPARATED BY THE PAIR (visits, emissions) -- every value
		// below re-measured in one run after the fixture last changed, because
		// editing the fixture expires every number derived from it and doing
		// that piecemeal has stranded these constants twice.
		//
		//	visits    emissions   shape
		//	3n+1      n+1         pristine
		//	2n+1      n+1         onVisit moved after the dedup check    DEFECT
		//	2n+2      n+1         `union` de-duplicated before the walk  benign
		//	2n+1      n           `union` truncated to the last file     DEFECT
		//	2n^2+3n+2 n+1         traversal-only per-file union build    DEFECT
		//	n^2+2n    n^2         per-file closure walk                  DEFECT
		//
		// NEITHER COUNTER SEPARATES THESE ALONE, which is why both are asserted
		// and why the classification reads the pair. The dedup-move and the
		// truncation both land on 2n+1 visits and differ only in emissions; the
		// truncation and the de-duplication differ only by one visit. An earlier
		// version keyed on visits alone and told a real truncation defect that
		// it was a benign refactor whose expectation was merely stale.
		if wantV, wantE := 3*n+1, n+1; visits != wantV || emissions != wantE {
			switch {
			case visits == 2*n+1 && emissions == wantE:
				t.Fatalf("n=%d: visits=%d (2n+1), emissions=%d. onVisit is being called AFTER the "+
					"dedup check, so repeated arrivals are no longer counted and the counter has "+
					"gone blind to a traversal-only regression, which is the shape it exists to "+
					"catch.", n, visits, emissions)
			case visits == 2*n+1 && emissions < wantE:
				t.Fatalf("n=%d: visits=%d (2n+1), emissions=%d (want %d). `union` is being "+
					"TRUNCATED rather than accumulated -- only the last visible file's imports "+
					"reach the second level, so every other visible file's package contribution "+
					"is silently dropped. Note this is one visit away from the benign "+
					"de-duplication refactor and identical to it in visits; the emission count is "+
					"what tells them apart.", n, visits, emissions, wantE)
			case visits == 2*n+2 && emissions == wantE:
				t.Fatalf("n=%d: visits=%d (2n+2), emissions=%d. That is `union` de-duplicated "+
					"before visibleFrom -- a BEHAVIOUR-PRESERVING refactor, since visibleFrom "+
					"already set-dedups, and still linear. Nothing is broken; the expectation is "+
					"stale. Re-derive it deliberately to %d rather than widening this check.",
					n, visits, emissions, visits)
			case visits >= n*n || emissions >= n*n:
				t.Fatalf("n=%d: visits=%d emissions=%d, want %d and %d. The per-file form is %d "+
					"visits / %d emissions and the traversal-only form %d visits. The single "+
					"union traversal has been replaced by a per-visible-file one, which is "+
					"O(n^2) here and O(n^3) across rebuildFileDescriptor's per-dependency pass "+
					"-- valid metadata becomes a CPU exhaustion input.",
					n, visits, emissions, wantV, wantE, n*n+2*n, n*n, 2*n*n+3*n+2)
			default:
				t.Fatalf("n=%d: visits=%d emissions=%d, want exactly %d and %d. None of the five "+
					"known shapes matches, so the traversal has changed in some other way -- "+
					"re-derive these expectations from a fresh measurement of every shape rather "+
					"than adjusting one of them.", n, visits, emissions, wantV, wantE)
			}
		}
		// AN EMISSIONS CEILING TOO, and the reasoning that once deleted it was
		// wrong in a way worth recording.
		//
		// The deletion argued that onPackageOnly fires at most once per DISTINCT
		// file reached while onVisit fires on every arrival, so emissions <=
		// visits "by construction" and no mutation could push one past a bound
		// without pushing the other -- hence no unique detector could exist.
		//
		// That quantified over all mutations a property of the CURRENT code.
		// Counterexample, measured: hoist the closure walk out and nest the
		// emission loop inside `visible` --
		//
		//	walked := visibleFrom(union)
		//	for range visible { for _, q := range walked { ...onPackageOnly... } }
		//
		// -- and visits stay at pristine 150/300/600 while emissions become
		// exactly n^2 (2500/10000/40000). One traversal, n^2 emissions. The
		// whole package stays green, because addPackage is idempotent so no
		// correctness arm can see it -- the same idempotence that hides the
		// traversal-only shape.
		//
		// So the two quantities are independent under mutation even though one
		// bounds the other today, and the ceiling has a unique detector after
		// all.
		//
		// Pristine emissions are exactly n, so this bar allows a 6x increase
		// before firing. (It is not "the same headroom as the visits bar" --
		// there is no visits bar any more, that check is now an exact equality.)
		// Both quadratic shapes are n times the bar rather than a constant
		// factor over it: n^2 emissions against 6n is n/6, so the margin grows
		// with n instead of needing re-tuning.
		if emissions > 6*n {
			t.Fatalf("n=%d: the second package level emitted %d packages, more than 6n=%d "+
				"(pristine is exactly n=%d). Emissions have gone quadratic while the closure walk "+
				"has not, so the level is re-emitting over an already-walked set -- idempotent "+
				"sink, invisible to every correctness arm, and still O(n^2) work.",
				n, emissions, 6*n, n)
		}
		//
		// The emissions FLOOR above has its own detector: a walk that ran but
		// found every reached file already fullyExposed emits zero while
		// visiting n.
		t.Logf("n=%d visits=%d emissions=%d (linear %d, quadratic ~%d)", n, visits, emissions, n, n*n)
	}
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
			if roll(11) == 0 {
				f.PublicDependency = append(f.PublicDependency, int32(len(f.Dependency)+3))
			}
			// A dangling path nothing declares.
			if roll(7) == 0 {
				f.Dependency = append(f.Dependency, "eq_missing.proto")
			}
			pool = append(pool, f)
		}
		main := &descriptorpb.FileDescriptorProto{
			Name:    proto.String(fmt.Sprintf("eq_%d_main.proto", seed)),
			Package: proto.String("eq.main"),
			Syntax:  proto.String("proto2"),
		}
		for j := 0; j < 1+roll(4); j++ {
			main.Dependency = append(main.Dependency,
				fmt.Sprintf("eq_%d_%d.proto", seed, roll(n)))
		}
		return main, pool
	}

	const graphs = 500
	nonEmpty, callDiffs := 0, 0
	for seed := range graphs {
		main, pool := build(seed, 6+seed%9)

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
