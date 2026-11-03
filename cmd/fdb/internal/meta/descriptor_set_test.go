package meta

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	configv1 "fdb.dev/cmd/fdb/gen/fdb/config/v1"
	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

func TestDescriptorSetSource_LoadCompilerAnnotations(t *testing.T) {
	t.Parallel()
	for _, recordsFile := range []string{"", "records.proto"} {
		name := recordsFile
		if name == "" {
			name = "auto-select"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := descriptorFixturePath("records.binpb")
			src := &DescriptorSetSource{Path: path, RecordsFile: recordsFile}
			if !strings.Contains(src.Name(), path) {
				t.Errorf("Name() = %q; want path %q", src.Name(), path)
			}
			if recordsFile != "" && !strings.Contains(src.Name(), recordsFile) {
				t.Errorf("Name() = %q; want records file %q", src.Name(), recordsFile)
			}
			md, err := src.Load(context.Background())
			if err != nil {
				t.Fatalf("Load compiler-generated descriptors: %v", err)
			}
			assertAnnotatedDescriptorMetaData(t, md, 3)
		})
	}
}

func TestDescriptorSetSource_LoadRecordsBeforeDependencies(t *testing.T) {
	t.Parallel()
	set := readDescriptorFixture(t, "records.binpb")
	records := descriptorFile(t, set, "records.proto")
	set.File = slices.DeleteFunc(set.File, func(file *descriptorpb.FileDescriptorProto) bool {
		return file == records
	})
	set.File = append([]*descriptorpb.FileDescriptorProto{records}, set.File...)
	path := writeDescriptorSet(t, set)
	md, err := (&DescriptorSetSource{Path: path}).Load(context.Background())
	if err != nil {
		t.Fatalf("Load with records preceding imports: %v", err)
	}
	assertAnnotatedDescriptorMetaData(t, md, 3)
}

