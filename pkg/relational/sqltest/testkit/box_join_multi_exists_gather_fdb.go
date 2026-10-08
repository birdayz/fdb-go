package testkit

// Cert for the under-EXISTS LEFT/RIGHT box unnest: at verdict None it takes the
// GATHERED ordinal cluster (admitted by unnestExistentialGatherOK /
// admitExistentialGather) and resolves positionally instead of by name. The row
// pins prove correctness against real FDB (the dup-named A.K/B.K leg the
// qualified EXISTS correlation must disambiguate — a name-keyed lookup would
// conflate it); the plan-only pins prove the admitted LEFT box plans cleanly
// (the INNER cluster, E-1a's class, still resolves by name). R1 (SelectMergeRule
// flattening the gathered select into the SARG-losing N-way existential wrap) is
// asserted absent in every plan.

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// existsGatherSchemaMetadata builds the A/B/EE/EEV schema shared by the row cert (FDB
// execution) and the plan-only sweep below. A(AID=1,K=100,ARR=[7,8]);
// B(BID=2,K=110) — A.K/B.K dup-named, the leg a qualified EXISTS correlation
// must disambiguate; EE(CK) is the leg-correlation table, EEV(VK) the element
// one.
func ExistsGatherSchemaMetadata(tb testing.TB) *recordlayer.RecordMetaData {
	tb.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("s3s0")
	b.AddTable("A", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("B", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	b.AddTable("EE", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CK", api.NewLongType(false), 1),
	}, []string{"CK"})
	b.AddTable("EEV", []metadata.ColumnSpec{
		metadata.NewColumnSpec("VK", api.NewLongType(false), 1),
	}, []string{"VK"})
	tmpl, err := b.Build()
	if err != nil {
		tb.Fatal(err)
	}
	return tmpl.Underlying()
}
