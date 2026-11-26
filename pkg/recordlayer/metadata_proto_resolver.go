// Portions derived from FoundationDB Record Layer (Index.java,
// RecordMetaDataBuilder.java, LiteralKeyExpression.java, FormerIndex.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/internal/recordlayer/protoscope"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// rebuildFileDescriptor reconstructs a FileDescriptor from a FileDescriptorProto
// and its dependencies. Uses topological ordering to handle transitive deps.
func rebuildFileDescriptor(
	recordsProto *descriptorpb.FileDescriptorProto,
	depsProto []*descriptorpb.FileDescriptorProto,
) (protoreflect.FileDescriptor, error) {
	// The dependency set is passed so the resolver can see imported types and
	// packages. Without it a name binding into an import falls through to the
	// root fallback, which answers a bare `.Name` that nothing declares -- wrong
	// for every cross-package reference, and FATAL for a cross-package extendee
	// because the descriptor then stops loading rather than merely binding
	// oddly.
	absolutizeFieldTypeNames(recordsProto, depsProto...)
	// Java's relational DDL stores records files whose enums share a value
	// name; protobuf-go needs them scoped to build the descriptor (the
	// retained source proto, not this clone, is what ToProto emits). A
	// dependency is protoc-compiled and cannot carry the collision.
	protoscope.ScopeEnumValuesAsJava(recordsProto)
	for _, dp := range depsProto {
		// Each dependency is resolved against the whole set too: dependencies
		// import one another, and `tuple_fields.proto` referencing a type from a
		// sibling is the same shape as the records proto doing it.
		absolutizeFieldTypeNames(dp, depsProto...)
	}
	// Build dependency resolver
	resolver := &descriptorResolver{files: make(map[string]protoreflect.FileDescriptor)}

	// Register well-known types from the global registry
	resolver.registerGlobalTypes()

	// Build a map for topological resolution
	depMap := make(map[string]*descriptorpb.FileDescriptorProto)
	for _, dp := range depsProto {
		depMap[dp.GetName()] = dp
	}

	// Topologically resolve dependencies: each dep may itself have deps.
	// `resolved` flags completed entries; `inProgress` detects cycles
	// (e.g. a crafted proto with A→B→A would otherwise recurse until
	// the stack overflows — Go fuzz hit this on a 4-byte adversarial
	// input to RecordMetaDataFromProto).
	resolved := make(map[string]bool)
	inProgress := make(map[string]bool)
	var resolve func(name string) error
	resolve = func(name string) error {
		if resolved[name] {
			return nil
		}
		if inProgress[name] {
			return fmt.Errorf("cyclic proto dependency involving %q", name)
		}
		if _, ok := resolver.files[name]; ok {
			resolved[name] = true
			return nil
		}
		// Check global registry for well-known types
		if fd, err := protoregistry.GlobalFiles.FindFileByPath(name); err == nil {
			resolver.files[name] = fd
			resolved[name] = true
			return nil
		}
		dp, ok := depMap[name]
		if !ok {
			return fmt.Errorf("dependency not found: %s", name)
		}
		inProgress[name] = true
		defer delete(inProgress, name)
		// Resolve transitive deps first
		for _, depName := range dp.Dependency {
			if err := resolve(depName); err != nil {
				return err
			}
		}
		fd, err := protodesc.NewFile(dp, resolver)
		if err != nil {
			return fmt.Errorf("dependency %s: %w", name, err)
		}
		resolver.files[string(fd.Path())] = fd
		resolved[name] = true
		return nil
	}

	for _, dp := range depsProto {
		if err := resolve(dp.GetName()); err != nil {
			return nil, err
		}
	}

	// Build the main file. protobuf-java builds a FileDescriptor with no name
	// (a metadata JSON's records file typically has none); protodesc requires
	// a path, so an unnamed records file is built under a placeholder. The
	// retained source proto, which ToProto emits, keeps the empty name.
	if recordsProto != nil && recordsProto.GetName() == "" {
		recordsProto.Name = proto.String(unnamedRecordsFile)
	}
	fd, err := protodesc.NewFile(recordsProto, resolver)
	if err != nil {
		return nil, fmt.Errorf("records: %w", err)
	}
	return fd, nil
}

// unnamedRecordsFile is the path an unnamed records file is built under.
const unnamedRecordsFile = "__unnamed_records__.proto"

