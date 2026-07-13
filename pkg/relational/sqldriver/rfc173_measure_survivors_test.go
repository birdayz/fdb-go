package sqldriver_test

// RFC-173 MEASUREMENT harness (NOT a regression pin — a diagnostic sweep).
//
// Purpose: with the values.NameReadForbidden gate ARMED, run target shapes in
// isolation and CLASSIFY each result as:
//   - OK            : ran green under the armed gate (no name-keyed read)
//   - SURVIVOR      : failed with the NameReadForbiddenError signature (a REAL
//                     name-keyed read boundary that has NOT been ordinalized)
//   - OTHER-ERROR   : failed for a DIFFERENT reason (translation / physicalization
//                     decline — a reach gap, NOT a name-keyed survivor)
//
// These tests never t.Fatal on a query error; they capture + classify so the
// full battery reports in one run. Run each top-level test ALONE so the
// process-global gate + plan cache are not polluted by sibling tests.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/embedded"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// classifyGateErr buckets an error observed under the armed gate.
func classifyGateErr(err error) string {
	if err == nil {
		return "OK"
	}
	s := err.Error()
	if strings.Contains(s, "name-keyed read of field") || strings.Contains(s, "NameReadForbidden") {
		return "SURVIVOR"
	}
	return "OTHER-ERROR"
}

// ---------------------------------------------------------------------------
// (a) struct/repeated-message unnest under EXISTS
// ---------------------------------------------------------------------------

