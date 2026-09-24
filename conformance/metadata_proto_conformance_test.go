//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
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
	// Options and Predicate ride the comparison (RFC-202 D11): UNIQUE lives
	// in the options map (IndexOptions.UNIQUE_OPTION) and a sparse index's
	// WHERE lives in the predicate — omitting either lets an index that
	// silently dropped them compare equal to Java's.
	Options   map[string]string `json:"options,omitempty"`
	Predicate string            `json:"predicate,omitempty"`
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
		// Whether the key is EXPLICIT is a property the metadata records, not
		// something to infer by comparing the resolved key against the union
		// field number: the resolved key is tuple-normalized (int64, as Java's
		// TupleTypeUtil.toTupleEquivalentValue makes it) while RecordTypeIndex
		// is an int, so an any-to-int comparison reports every type as
		// explicit regardless of what was set.
		if rt.HasExplicitRecordTypeKey() {
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
		if len(idx.Options) > 0 {
			is.Options = idx.Options
		}
		if pred := idx.GetPredicateProto(); pred != nil {
			if b, err := protoJSONOpts.Marshal(pred); err == nil {
				is.Predicate = string(b)
			} else {
				is.Predicate = fmt.Sprintf("<error: %v>", err)
			}
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

	for _, tc := range []struct {
		name    string
		oldOpts map[string]string
		newOpts map[string]string
		ignored []string
		text    bool
		valid   bool
	}{
		{name: "addition", newOpts: map[string]string{"applicationTag": "new"}, ignored: []string{"applicationTag"}, valid: true},
		{name: "removal", oldOpts: map[string]string{"applicationTag": "old"}, ignored: []string{"applicationTag"}, valid: true},
		{name: "change", oldOpts: map[string]string{"applicationTag": "old"}, newOpts: map[string]string{"applicationTag": "new"}, ignored: []string{"applicationTag"}, valid: true},
		{name: "default strict", newOpts: map[string]string{"applicationTag": "new"}},
		{name: "case sensitive", newOpts: map[string]string{"ApplicationTag": "new"}, ignored: []string{"applicationTag"}},
		{name: "explicit unique opt out", newOpts: map[string]string{recordlayer.IndexOptionUnique: "true"}, ignored: []string{recordlayer.IndexOptionUnique}, valid: true},
		{name: "unrelated unique strict", newOpts: map[string]string{recordlayer.IndexOptionUnique: "true"}, ignored: []string{"applicationTag"}},
		{name: "explicit default tokenizer", text: true, newOpts: map[string]string{recordlayer.IndexOptionTextTokenizerName: recordlayer.DefaultTextTokenizerName}, valid: true},
		{name: "implicit default tokenizer", text: true, oldOpts: map[string]string{recordlayer.IndexOptionTextTokenizerName: recordlayer.DefaultTextTokenizerName}, valid: true},
	} {
		It("matches ignored option evolution in Java: "+tc.name, func() {
			build := func(version int, options map[string]string) *recordlayer.RecordMetaData {
				b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
				b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
				b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
				idx := recordlayer.NewIndex("price", recordlayer.Field("price"))
				idx.AddedVersion, idx.LastModifiedVersion, idx.Options = 1, 1, options
				if tc.text {
					idx.Type = recordlayer.IndexTypeText
					idx.RootExpression = recordlayer.Nest("flower", recordlayer.Field("type"))
				}
				b.AddIndex("Order", idx)
				b.SetVersion(version)
				md, err := b.Build()
				Expect(err).NotTo(HaveOccurred())
				return md
			}
			old, new := build(1, tc.oldOpts), build(2, tc.newOpts)
			validator := recordlayer.NewMetaDataEvolutionValidator().SetIgnoredIndexOptions(tc.ignored).Build()
			Expect(validator.Validate(old, new) == nil).To(Equal(tc.valid))
			bytesFor := func(md *recordlayer.RecordMetaData) []byte {
				p, err := md.ToProto()
				Expect(err).NotTo(HaveOccurred())
				bytes, err := proto.Marshal(p)
				Expect(err).NotTo(HaveOccurred())
				return bytes
			}
			oldBytes, newBytes := bytesFor(old), bytesFor(new)
			var result struct {
				Valid bool   `json:"valid"`
				Error string `json:"error"`
			}
			Expect(java.InvokeAs(ctx, "validateMetaDataEvolutionOptions", map[string]any{
				"oldProtoBytes": bytesToInts(oldBytes), "newProtoBytes": bytesToInts(newBytes),
				"ignoredOptions": append([]string{}, tc.ignored...),
			}, &result)).To(Succeed())
			Expect(result.Valid).To(Equal(tc.valid), result.Error)
			if !tc.valid {
				Expect(result.Error).NotTo(BeEmpty())
			}
			// Re-serialize through Java, then validate the Java-produced metadata
			// in Go too; neither engine's expected result comes from the other.
			fromJava := func(bytes []byte) *recordlayer.RecordMetaData {
				var reserialized serializeResult
				Expect(java.InvokeAs(ctx, "reserializeMetaData", map[string]any{"protoBytes": bytesToInts(bytes)}, &reserialized)).To(Succeed())
				Expect(reserialized.ProtoBytes).NotTo(BeEmpty())
				wire := make([]byte, len(reserialized.ProtoBytes))
				for i, b := range reserialized.ProtoBytes {
					Expect(b).To(BeNumerically(">=", 0))
					Expect(b).To(BeNumerically("<=", 255))
					wire[i] = byte(b)
				}
				var p gen.MetaData
				Expect(proto.Unmarshal(wire, &p)).To(Succeed())
				md, err := recordlayer.RecordMetaDataFromProto(&p)
				Expect(err).NotTo(HaveOccurred())
				return md
			}
			Expect(validator.Validate(fromJava(oldBytes), fromJava(newBytes)) == nil).To(Equal(tc.valid))
			fmt.Fprintf(GinkgoWriter, "IGNORED_OPTIONS_INTEROP %s valid=%t\n", tc.name, tc.valid)
		})
	}

	for _, tc := range []struct {
		expression recordlayer.KeyExpression
		message    string
	}{
		{recordlayer.Field("price"), "non-string type"},
		{recordlayer.FanOut("tags"), "repeated field"},
	} {
		It("rejects invalid TEXT bodies in Go and Java: "+tc.message, func() {
			p, err := buildGoMetaData("with_indexes").ToProto()
			Expect(err).NotTo(HaveOccurred())
			found := false
			for _, idx := range p.Indexes {
				if idx.GetName() == "Order$price" {
					found = true
					idx.Type = proto.String(recordlayer.IndexTypeText)
					idx.RootExpression = tc.expression.ToKeyExpression()
				}
			}
			Expect(found).To(BeTrue())
			_, err = recordlayer.RecordMetaDataFromProto(p)
			Expect(err).To(MatchError(ContainSubstring(tc.message)))
			bytes, err := proto.Marshal(p)
			Expect(err).NotTo(HaveOccurred())
			var summary metaDataSummary
			err = java.InvokeAs(ctx, "deserializeMetaData", map[string]any{"protoBytes": bytesToInts(bytes)}, &summary)
			Expect(err).To(MatchError(ContainSubstring(tc.message)))
			fmt.Fprintf(GinkgoWriter, "TEXT_BODY_INTEROP rejected=%s\n", tc.message)
		})
	}

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
		// RFC-202 D11: options carry UNIQUE; the predicate carries a sparse
		// index's WHERE. Both compare, or a dropped clause reads as equal.
		Expect(normalizedOptionsMap(actual[i].Options)).To(Equal(normalizedOptionsMap(expected[i].Options)),
			"index options mismatch for %s", actual[i].Name)
		Expect(normalizePredicateJSON(actual[i].Predicate)).To(Equal(normalizePredicateJSON(expected[i].Predicate)),
			"index predicate mismatch for %s", actual[i].Name)
	}
}