// descriptorResolver implements protodesc.Resolver for rebuilding FileDescriptors.
type descriptorResolver struct {
	files map[string]protoreflect.FileDescriptor
	// global is the registry consulted for imports `files` does not carry. It is
	// a SEAM: nil means protoregistry.GlobalFiles, and a test can substitute its
	// own to drive error paths the real registry cannot be made to produce on
	// demand -- the AMBIGUITY error above all, which is the entire reason
	// FindFileByPath passes the registry's error through instead of flattening
	// it to NotFound. Without the seam that direction was untestable.
	//
	// The type is the INTERFACE, and that is load-bearing rather than
	// stylistic: a *protoregistry.Files cannot be driven into ambiguity at all.
	// RegisterFile refuses a second file at an already-registered path for every
	// receiver except GlobalFiles itself, so the `multiple files named` arm of
	// FindFileByPath is unreachable through any registry a test can construct.
	// Only a stub reaches it.
	global protodesc.Resolver
}

func (r *descriptorResolver) globalFiles() protodesc.Resolver {
	if r.global != nil {
		return r.global
	}
	return protoregistry.GlobalFiles
}

func (r *descriptorResolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if fd, ok := r.files[path]; ok {
		return fd, nil
	}
	// Fall back to global registry for well-known types
	fd, err := r.globalFiles().FindFileByPath(path)
	if err == nil {
		r.files[path] = fd
		return fd, nil
	}
	// THE REGISTRY'S OWN ERROR, not an unconditional sentinel. protodesc compares
	// against protoregistry.NotFound with `==` (desc.go's import resolution), so
	// a genuine miss has to arrive as that exact value -- and GlobalFiles already
	// returns exactly it for a miss, so passing the error through is both correct
	// and simpler.
	//
	// Rewriting every failure to NotFound would go too far in the other
	// direction: FindFileByPath also reports AMBIGUITY when the registry holds
	// more than one descriptor at a path, and flattening that to "missing" lets
	// protodesc treat an ambiguous import as an allowed absent one. A miss is
	// tolerable; a conflict is not, and the two must stay distinguishable.
	return nil, err
}

func (r *descriptorResolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	for _, fd := range r.files {
		if d := findInFile(fd, name); d != nil {
			return d, nil
		}
	}
	// Fall back to global registry
	d, err := r.globalFiles().FindDescriptorByName(name)
	if err == nil {
		return d, nil
	}
	// THE SENTINEL, NOT A DESCRIPTIVE ERROR. protodesc's resolver walks a
	// relative name outward through enclosing scopes, and it decides whether to
	// KEEP WALKING by comparing this error to protoregistry.NotFound with `!=`
	// -- a direct equality (`desc_resolve.go`'s findDescriptor), not errors.Is,
	// so a wrapped sentinel does not survive it either. Returning anything else
	// is treated as fatal and ABORTS the walk at the very first candidate, which
	// turns "not here, try the enclosing scope" into "cannot resolve type".
	//
	// That is not hypothetical: it is why relative names appeared not to resolve
	// at all, and it was misdiagnosed once already as protodesc failing to walk
	// outward. protodesc walks correctly; this resolver was stopping it.
	return nil, protoregistry.NotFound
}

