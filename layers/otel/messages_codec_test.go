package otel

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// TestNextWriterID checks every shard against a brute-force search for the
// smallest ID above the counter that lands in the shard.
func TestNextWriterID(t *testing.T) {
	t.Parallel()
	if got := nextWriterID(0, 0); got != messageTextShards {
		t.Fatalf("nextWriterID(0, 0) = %d, want %d", got, messageTextShards)
	}
	if got := nextWriterID(0, 1); got != 1 {
		t.Fatalf("nextWriterID(0, 1) = %d, want 1", got)
	}
	for _, last := range []int64{0, 1, 15, 16, 17, 31, 32, 1000} {
		for shard := range int64(messageTextShards) {
			want := last + 1
			for want%messageTextShards != shard {
				want++
			}
			if got := nextWriterID(last, shard); got != want {
				t.Fatalf("nextWriterID(%d, %d) = %d, want %d", last, shard, got, want)
			}
		}
	}
}

// TestSplitMessageParts pins the part layout of an element's MESSAGES rows:
// raw chunks of at most messagePartRawBytes, and role and text segments only
// with fulltext.
func TestSplitMessageParts(t *testing.T) {
	t.Parallel()
	raw := deltaRandom(2*messagePartRawBytes+7, 3)
	mp, err := splitMessageParts(&messageElement{encoding: messageEncodingAny, raw: raw}, false, messageSegmentTokens)
	if err != nil {
		t.Fatal(err)
	}
	if mp.role != "" || mp.segments != nil || mp.unindexed != 0 || len(mp.chunks) != 3 || len(mp.chunks[0]) != messagePartRawBytes || len(mp.chunks[1]) != messagePartRawBytes || len(mp.chunks[2]) != 7 {
		t.Fatalf("raw-only parts: role %q, %d segments, %d unindexed, %d chunks", mp.role, len(mp.segments), mp.unindexed, len(mp.chunks))
	}
	if !bytes.Equal(bytes.Join(mp.chunks, nil), raw) {
		t.Fatal("chunks do not reassemble the element")
	}

	text := largeText(4*messageSegmentBytes) + " " + strings.Repeat("z", maxIndexedTokenBytes+1)
	el := &messageElement{encoding: messageEncodingAny, raw: ftsAnyWire(t, deltaKvlist(deltaKV("role", deltaStr(" User ")), deltaKV("content", deltaStr(text))))}
	mp, err = splitMessageParts(el, true, messageSegmentTokens)
	if err != nil {
		t.Fatal(err)
	}
	want, unindexed, err := segmentMessageText(text, messageSegmentTokens)
	if err != nil {
		t.Fatal(err)
	}
	if mp.role != "user" || len(mp.segments) < 2 || !reflect.DeepEqual(mp.segments, want) || unindexed != 1 || mp.unindexed != 1 || len(mp.chunks) != 1 || !bytes.Equal(mp.chunks[0], el.raw) {
		t.Fatalf("fulltext parts: role %q, %d segments (want %d), %d unindexed, %d chunks", mp.role, len(mp.segments), len(want), mp.unindexed, len(mp.chunks))
	}

	long := &messageElement{encoding: messageEncodingAny, raw: ftsAnyWire(t, deltaKvlist(deltaKV("role", deltaStr(strings.Repeat("r", maxMessageRoleBytes+1)))))}
	_, err = splitMessageParts(long, true, messageSegmentTokens)
	requireTyped[*AdmissionError](t, "overlong role", err)
	if _, err := splitMessageParts(long, false, messageSegmentTokens); err != nil {
		t.Fatalf("raw-only parts check the role: %v", err)
	}
}
