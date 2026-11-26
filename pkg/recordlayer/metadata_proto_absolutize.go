// Portions derived from FoundationDB Record Layer (Index.java,
// RecordMetaDataBuilder.java, LiteralKeyExpression.java, FormerIndex.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// AbsolutizeFieldTypeNames is the exported form of
// absolutizeFieldTypeNames, for builders that retain a Java-shaped
// (relative-name) proto but need a protodesc-buildable clone.
//
// IT TAKES NO DEPENDENCY SET, and that is deliberate rather than an oversight
// mirroring the unexported form's variadic tail -- the question is worth
// answering here because the sole caller
// (internal/relational/metadata's buildFileDescriptor) holds two
// protoreflect.FileDescriptors two lines before calling this and visibly does
// not pass them.
//
// It does not need to. That caller sets fd.Dependency to
// `tuple_fields.proto` and `record_metadata_options.proto`, both of which are
// generated Go and therefore in protoregistry.GlobalFiles, so
// absolutizeFieldTypeNames resolves and seeds them through the global-registry
// loop -- the same route stored metadata takes for the dependencies
// defaultExcludedDependencies strips. Threading them in explicitly as well
// would seed the identical symbols twice.
//
// A caller whose dependencies are NOT globally registered must use the
// unexported form and pass them, or the walk will not see their types.
func AbsolutizeFieldTypeNames(fd *descriptorpb.FileDescriptorProto) {
	absolutizeFieldTypeNames(fd)
}

// absolutizeFieldTypeNames rewrites relative `type_name` values to absolute ones
// for builders that retain a Java-shaped (relative-name) proto but need a
// protodesc-buildable clone.
//
// IT RESOLVES BY LEXICAL SCOPE. Protobuf resolves a relative name by walking
// OUTWARD from the declaring scope and taking the first match, so `Inner` named
// inside `Host` means `Host.Inner` whenever `Host` declares one, even if a
// top-level `Inner` also exists. Prefixing every name with `.pkg.` would
// silently rebind it to a different, valid type with no error from protodesc.
// Pinned by TestAbsolutizeFieldTypeNamesPreservesLexicalScope.
//
// Like protoc and Java, it resolves the FIRST COMPONENT outward and then
// requires the rest beneath it. protodesc instead retries the WHOLE reference at
// each enclosing scope, so leaving relative names to protodesc diverges from
// Java on compound names. Pinned by
// TestAbsolutizeFieldTypeNamesResolvesTheFirstComponentOutward.
//
// A NAME NO SCOPE DECLARES FALLS BACK TO A ROOT LOOKUP, `"." + name`, which is
// what Java does at the same point (Descriptors.java's lookupSymbol, the
// fall-through after the scope loop). Pinned by
// TestAbsolutizeFieldTypeNamesFallsBackForNamesTheFileDoesNotDeclare.
//
// DO NOT DELETE THIS PASS: protodesc cannot build a field that shadows its own
// type. RFC-204's `struct_uuid_vector` schema template is
// `CREATE TYPE AS STRUCT SV (...) CREATE TABLE T (id BIGINT, sv SV, ...)`, so
// message `T` carries a field named `SV` whose type is also `SV`. protodesc
// registers fields into the same by-name map it resolves type references
// against (desc_init.go's makeBase), so relative `SV` from inside `T` finds the
// FIELD and fails with "unknown kind". protoc's type_name lookup skips non-type
// symbols, and `declared` holds no fields, so this pass agrees with protoc.
// Without it that template stops loading. Pinned by
// TestAbsolutizationIsRequiredWhenAFieldShadowsItsOwnType.
//
// THE DEPENDENCY SEEDING CARRIES THE AGREEMENT, NOT THE FALLBACK. With a
// complete dependency set the walk always finds the first component, so the
// fallback is unreachable on any protoc-accepted descriptor and the root rule
// rests on Java's authority alone. The set is INCOMPLETE where
// `defaultExcludedDependencies` strips imports from stored metadata, which the
// global-registry seeding below repairs (pinned by
// TestAbsolutizeFieldTypeNamesSeesGloballyRegisteredImports).
//
// That repair is load-bearing for metadata Java writes: a user proto's package
// round-trips through RecordMetaData.toProto(), and every proto under
// fdb-record-layer-core/src/test/proto and src/testFixtures/proto declares one.
// `test_records_tuple_fields.proto` is the ancestor-package shape: package
// `com.apple.foundationdb.record.testTupleFields`, importing
// `tuple_fields.proto` at `package com.apple.foundationdb.record` (an import
// `defaultExcludedDependencies` strips), and writing a bare `UUID`. Metadata
// this port's SQL layer writes has no package, so a walk that stops at root and
// the root fallback answer alike and the seeding is unobservable there. Pinned
// by TestAbsolutizeFieldTypeNamesStopsAtAPackageScope and
// TestAbsolutizeFieldTypeNamesResolvesIntoAnAncestorPackageDependency; see
// their shared preamble for why the obvious fixtures cannot separate a stopped
// walk from the fallback.
func absolutizeFieldTypeNames(fd *descriptorpb.FileDescriptorProto, deps ...*descriptorpb.FileDescriptorProto) {
	pkg := fd.GetPackage()
	pkgPrefix := "."
	if pkg != "" {
		pkgPrefix = "." + pkg + "."
	}

	declared := seedDeclaredSymbols(fd, deps)

	for _, m := range fd.GetMessageType() {
		absolutizeMessageRefs(declared, pkgPrefix, m)
	}
	// Same for extensions at the file level, whose scope is the package.
	for _, ext := range fd.GetExtension() {
		absolutizeFieldRefs(declared, ext, pkgPrefix)
	}
}