// buildStructExistsMetadata builds a metadata with:
//   - PA: PID int64, K int64, PB repeated<SItem{BSKU string, BQTY int64}>
//   - OU: OID int64, K int64
//   - EE: EID int64, CK int64
//
// so an EXISTS subquery can lateral-unnest the struct column PB.
func buildStructExistsMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("struct_exists_test.proto"),
		Package: proto.String("fdb.test.structexists"),
		Syntax:  proto.String("proto2"),
	}
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()

	sitem := &descriptorpb.DescriptorProto{
		Name: proto.String("SItem"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("BSKU"), Number: proto.Int32(1), Label: opt, Type: str},
			{Name: proto.String("BQTY"), Number: proto.Int32(2), Label: opt, Type: i64},
		},
	}
	pa := &descriptorpb.DescriptorProto{
		Name: proto.String("PA"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("PID"), Number: proto.Int32(1), Label: opt, Type: i64},
			{Name: proto.String("K"), Number: proto.Int32(2), Label: opt, Type: i64},
			{
				Name: proto.String("PB"), Number: proto.Int32(3), Label: rep, Type: msg,
				TypeName: proto.String(".fdb.test.structexists.SItem"),
			},
		},
	}
	ou := &descriptorpb.DescriptorProto{
		Name: proto.String("OU"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("OID"), Number: proto.Int32(1), Label: opt, Type: i64},
			{Name: proto.String("K"), Number: proto.Int32(2), Label: opt, Type: i64},
		},
	}
	ee := &descriptorpb.DescriptorProto{
		Name: proto.String("EE"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("EID"), Number: proto.Int32(1), Label: opt, Type: i64},
			{Name: proto.String("CK"), Number: proto.Int32(2), Label: opt, Type: i64},
		},
	}
	union := &descriptorpb.DescriptorProto{
		Name: proto.String("UnionDescriptor"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("_PA"), Number: proto.Int32(1), Label: opt, Type: msg,
				TypeName: proto.String(".fdb.test.structexists.PA"),
			},
			{
				Name: proto.String("_OU"), Number: proto.Int32(2), Label: opt, Type: msg,
				TypeName: proto.String(".fdb.test.structexists.OU"),
			},
			{
				Name: proto.String("_EE"), Number: proto.Int32(3), Label: opt, Type: msg,
				TypeName: proto.String(".fdb.test.structexists.EE"),
			},
		},
	}
	fdp.MessageType = []*descriptorpb.DescriptorProto{sitem, pa, ou, ee, union}

	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	b.SetSplitLongRecords(false)
	b.SetStoreRecordVersions(false)
	b.SetVersion(1)
	b.SetRecordCountKey(recordlayer.RecordTypeKey())
	b.GetRecordType("PA").SetPrimaryKey(recordlayer.Field("PID"))
	b.GetRecordType("OU").SetPrimaryKey(recordlayer.Field("OID"))
	b.GetRecordType("EE").SetPrimaryKey(recordlayer.Field("EID"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

func TestFDB_RFC173_Measure_StructNestedUnnestExists(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	md := buildStructExistsMetadata(t)

	paDesc := md.GetRecordType("PA").Descriptor
	pbFD := paDesc.Fields().ByName("PB")
	sitemDesc := pbFD.Message()
	ouDesc := md.GetRecordType("OU").Descriptor
	eeDesc := md.GetRecordType("EE").Descriptor

	mkItem := func(sku string, qty int64) protoreflect.Value {
		im := dynamicpb.NewMessage(sitemDesc)
		im.Set(sitemDesc.Fields().ByName("BSKU"), protoreflect.ValueOfString(sku))
		im.Set(sitemDesc.Fields().ByName("BQTY"), protoreflect.ValueOfInt64(qty))
		return protoreflect.ValueOfMessage(im)
	}
	mkPA := func(pid, k int64, items ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(paDesc)
		m.Set(paDesc.Fields().ByName("PID"), protoreflect.ValueOfInt64(pid))
		m.Set(paDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(k))
		l := m.NewField(pbFD).List()
		for _, it := range items {
			l.Append(it)
		}
		m.Set(pbFD, protoreflect.ValueOfList(l))
		return m
	}
	mk2 := func(d protoreflect.MessageDescriptor, f1, f2 string, v1, v2 int64) proto.Message {
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(f1)), protoreflect.ValueOfInt64(v1))
		m.Set(d.Fields().ByName(protoreflect.Name(f2)), protoreflect.ValueOfInt64(v2))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		// NOTE: these three record types share one store keyspace and the
		// hand-built proto primary keys carry NO record-type discriminator, so
		// PKs must be DISJOINT across tables or records collide/overwrite.
		recs := []proto.Message{
			// PA: pid1 K=100 [a:1,b:2]; pid2 K=110 [c:3]; pid3 K=100 (empty).
			mkPA(1, 100, mkItem("a", 1), mkItem("b", 2)),
			mkPA(2, 110, mkItem("c", 3)),
			mkPA(3, 100),
			// OU: oid11 K=100, oid12 K=110, oid13 K=999.
			mk2(ouDesc, "OID", "K", 11, 100),
			mk2(ouDesc, "OID", "K", 12, 110),
			mk2(ouDesc, "OID", "K", 13, 999),
			// EE: CK=100, CK=110.
			mk2(eeDesc, "EID", "CK", 21, 100),
			mk2(eeDesc, "EID", "CK", 22, 110),
		}
		for _, r := range recs {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	// runShape plans+executes and returns (rowStrings, err) — never fatals on
	// a query-time error (the gate error is what we are measuring).
	runShape := func(shapeSQL string) ([]string, error) {
		plan, subs, perr := embedded.PlanRecordQueryWithSubqueries(shapeSQL, md, nil)
		if perr != nil {
			return nil, perr
		}
		var out []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			evalCtx, bindErr := prebindScalarSubqueries(ctx, store, subs)
			if bindErr != nil {
				return nil, bindErr
			}
			cursor, cErr := executor.ExecutePlan(ctx, plan, store, evalCtx, nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cursor.Close()
			rows, rErr := executor.CollectAll(ctx, cursor)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				out = append(out, fmt.Sprintf("%v", executor.RowValue(r)))
			}
			return nil, nil
		})
		sort.Strings(out)
		return out, eerr
	}

	shapes := []struct {
		name string
		sql  string
	}{
		{
			"noncorr_exists_struct_unnest",
			`SELECT "OID" FROM OU WHERE EXISTS (SELECT 1 FROM PA AS s, s."PB" AS B)`,
		},
		{
			"noncorr_not_exists_struct_unnest",
			`SELECT "OID" FROM OU WHERE NOT EXISTS (SELECT 1 FROM PA AS s, s."PB" AS B)`,
		},
		{
			"outer_corr_exists_struct_unnest",
			`SELECT "OID" FROM OU WHERE EXISTS (SELECT 1 FROM PA AS s, s."PB" AS B WHERE s."K" = OU."K")`,
		},
		{
			"outer_corr_not_exists_struct_unnest",
			`SELECT "OID" FROM OU WHERE NOT EXISTS (SELECT 1 FROM PA AS s, s."PB" AS B WHERE s."K" = OU."K")`,
		},
		{
			"CONTROL_base_scan_ou",
			`SELECT "OID" FROM OU`,
		},
		{
			"CONTROL_base_scan_pa",
			`SELECT "PID" FROM PA`,
		},
		{
			"CONTROL_plain_struct_unnest",
			`SELECT B FROM PA AS s, s."PB" AS B`,
		},
		{
			"CONTROL_struct_unnest_where",
			`SELECT B FROM PA AS s, s."PB" AS B WHERE s."K" = 100`,
		},
		{
			"CONTROL_plain_exists_scan",
			`SELECT "OID" FROM OU WHERE EXISTS (SELECT 1 FROM PA AS s WHERE s."K" = OU."K")`,
		},
		{
			"struct_unnest_select_struct_elem",
			`SELECT B FROM PA AS s, s."PB" AS B WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = s."K")`,
		},
		{
			"struct_unnest_proj_outer_and_elem",
			`SELECT s."K", B FROM PA AS s, s."PB" AS B WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = s."K")`,
		},
		{
			"ordinality_struct_unnest",
			`SELECT B, "O" FROM PA AS s, s."PB" AS B AT "O"`,
		},
		{
			"agg_over_lateral_unnest",
			`SELECT s."K", COUNT(*) FROM PA AS s, s."PB" AS B GROUP BY s."K"`,
		},
	}
	defer func() { values.NameReadForbidden = false }()
	for _, sh := range shapes {
		values.NameReadForbidden = false
		baseRows, baseErr := runShape(sh.sql)
		values.NameReadForbidden = true
		rows, err := runShape(sh.sql)
		values.NameReadForbidden = false
		t.Logf("[MEASURE %-38s] class=%-11s\n  gateOFF rows=%v err=%v\n  gateON  rows=%v err=%v",
			sh.name, classifyGateErr(err), baseRows, baseErr, rows, err)
	}
}

