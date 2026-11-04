//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// TextQuerySpec is one text query run by both engines: TextQuerySteps.Spec in
// text_query_conformance.java.
type TextQuerySpec struct {
	Index               string   `json:"index"`
	Type                string   `json:"type"`
	Text                *string  `json:"text,omitempty"`
	Tokens              []string `json:"tokens,omitempty"`
	MaxDistance         int      `json:"maxDistance"`
	Strict              bool     `json:"strict"`
	Grouping            *string  `json:"grouping,omitempty"`
	SuffixLow           *int64   `json:"suffixLow,omitempty"`
	SuffixHigh          *int64   `json:"suffixHigh,omitempty"`
	Reverse             bool     `json:"reverse"`
	Limit               int      `json:"limit"`
	Skip                int      `json:"skip"`
	ScannedRecordsLimit int      `json:"scannedRecordsLimit"`
	Continuation        *string  `json:"continuation,omitempty"`
}

func (s TextQuerySpec) String() string {
	b, _ := json.Marshal(s)
	return string(b)
}

// textPage is one page of a query: its items (entry key/value hex, or primary
// keys), the continuation hex (nil at the end) and the no-next reason.
type textPage struct {
	Items        []string
	Continuation *string
	Reason       string
}

func strPtr(s string) *string { return &s }

var javaNoNextReason = map[recordlayer.NoNextReason]string{
	recordlayer.SourceExhausted:    "SOURCE_EXHAUSTED",
	recordlayer.ReturnLimitReached: "RETURN_LIMIT_REACHED",
	recordlayer.TimeLimitReached:   "TIME_LIMIT_REACHED",
	recordlayer.ScanLimitReached:   "SCAN_LIMIT_REACHED",
	recordlayer.ByteLimitReached:   "BYTE_LIMIT_REACHED",
}

var textComparisonTypes = map[string]recordlayer.TextComparisonType{
	"TEXT_CONTAINS_ALL":          recordlayer.TextContainsAll,
	"TEXT_CONTAINS_ALL_WITHIN":   recordlayer.TextContainsAllWithin,
	"TEXT_CONTAINS_ANY":          recordlayer.TextContainsAny,
	"TEXT_CONTAINS_PHRASE":       recordlayer.TextContainsPhrase,
	"TEXT_CONTAINS_PREFIX":       recordlayer.TextContainsPrefix,
	"TEXT_CONTAINS_ALL_PREFIXES": recordlayer.TextContainsAllPrefixes,
	"TEXT_CONTAINS_ANY_PREFIX":   recordlayer.TextContainsAnyPrefix,
}

// goTextComparison is TextQuerySteps.comparison.
func goTextComparison(s TextQuerySpec) *recordlayer.TextComparison {
	typ := textComparisonTypes[s.Type]
	switch {
	case typ == recordlayer.TextContainsAllWithin && s.Tokens != nil:
		return recordlayer.NewTextWithMaxDistanceComparisonTokens(s.Tokens, s.MaxDistance, "", "")
	case typ == recordlayer.TextContainsAllWithin:
		return recordlayer.NewTextWithMaxDistanceComparison(*s.Text, s.MaxDistance, "", "")
	case typ == recordlayer.TextContainsAllPrefixes && s.Tokens != nil:
		return recordlayer.NewTextContainsAllPrefixesComparisonTokens(s.Tokens, s.Strict,
			recordlayer.ProbableIntersectionDefaultExpectedResults, recordlayer.ProbableIntersectionDefaultFalsePositivePercentage, "", "")
	case typ == recordlayer.TextContainsAllPrefixes:
		return recordlayer.NewTextContainsAllPrefixesComparison(*s.Text, s.Strict,
			recordlayer.ProbableIntersectionDefaultExpectedResults, recordlayer.ProbableIntersectionDefaultFalsePositivePercentage, "", "")
	case s.Tokens != nil:
		return recordlayer.NewTextComparisonTokens(typ, s.Tokens, "", "")
	}
	return recordlayer.NewTextComparison(typ, *s.Text, "", "")
}

// goTextFilterComparison is the comparison Java's Text builder makes for the
// planner step: containsPrefix keeps its prefix as a one-token list.
func goTextFilterComparison(s TextQuerySpec) *recordlayer.TextComparison {
	if s.Type == "TEXT_CONTAINS_PREFIX" {
		return recordlayer.NewTextComparisonTokens(recordlayer.TextContainsPrefix, []string{*s.Text}, "", "")
	}
	return goTextComparison(s)
}

