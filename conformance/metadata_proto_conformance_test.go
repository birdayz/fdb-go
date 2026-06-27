//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// metaDataSummary is the structure returned by Java's deserializeMetaData/serializeMetaData.
type metaDataSummary struct {
	Version             int                  `json:"version"`
	SplitLongRecords    bool                 `json:"splitLongRecords"`
	StoreRecordVersions bool                 `json:"storeRecordVersions"`
	RecordCountKey      *string              `json:"recordCountKey,omitempty"`
	RecordTypes         []recordTypeSummary  `json:"recordTypes"`
	Indexes             []indexSummary       `json:"indexes"`
	FormerIndexes       []formerIndexSummary `json:"formerIndexes"`
}

type recordTypeSummary struct {
	Name            string `json:"name"`
	SinceVersion    *int   `json:"sinceVersion,omitempty"`
	ExplicitTypeKey any    `json:"explicitTypeKey,omitempty"`
}

type indexSummary struct {
	Name                string `json:"name"`
	Type                string `json:"type"`
	RootExpression      string `json:"rootExpression"`
	SubspaceKey         string `json:"subspaceKey"`
	AddedVersion        int    `json:"addedVersion"`
	LastModifiedVersion int    `json:"lastModifiedVersion"`
}

type formerIndexSummary struct {
	FormerName     string `json:"formerName"`
	SubspaceKey    string `json:"subspaceKey"`
	AddedVersion   int    `json:"addedVersion"`
	RemovedVersion int    `json:"removedVersion"`
}

type serializeResult struct {
	ProtoBytes []int           `json:"protoBytes"`
	Summary    metaDataSummary `json:"summary"`
}

var protoJSONOpts = protojson.MarshalOptions{EmitDefaultValues: true}

func keyExprString(ke recordlayer.KeyExpression) string {
	b, err := protoJSONOpts.Marshal(ke.ToKeyExpression())
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	return string(b)
}

// clearProto2Defaults recursively clears proto2 optional fields that are
// explicitly set to their default value. This normalizes "not set" vs
// "explicitly set to default" so that JSON comparison is stable across
// Go (never sets defaults) and Java (materializes defaults on roundtrip).
func clearProto2Defaults(m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			// Recurse into each message in repeated message fields
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				clearProto2Defaults(list.Get(i).Message())
			}
		case fd.IsMap() && fd.MapValue().Kind() == protoreflect.MessageKind:
			// Recurse into message values in map fields
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				clearProto2Defaults(mv.Message())
				return true
			})
		case fd.IsList(), fd.IsMap():
			// skip non-message collections
		case fd.Kind() == protoreflect.MessageKind:
			clearProto2Defaults(v.Message())
		case fd.HasPresence() && fd.Cardinality() != protoreflect.Required:
			// Only clear optional fields — required fields must stay set.
			def := fd.Default()
			switch fd.Kind() {
			case protoreflect.EnumKind:
				if v.Enum() == def.Enum() {
					m.Clear(fd)
				}
			case protoreflect.BytesKind:
				if string(v.Bytes()) == string(def.Bytes()) {
					m.Clear(fd)
				}
			default:
				if v.Interface() == def.Interface() {
					m.Clear(fd)
				}
			}
		}
		return true
	})
}

