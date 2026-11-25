//go:build !libfdbc

package fdbclient

import (
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
)

// Backend names the FDB client compiled into this binary.
const Backend = "pure-go"

// apiVersion is the FDB API version fdbclient selects when the app has not already
// selected one. It matches the 7.3.77 server (and the libfdb_c binding's 730).
const apiVersion = 730

// Open opens clusterFile on the from-scratch pure-Go FDB client (the default).
// Build with -tags libfdbc to open on Apple's libfdb_c instead, with no change
// to this call.
//
// Open selects the FDB API version if the app has not already (standard FDB is
// select-then-open). An app that selected its own version keeps it — we never
// override it; we only fill in the default so fdbclient.Open is a single call. The
// libfdb_c variant does the equivalent inside libfdbc.Open (it pins 730, the only
// version its binding speaks).
func Open(clusterFile string) (fdb.BackendDatabase, error) {
	if _, err := fdb.GetAPIVersion(); err != nil {
		if err := fdb.APIVersion(apiVersion); err != nil {
			return nil, err
		}
	}
	return fdb.OpenDatabase(clusterFile)
}

// SetKnob refuses every client knob: the pure-Go client has no knob table, and
// claiming to honor a native knob it ignores would hide the configuration.
func SetKnob(knob string) error {
	name, _, _ := strings.Cut(knob, "=")
	return &UnsupportedKnobError{Knob: name}
}

// UnsupportedKnobError is a client knob the compiled-in client cannot honor.
type UnsupportedKnobError struct{ Knob string }

func (e *UnsupportedKnobError) Error() string {
	return "client knob " + e.Knob + " is not supported by the " + Backend +
		" client (build with -tags libfdbc to set libfdb_c knobs)"
}

// FDBCode is invalid_option_value (2006), libfdb_c's answer to a knob it
// cannot set.
func (e *UnsupportedKnobError) FDBCode() int { return 2006 }
