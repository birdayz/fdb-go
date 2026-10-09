package vectorindex

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	. "github.com/onsi/gomega"
)

var _ = Describe("MetaDataEvolutionValidator (vector)", func() {
	buildMetaData := func(version int, configure func(b *recordlayer.RecordMetaDataBuilder)) *recordlayer.RecordMetaData {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
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
		old := buildMetaData(1, func(b *recordlayer.RecordMetaDataBuilder) {
			idx := recordlayer.NewIndex("idx_vec", recordlayer.Field("price"))
			idx.Type = recordlayer.IndexTypeVector
			idx.Options = map[string]string{recordlayer.IndexOptionVectorMetric: "EUCLIDEAN_METRIC"}
			b.AddIndex("Order", idx)
		})

		new := buildMetaData(2, func(b *recordlayer.RecordMetaDataBuilder) {
			idx := recordlayer.NewIndex("idx_vec", recordlayer.Field("price"))
			idx.Type = recordlayer.IndexTypeVector
			idx.Options = map[string]string{recordlayer.IndexOptionVectorMetric: "COSINE_METRIC"}
			b.AddIndex("Order", idx)
		})

		err := recordlayer.ValidateEvolution(old, new)
		var evolErr *recordlayer.MetaDataEvolutionError
		Expect(errors.As(err, &evolErr)).To(BeTrue())
		Expect(evolErr.Message).To(ContainSubstring("hnswMetric"))
	})

	It("allows HNSW runtime option change (concurrency)", func() {
		old := buildMetaData(1, func(b *recordlayer.RecordMetaDataBuilder) {
			idx := recordlayer.NewIndex("idx_vec", recordlayer.Field("price"))
			idx.Type = recordlayer.IndexTypeVector
			idx.Options = map[string]string{recordlayer.IndexOptionHNSWMaxNumConcurrentNodeFetches: "16"}
			b.AddIndex("Order", idx)
		})

		new := buildMetaData(2, func(b *recordlayer.RecordMetaDataBuilder) {
			idx := recordlayer.NewIndex("idx_vec", recordlayer.Field("price"))
			idx.Type = recordlayer.IndexTypeVector
			idx.Options = map[string]string{recordlayer.IndexOptionHNSWMaxNumConcurrentNodeFetches: "32"}
			b.AddIndex("Order", idx)
		})

		err := recordlayer.ValidateEvolution(old, new)
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects HNSW structural change when value actually differs", func() {
		old := buildMetaData(1, func(b *recordlayer.RecordMetaDataBuilder) {
			idx := recordlayer.NewIndex("idx_vec", recordlayer.Field("price"))
			idx.Type = recordlayer.IndexTypeVector
			idx.Options = map[string]string{recordlayer.IndexOptionHNSWM: "16"}
			b.AddIndex("Order", idx)
		})

		new := buildMetaData(2, func(b *recordlayer.RecordMetaDataBuilder) {
			idx := recordlayer.NewIndex("idx_vec", recordlayer.Field("price"))
			idx.Type = recordlayer.IndexTypeVector
			idx.Options = map[string]string{recordlayer.IndexOptionHNSWM: "32", recordlayer.IndexOptionHNSWMMax: "32"}
			b.AddIndex("Order", idx)
		})

		err := recordlayer.ValidateEvolution(old, new)
		var evolErr *recordlayer.MetaDataEvolutionError
		Expect(errors.As(err, &evolErr)).To(BeTrue())
		Expect(evolErr.Message).To(ContainSubstring("hnswM"))
	})

	// SPFresh (RFC-094) option validation: every structural option immutable.
	It("rejects SPFresh structural option change (spfreshLmax)", func() {
		old := buildMetaData(1, func(b *recordlayer.RecordMetaDataBuilder) {
			idx := recordlayer.NewIndex("idx_spf", recordlayer.Field("price"))
			idx.Type = recordlayer.IndexTypeVectorSPFresh
			idx.Options = map[string]string{recordlayer.IndexOptionSPFreshLmax: "256"}
			b.AddIndex("Order", idx)
		})
		new := buildMetaData(2, func(b *recordlayer.RecordMetaDataBuilder) {
			idx := recordlayer.NewIndex("idx_spf", recordlayer.Field("price"))
			idx.Type = recordlayer.IndexTypeVectorSPFresh
			idx.Options = map[string]string{recordlayer.IndexOptionSPFreshLmax: "512"}
			b.AddIndex("Order", idx)
		})
		err := recordlayer.ValidateEvolution(old, new)
		var evolErr *recordlayer.MetaDataEvolutionError
		Expect(errors.As(err, &evolErr)).To(BeTrue())
		Expect(evolErr.Message).To(ContainSubstring("spfreshLmax"))
	})

	It("rejects SPFresh alpha change (the closure-sizing invariant)", func() {
		old := buildMetaData(1, func(b *recordlayer.RecordMetaDataBuilder) {
			idx := recordlayer.NewIndex("idx_spf", recordlayer.Field("price"))
			idx.Type = recordlayer.IndexTypeVectorSPFresh
			idx.Options = map[string]string{recordlayer.IndexOptionSPFreshAlpha: "1.2"}
			b.AddIndex("Order", idx)
		})
		new := buildMetaData(2, func(b *recordlayer.RecordMetaDataBuilder) {
			idx := recordlayer.NewIndex("idx_spf", recordlayer.Field("price"))
			idx.Type = recordlayer.IndexTypeVectorSPFresh
			idx.Options = map[string]string{recordlayer.IndexOptionSPFreshAlpha: "1.5"}
			b.AddIndex("Order", idx)
		})
		err := recordlayer.ValidateEvolution(old, new)
		var evolErr *recordlayer.MetaDataEvolutionError
		Expect(errors.As(err, &evolErr)).To(BeTrue())
		Expect(evolErr.Message).To(ContainSubstring("spfreshAlpha"))
	})

	It("accepts unchanged SPFresh options across versions", func() {
		build := func(version int) *recordlayer.RecordMetaData {
			return buildMetaData(version, func(b *recordlayer.RecordMetaDataBuilder) {
				idx := recordlayer.NewIndex("idx_spf", recordlayer.Field("price"))
				idx.Type = recordlayer.IndexTypeVectorSPFresh
				idx.Options = map[string]string{
					recordlayer.IndexOptionSPFreshNumDimensions: "128",
					recordlayer.IndexOptionSPFreshLmax:          "256",
				}
				b.AddIndex("Order", idx)
			})
		}
		Expect(recordlayer.ValidateEvolution(build(1), build(2))).To(Succeed())
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
		{"M set to its default", map[string]string{}, map[string]string{recordlayer.IndexOptionHNSWM: "16"}, recordlayer.IndexOptionHNSWM, false},
		{"M changed", map[string]string{}, map[string]string{recordlayer.IndexOptionHNSWM: "8"}, recordlayer.IndexOptionHNSWM, true},
		{"metric set to its default", map[string]string{}, map[string]string{recordlayer.IndexOptionVectorMetric: "EUCLIDEAN_METRIC"}, recordlayer.IndexOptionVectorMetric, false},
		{"metric changed", map[string]string{}, map[string]string{recordlayer.IndexOptionVectorMetric: "COSINE_METRIC"}, recordlayer.IndexOptionVectorMetric, true},
		{"a metric under its alias, the default", map[string]string{}, map[string]string{"vectorMetric": "EUCLIDEAN_METRIC"}, "vectorMetric", false},
		{"a metric under its alias, changed", map[string]string{}, map[string]string{"vectorMetric": "COSINE_METRIC"}, "vectorMetric", true},
		{"RaBitQ extra bits set to their default", map[string]string{}, map[string]string{recordlayer.IndexOptionHNSWRaBitQNumExBits: "4"}, recordlayer.IndexOptionHNSWRaBitQNumExBits, false},
		// Integer.parseInt reads any Unicode decimal digit.
		{
			"RaBitQ extra bits in fullwidth digits",
			map[string]string{recordlayer.IndexOptionHNSWUseRaBitQ: "true", recordlayer.IndexOptionHNSWRaBitQNumExBits: "8"},
			map[string]string{recordlayer.IndexOptionHNSWUseRaBitQ: "true", recordlayer.IndexOptionHNSWRaBitQNumExBits: "\uff18"},
			recordlayer.IndexOptionHNSWRaBitQNumExBits, false,
		},
		{
			"RaBitQ extra bits changed",
			map[string]string{recordlayer.IndexOptionHNSWUseRaBitQ: "true", recordlayer.IndexOptionHNSWRaBitQNumExBits: "8"},
			map[string]string{recordlayer.IndexOptionHNSWUseRaBitQ: "true", recordlayer.IndexOptionHNSWRaBitQNumExBits: "4"},
			recordlayer.IndexOptionHNSWRaBitQNumExBits, true,
		},
		{
			"RaBitQ extra bits changed under their alias",
			map[string]string{recordlayer.IndexOptionHNSWUseRaBitQ: "true"},
			map[string]string{recordlayer.IndexOptionHNSWUseRaBitQ: "true", "vectorRaBitQNumExBits": "5"},
			"vectorRaBitQNumExBits", true,
		},
	} {
		oldIdx := &recordlayer.Index{Name: "v", Type: recordlayer.IndexTypeVector, Options: c.old}
		newIdx := &recordlayer.Index{Name: "v", Type: recordlayer.IndexTypeVector, Options: c.new}
		changed := map[string]bool{c.option: true}
		err := validateVectorIndexOptions(oldIdx, newIdx, changed)
		var evolErr *recordlayer.MetaDataEvolutionError
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
	err := validateVectorIndexOptions(&recordlayer.Index{Name: "v", Type: recordlayer.IndexTypeVector, Options: map[string]string{}},
		&recordlayer.Index{Name: "v", Type: recordlayer.IndexTypeVector, Options: map[string]string{recordlayer.IndexOptionVectorMetric: "COSINE"}},
		map[string]bool{recordlayer.IndexOptionVectorMetric: true})
	var iae *recordlayer.IllegalArgumentError
	if !errors.As(err, &iae) || iae.Message != "No enum constant com.apple.foundationdb.linear.Metric.COSINE" {
		t.Errorf("an unrecognized metric: %v, want Metric.valueOf's refusal", err)
	}
}
