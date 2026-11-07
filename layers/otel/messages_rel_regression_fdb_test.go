package otel

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Message regressions of the size fallback and overlong tokens on tenant
// stores, through relMsgEnv.

func TestRelMessageRegressionsRealFDB(t *testing.T) {
	t.Parallel()
	h := newQRHarness(t)
	for name, test := range map[string]func(*testing.T, *qrHarness){
		"large_nonposting":         testRelLargeNonposting,
		"overlong_normalized_word": testRelOverlongNormalizedWord,
		"sibling_fallback":         testRelSiblingFallback,
		"multipart_fallback":       testRelMultipartFallback,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, h)
		})
	}
}

// relStoredMessages lists the MESSAGES rows of the store, committed.
func relStoredMessages(ctx context.Context, e *relMsgEnv) ([]*otelv1pb.MESSAGES, error) {
	var out []*otelv1pb.MESSAGES
	_, err := e.r.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := e.r.store.ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		out = nil
		rows, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
		for _, r := range rows {
			if m, ok := r.Record.(*otelv1pb.MESSAGES); ok {
				out = append(out, m)
			}
		}
		return nil, err
	})
	return out, err
}

// pendingMessages lists the MESSAGES rows a transaction's store sees.
func pendingMessages(ctx context.Context, store *recordlayer.FDBRecordStore) ([]*otelv1pb.MESSAGES, error) {
	rows, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
	var out []*otelv1pb.MESSAGES
	for _, r := range rows {
		if m, ok := r.Record.(*otelv1pb.MESSAGES); ok {
			out = append(out, m)
		}
	}
	return out, err
}

// testRelLargeNonposting: large messages of whitespace or one long word keep
// their raw bytes and positions. A 12 MiB word is one overlong token:
// MESSAGES_TEXT holds otelsql.OverlongTokenPlaceholder in its position, so
// the word's message has the placeholder's posting and a query for the
// placeholder finds it.
func testRelLargeNonposting(t *testing.T, h *qrHarness) {
	for _, name := range []string{"whitespace", "word", "mixed-whitespace", "mixed-word"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			middle := strings.Repeat(" ", 12<<20)
			word := strings.Contains(name, "word")
			wantUnindexed := int64(0)
			if word {
				middle = strings.Repeat("x", 12<<20)
				wantUnindexed = 1
			}
			text := middle
			mixed := strings.HasPrefix(name, "mixed-")
			if mixed {
				text = "alpha " + middle + " omega"
			}
			wantPostings := map[[2]bool]int64{{false, false}: 0, {false, true}: 1, {true, false}: 2, {true, true}: 3}[[2]bool{mixed, word}]
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(text))))
			stats, err := e.export(ctx, []*tracepb.ResourceSpans{input})
			if err != nil {
				t.Fatal(err)
			}
			if stats.Messages.Written != 1 || stats.Messages.UnindexedTokens != wantUnindexed || stats.Messages.Postings != wantPostings {
				t.Fatalf("raw-token counters: %+v, want unindexed %d, postings %d", stats.Messages, wantUnindexed, wantPostings)
			}
			got, err := e.load(ctx, ftsTrace(1), ftsSpan(1))
			if err != nil {
				t.Fatal(err)
			}
			requireProtoEqual(t, "large raw reconstruction", got, input)
			placeholder := 0
			if word {
				placeholder = 1
			}
			if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: otelsql.OverlongTokenPlaceholder}); n != placeholder {
				t.Fatalf("placeholder search: %d, want %d", n, placeholder)
			}
			if mixed {
				if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "alpha omega", Mode: MessageMatchAll}); n != 1 {
					t.Fatalf("all search: %d", n)
				}
				wantPhrase := 1 - placeholder
				if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "alpha omega", Mode: MessageMatchPhrase}); n != wantPhrase {
					t.Fatalf("phrase search %d want %d", n, wantPhrase)
				}
				if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "alphaomega", Mode: MessageMatchAll}); n != 0 {
					t.Fatalf("false concatenated token: %d", n)
				}
				id := e.presence(t, ctx, []*tracepb.ResourceSpans{input})[0]
				e.run(t, ctx, func(store *recordlayer.FDBRecordStore) error {
					cmp := recordlayer.NewTextWithMaxDistanceComparisonTokens([]string{"alpha", "omega"}, 1, "", "")
					group := tuple.Tuple{"", otelsql.MessageShard(id.writer)}
					ids, err := textScanMatches(ctx, store, recordlayer.NewTextScan(store.GetRecordMetaData().GetIndex(messagesTextIndex), group, cmp, nil), 0, newQueryWork(e.r.cfg))
					if err == nil && len(ids) != wantPhrase {
						t.Errorf("ALL_WITHIN distance 1=%v want %d", ids, wantPhrase)
					}
					return err
				})
			}
			again, err := e.export(ctx, []*tracepb.ResourceSpans{input})
			if err != nil || again.Messages.Written != 0 || again.Messages.Present != 1 {
				t.Fatalf("large dedup: %+v / %v", again, err)
			}
		})
	}
}