// normalizedOptionsMap treats nil and empty as the same absent-options state.
func normalizedOptionsMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return map[string]string{}
	}
	return m
}

// normalizePredicateJSON re-marshals a Predicate JSON through Go's proto
// binary layer, normalizing proto2 field presence exactly like
// normalizeKeyExprJSON does for key expressions. "" (no predicate) maps to "".
func normalizePredicateJSON(s string) string {
	if s == "" {
		return ""
	}
	pred := &gen.Predicate{}
	if err := protojson.Unmarshal([]byte(s), pred); err != nil {
		return s
	}
	clearProto2Defaults(pred.ProtoReflect())
	b, err := protoJSONOpts.Marshal(pred)
	if err != nil {
		return s
	}
	return string(b)
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

// buildUnionInteropSchema deliberately keeps union field names while swapping
// their referenced types. Names must not override protobuf field-number identity.
func buildUnionInteropSchema(version int, swapped, incompatible bool) *recordlayer.RecordMetaData {
	field := func(name string, number int32, kind descriptorpb.FieldDescriptorProto_Type, target string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Type: kind.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}
		if target != "" {
			f.TypeName = proto.String(target)
		}
		return f
	}
	stringKind, numberKind := descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_TYPE_INT64
	if swapped && !incompatible {
		stringKind, numberKind = numberKind, stringKind
	}
	message := func(name string, kind descriptorpb.FieldDescriptorProto_Type) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name:  proto.String(name),
			Field: []*descriptorpb.FieldDescriptorProto{field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""), field("payload", 2, kind, ""), field("state", 3, descriptorpb.FieldDescriptorProto_TYPE_ENUM, ".union_interop."+name+".State")},
			EnumType: []*descriptorpb.EnumDescriptorProto{{Name: proto.String("State"), Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("UNKNOWN"), Number: proto.Int32(0)}, {Name: proto.String("READY"), Number: proto.Int32(1)},
			}}},
		}
	}
	first, second := ".union_interop.Alpha", ".union_interop.Beta"
	if swapped {
		first, second = second, first
	}
	options := &descriptorpb.MessageOptions{}
	proto.SetExtension(options, gen.E_Record, &gen.RecordTypeOptions{Usage: gen.RecordTypeOptions_UNION.Enum()})
	envelope := &descriptorpb.DescriptorProto{Name: proto.String("Envelope"), Options: options, Field: []*descriptorpb.FieldDescriptorProto{
		field("_Alpha", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, first), field("_Beta", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, second), field("high", 9, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, first), field("middle", 4, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, first),
	}}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{Name: proto.String("union_interop.proto"), Package: proto.String("union_interop"), Syntax: proto.String("proto2"), Dependency: []string{"record_metadata_options.proto"}, MessageType: []*descriptorpb.DescriptorProto{message("Alpha", stringKind), message("Beta", numberKind), envelope}}, protoregistry.GlobalFiles)
	Expect(err).NotTo(HaveOccurred())
	b := recordlayer.NewRecordMetaDataBuilder().SetRecordsWithUnionName(fd, "Envelope")
	b.GetRecordType("Alpha").SetPrimaryKey(recordlayer.Field("id"))
	b.GetRecordType("Beta").SetPrimaryKey(recordlayer.Field("id"))
	index := recordlayer.NewIndex("by_payload", recordlayer.Field("payload"))
	index.AddedVersion, index.LastModifiedVersion = 1, 1
	indexedType := "Alpha"
	if swapped {
		indexedType = "Beta"
	}
	b.AddIndex(indexedType, index)
	b.SetVersion(version)
	md, err := b.Build()
	Expect(err).NotTo(HaveOccurred())
	return md
}

