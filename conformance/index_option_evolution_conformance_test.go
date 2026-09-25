//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// javaAnyVerdict is validateMetaDataEvolutionAnyVerdict's reply: valid, or
// the class and message of whatever Java threw.
type javaAnyVerdict struct {
	Valid bool   `json:"valid"`
	Error string `json:"error"`
	Class string `json:"class"`
	// CauseClass and CauseError are the exception's direct cause, empty when
	// it has none.
	CauseClass string `json:"causeClass"`
	CauseError string `json:"causeError"`
}

// optionEvolutionMetaData is the demo records with one index of the given
// kind carrying the given options, at the given meta-data version; the index
// is added at the same builder version on both sides of a shape, so only its
// options differ.
func optionEvolutionMetaData(kind string, options map[string]string, version int) *gen.MetaData {
	var index *recordlayer.Index
	recordType := "Order"
	switch kind {
	case "rank":
		index = recordlayer.NewRankIndex("idx", recordlayer.GroupBy(recordlayer.Field("price")))
	case "rtree":
		index = recordlayer.NewMultidimensionalIndex("idx", recordlayer.Dimensions(
			recordlayer.Concat(recordlayer.Field("coord_x"), recordlayer.Field("coord_y")), 0, 2))
	case "text":
		index = recordlayer.NewTextIndex("idx", recordlayer.Field("name"))
		recordType = "Customer"
	case "value":
		index = recordlayer.NewIndex("idx", recordlayer.Field("price"))
	default:
		Fail("unknown index kind " + kind)
	}
	for k, v := range options {
		index.Options[k] = v
	}
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex(recordType, index)
	md, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	p, err := md.ToProto()
	Expect(err).NotTo(HaveOccurred())
	p.Version = proto.Int32(int32(version))
	return p
}