// testRelOverlongNormalizedWord: a 12 MiB raw word whose normalized token is
// short ("a" and combining marks) is indexed as that token, alone or between
// other words, is not counted unindexed, and loads back unchanged.
func testRelOverlongNormalizedWord(t *testing.T, h *qrHarness) {
	for _, between := range []bool{false, true} {
		t.Run(fmt.Sprintf("between=%t", between), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			text := "a" + strings.Repeat("\u0301", 6<<20)
			query := "a"
			if between {
				text = "zebra " + text + " omega"
				query = "zebra a omega"
			}
			segments, unindexed, err := segmentMessageText(text, messageSegmentTokens)
			if err != nil || unindexed != 0 || len(segments) == 0 {
				t.Fatalf("normalized segments=%d unindexed=%d err=%v", len(segments), unindexed, err)
			}
			for _, segment := range segments {
				if len(segment.text) > messageSegmentBytes {
					t.Fatalf("indexed segment bytes=%d", len(segment.text))
				}
			}
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(text))))
			stats, err := e.export(ctx, []*tracepb.ResourceSpans{input})
			if err != nil {
				t.Fatal(err)
			}
			if stats.Messages.Written != 1 || stats.Messages.UnindexedTokens != 0 {
				t.Fatalf("message outcomes: %+v", stats.Messages)
			}
			got, err := e.load(ctx, ftsTrace(1), ftsSpan(1))
			if err != nil {
				t.Fatal(err)
			}
			requireProtoEqual(t, "lossless raw combining word", got, input)
			if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: query, Mode: MessageMatchAll}); n != 1 {
				t.Fatalf("normalized search count=%d", n)
			}
		})
	}
}

