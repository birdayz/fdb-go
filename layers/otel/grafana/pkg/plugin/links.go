package plugin

import (
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// Grafana interpolates these in internal link queries per row.
const (
	valueRaw   = "${__value.raw}"
	rowTraceID = "${__data.fields.traceID}"
)

func addLinks(f *data.Field, links ...data.DataLink) {
	if f.Config == nil {
		f.Config = &data.FieldConfig{}
	}
	f.Config.Links = append(f.Config.Links, links...)
}

// internalLink targets this datasource with query; Explore and TraceView open it.
func internalLink(inst backend.DataSourceInstanceSettings, title string, query map[string]any) data.DataLink {
	return data.DataLink{
		Title: title,
		Internal: &data.InternalDataLink{
			DatasourceUID:  inst.UID,
			DatasourceName: inst.Name,
			Query:          query,
		},
	}
}

func linkQuery(queryType, tenant string, kv ...string) map[string]any {
	q := map[string]any{"queryType": queryType}
	if tenant != "" {
		q["tenant"] = tenant
	}
	for i := 0; i+1 < len(kv); i += 2 {
		q[kv[i]] = kv[i+1]
	}
	return q
}

// traceLink opens the trace whose ID is the field's value.
func traceLink(inst backend.DataSourceInstanceSettings, tenant string) data.DataLink {
	return internalLink(inst, "Trace: "+valueRaw, linkQuery(queryTypeTrace, tenant, "traceId", valueRaw))
}

// spanLogsLinks go on the trace frame's spanID field; TraceView shows them
// as span links: the span's logs and the whole trace's logs.
func spanLogsLinks(inst backend.DataSourceInstanceSettings, tenant string) []data.DataLink {
	return []data.DataLink{
		internalLink(inst, "Logs for this span", linkQuery(queryTypeLogs, tenant, "traceId", rowTraceID, "spanId", valueRaw)),
		internalLink(inst, "Logs for this trace", linkQuery(queryTypeLogs, tenant, "traceId", rowTraceID)),
	}
}