func TestDescriptorSetSource_MetadataVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		fixture     string
		recordsFile string
		version     *int32
		want        int
		invalid     bool
	}{
		{name: "derived", fixture: "records.binpb", want: 3},
		{name: "override", fixture: "records.binpb", version: proto.Int32(17), want: 17},
		{name: "explicit-zero", fixture: "records.binpb", version: proto.Int32(0), want: 0, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md, err := (&DescriptorSetSource{
				Path:        descriptorFixturePath(tc.fixture),
				RecordsFile: tc.recordsFile,
				Version:     tc.version,
			}).Load(context.Background())
			if tc.invalid {
				var versionErr *recordlayer.IndexVersionTooNewError
				if md != nil || !errors.As(err, &versionErr) || versionErr.MetaDataVersion != 0 {
					t.Fatalf("explicit zero Load = (%v, %v); want IndexVersionTooNewError at metadata version zero", md, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if md.Version() != tc.want {
				t.Errorf("Version() = %d; want %d", md.Version(), tc.want)
			}
		})
	}
}

func TestDescriptorSetSource_VersionBeforeIndexes(t *testing.T) {
	t.Parallel()
	path := descriptorFixturePath("records.binpb")
	md, err := (&DescriptorSetSource{Path: path, Version: proto.Int32(2)}).Load(context.Background())
	if md != nil || err == nil {
		t.Fatalf("Load = (%v, %v); want metadata version validation error", md, err)
	}
	var versionErr *recordlayer.IndexVersionTooNewError
	if !errors.As(err, &versionErr) {
		t.Errorf("error = %v; want IndexVersionTooNewError", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not identify source %q", err, path)
	}
}

func TestDescriptorSetSource_RoundTripThroughMetaFile(t *testing.T) {
	t.Parallel()
	md, err := (&DescriptorSetSource{
		Path:    descriptorFixturePath("records.binpb"),
		Version: proto.Int32(23),
	}).Load(context.Background())
	if err != nil {
		t.Fatalf("Load descriptor set: %v", err)
	}
	var buf bytes.Buffer
	if err := recordlayer.WriteRecordMetaData(md, &buf); err != nil {
		t.Fatalf("WriteRecordMetaData: %v", err)
	}
	path := writeDescriptorBytes(t, "meta.pb", buf.Bytes())
	restored, err := (&FileSource{Path: path}).Load(context.Background())
	if err != nil {
		t.Fatalf("Load serialized metadata: %v", err)
	}
	assertAnnotatedDescriptorMetaData(t, restored, 23)
	originalProto, err := md.ToProto()
	if err != nil {
		t.Fatalf("original ToProto: %v", err)
	}
	assertCompilerDescriptorProtos(t, originalProto, readDescriptorFixture(t, "records.binpb"))
	restoredProto, err := restored.ToProto()
	if err != nil {
		t.Fatalf("restored ToProto: %v", err)
	}
	if !proto.Equal(originalProto, restoredProto) {
		t.Error("descriptor-derived metadata changed after serialization and reload")
	}
}

func TestDescriptorSetSource_PreservesInputDescriptorPresence(t *testing.T) {
	t.Parallel()
	input := readDescriptorFixture(t, "records.binpb")
	records := descriptorFile(t, input, "records.proto")
	// Reflection reconstruction omits explicit proto2 syntax; the source proto
	// must retain that presence as well as an omitted field json_name.
	records.Syntax = proto.String("proto2")
	descriptorField(t, descriptorMessage(t, records, "Customer"), "id").JsonName = nil
	dependency := descriptorFile(t, input, "details.proto")
	dependency.Syntax = proto.String("proto2")
	dependency.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x07}) // Unknown field 100, varint 7.
	address := descriptorMessage(t, dependency, "Contact").GetNestedType()[0]
	descriptorField(t, address, "city").JsonName = nil
	md, err := (&DescriptorSetSource{Path: writeDescriptorSet(t, input)}).Load(t.Context())
	if err != nil {
		t.Fatalf("Load compiler descriptors with explicit default presence: %v", err)
	}
	output, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	assertCompilerDescriptorProtos(t, output, input)
	var buf bytes.Buffer
	if err := recordlayer.WriteRecordMetaData(md, &buf); err != nil {
		t.Fatalf("serialize presence-sensitive metadata: %v", err)
	}
	restored, err := (&FileSource{Path: writeDescriptorBytes(t, "presence-meta.pb", buf.Bytes())}).Load(t.Context())
	if err != nil {
		t.Fatalf("reload presence-sensitive metadata: %v", err)
	}
	fromProto, err := recordlayer.RecordMetaDataFromProto(output)
	if err != nil {
		t.Fatalf("rebuild presence-sensitive metadata: %v", err)
	}
	output.Dependencies[0].Syntax = nil
	output.Dependencies[0].ProtoReflect().SetUnknown(nil)
	for _, source := range []*recordlayer.RecordMetaData{md, restored, fromProto} {
		got, err := source.ToProto()
		if err != nil {
			t.Fatalf("serialize retained dependency sources: %v", err)
		}
		assertCompilerDescriptorProtos(t, got, input)
	}
}

func TestDescriptorSetSource_ExplicitSelectionAmongRecordsFiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file     string
		typeName string
		union    string
	}{
		{file: "records.proto", typeName: "Customer", union: "AnnotatedUnion"},
		{file: "other_records.proto", typeName: "OtherRecord", union: "RecordTypeUnion"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			md, err := (&DescriptorSetSource{
				Path:        descriptorFixturePath("multiple.binpb"),
				RecordsFile: tc.file,
			}).Load(context.Background())
			if err != nil {
				t.Fatalf("Load explicit records file: %v", err)
			}
			if md.FileDescriptor().Path() != tc.file {
				t.Errorf("records descriptor = %q; want %q", md.FileDescriptor().Path(), tc.file)
			}
			if len(md.RecordTypes()) != 1 || md.GetRecordType(tc.typeName) == nil {
				t.Errorf("RecordTypes() = %v; want only %s", md.RecordTypes(), tc.typeName)
			}
			if got := string(md.GetUnionDescriptor().Name()); got != tc.union {
				t.Errorf("union = %q; want %q", got, tc.union)
			}
		})
	}
}

func TestDescriptorSetSource_AutoSelectConventionalUnion(t *testing.T) {
	t.Parallel()
	set := readDescriptorFixture(t, "multiple.binpb")
	set.File = slices.DeleteFunc(set.File, func(file *descriptorpb.FileDescriptorProto) bool {
		return file.GetName() == "records.proto"
	})
	path := writeDescriptorSet(t, set)
	md, err := (&DescriptorSetSource{Path: path}).Load(context.Background())
	if err != nil {
		t.Fatalf("Load conventional RecordTypeUnion: %v", err)
	}
	if md.FileDescriptor().Path() != "other_records.proto" || md.GetRecordType("OtherRecord") == nil {
		t.Errorf("auto-selection chose %q with records %v; want other_records.proto/OtherRecord", md.FileDescriptor().Path(), md.RecordTypes())
	}
}

