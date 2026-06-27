package client

import (
	"testing"

	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestParseCoordinatorResponse_Forward proves the coordinator-reply parser reads
// the ClientDBInfo.forward field (slot-3 presence tag + slot-4 value) — the
// connection string the coordinators hand back during a `coordinators auto`/`change`
// rotation (RFC-111 §3, test 1). Revert-proof: drop the forward-read in
// parseClientDBInfoFromReader and this goes red (Forward stays empty).
func TestParseCoordinatorResponse_Forward(t *testing.T) {
	t.Parallel()
	const fwd = "fwd:newid@2.2.2.2:4500,3.3.3.3:4500"
	body := (&types.ClientDBInfo{HasForward: true, Forward: []byte(fwd)}).MarshalFDB()

	info, err := parseCoordinatorResponse(body)
	if err != nil {
		t.Fatalf("parseCoordinatorResponse: %v", err)
	}
	if info.Forward != fwd {
		t.Fatalf("Forward = %q, want %q", info.Forward, fwd)
	}
}

// TestParseCoordinatorResponse_NoForward proves a normal reply (proxies, no
// forward) leaves Forward empty so the topology path applies proxies rather than
// chasing a forward.
func TestParseCoordinatorResponse_NoForward(t *testing.T) {
	t.Parallel()
	body := (&types.ClientDBInfo{}).MarshalFDB()
	info, err := parseCoordinatorResponse(body)
	if err != nil {
		t.Fatalf("parseCoordinatorResponse: %v", err)
	}
	if info.Forward != "" {
		t.Fatalf("Forward = %q, want empty", info.Forward)
	}
}
