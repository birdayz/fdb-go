package recordlayer

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("MetaDataEvolutionValidator (vector)", func() {
	buildMetaData := func(version int, configure func(b *RecordMetaDataBuilder)) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		if configure != nil {
			configure(builder)
		}
		builder.SetVersion(version)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	// VECTOR (HNSW) option validation
	It("rejects HNSW structural option change (metric)", func() {
		old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("idx_vec", Field("price"))
			idx.Type = IndexTypeVector
			idx.Options = map[string]string{IndexOptionVectorMetric: "EUCLIDEAN_METRIC"}
			b.AddIndex("Order", idx)
		})

		new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("idx_vec", Field("price"))
			idx.Type = IndexTypeVector
			idx.Options = map[string]string{IndexOptionVectorMetric: "COSINE_METRIC"}
			b.AddIndex("Order", idx)
		})

		err := ValidateEvolution(old, new)
		var evolErr *MetaDataEvolutionError
		Expect(errors.As(err, &evolErr)).To(BeTrue())
		Expect(evolErr.Message).To(ContainSubstring("hnswMetric"))
	})

	It("allows HNSW runtime option change (concurrency)", func() {
		old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("idx_vec", Field("price"))
			idx.Type = IndexTypeVector
			idx.Options = map[string]string{IndexOptionHNSWMaxNumConcurrentNodeFetches: "16"}
			b.AddIndex("Order", idx)
		})

		new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("idx_vec", Field("price"))
			idx.Type = IndexTypeVector
			idx.Options = map[string]string{IndexOptionHNSWMaxNumConcurrentNodeFetches: "32"}
			b.AddIndex("Order", idx)
		})

		err := ValidateEvolution(old, new)
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects HNSW structural change when value actually differs", func() {
		old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("idx_vec", Field("price"))
			idx.Type = IndexTypeVector
			idx.Options = map[string]string{IndexOptionHNSWM: "16"}
			b.AddIndex("Order", idx)
		})

		new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("idx_vec", Field("price"))
			idx.Type = IndexTypeVector
			idx.Options = map[string]string{IndexOptionHNSWM: "32", IndexOptionHNSWMMax: "32"}
			b.AddIndex("Order", idx)
		})

		err := ValidateEvolution(old, new)
		var evolErr *MetaDataEvolutionError
		Expect(errors.As(err, &evolErr)).To(BeTrue())
		Expect(evolErr.Message).To(ContainSubstring("hnswM"))
	})

	// SPFresh (RFC-094) option validation: every structural option immutable.
	It("rejects SPFresh structural option change (spfreshLmax)", func() {
		old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("idx_spf", Field("price"))
			idx.Type = IndexTypeVectorSPFresh
			idx.Options = map[string]string{IndexOptionSPFreshLmax: "256"}
			b.AddIndex("Order", idx)
		})
		new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("idx_spf", Field("price"))
			idx.Type = IndexTypeVectorSPFresh
			idx.Options = map[string]string{IndexOptionSPFreshLmax: "512"}
			b.AddIndex("Order", idx)
		})
		err := ValidateEvolution(old, new)
		var evolErr *MetaDataEvolutionError
		Expect(errors.As(err, &evolErr)).To(BeTrue())
		Expect(evolErr.Message).To(ContainSubstring("spfreshLmax"))
	})

	It("rejects SPFresh alpha change (the closure-sizing invariant)", func() {
		old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("idx_spf", Field("price"))
			idx.Type = IndexTypeVectorSPFresh
			idx.Options = map[string]string{IndexOptionSPFreshAlpha: "1.2"}
			b.AddIndex("Order", idx)
		})
		new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("idx_spf", Field("price"))
			idx.Type = IndexTypeVectorSPFresh
			idx.Options = map[string]string{IndexOptionSPFreshAlpha: "1.5"}
			b.AddIndex("Order", idx)
		})
		err := ValidateEvolution(old, new)
		var evolErr *MetaDataEvolutionError
		Expect(errors.As(err, &evolErr)).To(BeTrue())
		Expect(evolErr.Message).To(ContainSubstring("spfreshAlpha"))
	})

	It("accepts unchanged SPFresh options across versions", func() {
		build := func(version int) *RecordMetaData {
			return buildMetaData(version, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_spf", Field("price"))
				idx.Type = IndexTypeVectorSPFresh
				idx.Options = map[string]string{
					IndexOptionSPFreshNumDimensions: "128",
					IndexOptionSPFreshLmax:          "256",
				}
				b.AddIndex("Order", idx)
			})
		}
		Expect(ValidateEvolution(build(1), build(2))).To(Succeed())
	})
})

