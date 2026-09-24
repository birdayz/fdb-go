package recordlayer

import (
	"errors"
	"strconv"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
)

// A TEXT index naming an unregistered tokenizer, or a tokenizer version outside
// the tokenizer's supported range, is a META-DATA defect and must be rejected
// when the schema is built — not at the first record save.
//
// Java rejects it in TextIndexMaintainerFactory.getIndexValidator().validate()
// (TextIndexMaintainerFactory.java:106-111): it resolves the tokenizer and calls
// tokenizer.validateVersion(...) as part of meta-data validation. Java ALSO
// validates inside DefaultTextTokenizer.tokenize (DefaultTextTokenizer.java:174).
// The two are not redundant — the second guards the write path against a version
// threaded in from a stored per-record tokenizer version, which meta-data
// validation never sees.
//
// Go had only the write-time half. These specs pin the meta-data half, so a
// schema carrying an unusable text index cannot be built and then fail later at
// an unrelated call site.
// multiVersionTestTokenizer is a registered tokenizer that accepts a RANGE of
// versions. The default tokenizer has MinVersion == MaxVersion, so it cannot
// express a version upgrade or downgrade at all; the meta-data evolution specs
// need a tokenizer for which versions 1..10 are legitimately in range, or their
// subject (the option CHANGING) is masked by a version-out-of-range rejection.
//
// Tokenization itself delegates to the default — nothing here tests tokenizing.
type multiVersionTestTokenizer struct{ name string }

const multiVersionTestTokenizerMaxVersion = 10

func (t *multiVersionTestTokenizer) Name() string    { return t.name }
func (t *multiVersionTestTokenizer) MinVersion() int { return GlobalMinVersion }
func (t *multiVersionTestTokenizer) MaxVersion() int { return multiVersionTestTokenizerMaxVersion }

func (t *multiVersionTestTokenizer) Tokenize(text string, version int, mode TokenizerMode) (TokenIterator, error) {
	if err := ValidateTokenizerVersion(t, version); err != nil {
		return nil, err
	}
	return defaultTextTokenizerInstance.Tokenize(text, defaultTextTokenizerInstance.MinVersion(), mode)
}

func (t *multiVersionTestTokenizer) TokenizeToMap(text string, version int, mode TokenizerMode) (map[string][]int, error) {
	return defaultTokenizeToMap(t, text, version, mode)
}

func (t *multiVersionTestTokenizer) TokenizeToList(text string, version int, mode TokenizerMode) ([]string, error) {
	return defaultTokenizeToList(t, text, version, mode)
}

// namedTestTokenizerFactory registers a multi-version tokenizer under a given
// name, so tests that need two DISTINCT but VALID tokenizer names have them.
// Before meta-data validation existed such tests could name any string at all —
// which is precisely the hole being closed, so they now have to name tokenizers
// that exist.
type namedTestTokenizerFactory struct{ name string }

func (f *namedTestTokenizerFactory) Name() string { return f.name }

func (f *namedTestTokenizerFactory) GetTokenizer() TextTokenizer {
	return &multiVersionTestTokenizer{name: f.name}
}

// registerTestTokenizers is idempotent: Register only errors when a DIFFERENT
// factory claims the same name, and these factories are compared by pointer, so
// the singletons below register once and re-register harmlessly.
//
// The names are deliberately implausible. Registration mutates a PROCESS-GLOBAL
// registry, so whatever is registered here stays valid for every other spec in
// this test binary — which slightly weakens the very gate this file installs. A
// name like "english" would be a plausible thing for another spec to use as its
// example of an UNREGISTERED tokenizer, and it would then silently pass for the
// wrong reason. These cannot be mistaken for a real tokenizer anyone would
// name, and `grep` confirms nothing else in pkg/recordlayer/ refers to them.
var (
	englishTestTokenizer = &namedTestTokenizerFactory{name: "metadata_validation_test_tokenizer_a"}
	frenchTestTokenizer  = &namedTestTokenizerFactory{name: "metadata_validation_test_tokenizer_b"}
)

func registerTestTokenizers() {
	_ = GlobalTextTokenizerRegistry().Register(englishTestTokenizer)
	_ = GlobalTextTokenizerRegistry().Register(frenchTestTokenizer)
}

func textIndexWithOptions(opts map[string]string) *Index {
	idx := NewIndex("Customer$text", Field("name"))
	idx.Type = IndexTypeText
	idx.Options = opts
	return idx
}

func buildWithTextIndex(idx *Index) error {
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Customer", idx)
	_, err := builder.Build()
	return err
}

