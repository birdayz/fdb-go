package otel

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

type chLayoutReceiptForTest struct {
	key  string
	sql  string
	body string
}

func chLayoutReceiptsForTest(database string, attrs bool) []chLayoutReceiptForTest {
	out := []chLayoutReceiptForTest{{
		key:  "spans_show_create",
		sql:  "SHOW CREATE TABLE " + database + ".spans FORMAT TSVRaw",
		body: "CREATE TABLE observed.spans (trace_id String)\nENGINE = MergeTree ORDER BY trace_id SETTINGS index_granularity = 16384, index_granularity_bytes = 1048576",
	}}
	if attrs {
		out = append(out, chLayoutReceiptForTest{
			key:  "attr_lookup_show_create",
			sql:  "SHOW CREATE TABLE " + database + ".attr_lookup FORMAT TSVRaw",
			body: "CREATE TABLE observed.attr_lookup (k String)\nENGINE = MergeTree ORDER BY k SETTINGS index_granularity = 32768, index_granularity_bytes = 2097152",
		})
	}
	return append(out,
		chLayoutReceiptForTest{
			key:  "merge_tree_settings",
			sql:  "SELECT name AS table,engine,engine_full,sorting_key,create_table_query FROM system.tables WHERE database={db:String} AND name IN ('spans','attr_lookup') ORDER BY name FORMAT JSONEachRow",
			body: `{"table":"spans", "engine":"MergeTree", "engine_full":"MergeTree ORDER BY trace_id SETTINGS index_granularity = 16384", "sorting_key":"trace_id", "create_table_query":"CREATE TABLE observed.spans SETTINGS index_granularity = 16384"}`,
		},
		chLayoutReceiptForTest{
			key:  "part_marks",
			sql:  "SELECT table,toString(sum(rows)) AS rows,toString(sum(marks)) AS marks,toString(count()) AS parts FROM system.parts WHERE database={db:String} AND active GROUP BY table ORDER BY table FORMAT JSONEachRow",
			body: "{\"table\":\"spans\",\"rows\":\"29\",\"marks\":\"11\",\"parts\":\"3\"}\n{\"table\":\"attr_lookup\",\"rows\":\"71\",\"marks\":\"13\",\"parts\":\"4\"}",
		},
		chLayoutReceiptForTest{
			key:  "projection_marks",
			sql:  "SELECT name,toString(sum(rows)) AS rows,toString(sum(marks)) AS marks,toString(count()) AS parts FROM system.projection_parts WHERE database={db:String} AND table='spans' AND active GROUP BY name ORDER BY name FORMAT JSONEachRow",
			body: "{\"name\":\"by_span\",\"rows\":\"29\",\"marks\":\"17\",\"parts\":\"5\"}\n{\"name\":\"by_trace\",\"rows\":\"29\",\"marks\":\"19\",\"parts\":\"6\"}",
		},
	)
}

func chLayoutAssertRequestForTest(t *testing.T, r *http.Request, receipt chLayoutReceiptForTest, database string) {
	t.Helper()
	if r.URL.Query().Get("query") != receipt.sql {
		t.Errorf("layout SQL=%q, want %q", r.URL.Query().Get("query"), receipt.sql)
	}
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("layout method/content type=%s/%q", r.Method, r.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) != 0 {
		t.Errorf("layout request body=%q error=%v", body, err)
	}
	params := map[string][]string{}
	for key, values := range r.URL.Query() {
		if strings.HasPrefix(key, "param_") {
			params[key] = values
		}
	}
	wantParams := map[string][]string{}
	if !strings.HasPrefix(receipt.sql, "SHOW CREATE TABLE ") {
		wantParams["param_db"] = []string{database}
	}
	if !reflect.DeepEqual(params, wantParams) {
		t.Errorf("layout bound parameters=%v, want %v", params, wantParams)
	}
}