func seedDeclaredSymbols(fd *descriptorpb.FileDescriptorProto, deps []*descriptorpb.FileDescriptorProto) map[string]bool {
	// declared holds every fully-qualified name a candidate scope may resolve
	// against: the types this file declares, the types its DEPENDENCIES declare,
	// and every PACKAGE prefix in play. All three are needed to agree with
	// protoc:
	//
	//   - Dependency types. Without them a name binding into an import falls
	//     through to the fallback, which answers a ROOT name: `UUID` inside
	//     `package com.apple.foundationdb.record.testTupleFields` would become
	//     `.UUID` where protoc walks out to
	//     `.com.apple.foundationdb.record.UUID` (Java's own
	//     test_records_tuple_fields.proto). No fallback rule substitutes for
	//     knowing the symbols.
	//   - Packages. Java's first-part lookup consults package descriptors
	//     (Descriptors.java's AGGREGATES_ONLY). Without them the walk can never
	//     stop at a package scope, so `probe.Inner` inside package `probe`
	//     would become `.probe.probe.Inner` instead of `.probe.Inner`.
	//   - Both together make a cross-package EXTENDEE work. Without dependency
	//     symbols a valid imported `other.Host` would be rewritten to
	//     `.probe.other.Host` and the descriptor would fail to load.
	declared := map[string]bool{}
	addDeclaredFile(declared, fd.GetPackage(), fd.GetMessageType(), fd.GetEnumType(), fd.GetService())

	// ONLY THE VISIBLE DEPENDENCIES, which is not the same as all of them.
	//
	// `deps` is whatever the caller had, and rebuildFileDescriptor hands over the
	// entire TRANSITIVE closure from stored metadata. Java resolves against a
	// narrower set: DescriptorPool's constructor seeds itself with the file's
	// DIRECT dependencies and then recurses through `public_dependency` only, so
	// a transitively-imported file that nobody re-exports is invisible to name
	// resolution.
	//
	// Seeding the whole closure would over-expose symbols of every kind: an
	// invisible file declaring `p.q.X` makes this walk stop at a scope Java never
	// considers and answer `.p.q.X.Y`, where Java climbs past and answers
	// `.p.X.Y`. That fails loudly rather than mis-binding, because protodesc
	// enforces the same visibility rule and refuses the over-exposed name:
	//
	//	cannot resolve type: resolved "p.q.X.Y",
	//	but "hidden.proto" is not imported
	//
	// so the over-exposure would reject metadata that Java loads.
	//
	// One traversal, not two, because the visible set CROSSES the two sources.
	// A direct import may be globally registered while the file it publicly
	// re-exports is a stored dependency, or the reverse; walking the stored
	// protos and the registry separately loses exactly those hand-offs.
	//
	// IMPORTS RESOLVED THROUGH THE GLOBAL REGISTRY, which `deps` does not carry.
	// `defaultExcludedDependencies` strips the Apple record-layer protos from the
	// stored dependency list -- `tuple_fields.proto` above all -- so a descriptor
	// in `package com.apple.foundationdb.record` referencing a bare `UUID` has
	// nothing in `deps` declaring it. The walk then runs out of scopes and the
	// root fallback answers `.UUID`, which exists nowhere: metadata Java accepts
	// stops loading.
	//
	// No fallback rule is a substitute for knowing the symbols -- a package
	// prefix happens to be right for this one shape and wrong in general, a root
	// lookup the other way round -- so the imports are resolved and collected
	// properly instead. TestAbsolutizeFieldTypeNamesSeesGloballyRegisteredImports
	// pins it. Registry lookups happen inside walkVisibleImports below, which is
	// what lets a globally-registered import re-export a stored one.
	addPkg := func(p string) {
		addDeclaredPackage(declared, p)
	}
	walkVisibleImports(fd, deps,
		func(dep *descriptorpb.FileDescriptorProto) {
			addDeclaredFile(declared, dep.GetPackage(), dep.GetMessageType(), dep.GetEnumType(), dep.GetService())
		},
		func(gfd protoreflect.FileDescriptor) {
			collectFromFileDescriptor(gfd, declared, addPkg)
		},
		// PACKAGE ONLY, no types: a visible dependency's own imports contribute
		// their package descriptors to this file's resolution but not their
		// symbols. See walkVisibleImports for why the two exposures differ.
		addPkg,
		nil) // no visit instrumentation in production

	return declared
}