var _ = Describe("Union metadata Java interoperability", func() {
	for _, incompatible := range []bool{false, true} {
		It(fmt.Sprintf("compares custom-union swaps against Java, incompatible=%t", incompatible), func() {
			old, new := buildUnionInteropSchema(1, false, false), buildUnionInteropSchema(2, true, incompatible)
			Expect(recordlayer.ValidateEvolution(old, new) == nil).To(Equal(!incompatible))
			bytesFor := func(md *recordlayer.RecordMetaData) []byte {
				p, err := md.ToProto()
				Expect(err).NotTo(HaveOccurred())
				data, err := proto.Marshal(p)
				Expect(err).NotTo(HaveOccurred())
				return data
			}
			var result struct {
				Valid bool   `json:"valid"`
				Error string `json:"error"`
			}
			java := NewJavaInvoker()
			ctx := context.Background()
			Expect(java.InvokeAs(ctx, "validateMetaDataEvolutionOptions", map[string]any{"oldProtoBytes": bytesToInts(bytesFor(old)), "newProtoBytes": bytesToInts(bytesFor(new)), "ignoredOptions": []string{}}, &result)).To(Succeed())
			Expect(result.Valid).To(Equal(!incompatible), result.Error)
			fromJava := func(md *recordlayer.RecordMetaData) *recordlayer.RecordMetaData {
				var result serializeResult
				Expect(java.InvokeAs(ctx, "reserializeMetaData", map[string]any{"protoBytes": bytesToInts(bytesFor(md))}, &result)).To(Succeed())
				Expect(result.ProtoBytes).NotTo(BeEmpty())
				var p gen.MetaData
				Expect(proto.Unmarshal(intsToBytes(result.ProtoBytes), &p)).To(Succeed())
				decoded, err := recordlayer.RecordMetaDataFromProto(&p)
				Expect(err).NotTo(HaveOccurred())
				return decoded
			}
			javaOld, javaNew := fromJava(old), fromJava(new)
			Expect(recordlayer.ValidateEvolution(javaOld, javaNew) == nil).To(Equal(!incompatible))
			Expect(javaNew.GetRecordType("Beta").GetRecordTypeKey()).To(Equal(int64(1)))
			state := javaNew.GetRecordType("Beta").Descriptor.Fields().ByName("state").Enum()
			Expect(state.FullName()).To(Equal(protoreflect.FullName("union_interop.Beta.State")))
			Expect(state.Values().ByNumber(1).Name()).To(Equal(protoreflect.Name("READY")))
			indexed := javaNew.RecordTypesForIndex(javaNew.GetIndex("by_payload"))
			Expect(indexed).To(HaveLen(1))
			Expect(indexed[0].Name).To(Equal("Beta"))
			Expect(javaNew.GetUnionFieldForRecordType(javaNew.GetRecordType("Beta")).Number()).To(Equal(protoreflect.FieldNumber(9)))
			fmt.Fprintf(GinkgoWriter, "UNION_METADATA_INTEROP incompatible=%t valid=%t\n", incompatible, result.Valid)
		})
	}
})

