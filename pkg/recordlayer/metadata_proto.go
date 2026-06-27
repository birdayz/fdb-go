package recordlayer

import (
	"fmt"
	"sort"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// ToProto serializes RecordMetaData to its protobuf representation.
// Matches Java's RecordMetaData.toProto().
func (m *RecordMetaData) ToProto() (*gen.MetaData, error) {
	md := &gen.MetaData{}

	// 1. File descriptor
	md.Records = protodesc.ToFileDescriptorProto(m.fileDescriptor)

	// 2. Dependencies (transitive)
	deps := collectDependencies(m.fileDescriptor)
	for _, dep := range deps {
		md.Dependencies = append(md.Dependencies, protodesc.ToFileDescriptorProto(dep))
	}

	// 3. Build index → record type name mapping
	indexRecordTypes := m.buildIndexRecordTypeMap()

	// 4. Indexes (sorted by name for determinism)
	indexNames := make([]string, 0, len(m.indexes))
	for name := range m.indexes {
		indexNames = append(indexNames, name)
	}
	sort.Strings(indexNames)

	for _, name := range indexNames {
		idx := m.indexes[name]
		idxProto, err := indexToProto(idx)
		if err != nil {
			return nil, fmt.Errorf("index %s: %w", name, err)
		}
		if rtNames, ok := indexRecordTypes[name]; ok {
			sort.Strings(rtNames)
			idxProto.RecordType = rtNames
		}
		md.Indexes = append(md.Indexes, idxProto)
	}

	// 5. Record types (sorted by name for determinism)
	rtNames := make([]string, 0, len(m.recordTypes))
	for name := range m.recordTypes {
		rtNames = append(rtNames, name)
	}
	sort.Strings(rtNames)

	for _, name := range rtNames {
		rt := m.recordTypes[name]
		rtProto := &gen.RecordType{
			Name: proto.String(rt.Name),
		}
		if rt.PrimaryKey != nil {
			rtProto.PrimaryKey = rt.PrimaryKey.ToKeyExpression()
		}
		if rt.SinceVersion > 0 {
			rtProto.SinceVersion = proto.Int32(int32(rt.SinceVersion))
		}
		if rt.explicitRecordTypeKey != nil {
			v, err := valueToProto(rt.explicitRecordTypeKey)
			if err != nil {
				return nil, fmt.Errorf("record type %s explicit key: %w", name, err)
			}
			rtProto.ExplicitKey = v
		}
		md.RecordTypes = append(md.RecordTypes, rtProto)
	}

	// 6. Former indexes
	for _, fi := range m.formerIndexes {
		fiProto, err := formerIndexToProto(fi)
		if err != nil {
			return nil, fmt.Errorf("former index %s: %w", fi.FormerName, err)
		}
		md.FormerIndexes = append(md.FormerIndexes, fiProto)
	}

	// 7. Flags
	md.SplitLongRecords = proto.Bool(m.splitLongRecords)
	md.StoreRecordVersions = proto.Bool(m.storeRecordVersions)
	md.Version = proto.Int32(int32(m.version))

	if m.recordCountKey != nil {
		md.RecordCountKey = m.recordCountKey.ToKeyExpression()
	}

	return md, nil
}

// RecordMetaDataFromProto deserializes a MetaData proto back into a RecordMetaData.
// Matches Java's RecordMetaDataBuilder.loadFromProto().
func RecordMetaDataFromProto(md *gen.MetaData) (*RecordMetaData, error) {
	if md == nil {
		return nil, &MetaDataError{Message: "nil metadata proto"}
	}

	// 1. Rebuild file descriptor from proto
	fd, err := rebuildFileDescriptor(md.Records, md.Dependencies)
	if err != nil {
		return nil, fmt.Errorf("rebuild file descriptor: %w", err)
	}

	// 2. Create builder and set records. Detect the union descriptor by name
	// ("UnionDescriptor") OR by the usage=UNION proto annotation so that
	// files like catalog_data.proto (which uses "CatalogUnion") round-trip
	// correctly. Mirrors Java's RecordMetaData.build() which calls
	// RecordMetaDataBuilder.setRecordsWithUnionDescriptor using whichever
	// message is annotated with usage=UNION.
	unionName := findUnionDescriptorName(fd)
	builder := NewRecordMetaDataBuilder().setRecordsWithUnionName(fd, unionName)

	// 3. Load indexes first (need them before record type association)
	indexMap := make(map[string]*Index)
	for _, idxProto := range md.Indexes {
		idx, err := indexFromProto(idxProto)
		if err != nil {
			return nil, fmt.Errorf("index %s: %w", idxProto.GetName(), err)
		}
		indexMap[idx.Name] = idx
	}

	// 4. Associate indexes with record types
	for _, idxProto := range md.Indexes {
		idx := indexMap[idxProto.GetName()]
		rtNames := idxProto.RecordType
		if len(rtNames) == 0 {
			// Universal index
			builder.addIndexCommon(idx)
			builder.universalIndexes = append(builder.universalIndexes, idx)
		} else if len(rtNames) == 1 {
			// Single-type index
			rt := builder.recordTypes[rtNames[0]]
			if rt == nil {
				return nil, &MetaDataError{Message: fmt.Sprintf("unknown record type %q referenced by index %q", rtNames[0], idxProto.GetName())}
			}
			builder.addIndexCommon(idx)
			rt.indexes = append(rt.indexes, idx)
		} else {
			// Multi-type index
			builder.addIndexCommon(idx)
			for _, name := range rtNames {
				rt := builder.recordTypes[name]
				if rt == nil {
					return nil, &MetaDataError{Message: fmt.Sprintf("unknown record type %q referenced by index %q", name, idxProto.GetName())}
				}
				rt.multiTypeIndexes = append(rt.multiTypeIndexes, idx)
			}
		}
	}

	// 5. Load record type properties (primary keys, explicit keys, since versions)
	for _, rtProto := range md.RecordTypes {
		rt := builder.recordTypes[rtProto.GetName()]
		if rt == nil {
			continue
		}
		if rtProto.PrimaryKey != nil {
			pk, err := KeyExpressionFromProto(rtProto.PrimaryKey)
			if err != nil {
				return nil, fmt.Errorf("record type %s primary key: %w", rtProto.GetName(), err)
			}
			rt.PrimaryKey = pk
		}
		if rtProto.SinceVersion != nil {
			rt.SinceVersion = int(rtProto.GetSinceVersion())
		}
		if rtProto.ExplicitKey != nil {
			rt.explicitRecordTypeKey = valueFromProto(rtProto.ExplicitKey)
		}
	}

	// 6. Load former indexes
	for _, fiProto := range md.FormerIndexes {
		fi, err := formerIndexFromProto(fiProto)
		if err != nil {
			return nil, fmt.Errorf("former index: %w", err)
		}
		builder.formerIndexes = append(builder.formerIndexes, fi)
	}

	// 7. Load flags
	if md.SplitLongRecords != nil {
		builder.splitLongRecords = md.GetSplitLongRecords()
	}
	if md.StoreRecordVersions != nil {
		builder.storeRecordVersions = md.GetStoreRecordVersions()
	}
	if md.Version != nil {
		builder.version = int(md.GetVersion())
	}
	if md.RecordCountKey != nil {
		ck, err := KeyExpressionFromProto(md.RecordCountKey)
		if err != nil {
			return nil, fmt.Errorf("record count key: %w", err)
		}
		builder.recordCountKey = ck
	}

	return builder.Build()
}

// buildIndexRecordTypeMap returns a map of index name → record type names.
// Universal indexes are NOT included (they have no record type association).
func (m *RecordMetaData) buildIndexRecordTypeMap() map[string][]string {
	result := make(map[string][]string)
	for _, rt := range m.recordTypes {
		for _, idx := range rt.indexes {
			result[idx.Name] = append(result[idx.Name], rt.Name)
		}
		for _, idx := range rt.multiTypeIndexes {
			result[idx.Name] = append(result[idx.Name], rt.Name)
		}
	}
	return result
}

// indexToProto serializes an Index to its protobuf representation.
// SubspaceKey is stored as tuple-packed bytes matching Java's Tuple.from(key).pack().
func indexToProto(idx *Index) (*gen.Index, error) {
	p := &gen.Index{
		Name: proto.String(idx.Name),
		Type: proto.String(idx.Type),
	}
	if idx.RootExpression != nil {
		p.RootExpression = idx.RootExpression.ToKeyExpression()
	}

	// SubspaceKey → tuple-packed bytes
	subKey := idx.SubspaceTupleKey()
	if subKey != nil {
		p.SubspaceKey = tuple.Tuple{subKey}.Pack()
	}

	if idx.LastModifiedVersion > 0 {
		p.LastModifiedVersion = proto.Int32(int32(idx.LastModifiedVersion))
	}
	if idx.AddedVersion > 0 {
		p.AddedVersion = proto.Int32(int32(idx.AddedVersion))
	}

	// Options
	for k, v := range idx.Options {
		p.Options = append(p.Options, &gen.Index_Option{
			Key:   proto.String(k),
			Value: proto.String(v),
		})
	}

	// Predicate (proto round-trip)
	if idx.predicateProto != nil {
		p.Predicate = idx.predicateProto
	}

	return p, nil
}

// indexFromProto deserializes an Index from protobuf.
func indexFromProto(p *gen.Index) (*Index, error) {
	idx := &Index{
		Name:    p.GetName(),
		Type:    p.GetType(),
		Options: make(map[string]string),
	}

	if p.RootExpression != nil {
		expr, err := KeyExpressionFromProto(p.RootExpression)
		if err != nil {
			return nil, fmt.Errorf("root expression: %w", err)
		}
		idx.RootExpression = expr
	}

	// SubspaceKey: decode tuple-packed bytes
	if len(p.SubspaceKey) > 0 {
		t, err := fastUnpack(p.SubspaceKey)
		if err != nil {
			return nil, fmt.Errorf("subspace key: %w", err)
		}
		if len(t) == 1 {
			idx.subspaceKey = t[0]
		}
	}
	if idx.subspaceKey == nil {
		idx.subspaceKey = idx.Name // Default
	}

	if p.LastModifiedVersion != nil {
		idx.LastModifiedVersion = int(p.GetLastModifiedVersion())
	}
	if p.AddedVersion != nil {
		idx.AddedVersion = int(p.GetAddedVersion())
	}

	for _, opt := range p.Options {
		idx.Options[opt.GetKey()] = opt.GetValue()
	}

	// Predicate: store proto and build evaluator
	if p.Predicate != nil {
		if err := idx.SetPredicateProto(p.Predicate); err != nil {
			return nil, err
		}
	}

	return idx, nil
}

// formerIndexToProto serializes a FormerIndex to protobuf.
func formerIndexToProto(fi *FormerIndex) (*gen.FormerIndex, error) {
	p := &gen.FormerIndex{
		FormerName: proto.String(fi.FormerName),
	}
	if fi.SubspaceKey != nil {
		p.SubspaceKey = tuple.Tuple{fi.SubspaceKey}.Pack()
	}
	if fi.RemovedVersion > 0 {
		p.RemovedVersion = proto.Int32(int32(fi.RemovedVersion))
	}
	if fi.AddedVersion > 0 {
		p.AddedVersion = proto.Int32(int32(fi.AddedVersion))
	}
	return p, nil
}

// formerIndexFromProto deserializes a FormerIndex from protobuf.
func formerIndexFromProto(p *gen.FormerIndex) (*FormerIndex, error) {
	fi := &FormerIndex{
		FormerName:     p.GetFormerName(),
		RemovedVersion: int(p.GetRemovedVersion()),
		AddedVersion:   int(p.GetAddedVersion()),
	}
	if len(p.SubspaceKey) > 0 {
		t, err := fastUnpack(p.SubspaceKey)
		if err != nil {
			return nil, fmt.Errorf("subspace key: %w", err)
		}
		if len(t) == 1 {
			fi.SubspaceKey = t[0]
		}
	}
	return fi, nil
}

// valueToProto serializes a Go value to a Value proto.
// Matches Java's LiteralKeyExpression.toProtoValue().
func valueToProto(v any) (*gen.Value, error) {
	if v == nil {
		return &gen.Value{}, nil
	}
	p := &gen.Value{}
	switch val := v.(type) {
	case int:
		p.LongValue = proto.Int64(int64(val))
	case int32:
		p.IntValue = proto.Int32(val)
	case int64:
		p.LongValue = proto.Int64(val)
	case float32:
		p.FloatValue = proto.Float32(val)
	case float64:
		p.DoubleValue = proto.Float64(val)
	case bool:
		p.BoolValue = proto.Bool(val)
	case string:
		p.StringValue = proto.String(val)
	case []byte:
		p.BytesValue = val
	default:
		return nil, fmt.Errorf("unsupported value type %T", v)
	}
	return p, nil
}

// valueFromProto deserializes a Value proto to a Go value.
// Matches Java's LiteralKeyExpression.fromProtoValue().
func valueFromProto(p *gen.Value) any {
	if p == nil {
		return nil
	}
	if p.LongValue != nil {
		return p.GetLongValue()
	}
	if p.IntValue != nil {
		return p.GetIntValue()
	}
	if p.DoubleValue != nil {
		return p.GetDoubleValue()
	}
	if p.FloatValue != nil {
		return p.GetFloatValue()
	}
	if p.BoolValue != nil {
		return p.GetBoolValue()
	}
	if p.StringValue != nil {
		return p.GetStringValue()
	}
	if p.BytesValue != nil {
		return p.BytesValue
	}
	return nil
}

// defaultExcludedDependencies matches Java's RecordMetaData.defaultExcludedDependencies.
// These are Apple Record Layer protos that Java resolves from its classpath at build time,
// so they must not be included in serialized metadata.
var defaultExcludedDependencies = map[string]bool{
	"record_metadata.proto":         true,
	"record_metadata_options.proto": true,
	"tuple_fields.proto":            true,
}

// findUnionDescriptorName returns the union message name. Tries
// "UnionDescriptor" (record-layer-core), "RecordTypeUnion" (fdb-relational),
// then scans for a usage=UNION annotation (e.g. "CatalogUnion").
func findUnionDescriptorName(fd protoreflect.FileDescriptor) string {
	const defaultName = "UnionDescriptor"
	const fdbRelationalName = "RecordTypeUnion"
	if fd.Messages().ByName(protoreflect.Name(defaultName)) != nil {
		return defaultName
	}
	if fd.Messages().ByName(protoreflect.Name(fdbRelationalName)) != nil {
		return fdbRelationalName
	}
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		msg := msgs.Get(i)
		opts, ok := msg.Options().(*descriptorpb.MessageOptions)
		if !ok || opts == nil {
			continue
		}
		ext := proto.GetExtension(opts, gen.E_Record)
		if rto, ok := ext.(*gen.RecordTypeOptions); ok && rto != nil {
			if rto.GetUsage() == gen.RecordTypeOptions_UNION {
				return string(msg.Name())
			}
		}
	}
	return defaultName // Fall back; setRecordsWithUnionName handles missing gracefully.
}

