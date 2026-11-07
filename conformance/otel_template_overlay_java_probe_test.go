//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/keyspace"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Java 4.14.2.0 over Go-written OTEL_V1 rows (compiled builders, zlib-1
// records). A store of TemplateBody alone (the rfc204 golden's template) is
// read in full. A store of TemplateDDL, whose Go-only overlay is
// MESSAGES_TEXT with tokenizer otel_message_v1, is refused at every
// statement: Java's catalog builds the schema's metadata, and its TEXT index
// validator knows no otel_message_v1 tokenizer.
var _ = Describe("OtelTemplateOverlayJavaProbe", func() {
	It("reads a body-only store and refuses the overlay's tokenizer", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		ks := keyspace.New(subspace.Sub())
		cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
		Expect(err).NotTo(HaveOccurred())
		ops := ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks)
		catalogRun := func(fn func(api.Transaction) error) {
			_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				txn := catalog.NewFDBTransaction(rc)
				if err := cat.Initialize(txn); err != nil {
					return nil, err
				}
				return nil, fn(txn)
			})
			Expect(err).NotTo(HaveOccurred())
		}
		suffix := strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))
		overlayPath, bodyPath, bodyTemplate := "/TEST/OTEL_OVERLAY_"+suffix, "/TEST/OTEL_BODY_"+suffix, "OTEL_V1_BODY_"+suffix
		// OTEL_V1 is dropped too: a catalog template Java cannot build must
		// not outlive the probe.
		defer catalogRun(func(txn api.Transaction) error {
			for _, path := range []string{overlayPath, bodyPath} {
				if err := ops.DropDatabase(path, false, api.Options{}).Execute(txn); err != nil {
					return err
				}
			}
			for _, name := range []string{otelsql.TemplateName, bodyTemplate} {
				if err := ops.DropSchemaTemplate(name, false, api.Options{}).Execute(txn); err != nil {
					return err
				}
			}
			return nil
		})

		overlay, err := otelsql.EnsureTenant(ctx, db, overlayPath, "S", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(overlay.MetaData.GetIndex("MESSAGES_TEXT")).NotTo(BeNil())
		body, err := embedded.BuildSchemaTemplateFromDDL("CREATE SCHEMA TEMPLATE " + bodyTemplate + " " + otelsql.TemplateBody)
		Expect(err).NotTo(HaveOccurred())
		catalogRun(func(txn api.Transaction) error {
			if err := ops.SaveSchemaTemplate(body, api.Options{}).Execute(txn); err != nil {
				return err
			}
			if err := ops.CreateDatabase(bodyPath, api.Options{}).Execute(txn); err != nil {
				return err
			}
			return ops.CreateSchema(bodyPath, "S", bodyTemplate, api.Options{}).Execute(txn)
		})
		bodyStore, err := otelsql.OpenTenantStoreCompiled(ctx, db, bodyPath, "S", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(bodyStore.MetaData.GetIndex("MESSAGES_TEXT")).To(BeNil())

		str := func(k, v string) *commonpb.KeyValue {
			return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
		}
		trace := []byte{0x0a, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
		resource := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "agent")}}
		refs := []otelsql.MessageRef{{Writer: 1, Seq: 1, Attribute: 1, Newest: true, Ordinals: []int{0}}}
		var records []proto.Message
		for i, model := range []string{"gpt-4o", "claude"} {
			row, err := otelsql.BuildCompiled(otelsql.Span{
				Resource: resource, Refs: refs[:i], OTLP: []byte{byte(i)},
				Span: &tracepb.Span{
					TraceId: trace, SpanId: []byte{0, 0, 0, 0, 0, 0, 0, byte(i + 1)}, Name: "chat", StartTimeUnixNano: uint64(100 * (i + 1)),
					Attributes: []*commonpb.KeyValue{str("gen_ai.request.model", model)},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			records = append(records, row)
		}
		for i, l := range []*logspb.LogRecord{
			{
				TraceId: trace, SpanId: []byte{0, 0, 0, 0, 0, 0, 0, 1}, TimeUnixNano: 150, SeverityText: "ERROR", EventName: "gen_ai.choice",
				Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "refused"}}, Attributes: []*commonpb.KeyValue{str("incident.bucket", "b0")},
			},
			{TimeUnixNano: 160, SeverityText: "INFO", Attributes: []*commonpb.KeyValue{str("incident.bucket", "b0")}},
		} {
			row, err := otelsql.BuildLogCompiled(otelsql.Log{Resource: resource, Log: l, EventID: fmt.Sprintf("e%d", i+1), EffectiveNs: int64(l.TimeUnixNano), TimeSource: "event"})
			Expect(err).NotTo(HaveOccurred())
			records = append(records, row)
		}
		msg, err := otelsql.BuildMessageCompiled(otelsql.MessagePart{
			Writer: 1, Seq: 1, Text: &otelsql.MessageText{Role: "user", Content: "hello world"},
			Raw: []byte("hello world"), ContentHash: otelsql.MessageContentHash("json", []byte("hello world")), Encoding: "json", RawBytes: 11, Parts: 1,
		})
		Expect(err).NotTo(HaveOccurred())
		records = append(records, msg, otelsql.BuildMessageTraceCompiled(1, 1, trace))
		zlib1, err := recordlayer.NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true).SetCompressionLevel(1).Build()
		Expect(err).NotTo(HaveOccurred())
		for _, ts := range []*otelsql.TenantStore{overlay, bodyStore} {
			ts.Serializer = zlib1
			_, err = db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				store, err := ts.Builder(rc).Open()
				if err != nil {
					return nil, err
				}
				return store.SaveRecordBatch(records)
			})
			Expect(err).NotTo(HaveOccurred())
		}

		javaRead := func(path, q string) string {
			var out struct {
				Rows [][]any `json:"rows"`
			}
			err := java.InvokeAs(ctx, "wsjQueryJava", map[string]any{"clusterFile": clusterFile, "dbPath": path, "schemaName": "S", "querySql": q}, &out)
			var je *JavaError
			if errors.As(err, &je) {
				return "ERROR " + je.SQLState + " " + je.ExceptionClass + " " + je.Message
			}
			Expect(err).NotTo(HaveOccurred(), q)
			return overlaySortedRows(out.Rows)
		}
		b64 := base64.StdEncoding.EncodeToString
		chunks, err := otelsql.PackRefChunks(refs)
		Expect(err).NotTo(HaveOccurred())
		hexTrace := fmt.Sprintf("X'%x'", trace)
		for _, c := range []struct{ sql, rows string }{
			{
				"SELECT span_id, name, start_ns, service_name FROM spans WHERE trace_id = " + hexTrace,
				fmt.Sprintf("[[%s chat 100 agent] [%s chat 200 agent]]", b64([]byte{0, 0, 0, 0, 0, 0, 0, 1}), b64([]byte{0, 0, 0, 0, 0, 0, 0, 2})),
			},
			{
				"SELECT s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.str_attrs AS a WHERE a.scope = 'span' AND a.attr_key = 'gen_ai.request.model' AND a.str_value = 'claude')",
				"[[200]]",
			},
			{"SELECT c.chunk, c.refs FROM spans AS s, s.ref_chunks AS c", fmt.Sprintf("[[0 %s]]", b64(chunks[0]))},
			{"SELECT event_id, severity_text, event_name, body_text FROM logs", "[[e1 ERROR gen_ai.choice refused] [e2 INFO <nil> <nil>]]"},
			{"SELECT l.event_id FROM logs AS l WHERE l.trace_id = " + hexTrace + " AND l.span_id = X'0000000000000001'", "[[e1]]"},
			{
				"SELECT l.event_id, l.trace_id FROM logs AS l WHERE EXISTS (SELECT 1 FROM l.str_attrs AS a WHERE a.scope = 'log' AND a.attr_key = 'incident.bucket' AND a.str_value = 'b0')",
				fmt.Sprintf("[[e1 %s] [e2 <nil>]]", b64(trace)),
			},
			{"SELECT writer, seq, role, content FROM messages", "[[1 1 user hello world]]"},
		} {
			got := javaRead(bodyPath, c.sql)
			refused := javaRead(overlayPath, c.sql)
			fmt.Fprintf(GinkgoWriter, "%s\n  java body    -> %s\n  java overlay -> %s\n", c.sql, got, refused)
			Expect(got).To(Equal(c.rows), c.sql)
			Expect(refused).To(Equal("ERROR 42000 MetaDataException unrecognized text tokenizer"), c.sql)
		}
	})
})

// overlaySortedRows renders rows in sorted order: the probe's SELECTs have no
// ORDER BY, which Java's planner may refuse without an ordering index.
func overlaySortedRows(rows [][]any) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprint(r)
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}
