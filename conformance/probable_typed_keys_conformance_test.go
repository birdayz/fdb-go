//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type conformanceBloomKey []byte

func (k conformanceBloomKey) FDBKey() fdb.Key { return fdb.Key(k) }

func typedProbableCursor(shape, alias string, lists [][]string, cont []byte) recordlayer.RecordCursor[string] {
	fns := make([]recordlayer.CursorFunction[string], len(lists))
	for i, list := range lists {
		fns[i] = func(c []byte) recordlayer.RecordCursor[string] { return recordlayer.FromListWithContinuation(list, c) }
	}
	key := func(label string) (tuple.Tuple, error) {
		n, err := strconv.Atoi(label)
		if err != nil {
			return nil, err
		}
		var v any
		switch shape {
		case "long":
			if alias == "uint" {
				v = uint(7 + n)
			} else {
				v = uint64(7 + n)
			}
		case "bytes":
			switch alias {
			case "key":
				v = fdb.Key(label)
			case "convertible":
				v = conformanceBloomKey(label)
			default:
				v = []byte(label)
			}
		case "nested":
			v = tuple.Tuple{label}
		case "float_nan":
			v = math.Float32frombits(0x7fc00001 + uint32(n))
		case "double_nan":
			v = math.Float64frombits(0x7ff8000000000001 + uint64(n))
		case "nested_float_nan":
			v = tuple.Tuple{math.Float32frombits(0x7fc00001 + uint32(n))}
		case "nested_double_nan":
			v = tuple.Tuple{math.Float64frombits(0x7ff8000000000001 + uint64(n))}
		}
		return tuple.Tuple{v}, nil
	}
	cursor, err := recordlayer.ProbableIntersection(key, fns, recordlayer.ProbableIntersectionDefaultExpectedResults, recordlayer.ProbableIntersectionDefaultFalsePositivePercentage, cont)
	Expect(err).NotTo(HaveOccurred())
	return cursor
}

var _ = Describe("Typed probable intersection compatibility boundaries", func() {
	// Java ByteString supplies structural byte equality to seenSet as well as
	// the byte funnel. Fresh Java byte[] identity is a separate contrast below.
	for _, spec := range [][2]string{{"long", "uint"}, {"long", "uint64"}, {"bytes", "raw"}, {"bytes", "key"}, {"bytes", "convertible"}, {"nested", "tuple"}} {
		for _, direction := range []string{"java_to_go", "go_to_java"} {
			It(fmt.Sprintf("exchanges actual Java structural-key continuations shape=%s alias=%s direction=%s", spec[0], spec[1], direction), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				lists := [][]string{{"0", "1", "2", "3"}, {"0", "1", "2", "3"}}
				wire, err := json.Marshal(lists)
				Expect(err).NotTo(HaveOccurred())
				java := NewJavaInvoker()
				var cont *string
				var population []string
				for page := 0; page < 10; page++ {
					var raw map[string]any
					Expect(java.InvokeAs(ctx, "probableIntersectionTypedKeys", map[string]any{"shape": spec[0], "listsJson": string(wire), "limit": 1, "continuation": cont}, &raw)).To(Succeed())
					jp := javaPage(raw)
					var bytes []byte
					if cont != nil {
						bytes, err = hex.DecodeString(*cont)
						Expect(err).NotTo(HaveOccurred())
					}
					gp, err := drainGoPage(ctx, recordlayer.LimitRowsCursor(typedProbableCursor(spec[0], spec[1], lists, bytes), 1), func(s string) string { return s })
					Expect(err).NotTo(HaveOccurred())
					Expect(gp).To(Equal(jp), "%v %s page=%d", spec, direction, page)
					population = append(population, gp.Items...)
					if direction == "java_to_go" {
						cont = jp.Continuation
					} else {
						cont = gp.Continuation
					}
					if cont == nil {
						break
					}
				}
				Expect(cont).To(BeNil(), "exchange must reach exhaustion")
				Expect(population).To(Equal([]string{"0", "1", "2", "3"}), "%v %s nonvacuous exchange", spec, direction)
			})
		}
	}

	It("contrasts actual Java fresh byte-array identity with Go packed byte-key equality", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		lists := [][]string{{"0", "1", "2", "3"}, {"0", "1", "2", "3"}}
		wire, err := json.Marshal(lists)
		Expect(err).NotTo(HaveOccurred())
		var raw map[string]any
		Expect(NewJavaInvoker().InvokeAs(ctx, "probableIntersectionTypedKeys", map[string]any{"shape": "raw_bytes", "listsJson": string(wire), "limit": 0, "continuation": nil}, &raw)).To(Succeed())
		jp := javaPage(raw)
		gp, err := drainGoPage(ctx, typedProbableCursor("bytes", "raw", lists, nil), func(s string) string { return s })
		Expect(err).NotTo(HaveOccurred())
		Expect(jp.Items).To(BeEmpty())
		Expect(gp.Items).To(Equal([]string{"0", "1", "2", "3"}))
		Expect(jp.Continuation).To(BeNil())
		Expect(gp.Continuation).To(BeNil())
		Expect(jp.Reason).To(Equal("SOURCE_EXHAUSTED"))
		Expect(gp.Reason).To(Equal("SOURCE_EXHAUSTED"))
	})

	It("pins Java boxed NaN suppression versus Go distinct packed stored-key identity", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		lists := [][]string{{"0", "1"}, {"0", "1"}}
		wire, err := json.Marshal(lists)
		Expect(err).NotTo(HaveOccurred())
		for _, shape := range []string{"float_nan", "double_nan", "nested_float_nan", "nested_double_nan"} {
			var raw map[string]any
			Expect(NewJavaInvoker().InvokeAs(ctx, "probableIntersectionTypedKeys", map[string]any{"shape": shape, "listsJson": string(wire), "limit": 0, "continuation": nil}, &raw)).To(Succeed())
			jp := javaPage(raw)
			gp, err := drainGoPage(ctx, typedProbableCursor(shape, "", lists, nil), func(s string) string { return s })
			Expect(err).NotTo(HaveOccurred())
			Expect(jp.Items).To(Equal([]string{"0"}), shape)
			Expect(gp.Items).To(Equal([]string{"0", "1"}), shape)
			Expect(jp.Continuation).To(BeNil())
			Expect(gp.Continuation).To(BeNil())
		}
	})
})
