package embedded

import (
	"sort"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// partitionCacheComponent is empty for ordinary metadata, preserving its exact
// existing cache key. Partition width is metadata identity, not a SQL literal.
func partitionCacheComponent(md *recordlayer.RecordMetaData) (string, error) {
	properties, err := metadataQueryProperties(md)
	return properties.partitionComponent, err
}

func scanPartitionCacheComponent(md *recordlayer.RecordMetaData) (string, error) {
	if md == nil {
		return "", nil
	}
	names := make([]string, 0, len(md.RecordTypes()))
	for name := range md.RecordTypes() {
		names = append(names, name)
	}
	sort.Strings(names)
	var component strings.Builder
	for _, name := range names {
		rt := md.GetRecordType(name)
		layout, err := recordlayer.DescribePartitionKey(rt)
		if err != nil {
			return "", err
		}
		if layout == nil {
			continue
		}
		writeLengthPrefixed(&component, name)
		writeLengthPrefixed(&component, string((tuple.Tuple{int64(1), rt.GetRecordTypeKey(), int64(layout.SourceField().Number()), int64(layout.SourceField().Kind()), layout.Spec().IntervalWidth()}).Pack()))
		pk, err := proto.MarshalOptions{Deterministic: true}.Marshal(layout.PhysicalPrimaryKey().ToKeyExpression())
		if err != nil {
			return "", &recordlayer.PartitionMetadataError{RecordType: name, Message: "cannot encode physical primary key", Cause: err}
		}
		writeLengthPrefixed(&component, string(pk))
		indexes := append([]*recordlayer.Index(nil), rt.GetAllIndexes()...)
		sort.Slice(indexes, func(i, j int) bool { return indexes[i].Name < indexes[j].Name })
		for _, index := range indexes {
			if _, err := layout.ProjectIndex(index); err != nil {
				return "", err
			}
			encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(index.RootExpression.ToKeyExpression())
			if err != nil {
				return "", &recordlayer.PartitionMetadataError{RecordType: name, IndexName: index.Name, Message: "cannot encode physical index key", Cause: err}
			}
			writeLengthPrefixed(&component, index.Name)
			writeLengthPrefixed(&component, string(encoded))
		}
		// Delimit the variable-length index list from the next record type.
		writeLengthPrefixed(&component, "")
	}
	return component.String(), nil
}

func logicalIndexExpression(md *recordlayer.RecordMetaData, index *recordlayer.Index) (recordlayer.KeyExpression, error) {
	for _, rt := range md.RecordTypesForIndex(index) {
		layout, err := recordlayer.DescribePartitionKey(rt)
		if err != nil {
			return nil, err
		}
		if layout == nil {
			continue
		}
		projection, err := layout.ProjectIndex(index)
		if err != nil {
			return nil, err
		}
		return projection.LogicalExpression(), nil
	}
	return index.RootExpression, nil
}
