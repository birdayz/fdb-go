package javacorpus

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	apiddl "fdb.dev/pkg/relational/api/ddl"
	"fdb.dev/pkg/relational/conformance/javayamsql"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

// The yaml-tests command pair `load schema template` and `set schema state`:
// Java's yaml-tests Command.getLoadSchemaTemplateCommand /
// getSetSchemaStateCommand and their CommandUtil helpers. Both reach the
// catalog directly, not through SQL, on the connection the setup block holds.

// yamlTestsProtoSets are the binary FileDescriptorSets Bazel builds from the
// vendored yaml-tests protos (//third_party/apple/fdb-record-layer:
// yamltests_main_proto and yamltests_test_proto), under Java's file names.
var yamlTestsProtoSets = []string{
	"third_party/apple/fdb-record-layer/yamltests_main_proto-descriptor-set.proto.bin",
	"third_party/apple/fdb-record-layer/yamltests_test_proto-descriptor-set.proto.bin",
}

var (
	yamlProtosOnce sync.Once
	yamlProtos     *protoregistry.Files
	yamlProtosErr  error
)

// yamlTestsProtos is the registry of the yaml-tests protos: what Java resolves
// by class name (`from <class>`) and by file name (a metadata JSON's
// `dependency` list). Imports outside the set (record_metadata_options.proto,
// descriptor.proto) resolve to the generated Go descriptors, so their options
// and extensions are the record layer's own.
func yamlTestsProtos() (*protoregistry.Files, error) {
	yamlProtosOnce.Do(func() {
		var protos []*descriptorpb.FileDescriptorProto
		for _, rel := range yamlTestsProtoSets {
			path, err := FindAbove(rel)
			if err != nil {
				yamlProtosErr = err
				return
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				yamlProtosErr = err
				return
			}
			var set descriptorpb.FileDescriptorSet
			if err := proto.Unmarshal(raw, &set); err != nil {
				yamlProtosErr = fmt.Errorf("%s: %w", rel, err)
				return
			}
			protos = append(protos, set.GetFile()...)
		}
		files := &protoregistry.Files{}
		resolver := &layeredResolver{local: files}
		// The sets are not topologically ordered across each other; build
		// whatever has its imports, until nothing is left.
		for len(protos) > 0 {
			var rest []*descriptorpb.FileDescriptorProto
			for _, fdp := range protos {
				fd, err := protodesc.NewFile(fdp, resolver)
				if err != nil {
					rest = append(rest, fdp)
					continue
				}
				if err := files.RegisterFile(fd); err != nil {
					yamlProtosErr = err
					return
				}
			}
			if len(rest) == len(protos) {
				_, err := protodesc.NewFile(rest[0], resolver)
				yamlProtosErr = fmt.Errorf("yaml-tests protos do not resolve: %w", err)
				return
			}
			protos = rest
		}
		yamlProtos = files
	})
	return yamlProtos, yamlProtosErr
}

// layeredResolver resolves the yaml-tests files first, then the generated ones.
type layeredResolver struct{ local *protoregistry.Files }

func (r *layeredResolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if fd, err := r.local.FindFileByPath(path); err == nil {
		return fd, nil
	}
	return protoregistry.GlobalFiles.FindFileByPath(path)
}

func (r *layeredResolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	if d, err := r.local.FindDescriptorByName(name); err == nil {
		return d, nil
	}
	return protoregistry.GlobalFiles.FindDescriptorByName(name)
}

// FindAbove locates rel by walking up from the working directory, as
// javayamsql.OpenCorpus does, so it works in the Bazel runfiles tree.
func FindAbove(rel string) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, filepath.FromSlash(rel))
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate %s above the working directory", rel)
		}
		dir = parent
	}
}

var (
	protoPackageRE   = regexp.MustCompile(`package\s+([\w.]+);`)
	javaPackageRE    = regexp.MustCompile(`option\s+java_package\s*=\s*"([^"]+)";`)
	javaOuterClassRE = regexp.MustCompile(`option\s+java_outer_classname\s*=\s*"([^"]+)";`)
)

// javaClassOf is the Java class protoc generates for fd: java_package (else
// the proto package) and java_outer_classname (else the file name without
// .proto and non-alphanumerics), CommandUtil.getFullClassName's rule.
func javaClassOf(fd protoreflect.FileDescriptor) string {
	opts, _ := fd.Options().(*descriptorpb.FileOptions)
	pkg := string(fd.Package())
	if opts.GetJavaPackage() != "" {
		pkg = opts.GetJavaPackage()
	}
	outer := opts.GetJavaOuterClassname()
	if outer == "" {
		base := strings.TrimSuffix(filepath.Base(fd.Path()), ".proto")
		outer = regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(base, "")
	}
	return pkg + "." + outer
}

