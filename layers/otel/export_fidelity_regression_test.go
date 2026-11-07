package otel

import (
	"strings"
	"testing"
)

func TestMessageLayoutValidationNamesVersionedIdentity(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Run, cfg.Backend = "layout_validation", "fdb"
	cfg.Messages = "fulltext"
	err := cfg.Validate()
	requireOtelKind(t, err, "config")
	if !strings.Contains(err.Error(), messagesFullText) {
		t.Fatalf("validation advertises the retired layout: %v", err)
	}
	cfg.Messages = messagesFullText
	if err := cfg.Validate(); err != nil {
		t.Fatalf("versioned layout refused: %v", err)
	}
}