// normalizeKeyExprJSON re-marshals a KeyExpression JSON through Go's proto binary layer
// then back to JSON, normalizing field presence (proto2 defaults) and whitespace.
func normalizeKeyExprJSON(s string) string {
	ke := &gen.KeyExpression{}
	if err := protojson.Unmarshal([]byte(s), ke); err != nil {
		return s
	}
	// Proto binary roundtrip normalizes proto2 field presence
	raw, err := proto.Marshal(ke)
	if err != nil {
		return s
	}
	ke2 := &gen.KeyExpression{}
	if err := proto.Unmarshal(raw, ke2); err != nil {
		return s
	}
	// Clear proto2 fields set to their default values — Java may materialize
	// these defaults during reserialization while Go leaves them unset.
	clearProto2Defaults(ke2.ProtoReflect())

	b, err := protojson.Marshal(ke2)
	if err != nil {
		return s
	}
	// Re-marshal through generic JSON to normalize whitespace
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func bytesToInts(b []byte) []int {
	ints := make([]int, len(b))
	for i, v := range b {
		ints[i] = int(v)
	}
	return ints
}

func intsToBytes(ints []int) []byte {
	b := make([]byte, len(ints))
	for i, v := range ints {
		b[i] = byte(v)
	}
	return b
}

// buildGoMetaData creates a Go RecordMetaData matching a specific config.
// MUST match the Java buildMetaData() configs exactly.
func buildGoMetaData(config string) *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))

	switch config {
	case "basic":
		// Just primary keys
	case "with_indexes":
		builder.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
		builder.AddIndex("Order", recordlayer.NewIndex("Order$quantity_price",
			recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price"))))
		builder.AddIndex("Customer", recordlayer.NewIndex("Customer$name", recordlayer.Field("name")))
	case "with_former_indexes":
		builder.AddIndex("Order", recordlayer.NewIndex("temp_idx", recordlayer.Field("price")))
		builder.RemoveIndex("temp_idx")
		builder.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	case "full":
		builder.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
		builder.AddIndex("Order", recordlayer.NewIndex("Order$quantity_price",
			recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price"))))
		builder.AddIndex("Customer", recordlayer.NewIndex("Customer$name", recordlayer.Field("name")))
		builder.AddIndex("Order", recordlayer.NewIndex("temp_idx", recordlayer.Field("quantity")))
		builder.RemoveIndex("temp_idx")
		builder.SetSplitLongRecords(true)
		builder.SetStoreRecordVersions(true)
	case "with_universal_index":
		builder.AddUniversalIndex(recordlayer.NewIndex("global_price", recordlayer.Field("price")))
	case "with_record_count":
		builder.SetRecordCountKey(recordlayer.EmptyKey())
	case "with_explicit_type_key":
		builder.GetRecordType("Order").SetRecordTypeKey(int64(42))
	default:
		panic("unknown config: " + config)
	}

	builder.SetVersion(5)
	md, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	return md
}

// extractGoSummary extracts a summary from Go RecordMetaData matching Java's format.
func extractGoSummary(md *recordlayer.RecordMetaData) metaDataSummary {
	s := metaDataSummary{
		Version:             md.Version(),
		SplitLongRecords:    md.IsSplitLongRecords(),
		StoreRecordVersions: md.IsStoreRecordVersions(),
	}

	// Record count key
	if rck := md.GetRecordCountKey(); rck != nil {
		str := keyExprString(rck)
		s.RecordCountKey = &str
	}

	// Record types (sorted by name)
	rtNames := make([]string, 0)
	for name := range md.RecordTypes() {
		rtNames = append(rtNames, name)
	}
	sort.Strings(rtNames)

	for _, name := range rtNames {
		rt := md.GetRecordType(name)
		rts := recordTypeSummary{
			Name: name,
		}
		if rt.SinceVersion != 0 {
			sv := rt.SinceVersion
			rts.SinceVersion = &sv
		}
		if rt.GetRecordTypeKey() != rt.RecordTypeIndex {
			rts.ExplicitTypeKey = rt.GetRecordTypeKey()
		}
		s.RecordTypes = append(s.RecordTypes, rts)
	}

	// Indexes (sorted by name)
	allIndexes := md.GetAllIndexes()
	idxNames := make([]string, 0, len(allIndexes))
	for name := range allIndexes {
		idxNames = append(idxNames, name)
	}
	sort.Strings(idxNames)

	for _, name := range idxNames {
		idx := allIndexes[name]
		is := indexSummary{
			Name:                idx.Name,
			Type:                idx.Type,
			RootExpression:      keyExprString(idx.RootExpression),
			SubspaceKey:         fmt.Sprint(idx.SubspaceTupleKey()),
			AddedVersion:        idx.AddedVersion,
			LastModifiedVersion: idx.LastModifiedVersion,
		}
		s.Indexes = append(s.Indexes, is)
	}

	// Former indexes
	for _, fi := range md.GetFormerIndexes() {
		s.FormerIndexes = append(s.FormerIndexes, formerIndexSummary{
			FormerName:     fi.FormerName,
			SubspaceKey:    fmt.Sprint(fi.SubspaceKey),
			AddedVersion:   fi.AddedVersion,
			RemovedVersion: fi.RemovedVersion,
		})
	}

	return s
}

var _ = Describe("RecordMetaData Proto Serialization Conformance", func() {
	var (
		ctx  context.Context
		java *JavaInvoker
	)

	BeforeEach(func() {
		ctx = context.Background()
		java = NewJavaInvoker()
	})

	configs := []string{"basic", "with_indexes", "with_former_indexes", "full", "with_universal_index", "with_record_count", "with_explicit_type_key"}
	for _, cfg := range configs {
		config := cfg // capture loop variable

		Describe(fmt.Sprintf("config=%s", config), func() {
			It("Go serializes, Java deserializes", func() {
				// Build Go metadata
				goMD := buildGoMetaData(config)
				goSummary := extractGoSummary(goMD)

				// Serialize to proto bytes
				mdProto, err := goMD.ToProto()
				Expect(err).NotTo(HaveOccurred())
				protoBytes, err := proto.Marshal(mdProto)
				Expect(err).NotTo(HaveOccurred())

				// Send to Java for deserialization
				var javaSummary metaDataSummary
				err = java.InvokeAs(ctx, "deserializeMetaData", map[string]any{
					"protoBytes": bytesToInts(protoBytes),
				}, &javaSummary)
				Expect(err).NotTo(HaveOccurred())

				// Compare summaries
				compareSummaries(javaSummary, goSummary)
			})

			It("Java serializes, Go deserializes", func() {
				// Java builds and serializes metadata
				var result serializeResult
				err := java.InvokeAs(ctx, "serializeMetaData", map[string]any{
					"config": config,
				}, &result)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.ProtoBytes).NotTo(BeEmpty(), "Java returned empty proto bytes")

				// Go deserializes
				mdProto := &gen.MetaData{}
				err = proto.Unmarshal(intsToBytes(result.ProtoBytes), mdProto)
				Expect(err).NotTo(HaveOccurred())

				goMD, err := recordlayer.RecordMetaDataFromProto(mdProto)
				Expect(err).NotTo(HaveOccurred())

				goSummary := extractGoSummary(goMD)
				javaSummary := result.Summary

				// Compare
				compareSummaries(goSummary, javaSummary)
			})

			It("Go serializes, Java re-serializes, Go deserializes roundtrip", func() {
				// Go builds metadata
				originalMD := buildGoMetaData(config)
				originalSummary := extractGoSummary(originalMD)

				// Serialize with Go
				mdProto, err := originalMD.ToProto()
				Expect(err).NotTo(HaveOccurred())
				goBytes, err := proto.Marshal(mdProto)
				Expect(err).NotTo(HaveOccurred())

				// Send to Java, Java deserializes and re-serializes
				var reResult serializeResult
				err = java.InvokeAs(ctx, "reserializeMetaData", map[string]any{
					"protoBytes": bytesToInts(goBytes),
				}, &reResult)
				Expect(err).NotTo(HaveOccurred())
				Expect(reResult.ProtoBytes).NotTo(BeEmpty(), "Java returned empty re-serialized bytes")

				// Verify Java's interpretation matches original
				compareSummaries(reResult.Summary, originalSummary)

				// Go deserializes Java's output
				roundtripProto := &gen.MetaData{}
				err = proto.Unmarshal(intsToBytes(reResult.ProtoBytes), roundtripProto)
				Expect(err).NotTo(HaveOccurred())

				roundtripMD, err := recordlayer.RecordMetaDataFromProto(roundtripProto)
				Expect(err).NotTo(HaveOccurred())

				roundtripSummary := extractGoSummary(roundtripMD)

				// Must match original
				compareSummaries(roundtripSummary, originalSummary)
			})
		})
	}
})

func compareSummaries(actual, expected metaDataSummary) {
	Expect(actual.Version).To(Equal(expected.Version), "version mismatch")
	Expect(actual.SplitLongRecords).To(Equal(expected.SplitLongRecords), "splitLongRecords mismatch")
	Expect(actual.StoreRecordVersions).To(Equal(expected.StoreRecordVersions), "storeRecordVersions mismatch")

	// Record count key
	if expected.RecordCountKey == nil {
		Expect(actual.RecordCountKey).To(BeNil(), "recordCountKey: expected nil but got %v", actual.RecordCountKey)
	} else {
		Expect(actual.RecordCountKey).NotTo(BeNil(), "recordCountKey: expected %v but got nil", *expected.RecordCountKey)
		Expect(normalizeKeyExprJSON(*actual.RecordCountKey)).To(Equal(normalizeKeyExprJSON(*expected.RecordCountKey)), "recordCountKey mismatch")
	}

	compareRecordTypes(actual.RecordTypes, expected.RecordTypes)
	compareMDIndexes(actual.Indexes, expected.Indexes)
	compareFormerIndexes(actual.FormerIndexes, expected.FormerIndexes)
}

func compareRecordTypes(actual, expected []recordTypeSummary) {
	// Sort both by name for stable comparison
	sort.Slice(actual, func(i, j int) bool { return actual[i].Name < actual[j].Name })
	sort.Slice(expected, func(i, j int) bool { return expected[i].Name < expected[j].Name })

	Expect(len(actual)).To(Equal(len(expected)), "record type count mismatch: got %d, want %d", len(actual), len(expected))
	for i := range expected {
		Expect(actual[i].Name).To(Equal(expected[i].Name), "record type name mismatch at index %d", i)

		// SinceVersion
		if expected[i].SinceVersion == nil {
			Expect(actual[i].SinceVersion).To(BeNil(),
				"record type %s: sinceVersion expected nil but got %v", expected[i].Name, actual[i].SinceVersion)
		} else {
			Expect(actual[i].SinceVersion).NotTo(BeNil(),
				"record type %s: sinceVersion expected %d but got nil", expected[i].Name, *expected[i].SinceVersion)
			Expect(*actual[i].SinceVersion).To(Equal(*expected[i].SinceVersion),
				"record type %s: sinceVersion mismatch", expected[i].Name)
		}

		// ExplicitTypeKey
		Expect(fmt.Sprint(actual[i].ExplicitTypeKey)).To(Equal(fmt.Sprint(expected[i].ExplicitTypeKey)),
			"record type %s: explicitTypeKey mismatch", expected[i].Name)
	}
}

func compareMDIndexes(actual, expected []indexSummary) {
	sort.Slice(actual, func(i, j int) bool { return actual[i].Name < actual[j].Name })
	sort.Slice(expected, func(i, j int) bool { return expected[i].Name < expected[j].Name })

	Expect(len(actual)).To(Equal(len(expected)), "index count mismatch: got %d, want %d", len(actual), len(expected))
	for i := range expected {
		Expect(actual[i].Name).To(Equal(expected[i].Name), "index name mismatch at index %d", i)
		Expect(actual[i].Type).To(Equal(expected[i].Type), "index type mismatch for %s", actual[i].Name)
		Expect(normalizeKeyExprJSON(actual[i].RootExpression)).To(Equal(normalizeKeyExprJSON(expected[i].RootExpression)), "index root expression mismatch for %s", actual[i].Name)
		Expect(actual[i].SubspaceKey).To(Equal(expected[i].SubspaceKey), "index subspace key mismatch for %s", actual[i].Name)
		Expect(actual[i].AddedVersion).To(Equal(expected[i].AddedVersion), "index added version mismatch for %s", actual[i].Name)
		Expect(actual[i].LastModifiedVersion).To(Equal(expected[i].LastModifiedVersion), "index last modified version mismatch for %s", actual[i].Name)
	}
}

func compareFormerIndexes(actual, expected []formerIndexSummary) {
	sort.Slice(actual, func(i, j int) bool { return actual[i].FormerName < actual[j].FormerName })
	sort.Slice(expected, func(i, j int) bool { return expected[i].FormerName < expected[j].FormerName })

	Expect(len(actual)).To(Equal(len(expected)),
		"former index count mismatch: got %d, want %d", len(actual), len(expected))
	for i := range expected {
		Expect(actual[i].FormerName).To(Equal(expected[i].FormerName), "former index name mismatch at index %d", i)
		Expect(actual[i].SubspaceKey).To(Equal(expected[i].SubspaceKey), "former index subspace key mismatch for %s", expected[i].FormerName)
		Expect(actual[i].AddedVersion).To(Equal(expected[i].AddedVersion), "former index added version mismatch for %s", expected[i].FormerName)
		Expect(actual[i].RemovedVersion).To(Equal(expected[i].RemovedVersion), "former index removed version mismatch for %s", expected[i].FormerName)
	}
}