// The index option checks of Java's MetaDataEvolutionValidator read each
// index's options through the maintainer's config parser
// (RankedSetIndexHelper.getConfig, MultiDimensionalIndexHelper.getConfig,
// TextIndexMaintainer.getIndexTokenizerVersion, Index.isUnique) and compare
// the EFFECTIVE values of the options that changed. Each shape runs through
// Java's validator and Go's: the verdicts agree, a refusal of the same class
// with Java's message the prefix of Go's.
var _ = Describe("Index option changes in meta-data evolution", func() {
	for _, c := range []struct {
		name     string
		kind     string
		old, new map[string]string
		// wantClass is Java's exception class, empty for a valid change.
		wantClass  string
		wantPrefix string
	}{
		// RANK: Java's four hash functions, by exact name.
		{"rank hash unset to JDK", "rank", nil, map[string]string{recordlayer.IndexOptionRankHashFunction: "JDK"}, "", ""},
		{"rank hash JDK to MURMUR3", "rank", map[string]string{recordlayer.IndexOptionRankHashFunction: "JDK"}, map[string]string{recordlayer.IndexOptionRankHashFunction: "MURMUR3"}, "MetaDataException", "rank hash function changed"},
		{"rank hash unset to RANDOM", "rank", nil, map[string]string{recordlayer.IndexOptionRankHashFunction: "RANDOM"}, "MetaDataException", "rank hash function changed"},
		{"rank hash CRC to CRC", "rank", map[string]string{recordlayer.IndexOptionRankHashFunction: "CRC"}, map[string]string{recordlayer.IndexOptionRankHashFunction: "CRC", recordlayer.IndexOptionRankNLevels: "6"}, "", ""},
		{"rank hash name in lower case", "rank", nil, map[string]string{recordlayer.IndexOptionRankHashFunction: "jdk"}, "RecordCoreArgumentException", "hash function not found: jdk"},
		{"rank hash unknown beside another change", "rank", map[string]string{recordlayer.IndexOptionRankHashFunction: "SHA"}, map[string]string{recordlayer.IndexOptionRankHashFunction: "SHA", recordlayer.IndexOptionRankNLevels: "6"}, "RecordCoreArgumentException", "hash function not found: SHA"},
		// RANK: levels by Integer.parseInt and the builder's [2, 8].
		{"rank levels set to the default", "rank", nil, map[string]string{recordlayer.IndexOptionRankNLevels: "6"}, "", ""},
		{"rank levels in Arabic-Indic digits", "rank", nil, map[string]string{recordlayer.IndexOptionRankNLevels: "\u0666"}, "", ""},
		{"rank levels changed", "rank", nil, map[string]string{recordlayer.IndexOptionRankNLevels: "4"}, "MetaDataException", "rank levels changed"},
		{"rank levels not an int", "rank", nil, map[string]string{recordlayer.IndexOptionRankNLevels: " 6"}, "NumberFormatException", `For input string: " 6"`},
		{"rank levels out of range", "rank", nil, map[string]string{recordlayer.IndexOptionRankNLevels: "9"}, "IllegalArgumentException", "levels must be between 2 and 8"},
		// RANK: duplicates by Boolean.parseBoolean.
		{"rank duplicates true in two cases", "rank", map[string]string{recordlayer.IndexOptionRankCountDuplicates: "true"}, map[string]string{recordlayer.IndexOptionRankCountDuplicates: "TRUE"}, "", ""},
		{"rank duplicates set to a non-boolean", "rank", nil, map[string]string{recordlayer.IndexOptionRankCountDuplicates: "yes"}, "", ""},
		{"rank duplicates turned on", "rank", nil, map[string]string{recordlayer.IndexOptionRankCountDuplicates: "True"}, "MetaDataException", "rank count duplicate changed"},
		// R-tree: the Hilbert flag is read only with the storage option set,
		// and then an absent flag is false.
		{"rtree Hilbert false without storage", "rtree", nil, map[string]string{recordlayer.IndexOptionRTreeStoreHilbertValues: "false"}, "", ""},
		{"rtree storage set to its default", "rtree", nil, map[string]string{recordlayer.IndexOptionRTreeStorage: "BY_NODE"}, "", ""},
		{"rtree Hilbert set beside storage", "rtree", map[string]string{recordlayer.IndexOptionRTreeStorage: "BY_NODE"}, map[string]string{recordlayer.IndexOptionRTreeStorage: "BY_NODE", recordlayer.IndexOptionRTreeStoreHilbertValues: "true"}, "MetaDataException", "rtree store Hilbert values changed"},
		{"rtree Hilbert TRUE beside storage", "rtree", map[string]string{recordlayer.IndexOptionRTreeStorage: "BY_SLOT", recordlayer.IndexOptionRTreeStoreHilbertValues: "true"}, map[string]string{recordlayer.IndexOptionRTreeStorage: "BY_SLOT", recordlayer.IndexOptionRTreeStoreHilbertValues: "TRUE"}, "", ""},
		{"rtree storage changed", "rtree", nil, map[string]string{recordlayer.IndexOptionRTreeStorage: "BY_SLOT"}, "MetaDataException", "rtree storage changed"},
		{"rtree storage in lower case", "rtree", nil, map[string]string{recordlayer.IndexOptionRTreeStorage: "by_node"}, "IllegalArgumentException", "No enum constant com.apple.foundationdb.async.rtree.RTree.Storage.by_node"},
		{"rtree node slot index turned on", "rtree", nil, map[string]string{recordlayer.IndexOptionRTreeUseNodeSlotIndex: "TRUE"}, "MetaDataException", "rtree use node slot index changed"},
		{"rtree minM set to its default", "rtree", nil, map[string]string{recordlayer.IndexOptionRTreeMinM: "16"}, "", ""},
		{"rtree minM zero", "rtree", nil, map[string]string{recordlayer.IndexOptionRTreeMinM: "0"}, "MetaDataException", "rtree minM changed"},
		{"rtree maxM changed, reported as minM", "rtree", nil, map[string]string{recordlayer.IndexOptionRTreeMaxM: "33"}, "MetaDataException", "rtree minM changed"},
		{"rtree splitS not an int", "rtree", nil, map[string]string{recordlayer.IndexOptionRTreeSplitS: "2.0"}, "NumberFormatException", `For input string: "2.0"`},
		// TEXT: the tokenizer version by Integer.parseInt. One it refuses never
		// reaches the option check: Java's text index validator parses it at
		// build (the build-verdict specs below).
		// The default tokenizer has only version 0 (a build refuses any
		// other), so these compare spellings of 0.
		{"text tokenizer version set to its default", "text", nil, map[string]string{recordlayer.IndexOptionTextTokenizerVersion: "0"}, "", ""},
		{"text tokenizer version in another spelling", "text", map[string]string{recordlayer.IndexOptionTextTokenizerVersion: "0"}, map[string]string{recordlayer.IndexOptionTextTokenizerVersion: "+\u0660"}, "", ""},
		// Uniqueness by Boolean.valueOf.
		{"unique true in two cases", "value", map[string]string{recordlayer.IndexOptionUnique: "true"}, map[string]string{recordlayer.IndexOptionUnique: "TRUE"}, "", ""},
		{"unique added in upper case", "value", nil, map[string]string{recordlayer.IndexOptionUnique: "TRUE"}, "MetaDataException", "index adds uniqueness constraint"},
		{"unique set to a non-boolean", "value", nil, map[string]string{recordlayer.IndexOptionUnique: "yes"}, "", ""},
	} {
		It("validates as Java does: "+c.name, func() {
			oldProto := optionEvolutionMetaData(c.kind, c.old, 3)
			newProto := optionEvolutionMetaData(c.kind, c.new, 4)
			var java javaAnyVerdict
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "validateMetaDataEvolutionAnyVerdict", map[string]any{
				"oldProtoBytes": bytesToInts(marshalMetaData(oldProto)),
				"newProtoBytes": bytesToInts(marshalMetaData(newProto)),
			}, &java)).To(Succeed())

			oldMD, err := recordlayer.RecordMetaDataFromProto(oldProto)
			Expect(err).NotTo(HaveOccurred())
			newMD, err := recordlayer.RecordMetaDataFromProto(newProto)
			Expect(err).NotTo(HaveOccurred())
			goErr := recordlayer.NewMetaDataEvolutionValidator().Build().Validate(oldMD, newMD)
			fmt.Fprintf(GinkgoWriter, "INDEX_OPTION_EVOLUTION %q java=%t %s %q go=%v\n", c.name, java.Valid, java.Class, java.Error, goErr)

			if c.wantClass == "" {
				Expect(java.Valid).To(BeTrue(), "Java: %s %s", java.Class, java.Error)
				Expect(goErr).NotTo(HaveOccurred())
				return
			}
			Expect(java.Valid).To(BeFalse())
			Expect(java.Class).To(Equal(c.wantClass))
			Expect(java.Error).To(HavePrefix(c.wantPrefix))
			Expect(goErr).To(HaveOccurred())
			var goMessage string
			switch c.wantClass {
			case "MetaDataException":
				var evolErr *recordlayer.MetaDataEvolutionError
				var mdErr *recordlayer.MetaDataError
				switch {
				case errors.As(goErr, &evolErr):
					goMessage = evolErr.Message
				case errors.As(goErr, &mdErr):
					goMessage = mdErr.Message
				default:
					Fail(fmt.Sprintf("Go error %T is not a MetaDataException's: %v", goErr, goErr))
				}
			case "RecordCoreArgumentException":
				var argErr *recordlayer.RecordCoreArgumentError
				Expect(errors.As(goErr, &argErr)).To(BeTrue(), "Go error %T: %v", goErr, goErr)
				goMessage = argErr.Message
			case "NumberFormatException":
				var nfErr *recordlayer.NumberFormatError
				Expect(errors.As(goErr, &nfErr)).To(BeTrue(), "Go error %T: %v", goErr, goErr)
				goMessage = nfErr.Error()
			case "IllegalArgumentException":
				var iaErr *recordlayer.IllegalArgumentError
				Expect(errors.As(goErr, &iaErr)).To(BeTrue(), "Go error %T: %v", goErr, goErr)
				goMessage = iaErr.Message
			default:
				Fail("unmapped Java class " + c.wantClass)
			}
			Expect(goMessage).To(HavePrefix(java.Error))
		})
	}
})