func fileByJavaClass(files *protoregistry.Files, class string) (protoreflect.FileDescriptor, error) {
	var found protoreflect.FileDescriptor
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if javaClassOf(fd) == class {
			found = fd
			return false
		}
		return true
	})
	if found == nil {
		return nil, fmt.Errorf("ClassNotFoundException: %s", class)
	}
	return found, nil
}

// parseLoadTemplate is CommandUtil.parseLoadTemplateString: exactly three
// space-separated tokens, "<template> from <source>".
func parseLoadTemplate(value string) (template, source string, err error) {
	tokens := strings.Fields(value)
	if len(tokens) != 3 {
		return "", "", fmt.Errorf("expecting load command consisting of 3 tokens")
	}
	if tokens[1] != "from" {
		return "", "", fmt.Errorf("expecting load command looking like X from Y")
	}
	return tokens[0], tokens[2], nil
}

// schemaTemplateFromLoadCommand is CommandUtil.fromProto: the metadata from a
// metadata JSON file (a source ending in .json, resolved against the working
// directory as Java's relative path is) or from a generated proto class, as a
// version-1 schema template.
func schemaTemplateFromLoadCommand(value, workingDir string) (api.SchemaTemplate, error) {
	name, source, err := parseLoadTemplate(value)
	if err != nil {
		return nil, err
	}
	files, err := yamlTestsProtos()
	if err != nil {
		return nil, err
	}
	var md *recordlayer.RecordMetaData
	if strings.HasSuffix(source, ".json") {
		md, err = loadRecordMetaDataFromJSON(files, filepath.Join(workingDir, filepath.FromSlash(source)))
	} else {
		var fd protoreflect.FileDescriptor
		if fd, err = fileByJavaClass(files, source); err == nil {
			md, err = recordlayer.NewRecordMetaDataBuilder().SetRecords(fd).Build()
		}
	}
	if err != nil {
		return nil, err
	}
	return metadata.NewRecordLayerSchemaTemplateWithVersion(name, md, 1)
}

// loadRecordMetaDataFromJSON is CommandUtil.loadRecordMetaDataFromJson: the
// MetaData proto parsed from JSON ignoring unknown fields (which, as with
// Java's JsonFormat, drops every proto2 extension), its dependencies resolved
// by file name, the FieldOptions extensions those dependencies declare
// recovered from the raw JSON, and the metadata built over them.
func loadRecordMetaDataFromJSON(files *protoregistry.Files, path string) (*recordlayer.RecordMetaData, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	md := &gen.MetaData{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, md); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	// These are added automatically, so they count as bundled.
	included := map[string]bool{
		"record_metadata.proto":         true,
		"record_metadata_options.proto": true,
		"tuple_fields.proto":            true,
	}
	var needed []string
	seen := map[string]bool{}
	need := func(dep string) {
		if !seen[dep] {
			seen[dep] = true
			needed = append(needed, dep)
		}
	}
	records, _ := obj["records"].(map[string]any)
	for _, d := range asArray(records["dependency"]) {
		need(fmt.Sprint(d))
	}
	// Dependencies whose definitions the JSON carries itself.
	for _, d := range asArray(obj["dependencies"]) {
		def, _ := d.(map[string]any)
		included[fmt.Sprint(def["name"])] = true
		for _, dep := range asArray(def["dependency"]) {
			need(fmt.Sprint(dep))
		}
	}
	var deps []protoreflect.FileDescriptor
	for _, dep := range needed {
		if included[dep] {
			continue
		}
		fd, err := files.FindFileByPath(dep)
		if err != nil {
			return nil, fmt.Errorf("ClassNotFoundException: no proto class for %s: %w", dep, err)
		}
		deps = append(deps, fd)
	}

	restoreFieldOptionExtensions(records, md.GetRecords(), deps)

	// Java hands the resolved files to the builder (addDependencies); the
	// MetaData proto's own dependency list is where Go's loader takes them.
	have := map[string]bool{}
	for _, d := range md.GetDependencies() {
		have[d.GetName()] = true
	}
	for _, fd := range deps {
		if !have[fd.Path()] {
			md.Dependencies = append(md.Dependencies, protodesc.ToFileDescriptorProto(fd))
		}
	}
	return recordlayer.RecordMetaDataFromProto(md)
}