// TestVectorOptionsComparedByEffectiveValue pins validateVectorIndexOptions to
// Java's disallowChange (VectorIndexOptionsHelper.java:120-147): an option set
// to its default where it was unspecified is no change, a changed value is
// refused with Java's message, and an unrecognized metric name is not taken
// for the default metric the lenient parser falls back to.
func TestVectorOptionsComparedByEffectiveValue(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		old, new map[string]string
		option   string
		refused  bool
	}{
		{"M set to its default", map[string]string{}, map[string]string{IndexOptionHNSWM: "16"}, IndexOptionHNSWM, false},
		{"M changed", map[string]string{}, map[string]string{IndexOptionHNSWM: "8"}, IndexOptionHNSWM, true},
		{"metric set to its default", map[string]string{}, map[string]string{IndexOptionVectorMetric: "EUCLIDEAN_METRIC"}, IndexOptionVectorMetric, false},
		{"metric changed", map[string]string{}, map[string]string{IndexOptionVectorMetric: "COSINE_METRIC"}, IndexOptionVectorMetric, true},
		{"a metric under its alias, the default", map[string]string{}, map[string]string{"vectorMetric": "EUCLIDEAN_METRIC"}, "vectorMetric", false},
		{"a metric under its alias, changed", map[string]string{}, map[string]string{"vectorMetric": "COSINE_METRIC"}, "vectorMetric", true},
		{"RaBitQ extra bits set to their default", map[string]string{}, map[string]string{IndexOptionHNSWRaBitQNumExBits: "4"}, IndexOptionHNSWRaBitQNumExBits, false},
		// Integer.parseInt reads any Unicode decimal digit.
		{
			"RaBitQ extra bits in fullwidth digits",
			map[string]string{IndexOptionHNSWUseRaBitQ: "true", IndexOptionHNSWRaBitQNumExBits: "8"},
			map[string]string{IndexOptionHNSWUseRaBitQ: "true", IndexOptionHNSWRaBitQNumExBits: "\uff18"},
			IndexOptionHNSWRaBitQNumExBits, false,
		},
		{
			"RaBitQ extra bits changed",
			map[string]string{IndexOptionHNSWUseRaBitQ: "true", IndexOptionHNSWRaBitQNumExBits: "8"},
			map[string]string{IndexOptionHNSWUseRaBitQ: "true", IndexOptionHNSWRaBitQNumExBits: "4"},
			IndexOptionHNSWRaBitQNumExBits, true,
		},
		{
			"RaBitQ extra bits changed under their alias",
			map[string]string{IndexOptionHNSWUseRaBitQ: "true"},
			map[string]string{IndexOptionHNSWUseRaBitQ: "true", "vectorRaBitQNumExBits": "5"},
			"vectorRaBitQNumExBits", true,
		},
	} {
		oldIdx := &Index{Name: "v", Type: IndexTypeVector, Options: c.old}
		newIdx := &Index{Name: "v", Type: IndexTypeVector, Options: c.new}
		changed := map[string]bool{c.option: true}
		err := validateVectorIndexOptions(oldIdx, newIdx, changed)
		var evolErr *MetaDataEvolutionError
		switch {
		case c.refused && (!errors.As(err, &evolErr) || !strings.HasPrefix(evolErr.Message, "attempted to change immutable vector index option")):
			t.Errorf("%s: %v, want Java's refusal", c.name, err)
		case c.refused && !strings.Contains(evolErr.Message, fmt.Sprintf("option=%q", c.option)):
			// Java's disallowChange names the name that changed, an alias
			// included.
			t.Errorf("%s: %v, want the refusal to name %s", c.name, err, c.option)
		case !c.refused && (err != nil || changed[c.option]):
			t.Errorf("%s: %v (still changed: %t), want admitted and handled", c.name, err, changed[c.option])
		}
	}
	// A metric no Metric constant names is Java's parse failure, Metric.valueOf's
	// IllegalArgumentException, not taken for the default.
	err := validateVectorIndexOptions(&Index{Name: "v", Type: IndexTypeVector, Options: map[string]string{}},
		&Index{Name: "v", Type: IndexTypeVector, Options: map[string]string{IndexOptionVectorMetric: "COSINE"}},
		map[string]bool{IndexOptionVectorMetric: true})
	var iae *IllegalArgumentError
	if !errors.As(err, &iae) || iae.Message != "No enum constant com.apple.foundationdb.linear.Metric.COSINE" {
		t.Errorf("an unrecognized metric: %v, want Metric.valueOf's refusal", err)
	}
}
