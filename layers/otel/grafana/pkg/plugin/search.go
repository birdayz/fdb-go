package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"fdb.dev/layers/otel"
)

// summaryHitLimit: root service, name and duration need each hit's spans
// (there is no parent_span_id index), so they are filled for this many hits.
const summaryHitLimit = 20

// searchHit is one matching trace.
type searchHit struct {
	traceID      string
	latest       int64  // newest matching witness, ns
	matchedSpans *int64 // nil when the mode does not report spans
}

func (d *Datasource) queryTraceSearch(ctx context.Context, pc backend.PluginContext, q backend.DataQuery) (backend.DataResponse, error) {
	m, err := parseQuery(q)
	if err != nil {
		return backend.DataResponse{}, err
	}
	tenant, err := d.tenant(m.Tenant)
	if err != nil {
		return backend.DataResponse{}, err
	}
	limit := d.cfg.SearchLimit
	if m.Limit != nil {
		limit = *m.Limit
	}
	if limit < 1 || limit > maxSearchLimit {
		return backend.DataResponse{}, queryErrorf("limit", "%d is outside 1..%d", limit, maxSearchLimit)
	}
	spanAttrs, err := toAttributes("spanPredicates", signalSpan, m.SpanPredicates)
	if err != nil {
		return backend.DataResponse{}, err
	}
	logAttrs, err := toAttributes("logPredicates", signalLog, m.LogPredicates)
	if err != nil {
		return backend.DataResponse{}, err
	}
	if err := checkSearchShape(m.Mode, len(spanAttrs), len(logAttrs)); err != nil {
		return backend.DataResponse{}, err
	}
	var frame *data.Frame
	var notices []data.Notice
	err = d.withReader(ctx, tenant, func(r otelReader) error {
		var err error
		frame, notices, err = d.search(ctx, r, m.Mode, spanAttrs, logAttrs, timeRange(q.TimeRange), limit)
		return err
	})
	if err != nil {
		return backend.DataResponse{}, err
	}
	frame.RefID = q.RefID
	traceField, _ := frame.FieldByName("traceID")
	addLinks(traceField, traceLink(linkTarget(pc, d.inst), tenant))
	frame.AppendNotices(notices...)
	return backend.DataResponse{Frames: data.Frames{frame}}, nil
}

// search runs one search mode on r and builds Tempo's search table.
func (d *Datasource) search(ctx context.Context, r otelReader, mode string, spanAttrs, logAttrs []otel.Attribute, window otel.TimeRange, limit int) (*data.Frame, []data.Notice, error) {
	var notices []data.Notice
	var hits []searchHit
	switch mode {
	case modeSameSpan:
		page, err := r.QuerySameSpan(ctx, otel.SameSpanQuery{Predicates: spanAttrs, TimeWindow: &window, Limit: limit})
		if err != nil {
			return nil, nil, err
		}
		hits = groupSpans(page.Spans)
		if len(page.Continuation) > 0 {
			notices = append(notices, data.Notice{Severity: data.NoticeSeverityInfo, Text: fmt.Sprintf("showing the newest %d matching spans; narrow the time range or raise the limit for more", limit)})
		}
	case modeExistential:
		res, err := r.QueryTraceExistential(ctx, otel.TraceExistentialQuery{Clauses: spanAttrs, LogClauses: logAttrs, TimeWindow: &window, Limit: limit})
		if err != nil {
			return nil, nil, err
		}
		for _, h := range res.Hits {
			seen := map[string]bool{}
			for _, ws := range h.Witnesses {
				for _, w := range ws {
					seen[w.ID] = true
				}
			}
			n := int64(len(seen))
			hits = append(hits, searchHit{traceID: h.Trace, latest: h.Latest, matchedSpans: &n})
		}
	case modeAttribute:
		res, err := r.Query(ctx, spanAttrs[0])
		if err != nil {
			return nil, nil, err
		}
		hits, notices = topHits(res, limit, d.cfg.SearchLimit)
	case modeTime:
		res, err := r.Time(ctx, window)
		if err != nil {
			return nil, nil, err
		}
		hits, notices = topHits(res, limit, d.cfg.SearchLimit)
	}

	frame := searchFrame(hits)
	if len(hits) <= summaryHitLimit {
		if err := d.addSummaries(ctx, r, frame, hits); err != nil {
			return nil, nil, err
		}
	} else {
		notices = append(notices, data.Notice{Severity: data.NoticeSeverityInfo, Text: fmt.Sprintf("more than %d traces: service, name and duration are omitted and start time is the newest matching span's", summaryHitLimit)})
	}
	return frame, notices, nil
}

// checkSearchShape refuses a mode/predicate combination before any read.
func checkSearchShape(mode string, spans, logs int) error {
	switch mode {
	case modeSameSpan:
		if spans == 0 || logs > 0 {
			return queryErrorf("mode", "sameSpan takes one or more span predicates and no log predicates")
		}
	case modeExistential:
		if spans+logs == 0 {
			return queryErrorf("mode", "existential takes at least one span or log predicate")
		}
	case modeAttribute:
		if spans != 1 || logs > 0 {
			return queryErrorf("mode", "attribute takes exactly one span predicate and no log predicates")
		}
	case modeTime:
		if spans+logs > 0 {
			return queryErrorf("mode", "time takes no predicates")
		}
	default:
		return queryErrorf("mode", "%q is not sameSpan, existential, attribute or time", mode)
	}
	return nil
}