var _ = Describe("TEXT index meta-data validation", func() {
	It("validates the text body descriptor rather than only the tokenizer", func() {
		for _, tc := range []struct {
			expr    KeyExpression
			message string
		}{
			{Field("price"), "non-string type"},
			{FanOut("tags"), "repeated field"},
			{EmptyKey(), "does not have text field"},
			{GroupBy(Field("price"), Field("order_id")), "non-string type"},
		} {
			b := baseBuilder()
			idx := NewIndex("body", tc.expr)
			idx.Type = IndexTypeText
			b.AddIndex("Order", idx)
			_, err := b.Build()
			Expect(err).To(MatchError(ContainSubstring(tc.message)))
		}
		for _, expr := range []KeyExpression{
			Nest("flower", Field("type")),
			GroupBy(Nest("flower", Field("type")), Field("order_id")),
		} {
			b := baseBuilder()
			idx := NewIndex("body", expr)
			idx.Type = IndexTypeText
			b.AddIndex("Order", idx)
			_, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
		}
		b := baseBuilder()
		idx := NewIndex("universal_body", Field("price"))
		idx.Type = IndexTypeText
		b.AddUniversalIndex(idx)
		_, err := b.Build()
		Expect(err).To(MatchError(ContainSubstring("non-string type")))
	})

	It("accepts the default tokenizer with no explicit version", func() {
		// The control. If this ever fails, the arms below stop proving anything:
		// a validator that rejects everything would pass all the negative specs.
		Expect(buildWithTextIndex(textIndexWithOptions(nil))).NotTo(HaveOccurred())
	})

	It("accepts an explicit in-range tokenizer version", func() {
		tok, err := GetTextTokenizer("")
		Expect(err).NotTo(HaveOccurred())
		Expect(tok.MinVersion()).To(BeNumerically("<=", tok.MaxVersion()))

		err = buildWithTextIndex(textIndexWithOptions(map[string]string{
			IndexOptionTextTokenizerVersion: strconv.Itoa(tok.MinVersion()),
		}))
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects an unregistered tokenizer name at build time", func() {
		err := buildWithTextIndex(textIndexWithOptions(map[string]string{
			IndexOptionTextTokenizerName: "no_such_tokenizer",
		}))
		Expect(err).To(HaveOccurred(), "an index naming a tokenizer that is not in the "+
			"registry is unusable; accepting it here defers the failure to the first record save")
		// Java's MetaDataException text (TextTokenizerRegistryImpl.getTokenizer); its
		// log info (the tokenizer name) is not part of Java's message, and Go
		// appends it.
		var mde *MetaDataError
		Expect(errors.As(err, &mde)).To(BeTrue(), "%T: %v", err, err)
		Expect(mde.Message).To(HavePrefix("unrecognized text tokenizer"))
	})

	It("rejects a tokenizer version above the tokenizer's maximum", func() {
		tok, err := GetTextTokenizer("")
		Expect(err).NotTo(HaveOccurred())

		err = buildWithTextIndex(textIndexWithOptions(map[string]string{
			IndexOptionTextTokenizerVersion: strconv.Itoa(tok.MaxVersion() + 1),
		}))
		Expect(err).To(HaveOccurred())

		var mde *MetaDataError
		Expect(errors.As(err, &mde)).To(BeTrue(),
			"Java throws MetaDataException here, so Go must surface *MetaDataError")
		Expect(mde.Message).To(HavePrefix("unknown tokenizer version"))
	})

	It("rejects a negative tokenizer version", func() {
		err := buildWithTextIndex(textIndexWithOptions(map[string]string{
			IndexOptionTextTokenizerVersion: "-1",
		}))
		Expect(err).To(HaveOccurred())
	})

	It("rejects a tokenizer version that is not an integer", func() {
		err := buildWithTextIndex(textIndexWithOptions(map[string]string{
			IndexOptionTextTokenizerVersion: "not-a-number",
		}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("could not be parsed as int"),
			"matches Java's MetaDataException message in "+
				"TextIndexMaintainer.getIndexTokenizerVersion")
	})
})

var _ = Describe("Multi-type TEXT body validation", func() {
	for _, shape := range []string{"valid", "numeric", "repeated", "missing"} {
		It("validates every covered descriptor: "+shape, func() {
			md := unionEvolutionSchema(GinkgoT(), 1, false, false, false)
			file := protodesc.ToFileDescriptorProto(md.GetRecordType("Alpha").Descriptor.ParentFile())
			file.MessageType[1].Field[1].Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
			expression := KeyExpression(Field("payload"))
			message := ""
			switch shape {
			case "numeric":
				file.MessageType[1].Field[1].Type = descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
				message = "non-string type"
			case "repeated":
				for _, record := range file.MessageType[:2] {
					record.Field[1].Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
				}
				expression = FanOut("payload")
				message = "repeated field"
			case "missing":
				file.MessageType[1].Field = file.MessageType[1].Field[:1]
				message = "does not have field: payload"
			}
			fd, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
			Expect(err).NotTo(HaveOccurred())
			b := NewRecordMetaDataBuilder().SetRecordsWithUnionName(fd, "Envelope")
			for _, name := range []string{"Alpha", "Beta"} {
				b.GetRecordType(name).SetPrimaryKey(Field("id"))
			}
			index := NewIndex("text", expression)
			index.Type = IndexTypeText
			b.AddMultiTypeIndex([]string{"Alpha", "Beta"}, index)
			result, err := b.Build()
			if shape == "valid" {
				Expect(err).NotTo(HaveOccurred())
				Expect(result.GetIndexesForRecordType("Alpha")).To(ContainElement(index))
				Expect(result.GetIndexesForRecordType("Beta")).To(ContainElement(index))
			} else {
				Expect(err).To(MatchError(ContainSubstring(message)))
			}
		})
	}
})