// testRelSiblingFallback: after a group commit of P and Q fails as too large,
// P's fallback commits and Q's fails; P's committed outcome is kept,
// terminal or not.
func testRelSiblingFallback(t *testing.T, h *qrHarness) {
	for _, terminal := range []bool{true, false} {
		t.Run(fmt.Sprintf("terminal=%t", terminal), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			var texts []string
			for _, prefix := range []string{"pword", "qword"} {
				words := make([]string, 600)
				for i := range words {
					words[i] = fmt.Sprintf("%s%04d", prefix, i)
				}
				texts = append(texts, strings.Join(words, " "))
			}
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(texts[0]), deltaStr(texts[1]))))
			d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input})
			if err != nil || len(d.elements) != 2 {
				t.Fatalf("fixture: %v / %v", d, err)
			}
			hash := func(i int) []byte { return otelsql.MessageContentHash(d.elements[i].encoding, d.elements[i].raw) }
			p, err := planRelMessage(d.elements[0], hash(0), true, 256)
			if err != nil {
				t.Fatal(err)
			}
			q, err := planRelMessage(d.elements[1], hash(1), true, 64)
			if err != nil {
				t.Fatal(err)
			}
			var extraRetry, initialFailure, selectedQ atomic.Bool
			sentinel := errors.New("Q terminal failure after P committed")
			e.m.hook = func(stage string, store *recordlayer.FDBRecordStore) error {
				if stage != "message-before-commit" {
					return nil
				}
				if !extraRetry.Swap(true) {
					return fdb.Error{Code: 1020}
				}
				ids, err := e.presenceErr(ctx, []*tracepb.ResourceSpans{input})
				if err != nil {
					return err
				}
				rows, err := pendingMessages(ctx, store)
				if err != nil {
					return err
				}
				if !initialFailure.Load() {
					heads := 0
					for _, m := range rows {
						if m.GetPART() == 0 && (string(m.GetCONTENT_HASH()) == string(hash(0)) || string(m.GetCONTENT_HASH()) == string(hash(1))) {
							heads++
						}
					}
					if heads != 2 || ids[0].valid() || ids[1].valid() {
						return errors.New("initial combined P/Q heads control failed")
					}
					initialFailure.Store(true)
					return fdb.Error{Code: 2101}
				}
				if selectedQ.Load() || !ids[0].valid() {
					return nil
				}
				if ids[1].valid() {
					return errors.New("Q became visible before selected failure")
				}
				pendingQ := false
				for _, m := range rows {
					if (messageID{m.GetWRITER(), m.GetSEQ()}) != ids[0] {
						pendingQ = true
					}
				}
				if !pendingQ {
					return nil
				}
				selectedQ.Store(true)
				if terminal {
					return sentinel
				}
				return fdb.Error{Code: 2101}
			}
			stats, err := e.export(ctx, []*tracepb.ResourceSpans{input})
			if !extraRetry.Load() || !initialFailure.Load() || !selectedQ.Load() {
				t.Fatalf("did not reach retry/combined/Q-failure boundaries: retry=%v combined=%v selectedQ=%v err=%v", extraRetry.Load(), initialFailure.Load(), selectedQ.Load(), err)
			}
			wantWritten, wantParts, wantPostings := int64(1), int64(len(p.records)), p.postings
			if terminal {
				if !errors.Is(err, sentinel) {
					t.Fatalf("terminal cause lost: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				wantWritten = 2
				wantParts += int64(len(q.records))
				wantPostings += q.postings
				got, err := e.load(ctx, ftsTrace(1), ftsSpan(1))
				if err != nil {
					t.Fatal(err)
				}
				requireProtoEqual(t, "sibling fallback", got, input)
			}
			if s := stats.Messages; s.Written != wantWritten || s.Present != 0 || s.Parts != wantParts || s.Postings != wantPostings {
				t.Fatalf("committed outcomes %+v want Written=%d Present=0 Parts=%d Postings=%d", s, wantWritten, wantParts, wantPostings)
			}
			e.m.hook = nil
			if ids := e.presence(t, ctx, []*tracepb.ResourceSpans{input}); !ids[0].valid() || ids[1].valid() == terminal {
				t.Fatalf("durable header outcomes: %v", ids)
			}
		})
	}
}