func toAttributes(field, signal string, ps []predicate) ([]otel.Attribute, error) {
	out := make([]otel.Attribute, 0, len(ps))
	for i, p := range ps {
		a, err := p.toAttribute(fmt.Sprintf("%s[%d]", field, i), signal)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// groupSpans folds span matches into traces in first-seen (newest) order.
func groupSpans(spans []otel.SpanMatch) []searchHit {
	idx := map[string]int{}
	var hits []searchHit
	for _, s := range spans {
		i, ok := idx[s.Trace]
		if !ok {
			i = len(hits)
			idx[s.Trace] = i
			var zero int64
			hits = append(hits, searchHit{traceID: s.Trace, latest: s.Start, matchedSpans: &zero})
		}
		*hits[i].matchedSpans++
		if s.Start > hits[i].latest {
			hits[i].latest = s.Start
		}
	}
	return hits
}

// topHits takes the attribute and time modes' hits, which the reader caps at
// its K (searchLimit), and applies the query's limit.
func topHits(res otel.QueryResult, limit, k int) ([]searchHit, []data.Notice) {
	var notices []data.Notice
	if limit > k {
		notices = append(notices, data.Notice{Severity: data.NoticeSeverityInfo, Text: fmt.Sprintf("attribute and time searches return at most %d traces (searchLimit)", k)})
	}
	var hits []searchHit
	for _, h := range res.Hits {
		if len(hits) == limit {
			break
		}
		hits = append(hits, searchHit{traceID: h.Trace, latest: h.Latest})
	}
	return hits, notices
}

// searchFrame follows Tempo's search table: traceID, startTime, plus the
// number of matching spans.
func searchFrame(hits []searchHit) *data.Frame {
	traceIDs := make([]string, len(hits))
	starts := make([]time.Time, len(hits))
	matched := make([]*int64, len(hits))
	for i, h := range hits {
		traceIDs[i] = h.traceID
		starts[i] = time.Unix(0, h.latest).UTC()
		matched[i] = h.matchedSpans
	}
	frame := data.NewFrame("Traces",
		data.NewField("traceID", nil, traceIDs).SetConfig(&data.FieldConfig{DisplayNameFromDS: "Trace ID"}),
		data.NewField("startTime", nil, starts).SetConfig(&data.FieldConfig{DisplayNameFromDS: "Start time"}),
		data.NewField("matchedSpans", nil, matched).SetConfig(&data.FieldConfig{DisplayNameFromDS: "Matched spans"}),
	)
	frame.Meta = &data.FrameMeta{PreferredVisualization: data.VisTypeTable, UniqueRowIDFields: []int{0}}
	return frame
}

// addSummaries reads each hit's trace for its root service, name, start
// and duration (Tempo's traceService, traceName, traceDuration).
func (d *Datasource) addSummaries(ctx context.Context, r otelReader, frame *data.Frame, hits []searchHit) error {
	services := make([]*string, len(hits))
	names := make([]*string, len(hits))
	durations := make([]*float64, len(hits))
	startField, _ := frame.FieldByName("startTime")
	for i, h := range hits {
		rss, _, err := r.TraceOTLP(ctx, h.traceID, d.traceBudget())
		if err != nil {
			return err
		}
		sum, ok := summarize(rss)
		if !ok {
			continue
		}
		services[i], names[i] = sum.service, sum.name
		durations[i] = &sum.durationMs
		startField.Set(i, time.Unix(0, sum.start).UTC())
	}
	frame.Fields = append(frame.Fields,
		data.NewField("traceService", nil, services).SetConfig(&data.FieldConfig{DisplayNameFromDS: "Service"}),
		data.NewField("traceName", nil, names).SetConfig(&data.FieldConfig{DisplayNameFromDS: "Name"}),
		data.NewField("traceDuration", nil, durations).SetConfig(&data.FieldConfig{DisplayNameFromDS: "Duration", Unit: "ms"}),
	)
	return nil
}

type traceSummary struct {
	service, name *string
	start         int64
	durationMs    float64
}

// summarize finds the root (a span without a parent; the earliest when
// there are several or none) and the trace's extent.
func summarize(rss []*tracepb.ResourceSpans) (traceSummary, bool) {
	var root spanEntry
	haveRoot, rootHasNoParent := false, false
	var minStart, maxEnd uint64
	n := 0
	for _, rs := range rss {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				if n == 0 || s.GetStartTimeUnixNano() < minStart {
					minStart = s.GetStartTimeUnixNano()
				}
				if s.GetEndTimeUnixNano() > maxEnd {
					maxEnd = s.GetEndTimeUnixNano()
				}
				n++
				noParent := len(s.GetParentSpanId()) == 0
				better := !haveRoot ||
					noParent && !rootHasNoParent ||
					noParent == rootHasNoParent && s.GetStartTimeUnixNano() < root.span.GetStartTimeUnixNano()
				if better {
					root = spanEntry{span: s, resource: rs.GetResource()}
					haveRoot, rootHasNoParent = true, noParent
				}
			}
		}
	}
	if !haveRoot {
		return traceSummary{}, false
	}
	service, _, _ := resourceToProcess(root.resource, "")
	name := root.span.GetName()
	dur := float64(int64(maxEnd)-int64(minStart)) / 1e6
	return traceSummary{service: &service, name: &name, start: int64(minStart), durationMs: dur}, true
}