// collectDependencies returns all transitive file descriptor dependencies,
// excluding Apple Record Layer protos (matching Java's defaultExcludedDependencies).
// Excluded protos and their transitive deps are skipped, matching Java's getDependencies().
func collectDependencies(fd protoreflect.FileDescriptor) []protoreflect.FileDescriptor {
	seen := make(map[string]bool)
	var deps []protoreflect.FileDescriptor

	var walk func(f protoreflect.FileDescriptor)
	walk = func(f protoreflect.FileDescriptor) {
		imports := f.Imports()
		for i := 0; i < imports.Len(); i++ {
			dep := imports.Get(i).FileDescriptor
			name := string(dep.Path())
			if seen[name] {
				continue
			}
			// Skip excluded protos and don't recurse into them (matches Java)
			if defaultExcludedDependencies[name] {
				continue
			}
			seen[name] = true
			deps = append(deps, dep)
			walk(dep)
		}
	}
	walk(fd)
	return deps
}

// absolutizeFieldTypeNames rewrites relative FieldDescriptorProto.type_name to absolute. protodesc.NewFile resolves relative type_names against the enclosing message scope; Java's buildFrom searches outward.
func absolutizeFieldTypeNames(fd *descriptorpb.FileDescriptorProto) {
	pkg := fd.GetPackage()
	prefix := "."
	if pkg != "" {
		prefix = "." + pkg + "."
	}
	var visitMessage func(msg *descriptorpb.DescriptorProto)
	visitMessage = func(msg *descriptorpb.DescriptorProto) {
		for _, f := range msg.GetField() {
			tn := f.GetTypeName()
			if tn != "" && tn[0] != '.' {
				absolute := prefix + tn
				f.TypeName = &absolute
			}
		}
		for _, nested := range msg.GetNestedType() {
			visitMessage(nested)
		}
	}
	for _, m := range fd.GetMessageType() {
		visitMessage(m)
	}
	// Same for extensions at the file level.
	for _, ext := range fd.GetExtension() {
		tn := ext.GetTypeName()
		if tn != "" && tn[0] != '.' {
			absolute := prefix + tn
			ext.TypeName = &absolute
		}
	}
}