func addDeclaredPackage(declared map[string]bool, p string) {
	if p == "" {
		return
	}
	// Every prefix is a scope protoc can stop at: `a.b.c` declares `.a`,
	// `.a.b` and `.a.b.c`.
	full := "."
	for _, part := range strings.Split(p, ".") {
		full += part
		declared[full] = true
		full += "."
	}
}

func collectDeclaredSymbols(declared map[string]bool, scope string, msgs []*descriptorpb.DescriptorProto, enums []*descriptorpb.EnumDescriptorProto) {
	for _, e := range enums {
		declared[scope+e.GetName()] = true
	}
	for _, m := range msgs {
		full := scope + m.GetName()
		declared[full] = true
		collectDeclaredSymbols(declared, full+".", m.GetNestedType(), m.GetEnumType())
	}
}

// collectDeclaredServices adds service names. Java's first-part lookup uses
// AGGREGATES_ONLY, which is message|enum|package|SERVICE (Descriptors.java),
// so a service shadows a compound name exactly as a message would: with
// service `p.q.X` in scope and an imported `p.X.Y`, Java stops at `p.q.X`,
// requires `Y` beneath it and REJECTS. Seeding messages, enums and packages
// but not services makes this walk continue outward and bind `.p.X.Y` --
// silently loading a descriptor Java refuses.
//
// AGAINST THE PINNED JAVA, WHICH MATTERS HERE because the rule changed.
// Measured from the protobuf-java jar MODULE.bazel pins (4.29.3):
// `isAggregate` is exactly four instanceof tests -- Descriptor, Enum,
// Package, Service -- and `lookupSymbol` contains no isType call at all, so
// a first-component hit is a hard stop whether or not the symbol is a type.
// protobuf 34.x ADDS a `filter != TYPES_ONLY || isType(result)` branch that
// keeps searching past a non-type hit; it sits after the compound-name
// guard, so it changes BARE names only.
//
// The two versions therefore disagree about a bare name that lands on a
// service, and this port follows the pin. Every shape the surrounding
// comments rest on is COMPOUND (`X.Y`, `probe.Inner`, `A.B`) and so is
// version-stable; do not "fix" the bare-name case against a newer Java
// without moving the pin first.
func collectDeclaredServices(declared map[string]bool, scope string, svcs []*descriptorpb.ServiceDescriptorProto) {
	for _, s := range svcs {
		declared[scope+s.GetName()] = true
	}
}