func goExecuteProperties(s TextQuerySpec) recordlayer.ExecuteProperties {
	props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(s.Limit).WithSkip(s.Skip)
	if s.ScannedRecordsLimit > 0 {
		props = props.WithScannedRecordsLimit(s.ScannedRecordsLimit)
	}
	return props
}

func continuationBytes(s TextQuerySpec) []byte {
	if s.Continuation == nil {
		return nil
	}
	b, err := hex.DecodeString(*s.Continuation)
	Expect(err).NotTo(HaveOccurred())
	return b
}

// drainGoPage reads a cursor to its no-next result.
func drainGoPage[T any](ctx context.Context, c recordlayer.RecordCursor[T], item func(T) string, afterItem ...func(int) error) (textPage, error) {
	defer c.Close()
	var p textPage
	for {
		r, err := c.OnNext(ctx)
		if err != nil {
			return p, err
		}
		if !r.HasNext() {
			b, err := r.GetContinuation().ToBytes()
			if err != nil {
				return p, err
			}
			if b != nil {
				p.Continuation = strPtr(hex.EncodeToString(b))
			}
			p.Reason = javaNoNextReason[r.GetNoNextReason()]
			return p, nil
		}
		p.Items = append(p.Items, item(r.GetValue()))
		for _, hook := range afterItem {
			if err := hook(len(p.Items)); err != nil {
				return p, err
			}
		}
	}
}

func entryItem(e *recordlayer.IndexEntry) string {
	return hex.EncodeToString(e.Key.Pack()) + "/" + hex.EncodeToString(e.Value.Pack())
}

func javaPage(raw map[string]any) textPage {
	var p textPage
	for _, it := range raw["items"].([]any) {
		switch v := it.(type) {
		case map[string]any:
			p.Items = append(p.Items, v["key"].(string)+"/"+v["value"].(string))
		case float64:
			p.Items = append(p.Items, fmt.Sprint(int64(v)))
		case string:
			p.Items = append(p.Items, v)
		}
	}
	if c, ok := raw["continuation"].(string); ok {
		p.Continuation = &c
	}
	p.Reason = raw["reason"].(string)
	return p
}

// textQueryMetaData is TextQuerySteps.createTextQueryMetaData.
func textQueryMetaData() (*recordlayer.RecordMetaData, error) {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Customer", recordlayer.NewTextIndex("name_text", recordlayer.Field("name")))
	builder.AddIndex("Customer", recordlayer.NewTextIndex("grouped_text", recordlayer.GroupBy(recordlayer.Field("name"), recordlayer.Field("email"))))
	builder.AddIndex("Customer", recordlayer.NewTextIndex("suffix_text", recordlayer.Concat(recordlayer.Field("name"), recordlayer.Field("price"))))
	return builder.Build()
}

// textQueryCorpusSize customers make every common token span many bunches
// (BunchedMap bunches hold 20 entries).
const textQueryCorpusSize = 1200

// textQueryCorpusName is customer i's name. "common" is in every record,
// "frequent" in every other, "rare" in three; the alpha/beta/gamma phrases and
// the pre- words recur with periods 7 and 5, so phrase matches and prefix
// tokens fall on bunch boundaries as well as inside bunches.
func textQueryCorpusName(i int) string {
	name := "common"
	if i%2 == 0 {
		name += " frequent"
	}
	if i == 17 || i == 600 || i == 1197 {
		name += " rare"
	}
	switch i % 7 {
	case 0:
		name += " alpha beta gamma"
	case 1:
		name += " gamma beta alpha"
	case 2:
		name += " alpha the beta"
	}
	switch i % 5 {
	case 0:
		name += " prefix"
	case 1:
		name += " prelude"
	case 2:
		name += " present prefix"
	}
	return name
}