// rebuildFileDescriptor reconstructs a FileDescriptor from a FileDescriptorProto
// and its dependencies. Uses topological ordering to handle transitive deps.
func rebuildFileDescriptor(
	recordsProto *descriptorpb.FileDescriptorProto,
	depsProto []*descriptorpb.FileDescriptorProto,
) (protoreflect.FileDescriptor, error) {
	absolutizeFieldTypeNames(recordsProto)
	for _, dp := range depsProto {
		absolutizeFieldTypeNames(dp)
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

	// Build the main file
	fd, err := protodesc.NewFile(recordsProto, resolver)
	if err != nil {
		return nil, fmt.Errorf("records: %w", err)
	}
	return fd, nil
}

// descriptorResolver implements protodesc.Resolver for rebuilding FileDescriptors.
type descriptorResolver struct {
	files map[string]protoreflect.FileDescriptor
}

func (r *descriptorResolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if fd, ok := r.files[path]; ok {
		return fd, nil
	}
	// Fall back to global registry for well-known types
	fd, err := protoregistry.GlobalFiles.FindFileByPath(path)
	if err == nil {
		r.files[path] = fd
		return fd, nil
	}
	return nil, fmt.Errorf("file not found: %s", path)
}

func (r *descriptorResolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	for _, fd := range r.files {
		if d := findInFile(fd, name); d != nil {
			return d, nil
		}
	}
	// Fall back to global registry
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
	if err == nil {
		return d, nil
	}
	return nil, fmt.Errorf("descriptor not found: %s", name)
}

// findInFile searches for a descriptor by name in a file.
func findInFile(fd protoreflect.FileDescriptor, name protoreflect.FullName) protoreflect.Descriptor {
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		if m.FullName() == name {
			return m
		}
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

// registerGlobalTypes registers well-known proto types (google/protobuf/descriptor.proto, etc.)
// that are imported by user protos.
func (r *descriptorResolver) registerGlobalTypes() {
	descFD := descriptorpb.File_google_protobuf_descriptor_proto
	r.files[string(descFD.Path())] = descFD
}
