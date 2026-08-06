package client

// Transaction tag carriage: the limits C++ enforces in TagSet::addTag, the
// TAG vs AUTO_THROTTLE_TAG split, the TagSet byte encoding the commit request
// carries, and the per-batch occurrence counts the GRV request carries.

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func errCode(t *testing.T, err error) int {
	t.Helper()
	var fe *wire.FDBError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *wire.FDBError, got %T (%v)", err, err)
	}
	return int(fe.Code)
}

// TagThrottle.actor.cpp:35 — length is checked before anything else, against
// MAX_TRANSACTION_TAG_LENGTH=255. 255 fits; 256 does not.
func TestSetTagTooLong(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}

	if err := tx.SetTag(strings.Repeat("a", maxTransactionTagLength)); err != nil {
		t.Fatalf("255-byte tag must be accepted, got %v", err)
	}
	err := tx.SetTag(strings.Repeat("b", maxTransactionTagLength+1))
	if err == nil {
		t.Fatal("256-byte tag must be rejected")
	}
	if got := errCode(t, err); got != 2110 {
		t.Errorf("tag_too_long code = %d, want 2110", got)
	}
}

// TagThrottle.actor.cpp:38 — MAX_TAGS_PER_TRANSACTION=5, and the guard is
// `>=` evaluated BEFORE the tag is added, so five tags fit and the sixth fails.
func TestSetTagTooMany(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	for i, tag := range []string{"a", "b", "c", "d", "e"} {
		if err := tx.SetTag(tag); err != nil {
			t.Fatalf("tag %d (%q) must be accepted, got %v", i, tag, err)
		}
	}
	if len(tx.Tags()) != maxTagsPerTransaction {
		t.Fatalf("expected %d tags, got %d", maxTagsPerTransaction, len(tx.Tags()))
	}
	err := tx.SetTag("f")
	if err == nil {
		t.Fatal("sixth tag must be rejected")
	}
	if got := errCode(t, err); got != 2109 {
		t.Errorf("too_many_tags code = %d, want 2109", got)
	}
}