func TestDescriptorSetSource_AmbiguousRecordsFiles(t *testing.T) {
	t.Parallel()
	path := descriptorFixturePath("multiple.binpb")
	md, err := (&DescriptorSetSource{Path: path}).Load(context.Background())
	if md != nil || err == nil {
		t.Fatalf("Load = (%v, %v); want ambiguous records files error", md, err)
	}
	for _, want := range []string{path, "multiple records files", "records.proto", "other_records.proto", "--records-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestDescriptorSetSource_MissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing.binpb")
	md, err := (&DescriptorSetSource{Path: path}).Load(context.Background())
	if md != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load = (%v, %v); want os.ErrNotExist", md, err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not identify source %q", err, path)
	}
}

func TestDescriptorSetSource_InvalidFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{name: "empty", want: "no files"},
		{name: "corrupt", data: []byte{0xff, 0xff, 0xff, 0xff, 0xff}, want: "FileDescriptorSet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeDescriptorBytes(t, tc.name+".binpb", tc.data)
			md, err := (&DescriptorSetSource{Path: path}).Load(context.Background())
			if md != nil || err == nil {
				t.Fatalf("Load = (%v, %v); want invalid descriptor set error", md, err)
			}
			for _, want := range []string{path, tc.want} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestDescriptorSetSource_InvalidSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		src  DescriptorSetSource
		want string
	}{
		{name: "empty-path", want: "path is empty"},
		{name: "negative-version", src: DescriptorSetSource{Path: descriptorFixturePath("records.binpb"), Version: proto.Int32(-1)}, want: "non-negative"},
		{name: "unknown-records-file", src: DescriptorSetSource{Path: descriptorFixturePath("records.binpb"), RecordsFile: "absent.proto"}, want: "absent.proto"},
		{name: "selected-file-without-union", src: DescriptorSetSource{Path: descriptorFixturePath("records.binpb"), RecordsFile: "details.proto"}, want: "Union descriptor is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md, err := tc.src.Load(context.Background())
			if md != nil || err == nil {
				t.Fatalf("Load = (%v, %v); want invalid source error", md, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestDescriptorSetSource_NoRecordsUnion(t *testing.T) {
	t.Parallel()
	set := readDescriptorFixture(t, "records.binpb")
	records := descriptorFile(t, set, "records.proto")
	records.MessageType = slices.DeleteFunc(records.MessageType, func(message *descriptorpb.DescriptorProto) bool {
		return message.GetName() == "AnnotatedUnion"
	})
	path := writeDescriptorSet(t, set)
	md, err := (&DescriptorSetSource{Path: path}).Load(context.Background())
	if md != nil || err == nil {
		t.Fatalf("Load = (%v, %v); want missing records union error", md, err)
	}
	for _, want := range []string{path, "no records file", "UNION"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestDescriptorSetSource_MissingImport(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"details.proto", "record_metadata_options.proto", "google/protobuf/descriptor.proto"} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			set := readDescriptorFixture(t, "records.binpb")
			descriptorFile(t, set, missing)
			set.File = slices.DeleteFunc(set.File, func(file *descriptorpb.FileDescriptorProto) bool {
				return file.GetName() == missing
			})
			path := writeDescriptorSet(t, set)
			md, err := (&DescriptorSetSource{Path: path, RecordsFile: "records.proto"}).Load(context.Background())
			if md != nil || err == nil {
				t.Fatalf("Load = (%v, %v); want missing import error", md, err)
			}
			for _, want := range []string{path, missing, "include imports"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestDescriptorSetSource_InvalidPrimaryKeyDeclarations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "missing", want: "must have a primary key"},
		{name: "multiple", want: "Only one primary key"},
		{name: "repeated", want: "Primary key cannot be set on a repeated field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			set := readDescriptorFixture(t, "records.binpb")
			customer := descriptorMessage(t, descriptorFile(t, set, "records.proto"), "Customer")
			id := descriptorField(t, customer, "id")
			// Negative cases mutate real compiler options; successful loads above
			// use the compiler output unchanged rather than synthetic extensions.
			switch tc.name {
			case "missing":
				proto.ClearExtension(id.Options, gen.E_Field)
			case "multiple":
				email := descriptorField(t, customer, "email")
				proto.SetExtension(email.Options, gen.E_Field, &gen.FieldOptions{PrimaryKey: proto.Bool(true)})
			case "repeated":
				proto.ClearExtension(id.Options, gen.E_Field)
				tags := descriptorField(t, customer, "tags")
				proto.SetExtension(tags.Options, gen.E_Field, &gen.FieldOptions{PrimaryKey: proto.Bool(true)})
			}
			path := writeDescriptorSet(t, set)
			md, err := (&DescriptorSetSource{Path: path}).Load(context.Background())
			if md != nil || err == nil {
				t.Fatalf("Load = (%v, %v); want invalid primary key error", md, err)
			}
			var metadataErr *recordlayer.MetaDataError
			if !errors.As(err, &metadataErr) {
				t.Errorf("error = %v; want wrapped MetaDataError", err)
			}
			for _, want := range []string{path, tc.want} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestDescriptorSetSource_CanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	md, err := (&DescriptorSetSource{Path: descriptorFixturePath("records.binpb")}).Load(ctx)
	if md != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("Load = (%v, %v); want context.Canceled", md, err)
	}
}

func TestFromContext_DescriptorSet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		version *int32
	}{
		{name: "unset-version"},
		{name: "explicit-zero", version: proto.Int32(0)},
		{name: "version-17", version: proto.Int32(17)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			version := tc.version
			path := descriptorFixturePath("records.binpb")
			ctx := &configv1.Context{
				Name: "descriptor-context",
				Metadata: &configv1.MetadataSource{Source: &configv1.MetadataSource_DescriptorSet{
					DescriptorSet: &configv1.DescriptorSetSource{Path: path, RecordsFile: "records.proto", MetadataVersion: version},
				}},
			}
			src, err := FromContext(ctx, nil, nil)
			if err != nil {
				t.Fatalf("FromContext without database: %v", err)
			}
			descriptorSrc, ok := src.(*DescriptorSetSource)
			if !ok {
				t.Fatalf("source type = %T; want *DescriptorSetSource", src)
			}
			if descriptorSrc.Path != path || descriptorSrc.RecordsFile != "records.proto" || !reflect.DeepEqual(descriptorSrc.Version, version) {
				t.Errorf("source = %+v; want path %q, records_file records.proto, version %v", descriptorSrc, path, version)
			}
			md, err := src.Load(context.Background())
			if version != nil && *version == 0 {
				var versionErr *recordlayer.IndexVersionTooNewError
				if md != nil || !errors.As(err, &versionErr) || versionErr.MetaDataVersion != 0 {
					t.Fatalf("configured zero Load = (%v, %v); want IndexVersionTooNewError at metadata version zero", md, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load configured source: %v", err)
			}
			wantVersion := 3
			if version != nil {
				wantVersion = int(*version)
			}
			if md.Version() != wantVersion || md.GetRecordType("Customer") == nil {
				t.Errorf("configured metadata version/types = %d/%v; want %d/Customer", md.Version(), md.RecordTypes(), wantVersion)
			}
		})
	}
}

func TestFromContext_DescriptorSetMissingPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		source *configv1.DescriptorSetSource
	}{
		{name: "nil-descriptor-source"},
		{name: "empty-path", source: &configv1.DescriptorSetSource{RecordsFile: "records.proto"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := &configv1.Context{
				Name: "descriptor-context",
				Metadata: &configv1.MetadataSource{Source: &configv1.MetadataSource_DescriptorSet{
					DescriptorSet: tc.source,
				}},
			}
			src, err := FromContext(ctx, nil, nil)
			if src != nil || err == nil {
				t.Fatalf("FromContext = (%v, %v); want missing descriptor path error", src, err)
			}
			for _, want := range []string{"descriptor_set.path", "descriptor-context"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestFromContext_DescriptorSetNegativeVersion(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{
		Name: "descriptor-context",
		Metadata: &configv1.MetadataSource{Source: &configv1.MetadataSource_DescriptorSet{
			DescriptorSet: &configv1.DescriptorSetSource{Path: descriptorFixturePath("records.binpb"), MetadataVersion: proto.Int32(-1)},
		}},
	}
	src, err := FromContext(ctx, nil, nil)
	if err == nil {
		_, err = src.Load(context.Background())
	}
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Errorf("configured negative version error = %v; want non-negative version requirement", err)
	}
}

func assertAnnotatedDescriptorMetaData(t *testing.T, md *recordlayer.RecordMetaData, wantVersion int) {
	t.Helper()
	if md == nil {
		t.Fatal("Load returned nil metadata")
	}
	if md.Version() != wantVersion {
		t.Errorf("Version() = %d; want %d", md.Version(), wantVersion)
	}
	if !md.IsSplitLongRecords() || !md.IsStoreRecordVersions() {
		t.Errorf("schema options = split:%t, versions:%t; want both true", md.IsSplitLongRecords(), md.IsStoreRecordVersions())
	}
	if md.FileDescriptor().Path() != "records.proto" {
		t.Errorf("records descriptor = %q; want records.proto", md.FileDescriptor().Path())
	}
	if got := string(md.GetUnionDescriptor().FullName()); got != "fdb.cli.meta.testdata.AnnotatedUnion" {
		t.Errorf("union = %q; want custom AnnotatedUnion", got)
	}
	if len(md.RecordTypes()) != 1 {
		t.Errorf("record type count = %d; want 1 (imported nested messages are not records)", len(md.RecordTypes()))
	}
	customer := md.GetRecordType("Customer")
	if customer == nil {
		t.Fatal("Customer record type is missing")
	}
	if customer.SinceVersion != 0 || !customer.HasExplicitRecordTypeKey() || customer.GetRecordTypeKey() != int64(42) {
		t.Errorf("record options = since:%d, explicit-key:%t, key:%v; want 0/true/42", customer.SinceVersion, customer.HasExplicitRecordTypeKey(), customer.GetRecordTypeKey())
	}
	if customer.Descriptor.Fields().Len() != 5 {
		t.Errorf("Customer field count = %d; want 5", customer.Descriptor.Fields().Len())
	}
	if customer.PrimaryKey == nil || !slices.Equal(customer.PrimaryKey.FieldNames(), []string{"id"}) {
		t.Fatalf("primary key = %v; want id from custom option", customer.PrimaryKey)
	}
	address := customer.Descriptor.Fields().ByName("address")
	if address == nil || address.Message() == nil {
		t.Fatal("imported address message is unresolved")
	}
	if got := string(address.Message().FullName()); got != "fdb.cli.meta.testdata.Contact.Address" {
		t.Errorf("address message = %q; want imported Contact.Address", got)
	}
	if address.Message().ParentFile().Path() != "details.proto" {
		t.Errorf("address dependency = %q; want details.proto", address.Message().ParentFile().Path())
	}
	city := address.Message().Fields().ByName("city")
	if city == nil || city.Kind() != protoreflect.StringKind {
		t.Error("imported nested Address.city string descriptor is missing")
	}

	msg := dynamicpb.NewMessage(customer.Descriptor)
	msg.Set(customer.Descriptor.Fields().ByName("id"), protoreflect.ValueOfInt64(101))
	msg.Set(customer.Descriptor.Fields().ByName("email"), protoreflect.ValueOfString("customer@example.test"))
	msg.Set(customer.Descriptor.Fields().ByName("score"), protoreflect.ValueOfInt64(37))
	tags := msg.Mutable(customer.Descriptor.Fields().ByName("tags")).List()
	tags.Append(protoreflect.ValueOfString("red"))
	tags.Append(protoreflect.ValueOfString("blue"))
	if got, err := customer.PrimaryKey.Evaluate(nil, msg); err != nil || !reflect.DeepEqual(got, [][]any{{int64(101)}}) {
		t.Errorf("primary key evaluation = (%v, %v); want [[101]]", got, err)
	}
	if len(md.GetAllIndexes()) != 3 || len(md.GetIndexesForRecordType("Customer")) != 3 {
		t.Errorf("index count = %d globally/%d for Customer; want 3/3", len(md.GetAllIndexes()), len(md.GetIndexesForRecordType("Customer")))
	}
	for _, tc := range []struct {
		field   string
		typ     string
		unique  bool
		fanType gen.Field_FanType
		values  [][]any
	}{
		{field: "email", typ: recordlayer.IndexTypeValue, unique: true, fanType: gen.Field_SCALAR, values: [][]any{{"customer@example.test"}}},
		{field: "score", typ: recordlayer.IndexTypeRank, fanType: gen.Field_SCALAR, values: [][]any{{int64(37)}}},
		{field: "tags", typ: recordlayer.IndexTypeValue, fanType: gen.Field_FAN_OUT, values: [][]any{{"red"}, {"blue"}}},
	} {
		name := "Customer$" + tc.field
		index := md.GetIndex(name)
		if index == nil {
			t.Errorf("index %s was not derived from its custom option", name)
			continue
		}
		if index.Type != tc.typ || index.IsUnique() != tc.unique {
			t.Errorf("%s type/unique = %q/%t; want %q/%t", name, index.Type, index.IsUnique(), tc.typ, tc.unique)
		}
		if index.RootExpression == nil {
			t.Errorf("%s has no root expression", name)
			continue
		}
		root := index.RootExpression.ToKeyExpression()
		if tc.typ == recordlayer.IndexTypeRank {
			grouping := root.GetGrouping()
			if grouping == nil || grouping.GetGroupedCount() != 1 {
				t.Errorf("rank root = %v; want ungrouped rank with one grouped column", root)
				continue
			}
			root = grouping.GetWholeKey()
		}
		if field := root.GetField(); field == nil || field.GetFieldName() != tc.field || field.GetFanType() != tc.fanType {
			t.Errorf("%s field expression = %v; want %s/%s", name, root, tc.field, tc.fanType)
		}
		if got, err := index.RootExpression.Evaluate(nil, msg); err != nil || !reflect.DeepEqual(got, tc.values) {
			t.Errorf("%s evaluation = (%v, %v); want %v", name, got, err, tc.values)
		}
	}
}

func assertCompilerDescriptorProtos(t *testing.T, output *gen.MetaData, input *descriptorpb.FileDescriptorSet) {
	t.Helper()
	if !proto.Equal(output.GetRecords(), descriptorFile(t, input, "records.proto")) {
		t.Error("serialized records descriptor differs from the input FileDescriptorProto")
	}
	// Apple options and their transitive imports are runtime-supplied; only
	// details.proto is embedded. Compare every field, including source info.
	if len(output.GetDependencies()) != 1 {
		t.Fatalf("embedded dependencies = %d; want only details.proto", len(output.GetDependencies()))
	}
	if !proto.Equal(output.GetDependencies()[0], descriptorFile(t, input, "details.proto")) {
		t.Error("serialized imported descriptor differs from the compiler input")
	}
}

func descriptorFixturePath(name string) string {
	return filepath.Join("testdata", name)
}

func readDescriptorFixture(t *testing.T, name string) *descriptorpb.FileDescriptorSet {
	t.Helper()
	path := descriptorFixturePath(name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read compiler-generated fixture %s: %v", path, err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(raw, set); err != nil {
		t.Fatalf("unmarshal compiler-generated fixture %s: %v", path, err)
	}
	seen := make(map[string]bool)
	for _, file := range set.File {
		if seen[file.GetName()] {
			t.Fatalf("fixture %s contains duplicate file %q", path, file.GetName())
		}
		seen[file.GetName()] = true
	}
	for _, required := range []string{"records.proto", "details.proto", "record_metadata_options.proto", "google/protobuf/descriptor.proto"} {
		if !seen[required] {
			t.Fatalf("compiler-generated fixture %s is missing %q", path, required)
		}
	}
	return set
}

func descriptorFile(t *testing.T, set *descriptorpb.FileDescriptorSet, name string) *descriptorpb.FileDescriptorProto {
	t.Helper()
	for _, file := range set.File {
		if file.GetName() == name {
			return file
		}
	}
	t.Fatalf("descriptor set is missing file %q", name)
	return nil
}

func descriptorMessage(t *testing.T, file *descriptorpb.FileDescriptorProto, name string) *descriptorpb.DescriptorProto {
	t.Helper()
	for _, message := range file.MessageType {
		if message.GetName() == name {
			return message
		}
	}
	t.Fatalf("descriptor file %q is missing message %q", file.GetName(), name)
	return nil
}

func descriptorField(t *testing.T, message *descriptorpb.DescriptorProto, name string) *descriptorpb.FieldDescriptorProto {
	t.Helper()
	for _, field := range message.Field {
		if field.GetName() == name {
			return field
		}
	}
	t.Fatalf("descriptor message %q is missing field %q", message.GetName(), name)
	return nil
}

func writeDescriptorSet(t *testing.T, set *descriptorpb.FileDescriptorSet) string {
	t.Helper()
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return writeDescriptorBytes(t, "records.binpb", raw)
}

func writeDescriptorBytes(t *testing.T, name string, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
