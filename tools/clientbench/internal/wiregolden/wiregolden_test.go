package wiregolden

import (
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/wire/types"
)

func marker(name string) Request {
	return Request{Kind: "GetValueRequest", Msg: &types.GetValueRequest{Key: []byte(MarkerPrefix + name)}}
}

func get(key string, version int64) Request {
	m := &types.GetValueRequest{Key: []byte(key), Version: version}
	m.Reply.Token[0] = 7     // a reply token differs per run
	m.SpanContext.SpanID = 9 // so does the span
	return Request{Kind: "GetValueRequest", Msg: m}
}

// Setup and the end marker's requests are left out; each scenario numbers
// its versions from V1; the key prefix, reply tokens and spans are normalized.
func TestTranscriptSplitsAndNormalizes(t *testing.T) {
	reqs := []Request{
		get("p/setup", 100), // before the first marker
		marker("one"),
		get("p/a", 500), get("p/b", 500), get("p/c", 600),
		marker("two"),
		get("p/a", 900),
		{Kind: "FileID(2828131)"},
		marker("~end"),
		get("p/after", 1000),
	}
	got := Transcript(reqs, "p/")
	want := `== one
GetValueRequest {Key='<p>a' Version=V1}
GetValueRequest {Key='<p>b' Version=V1}
GetValueRequest {Key='<p>c' Version=V2}
== two
GetValueRequest {Key='<p>a' Version=V1}
FileID(2828131)
`
	if got != want {
		t.Fatalf("WIREGOLDEN_TRANSCRIPT:\n%s\nwant\n%s", got, want)
	}
}

// A self-conflicting range key's random bytes are normalized, whatever follows.
func TestTranscriptSelfConflictKey(t *testing.T) {
	sc := selfConflictPrefix + strings.Repeat("\x9f", 16)
	m := &types.CommitTransactionRequest{}
	m.Transaction.ReadConflictRanges = []types.KeyRangeRef{{Begin: []byte(sc), End: []byte(sc + "\x00")}}
	m.Transaction.ReadSnapshot = 42
	m.IdempotencyId = []byte("id")
	got := Transcript([]Request{marker("sc"), {Kind: "CommitTransactionRequest", Msg: m}}, "p/")
	want := `== sc
CommitTransactionRequest {Transaction={ReadConflictRanges=[{Begin='\xff/SC/<random>' End='\xff/SC/<random>\x00'}] ReadSnapshot=V1}}
`
	if got != want {
		t.Fatalf("WIREGOLDEN_SC:\n%s\nwant\n%s", got, want)
	}
}

// Versions with a fixed meaning keep their value.
func TestTranscriptKeepsSpecialVersions(t *testing.T) {
	got := Transcript([]Request{marker("v"), get("k", 1<<63-1), get("k", -1)}, "")
	want := "== v\nGetValueRequest {Key='k' Version=9223372036854775807}\nGetValueRequest {Key='k' Version=-1}\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestFileIDSkipsProtocolVersion(t *testing.T) {
	plain := []byte{0, 0, 0, 0, 0x2a, 0, 0, 0}
	versioned := append([]byte{1, 2, 3, 4, 5, 6, 0xDB, 0x0F}, plain...)
	for _, b := range [][]byte{plain, versioned} {
		if id, ok := fileID(b); !ok || id != 0x2a {
			t.Fatalf("fileID(%x) = %d, %v", b, id, ok)
		}
	}
	if _, ok := fileID([]byte{1, 2}); ok {
		t.Fatal("a short body has no file identifier")
	}
}