// The count guard sits BEFORE the duplicate check in C++, so re-adding an
// existing tag to a FULL set still fails. A dedup-first implementation would
// return nil here and silently diverge.
func TestSetTagDuplicateAtCapStillTooMany(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	for _, tag := range []string{"a", "b", "c", "d", "e"} {
		if err := tx.SetTag(tag); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	err := tx.SetTag("a") // already present, but the set is full
	if err == nil {
		t.Fatal("duplicate tag on a full set must still report too_many_tags")
	}
	if got := errCode(t, err); got != 2109 {
		t.Errorf("code = %d, want 2109", got)
	}
}

// Below the cap, a duplicate is silently absorbed and does not grow the set
// (TagThrottle.actor.cpp:42-47 pushes only when find() misses).
func TestSetTagDuplicateBelowCapIsIgnored(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	for i := 0; i < 3; i++ {
		if err := tx.SetTag("same"); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	if got := tx.Tags(); len(got) != 1 || got[0] != "same" {
		t.Fatalf("expected exactly one tag, got %v", got)
	}
}

// Length is validated before the count: an over-long tag pushed at a full set
// must report tag_too_long, not too_many_tags.
func TestSetTagLengthCheckedBeforeCount(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	for _, tag := range []string{"a", "b", "c", "d", "e"} {
		if err := tx.SetTag(tag); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	err := tx.SetTag(strings.Repeat("x", maxTransactionTagLength+1))
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := errCode(t, err); got != 2110 {
		t.Errorf("code = %d, want 2110 (length is checked first)", got)
	}
}

// AUTO_THROTTLE_TAG lands in both sets; plain TAG lands only in the
// transaction set (NativeAPI.actor.cpp:7115-7124).
func TestSetAutoThrottleTagPopulatesBothSets(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	if err := tx.SetTag("plain"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetAutoThrottleTag("auto"); err != nil {
		t.Fatal(err)
	}

	if got, want := strings.Join(tx.Tags(), ","), "plain,auto"; got != want {
		t.Errorf("Tags() = %q, want %q", got, want)
	}
	if got, want := strings.Join(tx.ReadTags(), ","), "auto"; got != want {
		t.Errorf("ReadTags() = %q, want %q — a plain TAG must not become a read tag", got, want)
	}
}

func TestSetAutoThrottleTagRespectsLimits(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	for _, tag := range []string{"a", "b", "c", "d", "e"} {
		if err := tx.SetTag(tag); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	err := tx.SetAutoThrottleTag("f")
	if err == nil {
		t.Fatal("auto-throttle tag past the cap must be rejected")
	}
	if got := errCode(t, err); got != 2109 {
		t.Errorf("code = %d, want 2109", got)
	}
	if len(tx.ReadTags()) != 0 {
		t.Errorf("rejected tag must not be added to the read set, got %v", tx.ReadTags())
	}
}

// TagSet's dynamic_size_traits: one length byte per tag, then its bytes, in
// insertion order, with no count prefix.
func TestEncodeTagSet(t *testing.T) {
	t.Parallel()
	if got := encodeTagSet(nil); got != nil {
		t.Errorf("empty set must encode to nil (absent optional), got %v", got)
	}
	got := string(encodeTagSet([]string{"tenant-a", "bulk"}))
	if want := "\x08tenant-a\x04bulk"; got != want {
		t.Errorf("encodeTagSet = %q, want %q", got, want)
	}
	// An empty tag is legal and encodes as a bare zero length byte.
	if got, want := string(encodeTagSet([]string{""})), "\x00"; got != want {
		t.Errorf("empty tag encoded as %q, want %q", got, want)
	}
}

// The GRV request carries per-tag OCCURRENCE COUNTS across the batch, not a
// tag set: C++ readVersionBatcher does ++tags[tag] per request
// (NativeAPI.actor.cpp:7347-7349).
func TestAggregateBatchTagsCountsOccurrences(t *testing.T) {
	t.Parallel()
	if got := aggregateBatchTags(nil); got != nil {
		t.Errorf("empty batch must aggregate to nil, got %v", got)
	}
	if got := aggregateBatchTags([]grvRequest{{}, {}}); got != nil {
		t.Errorf("untagged batch must aggregate to nil, got %v", got)
	}

	got := aggregateBatchTags([]grvRequest{
		{tags: []string{"alpha", "beta"}},
		{tags: []string{"alpha"}},
		{tags: nil},
		{tags: []string{"gamma", "alpha"}},
	})
	want := []types.TransactionTagCount{
		{Tag: []byte("alpha"), Count: 3},
		{Tag: []byte("beta"), Count: 1},
		{Tag: []byte("gamma"), Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if string(got[i].Tag) != string(want[i].Tag) || got[i].Count != want[i].Count {
			t.Errorf("entry %d = {%q,%d}, want {%q,%d}",
				i, got[i].Tag, got[i].Count, want[i].Tag, want[i].Count)
		}
	}
}

// A tagged transaction must put its tags on the COMMIT request bytes, as an
// Optional<TagSet> blob. Untagged commits must leave the optional absent rather
// than sending an empty one (NativeAPI.actor.cpp:6815-6816 assigns tagSet only
// when the set is non-empty).
func TestBuildCommitTransactionRequestCarriesTagSet(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	if err := tx.SetTag("tenant-a"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetTag("bulk"); err != nil {
		t.Fatal(err)
	}
	tx.mutations = []Mutation{{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")}}

	body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
	defer marshalBufPool.Put(poolBuf)

	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("unmarshal commit request: %v", err)
	}
	if !req.HasTagSet {
		t.Fatal("tagged commit must set the tagSet optional")
	}
	if got, want := string(req.TagSet), "\x08tenant-a\x04bulk"; got != want {
		t.Errorf("commit tagSet = %q, want %q", got, want)
	}

	// Untagged commit: the optional must stay absent.
	plain := &Transaction{}
	plain.mutations = []Mutation{{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")}}
	body2, poolBuf2 := buildCommitTransactionRequest(plain, transport.UID{}, plain.mutations, plain.writeConflicts)
	defer marshalBufPool.Put(poolBuf2)

	var req2 types.CommitTransactionRequest
	if err := req2.UnmarshalFDB(body2); err != nil {
		t.Fatalf("unmarshal untagged commit request: %v", err)
	}
	if req2.HasTagSet {
		t.Errorf("untagged commit must not set tagSet, got %q", req2.TagSet)
	}
}

// A tagged transaction must put its tags on the GRV request bytes. Marshalling
// and re-parsing is what proves they left the struct, since an unpopulated
// field and an empty one are indistinguishable in the struct itself.
func TestBuildGetReadVersionRequestCarriesTags(t *testing.T) {
	t.Parallel()
	tags := []types.TransactionTagCount{{Tag: []byte("tenant-a"), Count: 2}}
	body := buildGetReadVersionRequest(transport.UID{First: 0xCAFE, Second: 0xBABE}, 0, 1, types.SpanContext{}, tags)

	var req types.GetReadVersionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("unmarshal GRV request: %v", err)
	}
	if len(req.Tags) != 1 {
		t.Fatalf("GRV request carried %d tags, want 1", len(req.Tags))
	}
	if string(req.Tags[0].Tag) != "tenant-a" || req.Tags[0].Count != 2 {
		t.Errorf("GRV tag = {%q,%d}, want {tenant-a,2}", req.Tags[0].Tag, req.Tags[0].Count)
	}

	// The untagged case must leave the vector empty, not emit a phantom entry.
	untagged := buildGetReadVersionRequest(transport.UID{First: 0xCAFE, Second: 0xBABE}, 0, 1, types.SpanContext{}, nil)
	var req2 types.GetReadVersionRequest
	if err := req2.UnmarshalFDB(untagged); err != nil {
		t.Fatalf("unmarshal untagged GRV request: %v", err)
	}
	if len(req2.Tags) != 0 {
		t.Errorf("untagged GRV request carried %d tags, want 0", len(req2.Tags))
	}
}