// testRelMultipartFallback: a large message whose tail committed before
// transaction_too_large is replanned under a fresh seq; the abandoned tails
// stay unpublished (no head, no hash) and unsearchable.
func testRelMultipartFallback(t *testing.T, h *qrHarness) {
	rng := rand.New(rand.NewPCG(7, 11))
	raw := make([]byte, 7<<20)
	for i := range raw {
		raw[i] = byte('a' + rng.IntN(26))
	}
	words := make([]string, 1100)
	for i := range words {
		words[i] = fmt.Sprintf("term%04d", i)
	}
	text := strings.Join(words, " ") + " " + string(raw)
	for _, exhaust := range []bool{false, true} {
		t.Run(fmt.Sprintf("exhaust=%t", exhaust), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			e := h.msgEnv(t, ctx, messagesFullText, nil)
			input := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr(text))))
			d, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{input})
			if err != nil {
				t.Fatal(err)
			}
			el := d.elements[0]
			hash := otelsql.MessageContentHash(el.encoding, el.raw)
			initial, err := planRelMessage(el, hash, true, messageSegmentTokens)
			if err != nil || len(initial.records) < 3 {
				t.Fatalf("initial large plan=%v err=%v", initial, err)
			}
			var extraRetry, selectedFailure atomic.Bool
			var abandoned messageID
			var committedParts int64
			e.m.hook = func(stage string, store *recordlayer.FDBRecordStore) error {
				if stage != "message-before-commit" {
					return nil
				}
				if !extraRetry.Swap(true) {
					return fdb.Error{Code: 1020}
				}
				if !selectedFailure.Load() {
					// Select by durable progress; the failing transaction holds only tails.
					pending, err := pendingMessages(ctx, store)
					if err != nil {
						return err
					}
					for _, m := range pending {
						if m.GetPART() == 0 {
							return errors.New("selected transaction was a head, not a tail")
						}
					}
					rows, err := relStoredMessages(ctx, e)
					if err != nil {
						return err
					}
					committedParts, abandoned = 0, messageID{}
					for _, m := range rows {
						if m.GetPART() == 0 {
							return errors.New("premature committed head")
						}
						abandoned = messageID{m.GetWRITER(), m.GetSEQ()}
						committedParts++
					}
					if committedParts == 0 {
						return nil // the first tail has not committed yet
					}
					selectedFailure.Store(true)
					return fdb.Error{Code: 2101}
				}
				if exhaust {
					return fdb.Error{Code: 2101}
				}
				return nil
			}
			stats, err := e.export(ctx, []*tracepb.ResourceSpans{input})
			if !extraRetry.Load() || !selectedFailure.Load() || committedParts == 0 || !abandoned.valid() {
				t.Fatalf("missing retry/committed tail evidence: retry=%v selected=%v err=%v", extraRetry.Load(), selectedFailure.Load(), err)
			}
			if exhaust {
				if code, ok := fdbErrorCode(err); !ok || code != 2101 {
					t.Fatalf("terminal cause=%v", err)
				}
				if stats.Messages.Written != 0 || stats.Messages.Present != 0 || stats.Messages.Parts != committedParts || stats.Messages.Postings != 0 {
					t.Fatalf("exhausted outcomes=%+v committedParts=%d", stats.Messages, committedParts)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				fallback, err := planRelMessage(el, hash, true, messageSegmentTokens/4)
				if err != nil {
					t.Fatal(err)
				}
				if s := stats.Messages; s.Written != 1 || s.Present != 0 || s.Parts != committedParts+int64(len(fallback.records)) || s.Postings != fallback.postings {
					t.Fatalf("fallback outcomes=%+v abandonedParts=%d publishedParts=%d", s, committedParts, len(fallback.records))
				}
				got, err := e.load(ctx, ftsTrace(1), ftsSpan(1))
				if err != nil {
					t.Fatal(err)
				}
				requireProtoEqual(t, "large-path fallback reconstruction", got, input)
			}
			e.m.hook = nil
			rows, err := relStoredMessages(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			retained := map[int64]bool{}
			for _, m := range rows {
				if (messageID{m.GetWRITER(), m.GetSEQ()}) != abandoned {
					continue
				}
				part := m.GetPART()
				if part <= 0 || part > committedParts || retained[part] {
					t.Fatalf("unexpected retained abandoned part: id=%v part=%d", abandoned, part)
				}
				retained[part] = true
			}
			if int64(len(retained)) != committedParts {
				t.Fatalf("abandoned tail retention: id=%v rows=%v want parts 1..%d", abandoned, retained, committedParts)
			}
			if id := e.presence(t, ctx, []*tracepb.ResourceSpans{input})[0]; id == abandoned || id.valid() == exhaust {
				t.Fatalf("dedup published abandoned/missing ID: %v", id)
			}
			want := 1
			if exhaust {
				want = 0
			}
			if n := relFtsCount(t, ctx, e, MessageFullTextQuery{Query: "term0000 term1099", Mode: MessageMatchAll}); n != want {
				t.Fatalf("abandoned IDs searchable: count=%d want=%d", n, want)
			}
		})
	}
}