func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}

// restoreFieldOptionExtensions is CommandUtil.restoreFieldOptionExtensions:
// for every FieldOptions extension one of the dependency files declares, keyed
// by its full name, copy the value the raw JSON gives a field's options into
// the parsed proto.
func restoreFieldOptionExtensions(fileJSON map[string]any, file *descriptorpb.FileDescriptorProto, deps []protoreflect.FileDescriptor) {
	byKey := map[string]protoreflect.ExtensionDescriptor{}
	fieldOptions := (&descriptorpb.FieldOptions{}).ProtoReflect().Descriptor().FullName()
	for _, dep := range deps {
		xs := dep.Extensions()
		for i := 0; i < xs.Len(); i++ {
			if x := xs.Get(i); x.ContainingMessage().FullName() == fieldOptions {
				byKey[string(x.FullName())] = x
			}
		}
	}
	if len(byKey) == 0 || file == nil {
		return
	}
	for i, m := range asArray(fileJSON["message_type"]) {
		if i < len(file.GetMessageType()) {
			restoreInMessage(m, file.GetMessageType()[i], byKey)
		}
	}
}

func restoreInMessage(messageJSON any, message *descriptorpb.DescriptorProto, byKey map[string]protoreflect.ExtensionDescriptor) {
	obj, _ := messageJSON.(map[string]any)
	for _, f := range asArray(obj["field"]) {
		fieldJSON, _ := f.(map[string]any)
		optionsJSON, ok := fieldJSON["options"].(map[string]any)
		if !ok {
			continue
		}
		number, err := fieldJSON["number"].(json.Number).Int64()
		if err != nil {
			continue
		}
		for _, field := range message.GetField() {
			if int64(field.GetNumber()) != number {
				continue
			}
			for key, value := range optionsJSON {
				x, ok := byKey[key]
				valueObj, isObj := value.(map[string]any)
				if !ok || !isObj {
					continue
				}
				if field.Options == nil {
					field.Options = &descriptorpb.FieldOptions{}
				}
				msg := toDynamicMessage(x.Message(), valueObj)
				proto.SetExtension(field.Options, dynamicpb.NewExtensionType(x), msg)
			}
			break
		}
	}
	for i, n := range asArray(obj["nested_type"]) {
		if i < len(message.GetNestedType()) {
			restoreInMessage(n, message.GetNestedType()[i], byKey)
		}
	}
}

// toDynamicMessage is CommandUtil.toDynamicMessage: the JSON object's fields
// by proto name, unknown names skipped.
func toDynamicMessage(desc protoreflect.MessageDescriptor, obj map[string]any) *dynamicpb.Message {
	msg := dynamicpb.NewMessage(desc)
	for key, value := range obj {
		field := desc.Fields().ByName(protoreflect.Name(key))
		if field == nil {
			continue
		}
		if arr, ok := value.([]any); ok && field.IsList() {
			list := msg.Mutable(field).List()
			for _, element := range arr {
				list.Append(toFieldValue(field, element))
			}
			continue
		}
		msg.Set(field, toFieldValue(field, value))
	}
	return msg
}

func toFieldValue(field protoreflect.FieldDescriptor, value any) protoreflect.Value {
	num, _ := value.(json.Number)
	switch field.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(fmt.Sprint(value))
	case protoreflect.BoolKind:
		b, _ := value.(bool)
		return protoreflect.ValueOfBool(b)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, _ := num.Int64()
		return protoreflect.ValueOfInt32(int32(n))
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, _ := num.Int64()
		return protoreflect.ValueOfUint32(uint32(n))
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, _ := num.Int64()
		return protoreflect.ValueOfInt64(n)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, _ := num.Int64()
		return protoreflect.ValueOfUint64(uint64(n))
	case protoreflect.FloatKind:
		f, _ := num.Float64()
		return protoreflect.ValueOfFloat32(float32(f))
	case protoreflect.DoubleKind:
		f, _ := num.Float64()
		return protoreflect.ValueOfFloat64(f)
	case protoreflect.EnumKind:
		v := field.Enum().Values().ByName(protoreflect.Name(fmt.Sprint(value)))
		if v == nil {
			return protoreflect.ValueOfEnum(0)
		}
		return protoreflect.ValueOfEnum(v.Number())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		obj, _ := value.(map[string]any)
		return protoreflect.ValueOfMessage(toDynamicMessage(field.Message(), obj))
	case protoreflect.BytesKind:
		b, _ := base64.StdEncoding.DecodeString(fmt.Sprint(value))
		return protoreflect.ValueOfBytes(b)
	}
	panic(fmt.Sprintf("unsupported field type %s for field %s", field.Kind(), field.FullName()))
}