var _ = Describe("Union name reuse and index association", func() {
	for _, wrongIndex := range []bool{false, true} {
		It(fmt.Sprintf("preserves renamed identity when the old name is reused, wrong-index=%t", wrongIndex), func() {
			old := buildUnionInteropSchema(1, false, false)
			oldProto, err := old.ToProto()
			Expect(err).NotTo(HaveOccurred())
			current, err := buildUnionInteropSchema(2, true, false).ToProto()
			Expect(err).NotTo(HaveOccurred())
			// Alpha -> Beta, Beta -> Gamma; a genuinely new Alpha occupies tag3.
			for _, message := range current.Records.MessageType {
				if message.GetName() == "Alpha" {
					message.Name = proto.String("Gamma")
					for _, field := range message.Field {
						if field.GetName() == "state" {
							field.TypeName = proto.String(".union_interop.Gamma.State")
						}
					}
				}
				if message.GetName() == "Envelope" {
					for _, field := range message.Field {
						if field.GetNumber() == 2 {
							field.TypeName = proto.String(".union_interop.Gamma")
						}
					}
					message.Field = append(message.Field, &descriptorpb.FieldDescriptorProto{Name: proto.String("_newAlpha"), Number: proto.Int32(3), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".union_interop.Alpha"), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()})
				}
			}
			for _, message := range oldProto.Records.MessageType {
				if message.GetName() == "Alpha" {
					current.Records.MessageType = append(current.Records.MessageType, proto.Clone(message).(*descriptorpb.DescriptorProto))
				}
			}
			for _, rt := range current.RecordTypes {
				if rt.GetName() == "Alpha" {
					rt.Name = proto.String("Gamma")
				}
			}
			for _, rt := range oldProto.RecordTypes {
				if rt.GetName() == "Alpha" {
					added := proto.Clone(rt).(*gen.RecordType)
					added.SinceVersion = proto.Int32(2)
					current.RecordTypes = append(current.RecordTypes, added)
				}
			}
			if wrongIndex {
				for _, index := range current.Indexes {
					if index.GetName() == "by_payload" {
						index.RecordType = []string{"Gamma"}
					}
				}
			}
			new, err := recordlayer.RecordMetaDataFromProto(current)
			Expect(err).NotTo(HaveOccurred())
			Expect(new.GetRecordType("Alpha").GetRecordTypeKey()).To(Equal(int64(3)))
			Expect(new.GetRecordType("Beta").GetRecordTypeKey()).To(Equal(int64(1)))
			Expect(new.GetRecordType("Gamma").GetRecordTypeKey()).To(Equal(int64(2)))
			Expect(recordlayer.ValidateEvolution(old, new) == nil).To(Equal(!wrongIndex))
			oldBytes, err := proto.Marshal(oldProto)
			Expect(err).NotTo(HaveOccurred())
			newBytes, err := proto.Marshal(current)
			Expect(err).NotTo(HaveOccurred())
			java := NewJavaInvoker()
			ctx := context.Background()
			var result struct {
				Valid bool   `json:"valid"`
				Error string `json:"error"`
			}
			Expect(java.InvokeAs(ctx, "validateMetaDataEvolutionOptions", map[string]any{"oldProtoBytes": bytesToInts(oldBytes), "newProtoBytes": bytesToInts(newBytes), "ignoredOptions": []string{}}, &result)).To(Succeed())
			Expect(result.Valid).To(Equal(!wrongIndex), result.Error)
			var rewritten serializeResult
			Expect(java.InvokeAs(ctx, "reserializeMetaData", map[string]any{"protoBytes": bytesToInts(newBytes)}, &rewritten)).To(Succeed())
			Expect(rewritten.ProtoBytes).NotTo(BeEmpty())
			var p gen.MetaData
			Expect(proto.Unmarshal(intsToBytes(rewritten.ProtoBytes), &p)).To(Succeed())
			reopened, err := recordlayer.RecordMetaDataFromProto(&p)
			Expect(err).NotTo(HaveOccurred())
			Expect(recordlayer.ValidateEvolution(old, reopened) == nil).To(Equal(!wrongIndex))
			fmt.Fprintf(GinkgoWriter, "UNION_NAME_REUSE_INTEROP wrong-index=%t valid=%t\n", wrongIndex, result.Valid)
		})
	}
})