var _ = Describe("TEXT query execution conformance", Ordered, func() {
	var (
		ctx    = context.Background()
		env    *TenantEnvironment
		md     *recordlayer.RecordMetaData
		ks     subspace.Subspace
		java   *JavaInvoker
		params func() map[string]any
	)

	BeforeAll(func() {
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("txq_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		md, err = textQueryMetaData()
		Expect(err).NotTo(HaveOccurred())
		ks = subspace.Sub(tuple.Tuple{})
		java = NewJavaInvoker()
		params = func() map[string]any {
			return map[string]any{"clusterFile": env.ClusterFile, "subspace": BytesToIntArray(ks.Bytes()), "tenantName": env.TenantName}
		}
		for batch := 0; batch < textQueryCorpusSize; batch += 100 {
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := batch; i < batch+100; i++ {
					if _, err := store.SaveRecord(&demopb.Customer{
						CustomerId: proto.Int64(int64(i)),
						Name:       proto.String(textQueryCorpusName(i)),
						Email:      proto.String(fmt.Sprintf("g%d", i%3)),
						Price:      proto.Int32(int32(i % 50)),
					}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterAll(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	goScanWithFault := func(s TextQuerySpec, timer *recordlayer.StoreTimer, afterItem ...func(int) error) (textPage, *recordlayer.ScanLimiterState) {
		type attemptResult struct {
			page  textPage
			state *recordlayer.ScanLimiterState
		}
		runner := recordlayer.NewFDBDatabaseRunner(env.RecordDB).SetContextConfig(&recordlayer.RecordContextConfig{Timer: timer})
		result, err := runner.RunWithRetry(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			timer.Reset()
			exec := goExecuteProperties(s)
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			var grouping tuple.Tuple
			if s.Grouping != nil {
				grouping = tuple.Tuple{*s.Grouping}
			}
			var suffix *recordlayer.TupleRange
			if s.SuffixLow != nil {
				r := recordlayer.TupleRangeBetween(tuple.Tuple{*s.SuffixLow}, tuple.Tuple{*s.SuffixHigh})
				suffix = &r
			}
			scan := recordlayer.NewTextScan(md.GetIndex(s.Index), grouping, goTextComparison(s), suffix)
			cursor, err := scan.Scan(store, continuationBytes(s), recordlayer.NewScanProperties(exec).WithReverse(s.Reverse))
			if err != nil {
				return nil, err
			}
			page, err := drainGoPage(ctx, cursor, entryItem, afterItem...)
			if err != nil {
				return nil, err
			}
			return attemptResult{page: page, state: exec.ScanState}, nil
		})
		Expect(err).NotTo(HaveOccurred(), s.String())
		final := result.(attemptResult)
		return final.page, final.state
	}

	goScan := func(s TextQuerySpec, timer *recordlayer.StoreTimer) (textPage, *recordlayer.ScanLimiterState) {
		return goScanWithFault(s, timer)
	}

	javaScan := func(s TextQuerySpec) textPage {
		p := params()
		p["specJson"] = s.String()
		var raw map[string]any
		Expect(java.InvokeAs(ctx, "textQueryScan", p, &raw)).To(Succeed(), s.String())
		return javaPage(raw)
	}

	goPlan := func(s TextQuerySpec) textPage {
		result, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			q := recordlayer.TextQuery{RecordTypes: []string{"Customer"}, Comparison: goTextFilterComparison(s), Reverse: s.Reverse}
			if s.Grouping != nil {
				q.GroupingKey = tuple.Tuple{*s.Grouping}
			}
			plan, err := recordlayer.PlanTextQuery(md, md.GetIndex(s.Index), q)
			if err != nil {
				return nil, err
			}
			cursor, err := plan.Execute(store, continuationBytes(s), goExecuteProperties(s))
			if err != nil {
				return nil, err
			}
			page, err := drainGoPage(ctx, cursor, func(r *recordlayer.FDBIndexedRecord) string {
				return fmt.Sprint(r.Record.PrimaryKey[0].(int64))
			})
			if err != nil {
				return nil, err
			}
			return page, nil
		})
		Expect(err).NotTo(HaveOccurred(), s.String())
		return result.(textPage)
	}

	javaPlan := func(s TextQuerySpec) (textPage, string) {
		p := params()
		p["specJson"] = s.String()
		var raw map[string]any
		Expect(java.InvokeAs(ctx, "textQueryPlan", p, &raw)).To(Succeed(), s.String())
		return javaPage(raw), raw["plan"].(string)
	}

	// allPages runs a spec to its end, each page from the previous page's
	// continuation.
	allPages := func(s TextQuerySpec, run func(TextQuerySpec) textPage) []textPage {
		var out []textPage
		for n := 0; n < 2000; n++ {
			p := run(s)
			out = append(out, p)
			if p.Continuation == nil {
				return out
			}
			s.Continuation = p.Continuation
		}
		Fail("query did not finish: " + s.String())
		return nil
	}

	items := func(pages []textPage) []string {
		var all []string
		for _, p := range pages {
			all = append(all, p.Items...)
		}
		return all
	}
	sorted := func(s []string) []string {
		out := append([]string{}, s...)
		sort.Strings(out)
		return out
	}
	distinct := func(s []string) []string {
		var out []string
		seen := map[string]bool{}
		for _, v := range sorted(s) {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
		return out
	}

	g1 := strPtr("g1")
	low, high := int64(10), int64(20)
	// Deterministic specs: the merge (if any) is ordered, so Java and Go must
	// return the same entries in the same order with the same continuations.
	ordered := []TextQuerySpec{
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("rare common frequent")},
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("frequent common")},
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("alpha prelude")},
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("rare absent common")},
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("common")},
		{Index: "name_text", Type: "TEXT_CONTAINS_ANY", Text: strPtr("rare prelude")},
		{Index: "name_text", Type: "TEXT_CONTAINS_ANY", Text: strPtr("absent nowhere")},
		{Index: "name_text", Type: "TEXT_CONTAINS_PHRASE", Text: strPtr("alpha beta gamma")},
		{Index: "name_text", Type: "TEXT_CONTAINS_PHRASE", Text: strPtr("beta alpha")},
		{Index: "name_text", Type: "TEXT_CONTAINS_PHRASE", Tokens: []string{"alpha", "", "beta"}},
		{Index: "name_text", Type: "TEXT_CONTAINS_PHRASE", Text: strPtr("common rare")},
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL_WITHIN", Text: strPtr("alpha gamma"), MaxDistance: 1},
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL_WITHIN", Text: strPtr("alpha gamma"), MaxDistance: 2},
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL_WITHIN", Text: strPtr("common prefix frequent"), MaxDistance: 3},
		{Index: "name_text", Type: "TEXT_CONTAINS_PREFIX", Text: strPtr("pre")},
		{Index: "name_text", Type: "TEXT_CONTAINS_PREFIX", Text: strPtr("prel")},
		{Index: "name_text", Type: "TEXT_CONTAINS_PREFIX", Text: strPtr("zzz")},
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL_PREFIXES", Text: strPtr("fre"), Strict: true},
		{Index: "name_text", Type: "TEXT_CONTAINS_ANY_PREFIX", Text: strPtr("ra")},
		{Index: "grouped_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("common frequent"), Grouping: g1},
		{Index: "grouped_text", Type: "TEXT_CONTAINS_PHRASE", Text: strPtr("alpha beta"), Grouping: g1},
		{Index: "grouped_text", Type: "TEXT_CONTAINS_ANY", Text: strPtr("rare gamma"), Grouping: g1},
		{Index: "grouped_text", Type: "TEXT_CONTAINS_PREFIX", Text: strPtr("pres"), Grouping: g1},
		{Index: "grouped_text", Type: "TEXT_CONTAINS_ALL_WITHIN", Text: strPtr("beta gamma"), MaxDistance: 1, Grouping: g1},
		{Index: "suffix_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("common frequent"), SuffixLow: &low, SuffixHigh: &high},
		{Index: "suffix_text", Type: "TEXT_CONTAINS_PHRASE", Text: strPtr("alpha beta"), SuffixLow: &low, SuffixHigh: &high},
		{Index: "suffix_text", Type: "TEXT_CONTAINS_ANY", Text: strPtr("rare prelude"), SuffixLow: &low, SuffixHigh: &high},
	}
	// Unordered specs: Java's UnorderedUnionCursor and ProbableIntersectionCursor
	// return results as FDB reads complete, so only the result sets compare.
	unordered := []TextQuerySpec{
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL_PREFIXES", Text: strPtr("pre fre"), Strict: true},
		{Index: "name_text", Type: "TEXT_CONTAINS_ALL_PREFIXES", Text: strPtr("pres ra comm"), Strict: false},
		{Index: "name_text", Type: "TEXT_CONTAINS_ANY_PREFIX", Text: strPtr("prel ra")},
		{Index: "grouped_text", Type: "TEXT_CONTAINS_ANY_PREFIX", Text: strPtr("pres gam"), Grouping: g1},
	}

	variants := func(s TextQuerySpec) []TextQuerySpec {
		var out []TextQuerySpec
		for _, reverse := range []bool{false, true} {
			for _, page := range []struct{ limit, skip int }{{0, 0}, {13, 2}, {151, 0}} {
				v := s
				v.Reverse, v.Limit, v.Skip = reverse, page.limit, page.skip
				out = append(out, v)
			}
		}
		return out
	}

	It("returns Java's entries, order and continuations for every ordered text comparison", func() {
		nonEmpty := 0
		for _, base := range ordered {
			for _, s := range variants(base) {
				javaPages := allPages(s, javaScan)
				goPages := allPages(s, func(s TextQuerySpec) textPage { p, _ := goScan(s, nil); return p })
				Expect(goPages).To(Equal(javaPages), s.String())
				if len(items(javaPages)) > 0 {
					nonEmpty++
				}
				// Each engine resumes the other's continuations to the same page.
				for i, jp := range javaPages[:len(javaPages)-1] {
					if i == 3 {
						break
					}
					next := s
					next.Continuation = jp.Continuation
					p, _ := goScan(next, nil)
					Expect(p).To(Equal(javaScan(next)), next.String())
				}
			}
		}
		Expect(nonEmpty).To(BeNumerically(">", len(ordered)*4), "most specs must match records")
	})

	// keySet is the distinct primary keys of entry items (key hex, minus the
	// token), which is what a probable intersection compares.
	keySet := func(entries []string, prefixColumns int) []string {
		var keys []string
		for _, e := range entries {
			keyHex, _, _ := strings.Cut(e, "/")
			b, err := hex.DecodeString(keyHex)
			Expect(err).NotTo(HaveOccurred())
			t, err := tuple.Unpack(b)
			Expect(err).NotTo(HaveOccurred())
			keys = append(keys, fmt.Sprint(t[prefixColumns:]))
		}
		return distinct(keys)
	}

	It("returns Java's result sets for the unordered prefix merges, whole and paged", func() {
		for _, base := range unordered {
			prefixColumns := 1
			if base.Grouping != nil {
				prefixColumns = 2
			}
			goRun := func(s TextQuerySpec) textPage { p, _ := goScan(s, nil); return p }
			whole := keySet(items(allPages(base, javaScan)), prefixColumns)
			Expect(whole).NotTo(BeEmpty(), base.String())
			for _, s := range variants(base) {
				if s.Skip > 0 {
					continue // which entries a skip drops depends on the merge order
				}
				javaAll := items(allPages(s, javaScan))
				goAll := items(allPages(s, goRun))
				switch {
				case s.Type == "TEXT_CONTAINS_ANY_PREFIX":
					// Every child entry exactly once, whatever the order.
					Expect(sorted(goAll)).To(Equal(sorted(javaAll)), s.String())
				case s.Limit == 0:
					// One pass consults no Bloom filter: exactly the intersection.
					Expect(keySet(goAll, prefixColumns)).To(Equal(keySet(javaAll, prefixColumns)), s.String())
					Expect(keySet(goAll, prefixColumns)).To(Equal(whole), s.String())
				default:
					// A resumed pass consults the Bloom filters, whose false positives
					// depend on the merge order: both engines return the intersection.
					Expect(keySet(goAll, prefixColumns)).To(ContainElements(whole), s.String())
					Expect(keySet(javaAll, prefixColumns)).To(ContainElements(whole), s.String())
				}
			}
		}
	})

	It("stops on a scanned-records limit at a continuation either engine resumes", func() {
		for _, base := range []TextQuerySpec{
			{Index: "name_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("common")},
			{Index: "name_text", Type: "TEXT_CONTAINS_ANY", Text: strPtr("rare prelude")},
			{Index: "name_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("frequent common")},
			{Index: "name_text", Type: "TEXT_CONTAINS_PHRASE", Text: strPtr("alpha beta gamma")},
		} {
			s := base
			s.ScannedRecordsLimit = 100
			javaPages := allPages(s, javaScan)
			goPages := allPages(s, func(s TextQuerySpec) textPage { p, _ := goScan(s, nil); return p })
			Expect(len(javaPages)).To(BeNumerically(">", 1), s.String())
			if len(goTextComparisonTokens(s)) == 1 || s.Type == "TEXT_CONTAINS_ANY" {
				// No intersection: the same entries are read, so pages match exactly.
				Expect(goPages).To(Equal(javaPages), s.String())
				continue
			}
			// An intersection seeks past entries Java reads, so its pages hold more
			// rows; the rows, their order, and every resume agree.
			Expect(items(goPages)).To(Equal(items(javaPages)), s.String())
			for _, jp := range javaPages[:len(javaPages)-1] {
				Expect(jp.Reason).To(Equal("SCAN_LIMIT_REACHED"))
				resumed := s
				resumed.Continuation = jp.Continuation
				resumed.ScannedRecordsLimit = 0
				Expect(items(allPages(resumed, func(s TextQuerySpec) textPage { p, _ := goScan(s, nil); return p }))).
					To(Equal(items(allPages(resumed, javaScan))), resumed.String())
			}
		}
	})

	It("plans and executes text index queries as Java's RecordQueryPlanner does", func() {
		for _, base := range append(append([]TextQuerySpec{}, ordered...), unordered...) {
			if base.SuffixLow != nil {
				continue // the planner has no suffix comparisons for a text scan
			}
			for _, s := range []TextQuerySpec{base, func() TextQuerySpec { v := base; v.Limit = 37; return v }()} {
				javaPages := allPages(s, func(s TextQuerySpec) textPage { p, _ := javaPlan(s); return p })
				goPages := allPages(s, goPlan)
				unorderedMerge := (s.Type == "TEXT_CONTAINS_ALL_PREFIXES" || s.Type == "TEXT_CONTAINS_ANY_PREFIX") && len(goTextComparisonTokens(s)) > 1
				switch {
				case !unorderedMerge:
					Expect(goPages).To(Equal(javaPages), s.String())
				case s.Strict || s.Type == "TEXT_CONTAINS_ANY_PREFIX" || s.Limit == 0:
					// Each page deduplicates only its own records.
					Expect(distinct(items(goPages))).To(Equal(distinct(items(javaPages))), s.String())
				default:
					// A non-strict paged probable intersection may add Bloom false
					// positives that depend on the merge order.
					whole := distinct(items(allPages(base, goPlan)))
					Expect(distinct(items(goPages))).To(ContainElements(whole), s.String())
					Expect(distinct(items(javaPages))).To(ContainElements(whole), s.String())
				}
			}
		}
		_, plan := javaPlan(TextQuerySpec{Index: "name_text", Type: "TEXT_CONTAINS_PREFIX", Text: strPtr("pre")})
		Expect(plan).To(ContainSubstring("name_text"))
	})

	It("discards partial scan pages, budgets and counters before a retry", func() {
		s := TextQuerySpec{Index: "name_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("common"), ScannedRecordsLimit: 100}
		baselineTimer := recordlayer.NewStoreTimer()
		baseline, baselineState := goScan(s, baselineTimer)
		Expect(baseline.Items).NotTo(BeEmpty())
		Expect(baseline.Reason).To(Equal("SCAN_LIMIT_REACHED"))

		timer := recordlayer.NewStoreTimer()
		faults, partialItems := 0, 0
		var partialBytes, partialKVs int64
		page, state := goScanWithFault(s, timer, func(items int) error {
			if faults == 0 {
				faults++
				partialItems = items
				partialBytes = timer.GetCount(recordlayer.CountTextScanBytes)
				partialKVs = timer.GetCount(recordlayer.CountTextScanKeyValues)
				return fdb.Error{Code: 1020}
			}
			return nil
		})
		Expect(faults).To(Equal(1))
		Expect(partialItems).To(Equal(1))
		Expect(partialBytes).To(BeNumerically(">", 0))
		Expect(partialKVs).To(BeNumerically(">", 0))
		Expect(page).To(Equal(baseline))
		Expect(state.BytesScanned()).To(Equal(baselineState.BytesScanned()))
		Expect(timer.GetCount(recordlayer.CountTextScanBytes)).To(Equal(baselineTimer.GetCount(recordlayer.CountTextScanBytes)))
		Expect(timer.GetCount(recordlayer.CountTextScanKeyValues)).To(Equal(baselineTimer.GetCount(recordlayer.CountTextScanKeyValues)))
	})

	It("reads about the rare token's postings when it is intersected with common tokens", func() {
		s := TextQuerySpec{Index: "name_text", Type: "TEXT_CONTAINS_ALL", Text: strPtr("rare common frequent")}
		timer := recordlayer.NewStoreTimer()
		page, state := goScan(s, timer)
		Expect(page).To(Equal(javaScan(s)))
		Expect(page.Items).To(HaveLen(1)) // record 600; 17 and 1197 are odd
		kvs := timer.GetCount(recordlayer.CountTextScanKeyValues)
		Expect(timer.GetCount(recordlayer.CountTextScanBytes)).To(Equal(state.BytesScanned()))
		Expect(timer.GetCount(recordlayer.CountTextScanSeeks)).To(BeNumerically(">", 0))
		// The rare, common and frequent maps hold the same records as in
		// pkg/recordlayer/text_scan_seek_test.go's 1200-record corpus, so the
		// seeking intersection reads the same 10 bunches derived there (Java's
		// streaming one reads 77).
		Expect(kvs).To(Equal(int64(10)))
	})

	It("matches Java's merge cursors over in-memory children, continuations included", func() {
		lists := [][]string{
			{"a", "c", "e", "g", "k"},
			{"b", "c", "f", "g", "k", "m"},
			{"c", "g", "h", "k"},
		}
		listsJSON, err := json.Marshal(lists)
		Expect(err).NotTo(HaveOccurred())
		fns := make([]recordlayer.CursorFunction[string], len(lists))
		for i, l := range lists {
			fns[i] = func(cont []byte) recordlayer.RecordCursor[string] {
				return recordlayer.FromListWithContinuation(l, cont)
			}
		}
		key := func(s string) (tuple.Tuple, error) { return tuple.Tuple{s}, nil }
		goMerge := func(kind string, cont []byte) recordlayer.RecordCursor[string] {
			var c recordlayer.RecordCursor[string]
			var err error
			switch kind {
			case "intersection":
				resume, err := recordlayer.DecodeIntersectionContinuation(cont, len(fns))
				Expect(err).NotTo(HaveOccurred())
				cursors := make([]recordlayer.RecordCursor[string], len(fns))
				for i, fn := range fns {
					switch {
					case !resume[i].Started:
						cursors[i] = fn(nil)
					case len(resume[i].Continuation) == 0:
						cursors[i] = recordlayer.Empty[string]()
					default:
						cursors[i] = fn(resume[i].Continuation)
					}
				}
				c = recordlayer.IntersectionResume(cursors, key, false, resume)
			case "union":
				c, err = recordlayer.Union(key, false, fns, cont)
			case "unordered_union":
				c, err = recordlayer.UnorderedUnion(fns, cont)
			case "probable_intersection":
				c, err = recordlayer.ProbableIntersection(key, fns, recordlayer.ProbableIntersectionDefaultExpectedResults,
					recordlayer.ProbableIntersectionDefaultFalsePositivePercentage, cont)
			}
			Expect(err).NotTo(HaveOccurred())
			return c
		}
		for _, kind := range []string{"intersection", "union", "unordered_union", "probable_intersection"} {
			for _, limit := range []int{0, 1, 2, 3} {
				var cont *string
				for n := 0; n < 50; n++ {
					var raw map[string]any
					Expect(java.InvokeAs(ctx, "mergeCursorOverLists", map[string]any{
						"kind": kind, "listsJson": string(listsJSON), "limit": limit, "continuation": cont,
					}, &raw)).To(Succeed())
					jp := javaPage(raw)
					var contBytes []byte
					if cont != nil {
						contBytes, err = hex.DecodeString(*cont)
						Expect(err).NotTo(HaveOccurred())
					}
					gp, err := drainGoPage(ctx, recordlayer.LimitRowsCursor(goMerge(kind, contBytes), limit), func(s string) string { return s })
					Expect(err).NotTo(HaveOccurred())
					Expect(gp).To(Equal(jp), fmt.Sprintf("%s limit=%d page=%d", kind, limit, n))
					if jp.Continuation == nil {
						break
					}
					cont = jp.Continuation
				}
			}
		}
	})
})

// goTextComparisonTokens is the spec's tokens as the index tokenizes them.
func goTextComparisonTokens(s TextQuerySpec) []string {
	if s.Tokens != nil {
		return s.Tokens
	}
	tokens, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(*s.Text, 0, recordlayer.TokenizerModeQuery)
	Expect(err).NotTo(HaveOccurred())
	return tokens
}