// schemaInstance is the part of the yaml-tests SchemaInstance proto the
// set-schema-state command reads.
type schemaInstance struct {
	name, databaseID string
	config           ddl.RecordLayerConfig
}

// parseSchemaInstance is CommandUtil.fromJson plus the RecordLayerConfig the
// command builds from it: JSON into SchemaInstance ignoring unknown fields,
// its index states by IndexState.fromCode, its store_info format version.
// Go has no user-version checker; a non-zero user version is refused rather
// than dropped.
func parseSchemaInstance(payload string) (*schemaInstance, error) {
	files, err := yamlTestsProtos()
	if err != nil {
		return nil, err
	}
	d, err := files.FindDescriptorByName("com.apple.foundationdb.relational.yamltests.generated.schemainstance.SchemaInstance")
	if err != nil {
		return nil, err
	}
	desc := d.(protoreflect.MessageDescriptor)
	msg := dynamicpb.NewMessage(desc)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true, Resolver: dynamicResolver(files)}).Unmarshal([]byte(payload), msg); err != nil {
		return nil, fmt.Errorf("set schema state: %w", err)
	}
	fields := desc.Fields()
	si := &schemaInstance{
		name:       msg.Get(fields.ByName("name")).String(),
		databaseID: msg.Get(fields.ByName("database_id")).String(),
		config:     ddl.RecordLayerConfig{IndexStates: map[string]recordlayer.IndexState{}},
	}
	storeInfo := msg.Get(fields.ByName("store_info")).Message()
	infoFields := storeInfo.Descriptor().Fields()
	si.config.FormatVersion = int32(storeInfo.Get(infoFields.ByName("formatVersion")).Int())
	if uv := storeInfo.Get(infoFields.ByName("userVersion")).Int(); uv != 0 {
		return nil, fmt.Errorf("set schema state: user version %d is not supported (Go has no user version checker)", uv)
	}
	msg.Get(fields.ByName("index_states")).Map().Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		si.config.IndexStates[k.String()] = recordlayer.IndexState(v.Enum())
		return true
	})
	return si, nil
}

// dynamicResolver resolves message types for protojson from files and the
// generated registry, as dynamic messages where files declares them.
func dynamicResolver(files *protoregistry.Files) *protoregistry.Types {
	types := &protoregistry.Types{}
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		ms := fd.Messages()
		for i := 0; i < ms.Len(); i++ {
			_ = types.RegisterMessage(dynamicpb.NewMessageType(ms.Get(i)))
		}
		return true
	})
	return types
}

// runSchemaCommand executes a `load schema template` or `set schema state`
// step on conn, Java's Command.applyMetadataOperationEmbedded: one transaction
// on the connection's backing catalog.
func (r *runner) runSchemaCommand(ctx context.Context, conn *sql.Conn, cmd *javayamsql.Command) error {
	var op func(apiddl.MetadataOperationsFactory, api.Transaction) error
	switch cmd.Kind {
	case javayamsql.CommandLoadSchemaTemplate:
		tmpl, err := schemaTemplateFromLoadCommand(cmd.Payload, r.cfg.WorkingDir)
		if err != nil {
			return err
		}
		op = func(f apiddl.MetadataOperationsFactory, txn api.Transaction) error {
			return f.SaveSchemaTemplate(tmpl, *api.NoOptions()).Execute(txn)
		}
	case javayamsql.CommandSetSchemaState:
		si, err := parseSchemaInstance(cmd.Payload)
		if err != nil {
			return err
		}
		op = func(f apiddl.MetadataOperationsFactory, txn api.Transaction) error {
			rl, ok := f.(*ddl.RecordLayerMetadataOperationsFactory)
			if !ok {
				return fmt.Errorf("set schema state needs the record-layer metadata factory, got %T", f)
			}
			return rl.SetStoreState(si.databaseID, si.name, si.config).Execute(txn)
		}
	default:
		return fmt.Errorf("unhandled command %q", cmd.Kind)
	}
	return conn.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("%s needs an embedded connection, got %T", cmd.Kind, dc)
		}
		return ec.ApplyMetadataOperation(ctx, op)
	})
}
