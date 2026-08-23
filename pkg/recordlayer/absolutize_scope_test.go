package recordlayer

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
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
				"import of a private import and which Java's DescriptorPool never sees. Both "+
				"names resolve, so this binds the wrong message with no error.",
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
// THE CONTROL is the second arm: the same graph with hidden's message moved so
// that only its package is in play cannot distinguish "package seeded" from
// "types seeded". So the arms assert opposite outcomes on the SAME graph, one
// about the package reaching two levels and one about the types stopping at
// one.
func TestAbsolutizeFieldTypeNamesSeesPackagesOfPrivateImportsButNotTheirTypes(t *testing.T) {
	t.Parallel()

	build := func() (main, dep, bridge, hidden *descriptorpb.FileDescriptorProto) {
		hidden = &descriptorpb.FileDescriptorProto{
			Name:    proto.String("pkgshadow_hidden.proto"),
			Package: proto.String("p.q.X"),
			Syntax:  proto.String("proto2"),
			// A MESSAGE that must stay invisible, in a package that must not.
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Y")}},
		}
		bridge = &descriptorpb.FileDescriptorProto{
			Name:       proto.String("pkgshadow_bridge.proto"),
			Package:    proto.String("p.bridge"),
			Syntax:     proto.String("proto2"),
			Dependency: []string{hidden.GetName()}, // PRIVATE
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

	t.Run("the package shadows", func(t *testing.T) {
		t.Parallel()

		main, dep, bridge, hidden := build()
		absolutizeFieldTypeNames(main, dep, bridge, hidden)

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

		// The discriminator: if `hidden`'s TYPES were seeded rather than only its
		// package, `.p.q.X.Y` would be a resolvable message and protodesc would
		// accept the rewritten descriptor. It must not -- Java rejects here, and
		// the whole point of the package/type split is that the name resolves to
		// a package with nothing beneath it.
		main, dep, bridge, hidden := build()
		absolutizeFieldTypeNames(main, dep, bridge, hidden)

		files := &protoregistry.Files{}
		for _, f := range []*descriptorpb.FileDescriptorProto{hidden, bridge, dep} {
			fdesc, err := protodesc.NewFile(f, files)
			if err != nil {
				t.Fatalf("building %s: %v", f.GetName(), err)
			}
			if err := files.RegisterFile(fdesc); err != nil {
				t.Fatalf("registering %s: %v", f.GetName(), err)
			}
		}
		if _, err := protodesc.NewFile(main, files); err == nil {
			t.Fatal("protodesc accepted `.p.q.X.Y`. Java rejects this descriptor: the first " +
				"component resolves to a PACKAGE and there is no `Y` beneath it. Accepting means " +
				"the private import's types leaked in alongside its package.")
		}
	})
}
