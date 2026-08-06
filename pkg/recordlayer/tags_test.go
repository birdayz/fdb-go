package recordlayer

// Record-layer tag limits. These are STRICTER than the FDB client's own
// 5 tags / 255 bytes: Java caps a tag at 16 characters
// (FDBRecordContextConfig.java:661-672), so a tag the client would happily
// accept is still rejected here.

import (
	"errors"
	"strings"
	"testing"
)

func tagErr(t *testing.T, err error) *TagValidationError {
	t.Helper()
	var e *TagValidationError
	if !errors.As(err, &e) {
		t.Fatalf("expected *TagValidationError, got %T (%v)", err, err)
	}
	return e
}

func TestValidateTagsAcceptsAtTheLimits(t *testing.T) {
	t.Parallel()
	// Exactly 5 tags, one of them exactly 16 characters: both bounds are
	// inclusive in Java (`> 5`, `> 16`).
	in := []string{strings.Repeat("a", MaxRecordContextTagLen), "b", "c", "d", "e"}
	got, err := ValidateTags(in)
	if err != nil {
		t.Fatalf("5 tags with a 16-char tag must be accepted, got %v", err)
	}
	if len(got) != 5 {
		t.Errorf("got %d tags, want 5", len(got))
	}
}

func TestValidateTagsRejectsSixth(t *testing.T) {
	t.Parallel()
	_, err := ValidateTags([]string{"a", "b", "c", "d", "e", "f"})
	if err == nil {
		t.Fatal("6 tags must be rejected")
	}
	if got, want := tagErr(t, err).Message, "At most 5 tags allowed"; got != want {
		t.Errorf("message = %q, want %q (Java's exact wording)", got, want)
	}
}

func TestValidateTagsRejectsSeventeenChars(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", MaxRecordContextTagLen+1)
	_, err := ValidateTags([]string{"ok", long})
	if err == nil {
		t.Fatal("17-character tag must be rejected")
	}
	e := tagErr(t, err)
	if got, want := e.Message, "Tag must be 16 characters or shorter"; got != want {
		t.Errorf("message = %q, want %q (Java's exact wording)", got, want)
	}
	if e.Tag != long {
		t.Errorf("offending tag = %q, want %q", e.Tag, long)
	}
}

// Java checks the SET SIZE before it checks any tag's length
// (FDBRecordContextConfig.java:662-669). The order is observable: a set that
// violates both must report the count error. This is the OPPOSITE of the FDB
// client's TagSet::addTag, which checks length first — so a single shared
// ordering cannot satisfy both layers, and getting it backwards here would
// silently change which message a caller sees.
func TestValidateTagsCountCheckedBeforeLength(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", MaxRecordContextTagLen+1)
	_, err := ValidateTags([]string{"a", "b", "c", "d", "e", long})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got, want := tagErr(t, err).Message, "At most 5 tags allowed"; got != want {
		t.Errorf("message = %q, want %q — the count is checked first", got, want)
	}
}

// Java stores tags in a Set<String>, so duplicates collapse BEFORE the size
// check. Seven entries that are five distinct tags must be accepted.
func TestValidateTagsDedupsBeforeCounting(t *testing.T) {
	t.Parallel()
	got, err := ValidateTags([]string{"a", "b", "a", "c", "d", "e", "b"})
	if err != nil {
		t.Fatalf("7 entries collapsing to 5 distinct tags must be accepted, got %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d tags, want 5 (%v)", len(got), got)
	}
}

// String.length() counts UTF-16 code units, not bytes. A 16-character
// multi-byte tag is 48 bytes but must still pass, so a byte-length check here
// would wrongly reject it.
func TestValidateTagsCountsCharactersNotBytes(t *testing.T) {
	t.Parallel()
	tag := strings.Repeat("日", MaxRecordContextTagLen) // 16 chars, 48 bytes
	if len(tag) <= MaxRecordContextTagLen {
		t.Fatalf("test is not exercising the byte/char difference: %d bytes", len(tag))
	}
	if _, err := ValidateTags([]string{tag}); err != nil {
		t.Errorf("16-character multi-byte tag must be accepted, got %v", err)
	}
}

func TestSetTagsSortsForDeterministicCommitBytes(t *testing.T) {
	t.Parallel()
	var cfg RecordContextConfig
	if err := cfg.SetTags([]string{"gamma", "alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(cfg.Tags, ","), "alpha,beta,gamma"; got != want {
		t.Errorf("Tags = %q, want %q", got, want)
	}
}

func TestSetTagsRejectsAndLeavesConfigUntouched(t *testing.T) {
	t.Parallel()
	cfg := RecordContextConfig{Tags: []string{"keep"}}
	if err := cfg.SetTags([]string{"a", "b", "c", "d", "e", "f"}); err == nil {
		t.Fatal("expected rejection")
	}
	if got, want := strings.Join(cfg.Tags, ","), "keep"; got != want {
		t.Errorf("a rejected SetTags must not modify the config: got %q, want %q", got, want)
	}
}

// applyTags must issue one SetTag per tag, in order — the record layer's
// contract with the client layer (FDBRecordContext.java:205-207).
type tagRecorderOptions struct {
	tags []string
	err  error
}

func (r *tagRecorderOptions) SetTag(tag string) error {
	if r.err != nil {
		return r.err
	}
	r.tags = append(r.tags, tag)
	return nil
}

func TestApplyTagsIssuesOneCallPerTag(t *testing.T) {
	t.Parallel()
	rec := &tagRecorderOptions{}
	if err := applyTagsTo(rec, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(rec.tags, ","), "alpha,beta"; got != want {
		t.Errorf("SetTag calls = %q, want %q", got, want)
	}
}

func TestApplyTagsEmptyIssuesNoCalls(t *testing.T) {
	t.Parallel()
	rec := &tagRecorderOptions{}
	if err := applyTagsTo(rec, nil); err != nil {
		t.Fatal(err)
	}
	if len(rec.tags) != 0 {
		t.Errorf("no tags must mean no SetTag calls, got %v", rec.tags)
	}
}