func addDeclaredFile(
	declared map[string]bool,
	pkg string,
	msgs []*descriptorpb.DescriptorProto,
	enums []*descriptorpb.EnumDescriptorProto,
	svcs []*descriptorpb.ServiceDescriptorProto,
) {
	prefix := "."
	if pkg != "" {
		prefix = "." + pkg + "."
	}
	addDeclaredPackage(declared, pkg)
	collectDeclaredSymbols(declared, prefix, msgs, enums)
	collectDeclaredServices(declared, prefix, svcs)
}

// resolveProtoTypeName returns the absolute form of a relative type reference as JAVA
// would resolve it from `scope`, and reports whether it rewrote anything.
// `scope` is the fully-qualified name of the declaring message plus a dot, or
// the package prefix at file level. Already-absolute and empty names are left
// alone.
//
// JAVA, not protoc, and the distinction is not pedantic -- the two disagree
// and this port follows Java by design. Descriptors.java's lookupSymbol takes
// the first component, searches enclosing scopes for it with
// SearchFilter.AGGREGATES_ONLY, and on a hit requires the remainder beneath
// it without resuming the outward search; isAggregate is
// Descriptor|EnumDescriptor|PackageDescriptor|ServiceDescriptor, so FIELDS
// ARE SKIPPED at every intermediate scope. This walk does the same, which is
// why `declared` holds messages, enums, packages and services and nothing
// else.
//
// MEASURED divergence from protoc, in the extendee position: given a message
// `Host` with a FIELD named `A` and a top-level message `A`, `extend A`
// inside `Host` is REJECTED by protoc ("A" is not a message type) and
// ACCEPTED by protobuf-java. Go accepts, matching Java. Pinned by
// TestExtendeeSkipsAFieldShadowAsJavaDoes.
//
// The mechanism, stated so it can be checked: protoc's CrossLinkField
// resolves `type_name` with an explicit LOOKUP_TYPES and `extendee` with
// LOOKUP_ALL -- but LOOKUP_ALL is the DEFAULT argument and is never written
// at the extendee call site, so grepping for it near that call finds
// nothing and appears to refute this. Read the default on the declaration
// of ResolveMode instead. protobuf-java has no such asymmetry: crossLink
// passes TYPES_ONLY on both axes, the same constant to the same overload.
//
// The converse case -- protodesc stopping on a field where BOTH protoc and
// Java skip it -- is the field/type collision documented above, and it is why
// this pass cannot simply be deleted.
func resolveProtoTypeName(declared map[string]bool, name, scope string) (string, bool) {
	if name == "" || name[0] == '.' {
		return name, false
	}
	// Protobuf resolves the FIRST COMPONENT outward, then requires the rest
	// beneath it: `A.B` inside `.p.X` tries `.p.X.A` before `.p.A`.
	first := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		first = name[:i]
	}
	for s := scope; ; {
		if declared[s+first] {
			return s + name, true
		}
		// Strip the innermost component and try the enclosing scope. `s`
		// always ends in '.', so drop it before searching for the next.
		trimmed := strings.TrimSuffix(s, ".")
		i := strings.LastIndexByte(trimmed, '.')
		if i < 0 {
			// THE WALK FELL OFF THE END: nothing this file or its
			// dependencies declares matches the first component. Java does a
			// ROOT lookup here (Descriptors.java's lookupSymbol, the
			// fall-through after the scope loop) -- it does NOT prepend the
			// file's own package, and neither does this: a package prefix
			// would turn `extend probe.Ext` inside package `probe` into
			// `.probe.probe.Ext`, which exists nowhere.
			//
			// The committed differential cannot arbitrate root-vs-package --
			// every case in it has a complete dependency set, so this branch
			// is unreachable there and both spellings score 0. See its
			// NOT-covered list. What decides it is Java, and the divergent
			// shapes are pinned by
			// TestAbsolutizeFieldTypeNamesFallsBackToRootNotToThePackage.
			//
			// Leaving the name RELATIVE is still not an option -- that is
			// what the absolutization exists for, and the necessity case is
			// pinned by TestAbsolutizationIsRequiredWhenAFieldShadowsItsOwnType.
			return "." + name, true
		}
		s = trimmed[:i+1]
	}
}

