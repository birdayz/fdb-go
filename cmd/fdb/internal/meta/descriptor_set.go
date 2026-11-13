package meta

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// DescriptorSetSource builds metadata from a compiled records schema's standard
// Record Layer custom options. The descriptor set must include its imports.
type DescriptorSetSource struct {
	Path        string
	RecordsFile string
	Version     *int32
}

func (s *DescriptorSetSource) Name() string {
	name := "descriptor-set:" + s.Path
	if s.RecordsFile != "" {
		name += "#" + s.RecordsFile
	}
	return name
}

func (s *DescriptorSetSource) Load(ctx context.Context) (*recordlayer.RecordMetaData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Path == "" {
		return nil, fmt.Errorf("descriptor set path is empty")
	}
	if s.Version != nil && *s.Version < 0 {
		return nil, fmt.Errorf("load %s: metadata version must be non-negative", s.Name())
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.Name(), err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(raw, set); err != nil {
		return nil, fmt.Errorf("load %s: parse FileDescriptorSet: %w", s.Name(), err)
	}
	if len(set.File) == 0 {
		return nil, fmt.Errorf("load %s: descriptor set contains no files", s.Name())
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("load %s: resolve descriptors (include imports with buf build --as-file-descriptor-set or protoc --include_imports): %w", s.Name(), err)
	}
	records, err := s.recordsFile(set)
	if err != nil {
		return nil, err
	}
	fd, err := files.FindFileByPath(records.GetName())
	if err != nil {
		return nil, fmt.Errorf("load %s: resolve records file: %w", s.Name(), err)
	}
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd).SetRecordsSourceProto(records).
		SetDependenciesSourceProtos(set.GetFile())
	if s.Version != nil {
		builder.SetVersion(int(*s.Version))
	}
	md, err := builder.Build()
	if err != nil {
		return nil, fmt.Errorf("load %s: build metadata from %s: %w", s.Name(), records.GetName(), err)
	}
	return md, nil
}

func (s *DescriptorSetSource) recordsFile(set *descriptorpb.FileDescriptorSet) (*descriptorpb.FileDescriptorProto, error) {
	var candidates []*descriptorpb.FileDescriptorProto
	for _, file := range set.File {
		if s.RecordsFile != "" {
			if file.GetName() == s.RecordsFile {
				return file, nil
			}
		} else if declaresRecordUnion(file) {
			candidates = append(candidates, file)
		}
	}
	if s.RecordsFile != "" {
		return nil, fmt.Errorf("load %s: records file %q is not in the descriptor set", s.Name(), s.RecordsFile)
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("load %s: no records file declares RecordTypeUnion or (record).usage = UNION; select a records file with --records-file", s.Name())
	}
	names := make([]string, 0, len(candidates))
	for _, file := range candidates {
		names = append(names, file.GetName())
	}
	slices.Sort(names)
	return nil, fmt.Errorf("load %s: multiple records files (%s); select one with --records-file", s.Name(), strings.Join(names, ", "))
}

// This selects candidate files only; the Record Layer builder validates the
// union, record types, primary keys, and index options without a second parser.
func declaresRecordUnion(file *descriptorpb.FileDescriptorProto) bool {
	for _, message := range file.GetMessageType() {
		if message.GetName() == "RecordTypeUnion" {
			return true
		}
		options := message.GetOptions()
		if record, ok := proto.GetExtension(options, gen.E_Record).(*gen.RecordTypeOptions); ok && record.GetUsage() == gen.RecordTypeOptions_UNION {
			return true
		}
	}
	return false
}
