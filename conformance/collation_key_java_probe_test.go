//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// Persist this measurement: matching locale/strength does not make x/text keys
// interchangeable with either Java provider's persisted index keys.
var _ = Describe("CollationKeyJavaProbe", func() {
	for _, provider := range []struct{ function, registry string }{
		{recordlayer.CollateFuncJRE, "com.apple.foundationdb.record.provider.common.text.TextCollatorRegistryJRE"},
		{recordlayer.CollateFuncICU, "com.apple.foundationdb.record.icu.TextCollatorRegistryICU"},
	} {
		It("pins provider and incompatible bytes for "+provider.function, func() {
			str := func(s string) *gen.KeyExpression {
				return &gen.KeyExpression{Value: &gen.Value{StringValue: proto.String(s)}}
			}
			long := func(v int64) *gen.KeyExpression {
				return &gen.KeyExpression{Value: &gen.Value{LongValue: proto.Int64(v)}}
			}
			cases := []struct {
				text, locale string
				strength     int64
			}{
				{"a", "", 0},
				{"A", "", 0},
				{"abc", "", 0},
				{"abc", "", 2},
				{"Résumé", "", 1},
				{"straße", "de_DE", 0},
				{"Zebra", "en_US", 2},
				{"", "", 0},
			}
			for _, c := range cases {
				expr := &gen.KeyExpression{Function: &gen.Function{
					Name:      proto.String(provider.function),
					Arguments: &gen.KeyExpression{Then: &gen.Then{Child: []*gen.KeyExpression{str(c.text), str(c.locale), long(c.strength)}}},
				}}
				exprBytes, err := proto.Marshal(expr)
				Expect(err).NotTo(HaveOccurred())
				var java struct {
					Packed          [][]int `json:"packed"`
					JavaVersion     string  `json:"javaVersion"`
					RuntimeVersion  string  `json:"runtimeVersion"`
					JavaVendor      string  `json:"javaVendor"`
					LocaleProviders string  `json:"localeProviders"`
					Registry        string  `json:"registry"`
					ICUVersion      string  `json:"icuVersion"`
				}
				Expect(NewJavaInvoker().InvokeAs(context.Background(), "evaluateCollationKeyJava", map[string]any{
					"expression": BytesToIntArray(exprBytes),
				}, &java)).To(Succeed())
				Expect(java.Registry).To(Equal(provider.registry))
				Expect(java.JavaVersion).NotTo(BeEmpty())
				Expect(java.RuntimeVersion).NotTo(BeEmpty())
				Expect(java.JavaVendor).NotTo(BeEmpty())
				Expect(java.LocaleProviders).NotTo(BeEmpty())
				Expect(java.ICUVersion).To(Equal("78.3.0.0"), "the pinned Record Layer ICU artifact moved")
				Expect(java.Packed).To(HaveLen(1))
				javaKey := make([]byte, len(java.Packed[0]))
				for i, b := range java.Packed[0] {
					Expect(b).To(BeNumerically(">=", 0))
					Expect(b).To(BeNumerically("<=", 255))
					javaKey[i] = byte(b)
				}

				goExpr, err := recordlayer.KeyExpressionFromProto(expr)
				Expect(err).NotTo(HaveOccurred())
				evaluated, err := goExpr.Evaluate(nil, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(evaluated).To(HaveLen(1))
				goTuple := make(tuple.Tuple, len(evaluated[0]))
				for i, value := range evaluated[0] {
					goTuple[i] = value
				}
				goKey := goTuple.Pack()

				label := fmt.Sprintf("%s %q locale=%q strength=%d", provider.function, c.text, c.locale, c.strength)
				fmt.Printf("COLLATION-KEY %s registry=%s runtime=%s vendor=%q providers=%q ICU=%s java=%x go=%x\n",
					label, java.Registry, java.RuntimeVersion, java.JavaVendor, java.LocaleProviders, java.ICUVersion, javaKey, goKey)
				Expect(goKey).NotTo(Equal(javaKey), "Go's key now matches Java for %s: re-measure both providers before relaxing the guard", label)
			}
		})
	}
})