func TestCHAuditMergeTreeLayoutReportsObservedReceipts(t *testing.T) {
	t.Parallel()
	for _, attrs := range []bool{false, true} {
		t.Run(fmt.Sprintf("attrs=%t", attrs), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			var b *CHBackend
			var receipts []chLayoutReceiptForTest
			b = chHTTPBackend(t, attrs, func(w http.ResponseWriter, r *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(receipts) {
					t.Errorf("extra layout request %d: %s", index, r.URL.Query().Get("query"))
					http.Error(w, "unexpected layout request", http.StatusBadRequest)
					return
				}
				chLayoutAssertRequestForTest(t, r, receipts[index], b.database)
				fmt.Fprint(w, " \n"+receipts[index].body+"\n\t")
			})
			b.cfg.CHIndexGranularity = 64
			receipts = chLayoutReceiptsForTest(b.database, attrs)
			wantCount := 4
			if attrs {
				wantCount = 5
			}
			if len(receipts) != wantCount || strings.Contains(receipts[0].body, "index_granularity = 64") || !strings.Contains(receipts[0].body, "index_granularity = 16384") {
				t.Fatal("layout witness population is empty or requested/observed settings do not differ")
			}
			details := map[string]string{"existing": "preserved"}
			if err := b.auditMergeTreeLayout(t.Context(), details); err != nil {
				t.Fatal(err)
			}
			if int(calls.Load()) != wantCount {
				t.Fatalf("layout requests=%d, want %d", calls.Load(), wantCount)
			}
			want := map[string]string{"existing": "preserved", "ch_index_granularity_requested": "64"}
			for _, receipt := range receipts {
				want[receipt.key] = receipt.body
			}
			if explanation := details["granularity_evidence"]; !strings.Contains(explanation, "not inferred") || !strings.Contains(explanation, "adaptive") {
				t.Fatalf("missing observed-versus-requested granularity explanation: %q", explanation)
			}
			want["granularity_evidence"] = details["granularity_evidence"]
			if !reflect.DeepEqual(details, want) {
				t.Fatalf("layout receipts changed raw server evidence or fabricated settings/marks: got=%v want=%v", details, want)
			}
		})
	}
}

func TestCHAuditMergeTreeLayoutFailuresStopAtEveryStage(t *testing.T) {
	t.Parallel()
	for _, attrs := range []bool{false, true} {
		// The independent expected population covers each SHOW CREATE plus
		// system.tables, parent marks, and projection marks in both layouts.
		stageCount := 4
		if attrs {
			stageCount = 5
		}
		for failAt := 0; failAt < stageCount; failAt++ {
			for _, failure := range []string{"http", "empty", "whitespace"} {
				t.Run(fmt.Sprintf("attrs=%t/stage=%d/%s", attrs, failAt, failure), func(t *testing.T) {
					t.Parallel()
					var calls atomic.Int64
					var b *CHBackend
					var receipts []chLayoutReceiptForTest
					cause := "controlled layout receipt failure " + strconv.Itoa(failAt)
					b = chHTTPBackend(t, attrs, func(w http.ResponseWriter, r *http.Request) {
						index := int(calls.Add(1)) - 1
						if index >= len(receipts) {
							t.Errorf("extra layout request %d", index)
							http.Error(w, "unexpected layout request", http.StatusBadRequest)
							return
						}
						chLayoutAssertRequestForTest(t, r, receipts[index], b.database)
						if index == failAt {
							if failure == "http" {
								http.Error(w, cause, http.StatusServiceUnavailable)
							} else if failure == "whitespace" {
								fmt.Fprint(w, " \n\t")
							}
							return
						}
						fmt.Fprint(w, receipts[index].body+"\n")
					})
					b.cfg.CHIndexGranularity = 64
					receipts = chLayoutReceiptsForTest(b.database, attrs)
					if len(receipts) != stageCount || failAt >= len(receipts) {
						t.Fatal("layout failure matrix has a missing stage witness")
					}
					details := map[string]string{"existing": "preserved"}
					err := b.auditMergeTreeLayout(t.Context(), details)
					if failure == "http" {
						var transport *HTTPError
						if !errors.As(err, &transport) || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), cause) {
							t.Fatalf("layout stage %d lost HTTP failure: %v", failAt, err)
						}
					} else {
						var audit *AuditError
						if !errors.As(err, &audit) {
							t.Fatalf("empty layout stage %d must return typed AuditError: %v", failAt, err)
						}
					}
					if got := int(calls.Load()); got != failAt+1 {
						t.Fatalf("layout failure stage=%d requests=%d, want %d (stop immediately)", failAt, got, failAt+1)
					}
					want := map[string]string{"existing": "preserved"}
					for _, receipt := range receipts[:failAt] {
						want[receipt.key] = receipt.body
					}
					if !reflect.DeepEqual(details, want) {
						t.Fatalf("layout failure fabricated failed/later receipts: got=%v want=%v", details, want)
					}
				})
			}
		}
	}
}