// ---------------------------------------------------------------------------
// (b) EXISTS-outer served by a COVERING index (reordered layout)
// ---------------------------------------------------------------------------

func TestFDB_RFC173_Measure_CoveringExistsOuter(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	// T(ID, V) PK(ID), covering index on V ⇒ index layout [V, ID] vs table [ID, V].
	// E(EID, CK) PK(EID).
	db := setupPlanShapeDB(t, "coverexists",
		"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX v_idx ON t (v) "+
			"CREATE TABLE e (eid BIGINT NOT NULL, ck BIGINT, PRIMARY KEY (eid))")

	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 100), (2, 110), (3, 999)")
	mwjoMustExec(t, db, ctx, "INSERT INTO e (eid, ck) VALUES (10, 100), (11, 110)")

	// Confirm the outer really is planned as a COVERING index scan on V. An
	// EQUALITY predicate on the indexed column forces V_IDX (a range like v>0
	// matching all rows lets the full Scan(T) win).
	for _, q := range []string{
		`SELECT v FROM t WHERE v = 100`,
		`SELECT v FROM t WHERE v >= 100`,
		`SELECT v FROM t WHERE v = 100 AND EXISTS (SELECT 1 FROM e WHERE e.ck = t.v)`,
		`SELECT v FROM t WHERE v = 999 AND NOT EXISTS (SELECT 1 FROM e WHERE e.ck = t.v)`,
		`SELECT t.v FROM t, e WHERE t.v = e.ck`,
		`SELECT v FROM t WHERE EXISTS (SELECT 1 FROM e WHERE e.ck = t.v) ORDER BY v`,
		`SELECT v FROM t WHERE v >= 100 AND EXISTS (SELECT 1 FROM e WHERE e.ck = t.v) ORDER BY v`,
	} {
		plan := planExplainVia(t, ctx, db, q)
		t.Logf("[PLAN] %s\n  => %s", q, plan)
	}

	runQ := func(q string) ([]string, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, fmt.Sprintf("%v", v.Int64))
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		sort.Strings(out)
		return out, nil
	}

	values.NameReadForbidden = true
	defer func() { values.NameReadForbidden = false }()

	shapes := []struct{ name, sql string }{
		{
			"covering_exists_outer_eq100",
			`SELECT v FROM t WHERE v = 100 AND EXISTS (SELECT 1 FROM e WHERE e.ck = t.v)`,
		},
		{
			"covering_not_exists_outer_eq999",
			`SELECT v FROM t WHERE v = 999 AND NOT EXISTS (SELECT 1 FROM e WHERE e.ck = t.v)`,
		},
		{
			"covering_exists_outer_ge",
			`SELECT v FROM t WHERE v >= 100 AND EXISTS (SELECT 1 FROM e WHERE e.ck = t.v)`,
		},
	}
	for _, sh := range shapes {
		rows, err := runQ(sh.sql)
		t.Logf("[MEASURE %-32s] class=%-11s rows=%v err=%v", sh.name, classifyGateErr(err), rows, err)
	}
}