var _ = Describe("Multi-type TEXT Java validation", func() {
	for _, shape := range []string{"valid", "numeric", "repeated", "missing"} {
		It("checks every covered record descriptor: "+shape, func() {
			p, err := buildUnionInteropSchema(1, false, false).ToProto()
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Indexes).To(HaveLen(1))
			p.Indexes[0].RecordType = []string{"Alpha", "Beta"}
			p.Indexes[0].Type = proto.String(recordlayer.IndexTypeText)
			p.Records.MessageType[1].Field[1].Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
			message := ""
			switch shape {
			case "numeric":
				p.Records.MessageType[1].Field[1].Type = descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
				message = "non-string type"
			case "repeated":
				for _, record := range p.Records.MessageType[:2] {
					record.Field[1].Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
				}
				p.Indexes[0].RootExpression = recordlayer.FanOut("payload").ToKeyExpression()
				message = "repeated field"
			case "missing":
				p.Records.MessageType[1].Field = p.Records.MessageType[1].Field[:1]
				message = "does not have field"
			}
			_, goErr := recordlayer.RecordMetaDataFromProto(p)
			encoded, err := proto.Marshal(p)
			Expect(err).NotTo(HaveOccurred())
			var summary metaDataSummary
			javaErr := NewJavaInvoker().InvokeAs(context.Background(), "deserializeMetaData", map[string]any{"protoBytes": bytesToInts(encoded)}, &summary)
			if shape == "valid" {
				Expect(goErr).NotTo(HaveOccurred())
				Expect(javaErr).NotTo(HaveOccurred())
			} else {
				Expect(goErr).To(MatchError(ContainSubstring(message)))
				if shape == "missing" {
					// Both engines: KeyExpression.InvalidExpressionException,
					// with one text.
					var invalid *JavaError
					Expect(errors.As(javaErr, &invalid)).To(BeTrue())
					Expect(invalid.ExceptionClass).To(Equal("InvalidExpressionException"))
					Expect(invalid.Message).To(Equal("Descriptor Beta does not have field: payload"))
					var keyErr *recordlayer.KeyExpressionError
					Expect(errors.As(goErr, &keyErr)).To(BeTrue())
					Expect(keyErr.Message).To(Equal(invalid.Message))
				} else {
					Expect(javaErr).To(MatchError(ContainSubstring(message)))
				}
			}
		})
	}
})