// walkVisibleImports visits every file `fd` may resolve names against, in
// Java's sense, and reports each with the exposure Java gives it.
//
// TYPES REACH ONE LEVEL; PACKAGES REACH TWO. That asymmetry is not a quirk to
// simplify away -- it falls out of how Descriptors.java searches, and getting it
// wrong diverges in both directions.
//
// A pool's `descriptorsByName` holds its own file's symbols AND a
// PackageDescriptor for every file in that pool's dependency set, because the
// DescriptorPool constructor calls addPackage over that set and addPackage
// writes into descriptorsByName. findSymbol then consults, in order, this
// pool's descriptorsByName and then `dependency.pool.descriptorsByName` for each
// dependency -- one level of pool indirection, no recursion.
//
// So for a file F whose visible set is V(F) = direct imports plus their
// recursive `public_dependency` closure:
//
//	TYPES visible to F    = F's own  U  { D's own types      : D in V(F) }
//	PACKAGES visible to F = packages(V(F))  U  { packages(V(D)) : D in V(F) }
//
// The second term of the package line is the one that is easy to miss: a
// dependency D that PRIVATELY imports H contributes H's PACKAGE to F, while
// H's messages, enums and services stay hidden. Seeding only V(F) drops that
// package shadow, so a `p.q` file referencing `X.Y` with a visible `.p.X.Y`
// climbs past `.p.q.X` and binds -- where Java stops at the package, requires
// `Y` beneath it, and REJECTS. That is Go accepting metadata Java cannot load,
// which is the direction the conformance rule forbids outright.
//
// `onPackageOnly` receives that second term. `onStored` and `onGlobal` receive
// the full-exposure set.
//
// ONE TRAVERSAL, TWO SOURCES, because the visible set crosses them. An import
// may be a stored descriptor in `pool`, or absent from stored metadata and
// recoverable only from the global registry -- `defaultExcludedDependencies`
// strips the Apple record-layer protos, so `tuple_fields.proto` always arrives
// the second way. Either kind may publicly re-export the other, so walking the
// two sources separately silently drops the hand-off.
//
// A path in neither source is skipped rather than treated as an error. That is
// not laxity: `allowUnknownDependencies` exists in Java for the same reason, and
// this is a pre-pass whose job is to seed a symbol table, not to validate the
// descriptor. protodesc validates, and rejects what it cannot resolve.
// onVisit, when non-nil, is called once per closure-walk step. It is an
// INSTRUMENTATION SEAM and production passes nil.
//
// It exists because the cost this function was rewritten to bound is not
// observable from any of the other callbacks. The rewrite replaced one closure
// walk per visible file with a single walk over the union of their imports; a
// regression that restores the per-file walk while still feeding the union is
// quadratic in TRAVERSAL and yet linear in `onPackageOnly` calls, because the
// final walk deduplicates. Counting emissions therefore cannot see it -- so the
// complexity guard counts visits instead, through here.
func walkVisibleImports(
	fd *descriptorpb.FileDescriptorProto,
	pool []*descriptorpb.FileDescriptorProto,
	onStored func(*descriptorpb.FileDescriptorProto),
	onGlobal func(protoreflect.FileDescriptor),
	onPackageOnly func(pkg string),
	onVisit func(path string),
) {
	byPath := make(map[string]*descriptorpb.FileDescriptorProto, len(pool))
	for _, d := range pool {
		// LAST writer wins, because rebuildFileDescriptor's own `depMap` is
		// built as `depMap[dp.GetName()] = dp` over this same slice and so keeps
		// the last entry at a duplicated path. protobuf-java agrees: its
		// FileDescriptor constructor fills nameToFileMap with HashMap.put in
		// array order.
		//
		// The two must agree or the failure is severe and confusing: the name
		// gets absolutized against one descriptor and then RESOLVED against a
		// different one carrying a different package, so BOTH orderings fail to
		// load -- the symbol table names a type the builder cannot supply.
		// Preferring the first here was an invented policy whose comment
		// asserted the opposite of what the resolver does.
		byPath[d.GetName()] = d
	}

	visible := collectVisibleClosure(fd.GetDependency(), byPath, onVisit)
	fullyExposed := make(map[string]bool, len(visible))
	for _, path := range visible {
		fullyExposed[path] = true
		if d, ok := byPath[path]; ok {
			onStored(d)
			continue
		}
		if g, err := protoregistry.GlobalFiles.FindFileByPath(path); err == nil {
			onGlobal(g)
		}
	}

	// The second package level: the packages of every visible file's OWN visible
	// set.
	//
	// ONE TRAVERSAL OVER THE UNION, not one per visible file, and the difference
	// is asymptotic rather than cosmetic. Written the obvious way -- call
	// collectVisibleClosure once per entry of `visible` -- each call walks the remaining
	// suffix of a public-import chain, so a chain of n files costs O(n^2) here;
	// and rebuildFileDescriptor runs this whole pass once per dependency, making
	// the total O(n^3). A few hundred descriptors then take seconds, which turns
	// VALID metadata into a CPU exhaustion input.
	//
	// The union form is exactly equivalent because public closure distributes
	// over union: with P the directs of every visible file,
	//
	//	U_{p in visible} V(p) = U_p (directs(p) U pubclosure(directs(p)))
	//	                      = P U pubclosure(P)
	//
	// so one collectVisibleClosure over P yields the same set in O(n), and `seen` inside
	// it keeps each file visited once.
	//
	// EQUALITY OF SETS, NOT OF CALL COUNTS, and the difference is load-bearing.
	// The per-file form could reach the same file from several visible parents
	// and call onPackageOnly once per arrival; the union form visits it once.
	// Differenced against the per-file form by
	// TestUnionSecondLevelMatchesPerFileSecondLevel, over randomized graphs with
	// cycles, self-imports, dangling paths and out-of-range public indices: 0
	// set mismatches, and a minority differing in CALL COUNT. That test is the
	// only thing in the tree that can check this -- the four hand-written
	// correctness arms stay green under the per-file form -- so it is committed
	// rather than quoted. That is safe only
	// because the sink is idempotent -- onPackageOnly wraps addDeclaredPackage, which does
	// `declared[full] = true` -- so a dropped repeat cannot change the result.
	// A sink that counted, accumulated or logged would make this rewrite a
	// behaviour change rather than an optimisation.
	var union []string
	for _, path := range visible {
		if _, direct, _, ok := fileImportInfo(byPath, path); ok {
			union = append(union, direct...)
		}
	}
	for _, q := range collectVisibleClosure(union, byPath, onVisit) {
		// Files already fully exposed contributed their package above. Skipping
		// them is presentational -- addDeclaredPackage is idempotent -- but it keeps
		// this loop's output to the level it is responsible for.
		if fullyExposed[q] {
			continue
		}
		if pkg, _, _, ok := fileImportInfo(byPath, q); ok && pkg != "" {
			onPackageOnly(pkg)
		}
	}
}