// ---------------------------------------------------------------------------
// (c) broad isolation battery
// ---------------------------------------------------------------------------

func TestFDB_RFC173_Measure_BroadBattery(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := setupPlanShapeDB(t, "battery",
		"CREATE TABLE orders (oid BIGINT NOT NULL, cust BIGINT, status STRING, amt BIGINT, PRIMARY KEY (oid)) "+
			"CREATE INDEX status_idx ON orders (status) "+
			"CREATE INDEX cust_idx ON orders (cust) "+
			"CREATE TABLE cust (cid BIGINT NOT NULL, name STRING, region STRING, PRIMARY KEY (cid)) "+
			"CREATE TABLE line (lid BIGINT NOT NULL, oid BIGINT, sku STRING, qty BIGINT, PRIMARY KEY (lid))")

	mwjoMustExec(t, db, ctx,
		"INSERT INTO cust (cid, name, region) VALUES (1,'alice','west'),(2,'bob','east'),(3,'carol','west')")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO orders (oid, cust, status, amt) VALUES "+
			"(10,1,'shipped',100),(11,1,'pending',50),(12,2,'shipped',200),(13,3,'shipped',30),(14,2,'pending',70)")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO line (lid, oid, sku, qty) VALUES (100,10,'x',2),(101,10,'y',1),(102,12,'x',5),(103,14,'z',3)")

	// A generic runner: returns concatenated row strings + err.
	runQ := func(q string) ([]string, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		n := len(cols)
		var out []string
		for rows.Next() {
			cells := make([]any, n)
			ptrs := make([]any, n)
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			parts := make([]string, n)
			for i, c := range cells {
				parts[i] = fmt.Sprintf("%v", c)
			}
			out = append(out, strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		sort.Strings(out)
		return out, nil
	}

	values.NameReadForbidden = true
	defer func() { values.NameReadForbidden = false }()

	shapes := []struct{ name, sql string }{
		// scan / filter / project
		{"scan_filter_project", `SELECT oid, amt FROM orders WHERE amt > 60`},
		{"index_equality", `SELECT oid FROM orders WHERE status = 'shipped'`},
		{"covering_index", `SELECT status FROM orders WHERE status = 'shipped'`},
		// aggregates
		{"agg_count_star", `SELECT COUNT(*) FROM orders`},
		{"agg_group_by", `SELECT status, COUNT(*) FROM orders GROUP BY status`},
		{"agg_sum_group", `SELECT cust, SUM(amt) FROM orders GROUP BY cust`},
		{"agg_min_max", `SELECT status, MIN(amt), MAX(amt) FROM orders GROUP BY status`},
		{"agg_having", `SELECT cust, COUNT(*) FROM orders GROUP BY cust HAVING COUNT(*) > 1`},
		{"agg_no_group", `SELECT SUM(amt), AVG(amt) FROM orders`},
		// joins
		{"inner_join", `SELECT c.name, o.amt FROM cust c, orders o WHERE c.cid = o.cust`},
		{"inner_join_3way", `SELECT c.name, l.sku FROM cust c, orders o, line l WHERE c.cid = o.cust AND o.oid = l.oid`},
		{"left_join", `SELECT c.name, o.amt FROM cust c LEFT JOIN orders o ON c.cid = o.cust`},
		{"agg_over_join", `SELECT c.region, SUM(o.amt) FROM cust c, orders o WHERE c.cid = o.cust GROUP BY c.region`},
		// EXISTS / NOT EXISTS
		{"exists_corr", `SELECT name FROM cust c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.cust = c.cid)`},
		{"not_exists_corr", `SELECT name FROM cust c WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.cust = c.cid)`},
		{"exists_over_join", `SELECT name FROM cust c WHERE EXISTS (SELECT 1 FROM orders o, line l WHERE o.cust = c.cid AND l.oid = o.oid)`},
		{"exists_noncorr", `SELECT name FROM cust c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.status = 'shipped')`},
		// IN subquery
		{"in_subquery", `SELECT name FROM cust c WHERE c.cid IN (SELECT cust FROM orders WHERE status = 'shipped')`},
		{"not_in_subquery", `SELECT name FROM cust c WHERE c.cid NOT IN (SELECT cust FROM orders WHERE status = 'pending')`},
		// set ops
		{"union", `SELECT cid FROM cust WHERE region = 'west' UNION SELECT cust FROM orders WHERE status = 'pending'`},
		{"union_all", `SELECT status FROM orders UNION ALL SELECT region FROM cust`},
		// distinct / order / limit
		{"distinct", `SELECT DISTINCT status FROM orders`},
		{"order_by", `SELECT oid FROM orders ORDER BY amt DESC`},
		{"limit", `SELECT oid FROM orders ORDER BY oid LIMIT 2`},
		// CTE
		{"cte", `WITH shipped AS (SELECT cust, amt FROM orders WHERE status = 'shipped') SELECT cust, SUM(amt) FROM shipped GROUP BY cust`},
		{"cte_join", `WITH w AS (SELECT cid, name FROM cust WHERE region = 'west') SELECT w.name, o.amt FROM w, orders o WHERE w.cid = o.cust`},
		// scalar subquery
		{"scalar_subquery", `SELECT name FROM cust c WHERE c.cid = (SELECT MIN(cust) FROM orders)`},
		{"correlated_scalar", `SELECT c.name, (SELECT COUNT(*) FROM orders o WHERE o.cust = c.cid) FROM cust c`},
		// nested / composed
		{"exists_in_agg", `SELECT c.region, COUNT(*) FROM cust c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.cust = c.cid) GROUP BY c.region`},
	}
	for _, sh := range shapes {
		rows, err := runQ(sh.sql)
		t.Logf("[MEASURE %-24s] class=%-11s rows=%v err=%v", sh.name, classifyGateErr(err), rows, err)
	}
}

// ---------------------------------------------------------------------------
// Focused characterization of the two aggregate-input survivors found by the
// broad battery: GROUP BY over a FILTERED / CTE / EXISTS-filtered input.
// Each shape is run gate-OFF (baseline correctness) then gate-ON (survivor
// probe) so a SURVIVOR is proven to be a REAL exercised boundary, not a reach
// gap. Run ALONE.
// ---------------------------------------------------------------------------

func TestFDB_RFC173_Measure_AggInputCharacterize(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := setupPlanShapeDB(t, "aggchar",
		"CREATE TABLE orders (oid BIGINT NOT NULL, cust BIGINT, status STRING, amt BIGINT, PRIMARY KEY (oid)) "+
			"CREATE INDEX status_idx ON orders (status) "+
			"CREATE TABLE cust (cid BIGINT NOT NULL, name STRING, region STRING, PRIMARY KEY (cid))")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO cust (cid, name, region) VALUES (1,'alice','west'),(2,'bob','east'),(3,'carol','west')")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO orders (oid, cust, status, amt) VALUES "+
			"(10,1,'shipped',100),(11,1,'pending',50),(12,2,'shipped',200),(13,3,'shipped',30),(14,2,'pending',70)")

	runQ := func(q string) ([]string, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		n := len(cols)
		var out []string
		for rows.Next() {
			cells := make([]any, n)
			ptrs := make([]any, n)
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			parts := make([]string, n)
			for i, c := range cells {
				parts[i] = fmt.Sprintf("%v", c)
			}
			out = append(out, strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		sort.Strings(out)
		return out, nil
	}

	shapes := []struct{ name, sql string }{
		// baseline: GROUP BY over a PLAIN scan (known ordinal — 763e75ab1)
		{"agg_plain_scan", `SELECT cust, SUM(amt) FROM orders GROUP BY cust`},
		// GROUP BY over a WHERE-FILTERED scan (no CTE, no EXISTS) — is the
		// PredicatesFilter between scan and aggregate the trigger?
		{"agg_filtered_scan", `SELECT cust, SUM(amt) FROM orders WHERE status = 'shipped' GROUP BY cust`},
		// GROUP BY over a CTE (the survivor, de-sugared = agg_filtered_scan)
		{"agg_cte", `WITH s AS (SELECT cust, amt FROM orders WHERE status = 'shipped') SELECT cust, SUM(amt) FROM s GROUP BY cust`},
		// GROUP BY over an EXISTS-filtered scan (the other survivor)
		{"agg_exists_filter", `SELECT c.region, COUNT(*) FROM cust c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.cust = c.cid) GROUP BY c.region`},
		// GROUP BY over a NOT-EXISTS-filtered scan
		{"agg_not_exists_filter", `SELECT c.region, COUNT(*) FROM cust c WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.cust = c.cid) GROUP BY c.region`},
		// COUNT(*) (no SUM operand) over a filtered scan — group-key-only read
		{"agg_count_filtered", `SELECT cust, COUNT(*) FROM orders WHERE status = 'shipped' GROUP BY cust`},
		// GROUP BY over a DERIVED TABLE (subquery in FROM = a Project/Map input)
		{"agg_derived_table", `SELECT cust, SUM(amt) FROM (SELECT cust, amt FROM orders) AS d GROUP BY cust`},
		// GROUP BY over a projecting CTE with a RENAME (Project input, aliased key)
		{"agg_cte_rename", `WITH s AS (SELECT cust AS ck, amt AS a FROM orders) SELECT ck, SUM(a) FROM s GROUP BY ck`},
		// GROUP BY over a DISTINCT input (dedup node above scan)
		{"agg_over_distinct", `SELECT cust, COUNT(*) FROM (SELECT DISTINCT cust, status FROM orders) AS d GROUP BY cust`},
		// nested aggregate: GROUP BY over an aggregate subquery
		{"agg_nested", `SELECT g, COUNT(*) FROM (SELECT cust AS g, COUNT(*) AS n FROM orders GROUP BY cust) AS d GROUP BY g`},
	}
	defer func() { values.NameReadForbidden = false }()
	for _, sh := range shapes {
		values.NameReadForbidden = false
		baseRows, baseErr := runQ(sh.sql)
		plan := planExplainVia(t, ctx, db, sh.sql)
		values.NameReadForbidden = true
		rows, err := runQ(sh.sql)
		values.NameReadForbidden = false
		t.Logf("[CHAR %-24s] class=%-11s plan=%s\n  gateOFF rows=%v err=%v\n  gateON  rows=%v err=%v",
			sh.name, classifyGateErr(err), plan, baseRows, baseErr, rows, err)
	}
}

// ---------------------------------------------------------------------------
// Remaining task axes: DML read path (UPDATE/DELETE WHERE) under the armed
// gate. These mutate rows they read; a name-keyed read on the DML read path
// would be a survivor. Run ALONE.
// ---------------------------------------------------------------------------

func TestFDB_RFC173_Measure_DMLReadPath(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := setupPlanShapeDB(t, "dml",
		"CREATE TABLE orders (oid BIGINT NOT NULL, cust BIGINT, status STRING, amt BIGINT, PRIMARY KEY (oid)) "+
			"CREATE INDEX status_idx ON orders (status)")
	seed := func() {
		mwjoMustExec(t, db, ctx, "DELETE FROM orders")
		mwjoMustExec(t, db, ctx,
			"INSERT INTO orders (oid, cust, status, amt) VALUES "+
				"(10,1,'shipped',100),(11,1,'pending',50),(12,2,'shipped',200)")
	}

	execDML := func(q string) error {
		_, err := db.ExecContext(ctx, q)
		return err
	}

	shapes := []struct {
		name  string
		stmt  string
		isDML bool
	}{
		{"insert_values", `INSERT INTO orders (oid, cust, status, amt) VALUES (20,3,'new',5)`, true},
		{"update_where_pk", `UPDATE orders SET amt = 999 WHERE oid = 10`, true},
		{"update_where_indexed", `UPDATE orders SET amt = 111 WHERE status = 'pending'`, true},
		{"delete_where_indexed", `DELETE FROM orders WHERE status = 'shipped'`, true},
		{"delete_where_filter", `DELETE FROM orders WHERE amt > 40`, true},
	}
	defer func() { values.NameReadForbidden = false }()
	for _, sh := range shapes {
		seed()
		values.NameReadForbidden = false
		baseErr := execDML(sh.stmt)
		seed()
		values.NameReadForbidden = true
		err := execDML(sh.stmt)
		values.NameReadForbidden = false
		t.Logf("[DML %-22s] class=%-11s gateOFF err=%v gateON err=%v", sh.name, classifyGateErr(err), baseErr, err)
	}
}
