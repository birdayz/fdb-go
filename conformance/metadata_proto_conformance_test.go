package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// metaDataSummary is the structure returned by Java's deserializeMetaData/serializeMetaData.
type metaDataSummary struct {
	Version             int                  `json:"version"`
	SplitLongRecords    bool                 `json:"splitLongRecords"`
	StoreRecordVersions bool                 `json:"storeRecordVersions"`
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

	// Record types (sorted by name)
	rtNames := make([]string, 0)
	for name := range md.RecordTypes() {
		rtNames = append(rtNames, name)
	}
	sort.Strings(rtNames)

	for _, name := range rtNames {
		rts := recordTypeSummary{
			Name: name,
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

	configs := []string{"basic", "with_indexes", "with_former_indexes", "full"}

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
				intBytes := make([]int, len(protoBytes))
				for i, b := range protoBytes {
					intBytes[i] = int(b)
				}

				var javaSummary metaDataSummary
				err = java.InvokeAs(ctx, "deserializeMetaData", map[string]any{
					"protoBytes": intBytes,
				}, &javaSummary)
				Expect(err).NotTo(HaveOccurred())

				// Compare summaries
				Expect(javaSummary.Version).To(Equal(goSummary.Version), "version mismatch")
				Expect(javaSummary.SplitLongRecords).To(Equal(goSummary.SplitLongRecords), "splitLongRecords mismatch")
				Expect(javaSummary.StoreRecordVersions).To(Equal(goSummary.StoreRecordVersions), "storeRecordVersions mismatch")

				// Compare record types by name
				compareRecordTypeNames(javaSummary.RecordTypes, goSummary.RecordTypes)

				// Compare indexes by name
				compareMDIndexes(javaSummary.Indexes, goSummary.Indexes)

				// Compare former indexes
				Expect(len(javaSummary.FormerIndexes)).To(Equal(len(goSummary.FormerIndexes)),
					"former index count mismatch")
				for i, jFI := range javaSummary.FormerIndexes {
					gFI := goSummary.FormerIndexes[i]
					Expect(jFI.FormerName).To(Equal(gFI.FormerName), "former index name mismatch")
					Expect(jFI.SubspaceKey).To(Equal(gFI.SubspaceKey), "former index subspace key mismatch")
					Expect(jFI.AddedVersion).To(Equal(gFI.AddedVersion), "former index added version mismatch")
					Expect(jFI.RemovedVersion).To(Equal(gFI.RemovedVersion), "former index removed version mismatch")
				}
			})

			It("Java serializes, Go deserializes", func() {
				// Java builds and serializes metadata
				var result serializeResult
				err := java.InvokeAs(ctx, "serializeMetaData", map[string]any{
					"config": config,
				}, &result)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.ProtoBytes).NotTo(BeEmpty(), "Java returned empty proto bytes")

				// Convert int array to byte slice
				protoBytes := make([]byte, len(result.ProtoBytes))
				for i, v := range result.ProtoBytes {
					protoBytes[i] = byte(v)
				}

				// Go deserializes
				mdProto := &gen.MetaData{}
				err = proto.Unmarshal(protoBytes, mdProto)
				Expect(err).NotTo(HaveOccurred())

				goMD, err := recordlayer.RecordMetaDataFromProto(mdProto)
				Expect(err).NotTo(HaveOccurred())

				goSummary := extractGoSummary(goMD)
				javaSummary := result.Summary

				// Compare
				Expect(goSummary.Version).To(Equal(javaSummary.Version), "version mismatch")
				Expect(goSummary.SplitLongRecords).To(Equal(javaSummary.SplitLongRecords), "splitLongRecords mismatch")
				Expect(goSummary.StoreRecordVersions).To(Equal(javaSummary.StoreRecordVersions), "storeRecordVersions mismatch")

				compareRecordTypeNames(goSummary.RecordTypes, javaSummary.RecordTypes)
				compareMDIndexes(goSummary.Indexes, javaSummary.Indexes)

				Expect(len(goSummary.FormerIndexes)).To(Equal(len(javaSummary.FormerIndexes)),
					"former index count mismatch")
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
				intBytes := make([]int, len(goBytes))
				for i, b := range goBytes {
					intBytes[i] = int(b)
				}

				raw, err := java.Invoke(ctx, "reserializeMetaData", map[string]any{
					"protoBytes": intBytes,
				})
				Expect(err).NotTo(HaveOccurred())

				// Parse Java's re-serialized bytes
				var reResult struct {
					ProtoBytes []int `json:"protoBytes"`
				}
				err = json.Unmarshal(raw, &reResult)
				Expect(err).NotTo(HaveOccurred())
				Expect(reResult.ProtoBytes).NotTo(BeEmpty(), "Java returned empty re-serialized bytes")

				javaBytes := make([]byte, len(reResult.ProtoBytes))
				for i, v := range reResult.ProtoBytes {
					javaBytes[i] = byte(v)
				}

				// Go deserializes Java's output
				roundtripProto := &gen.MetaData{}
				err = proto.Unmarshal(javaBytes, roundtripProto)
				Expect(err).NotTo(HaveOccurred())

				roundtripMD, err := recordlayer.RecordMetaDataFromProto(roundtripProto)
				Expect(err).NotTo(HaveOccurred())

				roundtripSummary := extractGoSummary(roundtripMD)

				// Must match original
				Expect(roundtripSummary.Version).To(Equal(originalSummary.Version))
				Expect(roundtripSummary.SplitLongRecords).To(Equal(originalSummary.SplitLongRecords))
				Expect(roundtripSummary.StoreRecordVersions).To(Equal(originalSummary.StoreRecordVersions))
				compareRecordTypeNames(roundtripSummary.RecordTypes, originalSummary.RecordTypes)
				compareMDIndexes(roundtripSummary.Indexes, originalSummary.Indexes)
				Expect(len(roundtripSummary.FormerIndexes)).To(Equal(len(originalSummary.FormerIndexes)))
			})
		})
	}
})

func compareRecordTypeNames(actual, expected []recordTypeSummary) {
	// Sort both by name for stable comparison
	sort.Slice(actual, func(i, j int) bool { return actual[i].Name < actual[j].Name })
	sort.Slice(expected, func(i, j int) bool { return expected[i].Name < expected[j].Name })

	Expect(len(actual)).To(Equal(len(expected)), "record type count mismatch: got %d, want %d", len(actual), len(expected))
	for i := range expected {
		Expect(actual[i].Name).To(Equal(expected[i].Name), "record type name mismatch at index %d", i)
	}
}

func compareMDIndexes(actual, expected []indexSummary) {
	sort.Slice(actual, func(i, j int) bool { return actual[i].Name < actual[j].Name })
	sort.Slice(expected, func(i, j int) bool { return expected[i].Name < expected[j].Name })

	Expect(len(actual)).To(Equal(len(expected)), "index count mismatch: got %d, want %d", len(actual), len(expected))
	for i := range expected {
		Expect(actual[i].Name).To(Equal(expected[i].Name), "index name mismatch at index %d", i)
		Expect(actual[i].Type).To(Equal(expected[i].Type), "index type mismatch for %s", actual[i].Name)
		Expect(actual[i].SubspaceKey).To(Equal(expected[i].SubspaceKey), "index subspace key mismatch for %s", actual[i].Name)
		Expect(actual[i].AddedVersion).To(Equal(expected[i].AddedVersion), "index added version mismatch for %s", actual[i].Name)
		Expect(actual[i].LastModifiedVersion).To(Equal(expected[i].LastModifiedVersion), "index last modified version mismatch for %s", actual[i].Name)
	}
}