// fileImportInfo reports a file's package, its DIRECT imports and the subset of
// those that are PUBLIC, from whichever source holds it.
func fileImportInfo(byPath map[string]*descriptorpb.FileDescriptorProto, path string) (pkg string, direct, public []string, found bool) {
	if d, ok := byPath[path]; ok {
		direct = d.GetDependency()
		for _, idx := range d.GetPublicDependency() {
			// public_dependency holds INDICES into dependency. Java's
			// FileDescriptor constructor throws on an out-of-range one; this
			// is a pre-pass, so it skips and lets protodesc produce the
			// error ("invalid or duplicate public import index"), which it
			// does for exactly these descriptors.
			if idx < 0 || int(idx) >= len(direct) {
				continue
			}
			public = append(public, direct[idx])
		}
		return d.GetPackage(), direct, public, true
	}
	if g, err := protoregistry.GlobalFiles.FindFileByPath(path); err == nil {
		imps := g.Imports()
		for i := 0; i < imps.Len(); i++ {
			imp := imps.Get(i)
			direct = append(direct, imp.Path())
			if imp.IsPublic {
				public = append(public, imp.Path())
			}
		}
		return string(g.Package()), direct, public, true
	}
	return "", nil, nil, false
}

// collectVisibleClosure returns V(directs): the given direct imports, plus
// everything reachable from them through public imports alone. Recursion is
// guarded on set-add, exactly as importPublicDependencies is.
func collectVisibleClosure(directs []string, byPath map[string]*descriptorpb.FileDescriptorProto, onVisit func(string)) []string {
	seen := map[string]bool{}
	var out []string
	var visit func(p string)
	visit = func(p string) {
		// Counted BEFORE the dedup check, because the work a quadratic form
		// does is the repeated arrival, not the repeated insertion.
		if onVisit != nil {
			onVisit(p)
		}
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
		_, _, public, ok := fileImportInfo(byPath, p)
		if !ok {
			return
		}
		for _, q := range public {
			visit(q)
		}
	}
	for _, p := range directs {
		visit(p)
	}
	return out
}

// findInFile searches for a descriptor by name in a file, INCLUDING nested
// types.
//
// It used to scan only the top-level messages and enums, so a legitimate
// `.pkg.Outer.Inner` was reported missing even though the file declares it --
// and, before the sentinel fix above, that miss was reported as a fatal error
// rather than a miss, aborting the caller's outward walk as well. Nested types
// are the common case for the descriptors this port loads.
func findInFile(fd protoreflect.FileDescriptor, name protoreflect.FullName) protoreflect.Descriptor {
	if d := findInMessages(fd.Messages(), name); d != nil {
		return d
	}
	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		e := enums.Get(i)
		if e.FullName() == name {
			return e
		}
	}
	return nil
}

// findInMessages searches msgs and everything declared beneath them.
func findInMessages(msgs protoreflect.MessageDescriptors, name protoreflect.FullName) protoreflect.Descriptor {
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		if m.FullName() == name {
			return m
		}
		enums := m.Enums()
		for j := 0; j < enums.Len(); j++ {
			if e := enums.Get(j); e.FullName() == name {
				return e
			}
		}
		if d := findInMessages(m.Messages(), name); d != nil {
			return d
		}
	}
	return nil
}

// registerGlobalTypes registers well-known proto types (google/protobuf/descriptor.proto, etc.)
// that are imported by user protos.
func (r *descriptorResolver) registerGlobalTypes() {
	descFD := descriptorpb.File_google_protobuf_descriptor_proto
	r.files[string(descFD.Path())] = descFD
}
