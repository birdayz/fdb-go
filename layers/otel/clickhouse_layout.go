package otel

import (
	"context"
	"strconv"
	"strings"
)

// Keep observed server DDL and marks separate from the requested setting:
// adaptive byte granularity and part boundaries also affect the mark count.
func (b *CHBackend) auditMergeTreeLayout(ctx context.Context, details map[string]string) error {
	tables := []string{"spans"}
	if b.attrs {
		tables = append(tables, "attr_lookup")
	}
	for _, table := range tables {
		wire, err := b.request(ctx, "SHOW CREATE TABLE "+b.database+"."+table+" FORMAT TSVRaw", nil, nil)
		if err != nil {
			return err
		}
		observed := strings.TrimSpace(string(wire))
		if observed == "" {
			return failf("audit", "CH layout", "missing SHOW CREATE evidence for %s", table)
		}
		details[table+"_show_create"] = observed
	}
	params := map[string]string{"db": b.database}
	for _, evidence := range []struct{ key, sql string }{
		{"merge_tree_settings", "SELECT name AS table,engine,engine_full,sorting_key,create_table_query FROM system.tables WHERE database={db:String} AND name IN ('spans','attr_lookup') ORDER BY name FORMAT JSONEachRow"},
		{"part_marks", "SELECT table,toString(sum(rows)) AS rows,toString(sum(marks)) AS marks,toString(count()) AS parts FROM system.parts WHERE database={db:String} AND active GROUP BY table ORDER BY table FORMAT JSONEachRow"},
		{"projection_marks", "SELECT name,toString(sum(rows)) AS rows,toString(sum(marks)) AS marks,toString(count()) AS parts FROM system.projection_parts WHERE database={db:String} AND table='spans' AND active GROUP BY name ORDER BY name FORMAT JSONEachRow"},
	} {
		wire, err := b.request(ctx, evidence.sql, nil, params)
		if err != nil {
			return err
		}
		observed := strings.TrimSpace(string(wire))
		if observed == "" {
			return failf("audit", "CH layout", "missing %s evidence", evidence.key)
		}
		details[evidence.key] = observed
	}
	details["ch_index_granularity_requested"] = strconv.Itoa(b.cfg.CHIndexGranularity)
	details["granularity_evidence"] = "SHOW CREATE and system.tables engine_full/create_table_query report server table settings; active parent and projection marks are observed separately, not inferred from the requested setting; adaptive byte granularity and part boundaries may reduce rows per granule"
	return nil
}