// absolutizeFieldRefs rewrites BOTH type references a field can carry.
//
// EXTENDEE IS NOT OPTIONAL HERE. The shape that needs it is a compound
// extendee shadowed by a nested MESSAGE:
//
//	message Host { message A {} extend A.B { ... } }   // package probe
//
// protoc rejects it -- `"A.B" is resolved to "probe.Host.A.B", which is not
// defined` -- while protodesc, left to itself, silently binds `.probe.A.B`,
// a DIFFERENT message. Rewriting the extendee under the first-component rule
// produces `.probe.Host.A.B`, which protodesc then refuses: protoc's answer,
// symbol for symbol.
//
// The same shape with a FIELD in place of the nested message does not need
// it: protoc, protobuf-java, protodesc unrewritten and this pass all accept
// it. C++ descriptor.cc says why: for a compound name whose first part is a
// non-aggregate, "We found a symbol but it's not an aggregate. Continue the
// loop."
//
// AND DO NOT ADD NON-TYPE SYMBOLS TO `declared` TO CHASE protoc. For a BARE
// shadowed extendee (`extend A` where Host declares a FIELD `A`) protoc
// rejects but protobuf-java 4.29.3 -- the MODULE.bazel pin, i.e. the spec
// this port follows -- ACCEPTS and binds `probe.A`, because its lookupSymbol
// probes the first part with AGGREGATES_ONLY and a FieldDescriptor can never
// be returned. protoc uses LOOKUP_ALL for extendee and LOOKUP_TYPES for
// type_name, which is where its asymmetry comes from; Java has no such
// asymmetry, and neither does this. Matching protoc here would move Go AWAY
// from Java.
func absolutizeFieldRefs(declared map[string]bool, f *descriptorpb.FieldDescriptorProto, scope string) {
	if abs, ok := resolveProtoTypeName(declared, f.GetTypeName(), scope); ok {
		f.TypeName = &abs
	}
	if abs, ok := resolveProtoTypeName(declared, f.GetExtendee(), scope); ok {
		f.Extendee = &abs
	}
}

func absolutizeMessageRefs(declared map[string]bool, scope string, msg *descriptorpb.DescriptorProto) {
	inner := scope + msg.GetName() + "."
	for _, f := range msg.GetField() {
		absolutizeFieldRefs(declared, f, inner)
	}
	// MESSAGE-SCOPED EXTENSIONS: `DescriptorProto.Extension`, the `extend`
	// block written INSIDE a message, which proto2 allows. Fixtures built
	// from a compiled Go file via protodesc.ToFileDescriptorProto carry
	// absolute names, so they cannot exercise this position.
	for _, ext := range msg.GetExtension() {
		absolutizeFieldRefs(declared, ext, inner)
	}
	for _, nested := range msg.GetNestedType() {
		absolutizeMessageRefs(declared, inner, nested)
	}
}

// collectFromFileDescriptor seeds `declared` from an already-built
// protoreflect.FileDescriptor -- the shape the global registry hands back.
//
// It exists because the stored dependency list is NOT the import list:
// defaultExcludedDependencies strips the Apple record-layer protos, so those
// imports arrive only through protoregistry.GlobalFiles and their symbols would
// otherwise be invisible to the resolution walk.
func collectFromFileDescriptor(fd protoreflect.FileDescriptor, declared map[string]bool, addPackage func(string)) {
	addPackage(string(fd.Package()))

	var msgs func(md protoreflect.MessageDescriptors)
	msgs = func(md protoreflect.MessageDescriptors) {
		for i := 0; i < md.Len(); i++ {
			m := md.Get(i)
			declared["."+string(m.FullName())] = true
			enums := m.Enums()
			for j := 0; j < enums.Len(); j++ {
				declared["."+string(enums.Get(j).FullName())] = true
			}
			msgs(m.Messages())
		}
	}
	msgs(fd.Messages())

	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		declared["."+string(enums.Get(i).FullName())] = true
	}
	svcs := fd.Services()
	for i := 0; i < svcs.Len(); i++ {
		declared["."+string(svcs.Get(i).FullName())] = true
	}
}
