package query

// The translator's INPUT CONTRACT for ORDER BY: a Sort never arrives over a
// Project.
//
// Every logical builder defers the SELECT-list projection PAST the sort
// (`postSortStripProj`), which is what keeps an aggregate's private
// [keys..., calls...] row addressable to ORDER BY at all. The arm in
// translateSort that once resolved sort keys against a projection's OUTPUT
// SPELLINGS was removed as unreachable dead code (RFC-197 item 4), and nothing
// below it reconstructs the capability.
//
// The builders are pinned not to emit the shape
// (TestSortNeverSitsOverAProjection and friends, core/embedded). This file is
// the OTHER half: the consumer refuses it. The two are not redundant — a
// builder pin says "we do not emit this", and it can only cover the builders
// someone remembered to enumerate. The consumer guard covers every builder
// there will ever be, including one added tomorrow by someone who never read
// either file.
//
// Why a consumer-side guard is legitimate here and not a special-case check:
// what the TRANSLATOR accepts as logical input is the translator's own
// contract. Physical operator placement is the memo's business and gets no
// such guard. This is the boundary between the logical builders and the
// translator, and a boundary is exactly where an input contract belongs.
//
// Without it the shape does not error — it falls through to the flat-name bake
// and resolves the sort key against whatever layout expressionOutputColumns
// reports for a projection, i.e. a leaf-name match in a DIFFERENT ordinal
// domain. The observable is a wrong sort ORDER, silently.

import (
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/query/logical"
)

func sortDeclineMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

func TestTranslateSort_DeclinesASortOverAProjection(t *testing.T) {
	t.Parallel()

	md := sortDeclineMetaData(t)

	// The shape no builder emits: the SELECT-list projection UNDER the sort.
	proj := logical.NewProject(logical.NewScan("Order", "o"), []string{"ORDER_ID"}, []string{""})
	sortOverProj := logical.NewSort(proj, []logical.SortKey{{Expr: "ORDER_ID"}})

	ref, _, err := TranslateToCascadesWithError(sortOverProj, md)
	if err == nil {
		t.Fatalf("Sort-over-Project translated without complaint (ref=%v). "+
			"translateSort has no ordinal-addressed pull-up onto a projection's "+
			"output, so the key falls through to a flat-name bake against a "+
			"different ordinal domain — a silently wrong sort order. The shape "+
			"must be REFUSED at the translator boundary, not interpreted.", ref)
	}
	if !strings.Contains(err.Error(), "ORDER BY over a projection") {
		t.Fatalf("Sort-over-Project was rejected, but not by the layering guard: %v\n"+
			"An incidental rejection is not a defense — it can move or disappear "+
			"without anyone noticing the contract went with it.", err)
	}
}

// The control. Without it the test above would pass just as happily on a
// translator that rejected every ORDER BY, and the guard would be pinning
// nothing about projections.
func TestTranslateSort_AcceptsTheLayeringTheBuildersActuallyEmit(t *testing.T) {
	t.Parallel()

	md := sortDeclineMetaData(t)

	// Project ABOVE Sort — what every builder emits.
	sortOverScan := logical.NewSort(
		logical.NewScan("Order", "o"),
		[]logical.SortKey{{Expr: "ORDER_ID"}},
	)
	projOverSort := logical.NewProject(sortOverScan, []string{"ORDER_ID"}, []string{""})

	if _, _, err := TranslateToCascadesWithError(projOverSort, md); err != nil {
		t.Fatalf("Project-over-Sort — the layering the builders emit — was rejected: %v", err)
	}

	// A bare sort over a scan must keep working too: the guard is about the
	// input's TYPE, and a type check that over-fires takes the whole ORDER BY
	// surface with it.
	if _, _, err := TranslateToCascadesWithError(sortOverScan, md); err != nil {
		t.Fatalf("Sort-over-Scan was rejected by the projection guard: %v", err)
	}
}