// Java's text index validator parses the tokenizer version when meta-data is
// built (TextIndexMaintainerFactory.java:108-110): a value Integer.parseInt
// refuses, the empty string included, is a MetaDataException at build, and an
// absent one is the global minimum. Go's Build reads it the same way.
var _ = Describe("Text tokenizer version parsed at build", func() {
	for _, c := range []struct {
		name    string
		version string
		valid   bool
	}{
		{"empty", "", false},
		{"with a space", " 1", false},
		{"plus sign", "+0", true},
		{"Arabic-Indic zero", "\u0660", true},
	} {
		It("builds as Java does: tokenizer version "+c.name, func() {
			p := optionEvolutionMetaDataUnbuilt(map[string]string{recordlayer.IndexOptionTextTokenizerVersion: c.version})
			var java javaVerdict
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "buildMetaDataVerdict", map[string]any{
				"protoBytes": bytesToInts(marshalMetaData(p)),
			}, &java)).To(Succeed())
			_, goErr := recordlayer.RecordMetaDataFromProto(p)
			fmt.Fprintf(GinkgoWriter, "TEXT_TOKENIZER_VERSION_BUILD %q java=%t %q go=%v\n", c.name, java.Valid, java.Error, goErr)
			if c.valid {
				Expect(java.Valid).To(BeTrue(), "Java: %s", java.Error)
				Expect(goErr).NotTo(HaveOccurred())
				return
			}
			Expect(java.Valid).To(BeFalse())
			Expect(java.Error).To(Equal("tokenizer version could not be parsed as int"))
			var mdErr *recordlayer.MetaDataError
			Expect(errors.As(goErr, &mdErr)).To(BeTrue(), "Go error %T: %v", goErr, goErr)
			Expect(mdErr.Message).To(HavePrefix(java.Error))
		})
	}
})

// optionEvolutionMetaDataUnbuilt is optionEvolutionMetaData's text index
// serialized without Go's Build, so a shape Go's Build would refuse can still
// be handed to both loaders.
func optionEvolutionMetaDataUnbuilt(options map[string]string) *gen.MetaData {
	p := optionEvolutionMetaData("text", nil, 3)
	for _, idx := range p.GetIndexes() {
		for k, v := range options {
			idx.Options = append(idx.Options, &gen.Index_Option{Key: proto.String(k), Value: proto.String(v)})
		}
	}
	return p
}