// Java's RecordMetaDataBuilder.validateRecords (validateDataTypes + validateUnion,
// RecordMetaDataBuilder.java:635-747) refuses these records descriptors whenever
// metadata is built from them; Go's port raises the same message. Each shape is
// built into a MetaData proto and given to both engines' proto loaders.
var _ = Describe("Java refuses the same records descriptors", func() {
	field := func(name string, number int32, kind descriptorpb.FieldDescriptorProto_Type, target string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(number), Type: kind.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
		if target != "" {
			f.TypeName = proto.String(target)
		}
		return f
	}
	usage := func(u gen.RecordTypeOptions_Usage) *descriptorpb.MessageOptions {
		o := &descriptorpb.MessageOptions{}
		proto.SetExtension(o, gen.E_Record, &gen.RecordTypeOptions{Usage: u.Enum()})
		return o
	}
	base := func() *descriptorpb.FileDescriptorProto {
		return &descriptorpb.FileDescriptorProto{
			Name: proto.String("validate_records.proto"), Package: proto.String("vr"), Syntax: proto.String("proto2"),
			Dependency: []string{"record_metadata_options.proto"},
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("T"), Field: []*descriptorpb.FieldDescriptorProto{field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")}},
				{Name: proto.String("Envelope"), Options: usage(gen.RecordTypeOptions_UNION), Field: []*descriptorpb.FieldDescriptorProto{
					field("_T", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T"),
				}},
			},
		}
	}
	// index adds a VALUE index on field id of the given record type to the
	// metadata, so a shape can ask what an index fault does beside a records one.
	index := func(recordType string) func(md *gen.MetaData) {
		return func(md *gen.MetaData) {
			md.Indexes = append(md.Indexes, &gen.Index{
				Name: proto.String("I"), RecordType: []string{recordType},
				RootExpression: recordlayer.Field("id").ToKeyExpression(),
				Type:           proto.String("value"), AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
			})
		}
	}
	shapes := []struct {
		name   string
		mutate func(f *descriptorpb.FileDescriptorProto)
		want   string
		// md, when set, changes the metadata around the records file; wantClass,
		// when set, is the Java exception class (MetaDataException otherwise).
		md        func(md *gen.MetaData)
		wantClass string
		// goRefuses, when set, is a declared divergence: the target loads the file
		// and Go refuses it with this message (ws-j-design.md section 4c, shape d).
		goRefuses string
	}{
		{"valid", func(*descriptorpb.FileDescriptorProto) {}, "", nil, "", ""},
		{"uint32", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[0].Field = append(f.MessageType[0].Field, field("u", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT32, ""))
		}, "Field u in message vr.T has illegal unsigned type UINT32", nil, "", ""},
		{"fixed64", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[0].Field = append(f.MessageType[0].Field, field("u", 2, descriptorpb.FieldDescriptorProto_TYPE_FIXED64, ""))
		}, "Field u in message vr.T has illegal unsigned type FIXED64", nil, "", ""},
		{"unsigned in a nested message", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name:  proto.String("Inner"),
				Field: []*descriptorpb.FieldDescriptorProto{field("n", 1, descriptorpb.FieldDescriptorProto_TYPE_FIXED32, "")},
			})
			f.MessageType[0].Field = append(f.MessageType[0].Field, field("inner", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.Inner"))
		}, "Field n in message vr.Inner has illegal unsigned type FIXED32", nil, "", ""},
		{"repeated union field", func(f *descriptorpb.FileDescriptorProto) {
			r := field("_T2", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T")
			r.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
			f.MessageType[1].Field = append(f.MessageType[1].Field, r)
		}, "Union field _T2 should not be repeated", nil, "", ""},
		// fetchUnionDescriptor (:322-358) decides these before validateUnion runs, so
		// a RecordTypeUnion message beside a usage=UNION one is a second union
		// candidate, not a union field.
		{"a second union candidate named RecordTypeUnion", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name: proto.String("RecordTypeUnion"), Options: usage(gen.RecordTypeOptions_RECORD),
				Field: []*descriptorpb.FieldDescriptorProto{field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")},
			})
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("_R", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.RecordTypeUnion"))
		}, "Only one union descriptor is allowed", nil, "", ""},
		{"the relational union as its own union field", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[1].Name = proto.String("RecordTypeUnion")
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("_R", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.RecordTypeUnion"))
		}, "Union message type RecordTypeUnion cannot be a union field.", nil, "", ""},
		// Precedence: the first fault wins, data types before the union, the union's
		// fields in field order.
		{"a non-message union field before a repeated one", func(f *descriptorpb.FileDescriptorProto) {
			r := field("_T2", 3, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T")
			r.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("x", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""), r)
		}, "Union field x is not a message", nil, "", ""},
		{"a repeated union field before a non-message one", func(f *descriptorpb.FileDescriptorProto) {
			r := field("_T2", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T")
			r.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
			f.MessageType[1].Field = append(f.MessageType[1].Field, r, field("x", 3, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""))
		}, "Union field _T2 should not be repeated", nil, "", ""},
		{"an unsigned field before a union fault", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[0].Field = append(f.MessageType[0].Field, field("u", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT32, ""))
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("x", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""))
		}, "Field u in message vr.T has illegal unsigned type UINT32", nil, "", ""},
		{"two usage=UNION messages", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name: proto.String("Other"), Options: usage(gen.RecordTypeOptions_UNION),
				Field: []*descriptorpb.FieldDescriptorProto{field("_T", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T")},
			})
		}, "Only one union descriptor is allowed", nil, "", ""},
		{"RecordTypeUnion with NESTED usage", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name: proto.String("RecordTypeUnion"), Options: usage(gen.RecordTypeOptions_NESTED),
				Field: []*descriptorpb.FieldDescriptorProto{field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")},
			})
		}, "Message type RecordTypeUnion cannot have NESTED usage", nil, "", ""},
		// A records file a Go build before the validateRecords port stored: the
		// union named by Go's old default, UnionDescriptor, with no usage option.
		{"a union named UnionDescriptor without a usage option", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[1].Name = proto.String("UnionDescriptor")
			f.MessageType[1].Options = nil
		}, "Union descriptor is required", nil, "", ""},
		{"no union", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = f.MessageType[:1]
		}, "Union descriptor is required", nil, "", ""},
		{"a union found by the name RecordTypeUnion", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[1].Name = proto.String("RecordTypeUnion")
			f.MessageType[1].Options = nil
		}, "", nil, "", ""},
		{"nested usage as a union field", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name: proto.String("N"), Options: usage(gen.RecordTypeOptions_NESTED),
				Field: []*descriptorpb.FieldDescriptorProto{field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")},
			})
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("_N", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.N"))
		}, "Union field _N has type N which is not a record", nil, "", ""},
		{"record usage missing from the union", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name: proto.String("R"), Options: usage(gen.RecordTypeOptions_RECORD),
				Field: []*descriptorpb.FieldDescriptorProto{field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")},
			})
		}, "Record message type R must be a union field.", nil, "", ""},
		{"non-message union field", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("x", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""))
		}, "Union field x is not a message", nil, "", ""},
		// Load order (RecordMetaDataBuilder.java:272-278): the subspace-key
		// settings, then the records, then the indexes. So an index fault never
		// hides a records fault, and a settings fault hides both.
		{"a valid records file with an index", func(*descriptorpb.FileDescriptorProto) {}, "", index("T"), "", ""},
		{"an index on an unknown record type", func(*descriptorpb.FileDescriptorProto) {}, "Unknown record type Nope", index("Nope"), "", ""},
		{"an index on an unknown record type beside a union fault", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("x", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""))
		}, "Union field x is not a message", index("Nope"), "", ""},
		// Per index, Java resolves the record types BEFORE it reads the index
		// (loadProtoExceptRecords, RecordMetaDataBuilder.java:187-219), and a
		// RecordType entry naming no record type is refused (:221, :986-990).
		{"an unknown record type on an index with a repeated option", func(*descriptorpb.FileDescriptorProto) {}, "Unknown record type Nope", func(md *gen.MetaData) {
			index("Nope")(md)
			md.Indexes[0].Options = []*gen.Index_Option{{Key: proto.String("a"), Value: proto.String("1")}, {Key: proto.String("a"), Value: proto.String("2")}}
		}, "", ""},
		{"an unknown record type on an index before a repeated option on the next", func(*descriptorpb.FileDescriptorProto) {}, "Unknown record type Nope", func(md *gen.MetaData) {
			index("Nope")(md)
			index("T")(md)
			md.Indexes[1].Name = proto.String("J")
			md.Indexes[1].Options = []*gen.Index_Option{{Key: proto.String("a"), Value: proto.String("1")}, {Key: proto.String("a"), Value: proto.String("2")}}
		}, "", ""},
		{"a record type entry naming no record type", func(*descriptorpb.FileDescriptorProto) {}, "Unknown record type Nope", func(md *gen.MetaData) {
			md.RecordTypes = append(md.RecordTypes, &gen.RecordType{Name: proto.String("Nope"), PrimaryKey: recordlayer.Field("id").ToKeyExpression()})
		}, "", ""},
		// A message named UnionDescriptor none of whose fields made a record type
		// before the upgrade (a SQL table or STRUCT of that name with scalar
		// columns) framed nothing and is not shape (d): both engines load the file.
		{"a record type named UnionDescriptor in the usage=UNION union", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name:  proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")},
			})
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("_UnionDescriptor", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.UnionDescriptor"))
		}, "", func(md *gen.MetaData) {
			md.RecordTypes = append(md.RecordTypes, &gen.RecordType{Name: proto.String("UnionDescriptor"), PrimaryKey: recordlayer.Field("id").ToKeyExpression()})
		}, "", ""},
		{"a nested message named UnionDescriptor", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name:  proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{field("x", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")},
			})
			f.MessageType[0].Field = append(f.MessageType[0].Field, field("s", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.UnionDescriptor"))
		}, "", nil, "", ""},
		{"a subspace-key-counter fault beside a union fault", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("x", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""))
		}, "subspaceKeyCounter is set but usesSubspaceKeyCounter is not set in the meta-data proto", func(md *gen.MetaData) { md.SubspaceKeyCounter = proto.Int64(3) }, "", ""},
		// Shape (d): the pre-upgrade Go loader took UnionDescriptor for the union (it
		// tried that name first) and the target takes the usage=UNION message.
		{"a message named UnionDescriptor beside the usage=UNION union", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name:  proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{field("_T", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T")},
			})
		}, "", nil, "", "records file names two union candidates: UnionDescriptor (the pre-upgrade Go choice) and Envelope " +
			"(the union the target uses); edit it so that only the union its records were written with is a candidate"},
		// Shape (d2): no message carries the usage option, the target takes the one
		// named RecordTypeUnion, and the pre-upgrade Go loader took UnionDescriptor.
		{"a message named UnionDescriptor beside a union named RecordTypeUnion", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[1].Name = proto.String("RecordTypeUnion")
			f.MessageType[1].Options = nil
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name:  proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{field("_T", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T")},
			})
		}, "", nil, "", "records file names two union candidates: UnionDescriptor (the pre-upgrade Go choice) and RecordTypeUnion " +
			"(the union the target uses); edit it so that only the union its records were written with is a candidate"},
		// Shape (d) wherever the UnionDescriptor sits, when one of its fields made a
		// record type before the upgrade: held by an envelope message, held by a
		// record type, or a table of that name with a column typed by a table.
		{"a legacy UnionDescriptor held by an envelope message", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name:  proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{field("_T", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T")},
			}, &descriptorpb.DescriptorProto{
				Name:  proto.String("Batch"),
				Field: []*descriptorpb.FieldDescriptorProto{field("records", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.UnionDescriptor")},
			})
		}, "", nil, "", "records file names two union candidates: UnionDescriptor (the pre-upgrade Go choice) and Envelope " +
			"(the union the target uses); edit it so that only the union its records were written with is a candidate"},
		{"a legacy UnionDescriptor held by a record type", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name:  proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{field("_T", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T")},
			})
			f.MessageType[0].Field = append(f.MessageType[0].Field, field("batch", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.UnionDescriptor"))
		}, "", nil, "", "records file names two union candidates: UnionDescriptor (the pre-upgrade Go choice) and Envelope " +
			"(the union the target uses); edit it so that only the union its records were written with is a candidate"},
		{"a table named UnionDescriptor with a table-typed column", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name: proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
					field("a", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T"),
				},
			})
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("_UnionDescriptor", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.UnionDescriptor"))
		}, "", func(md *gen.MetaData) {
			md.RecordTypes = append(md.RecordTypes, &gen.RecordType{Name: proto.String("UnionDescriptor"), PrimaryKey: recordlayer.Field("id").ToKeyExpression()})
		}, "", "records file names two union candidates: UnionDescriptor (the pre-upgrade Go choice) and Envelope " +
			"(the union the target uses); edit it so that only the union its records were written with is a candidate"},
		// The stored path replays the pre-upgrade loader, which refused a record
		// type with no stored primary key and an index on a record type its loop did
		// not make: such a file framed nothing, and both engines load it.
		{"a table named UnionDescriptor with a STRUCT-typed column", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name:  proto.String("S"),
				Field: []*descriptorpb.FieldDescriptorProto{field("x", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")},
			}, &descriptorpb.DescriptorProto{
				Name: proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
					field("s", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.S"),
				},
			})
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("_UnionDescriptor", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.UnionDescriptor"))
		}, "", func(md *gen.MetaData) {
			md.RecordTypes = append(md.RecordTypes, &gen.RecordType{Name: proto.String("UnionDescriptor"), PrimaryKey: recordlayer.Field("id").ToKeyExpression()})
		}, "", ""},
		{"a table named UnionDescriptor with a table-typed column and an index", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name: proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
					field("a", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T"),
				},
			})
			f.MessageType[1].Field = append(f.MessageType[1].Field, field("_UnionDescriptor", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.UnionDescriptor"))
		}, "", func(md *gen.MetaData) {
			md.RecordTypes = append(md.RecordTypes, &gen.RecordType{Name: proto.String("UnionDescriptor"), PrimaryKey: recordlayer.Field("id").ToKeyExpression()})
			index("UnionDescriptor")(md)
		}, "", ""},
		// One none of whose fields made a record type framed nothing, even where
		// no record type reaches it: both engines load the file.
		{"a scalar UnionDescriptor only an unreached message holds", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name:  proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{field("x", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")},
			}, &descriptorpb.DescriptorProto{
				Name:  proto.String("Holder"),
				Field: []*descriptorpb.FieldDescriptorProto{field("s", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.UnionDescriptor")},
			})
		}, "", nil, "", ""},
	}
	for _, shape := range shapes {
		It("refuses or builds: "+shape.name, func() {
			fdp := base()
			shape.mutate(fdp)
			md := &gen.MetaData{
				Records: fdp,
				RecordTypes: []*gen.RecordType{{
					Name: proto.String("T"), PrimaryKey: recordlayer.Field("id").ToKeyExpression(),
				}},
				Version: proto.Int32(1),
			}
			if shape.md != nil {
				shape.md(md)
			}
			_, goErr := recordlayer.RecordMetaDataFromProto(md)
			encoded, err := proto.Marshal(md)
			Expect(err).NotTo(HaveOccurred())
			var summary metaDataSummary
			javaErr := NewJavaInvoker().InvokeAs(context.Background(), "deserializeMetaData", map[string]any{"protoBytes": bytesToInts(encoded)}, &summary)
			fmt.Fprintf(GinkgoWriter, "VALIDATE_RECORDS %s go=%v java=%v\n", shape.name, goErr, javaErr)
			if shape.goRefuses != "" {
				// Shape (d): Go read the file's message named UnionDescriptor
				// as the union before the upgrade and refused the file after
				// it; it now loads it as the target does (RFC-257,
				// "Verification and review gates" item 9: data only a
				// pre-release Go build wrote is not supported).
				Expect(javaErr).NotTo(HaveOccurred(), "the target loads %s", shape.name)
				Expect(goErr).NotTo(HaveOccurred(), "Go loads %s as the target does", shape.name)
				return
			}
			if shape.want == "" {
				Expect(goErr).NotTo(HaveOccurred())
				Expect(javaErr).NotTo(HaveOccurred())
				return
			}
			var je *JavaError
			Expect(errors.As(javaErr, &je)).To(BeTrue(), "Java refuses %s: %v", shape.name, javaErr)
			wantClass := shape.wantClass
			if wantClass == "" {
				wantClass = "MetaDataException"
			}
			Expect(je.ExceptionClass).To(Equal(wantClass))
			Expect(je.Message).To(Equal(shape.want), "measured Java message")
			var me *recordlayer.MetaDataError
			Expect(errors.As(goErr, &me)).To(BeTrue(), "Go refuses %s: %v", shape.name, goErr)
			Expect(me.Message).To(Equal(shape.want))
		})
	}
})
