package recordlayer

import (
	"errors"
	"testing"

	"fdb.dev/pkg/internal/fdbclient"
)

// TestClientKnobValidation pins Java's FDBDatabaseFactoryImpl.validateKnobValue
// (FDBDatabaseFactoryTest's knob cases): integers as Integer/Long.decode reads
// them without '#', doubles as Double.parseDouble, booleans as true/false or an
// int, strings as anything.
func TestClientKnobValidation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		knob  FDBClientKnob
		value string
		ok    bool
	}{
		{KnobTLSClientHandshakeThreads, "4", true},
		{KnobTLSClientHandshakeThreads, "+4", true},
		{KnobTLSClientHandshakeThreads, "-0x80000000", true},
		{KnobTLSClientHandshakeThreads, "0X1f", true},
		{KnobTLSClientHandshakeThreads, "017", true},
		{KnobTLSClientHandshakeThreads, "0", true},
		{KnobTLSClientHandshakeThreads, "#1f", false},
		{KnobTLSClientHandshakeThreads, "08", false},
		{KnobTLSClientHandshakeThreads, "0x", false},
		{KnobTLSClientHandshakeThreads, "0x-5", false},
		{KnobTLSClientHandshakeThreads, "2147483648", false},
		{KnobTLSClientHandshakeThreads, "1.5", false},
		{KnobTLSClientHandshakeThreads, "", false},
		{KnobTLSClientHandshakeThreads, " 4", false},
		{KnobPacketLimit, "2147483648", true},
		{KnobPacketLimit, "9223372036854775808", false},
		{KnobMaxReconnectionTime, "0.5", true},
		{KnobMaxReconnectionTime, " 1e3 ", true},
		{KnobMaxReconnectionTime, ".5d", true},
		{KnobMaxReconnectionTime, "NaN", true},
		{KnobMaxReconnectionTime, "-Infinity", true},
		{KnobMaxReconnectionTime, "0x1.8p1", true},
		{KnobMaxReconnectionTime, "inf", false},
		{KnobMaxReconnectionTime, "1_0", false},
		{KnobMaxReconnectionTime, "abc", false},
		{KnobLogConnectionAttemptsEnabled, "TRUE", true},
		{KnobLogConnectionAttemptsEnabled, "false", true},
		{KnobLogConnectionAttemptsEnabled, "1", true},
		{KnobLogConnectionAttemptsEnabled, "yes", false},
		{KnobConnectionLogDirectory, "", true},
	} {
		err := validateKnobValue(c.knob, c.value)
		if c.ok != (err == nil) {
			t.Errorf("%s = %q: %v, want valid %v", c.knob, c.value, err, c.ok)
		}
		var argErr *RecordCoreArgumentError
		if err != nil && (!errors.As(err, &argErr) || argErr.ClientKnobName != c.knob.KnobName() || argErr.Expected != string(c.knob.ValueType())) {
			t.Errorf("%s = %q: %#v, want the knob's name and expected type", c.knob, c.value, err)
		}
	}
	if len(clientKnobTypes) != 19 {
		t.Errorf("%d typed knobs, want Java's 19", len(clientKnobTypes))
	}
}

// TestClientKnobFactory pins the factory's knob API: a typed name goes through
// its type check even by name, another name must be nonblank and free of '=',
// values replace in place, ClearKnobs works until the client starts, and the
// pure-Go client refuses a recorded knob at the first open instead of
// ignoring it.
func TestClientKnobFactory(t *testing.T) {
	t.Parallel()
	f := NewFDBDatabaseFactory()
	if err := f.SetKnobByName("max_reconnection_time", "soon"); err == nil {
		t.Error("a typed knob set by name skipped its type check")
	}
	for _, bad := range []string{"", "  ", "a=b"} {
		var argErr *RecordCoreArgumentError
		if err := f.SetKnobByName(bad, "1"); !errors.As(err, &argErr) || argErr.Message != "invalid client knob name" {
			t.Errorf("knob name %q: %v, want invalid client knob name", bad, err)
		}
	}
	if err := f.SetKnobByName("some_future_knob", "x"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetKnob(KnobMaxReconnectionTime, "0.5"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetKnob(KnobMaxReconnectionTime, "1.0"); err != nil {
		t.Fatal(err)
	}
	if got := f.Knobs(); len(got) != 2 || got["max_reconnection_time"] != "1.0" || got["some_future_knob"] != "x" {
		t.Fatalf("Knobs() = %v", got)
	}
	if f.knobOrder[0] != "some_future_knob" || f.knobOrder[1] != "max_reconnection_time" {
		t.Errorf("knob order %v, want first-set order", f.knobOrder)
	}
	if err := f.ClearKnobs(); err != nil || len(f.Knobs()) != 0 {
		t.Fatalf("ClearKnobs before start: %v, %v", err, f.Knobs())
	}

	if fdbclient.Backend != "pure-go" {
		t.Skip("the open refusal is the pure-Go client's")
	}
	if err := f.SetKnob(KnobPacketLimit, "1000"); err != nil {
		t.Fatal(err)
	}
	_, err := f.GetDatabase("/nonexistent/fdb.cluster")
	var unsupported *fdbclient.UnsupportedKnobError
	if !errors.As(err, &unsupported) || unsupported.Knob != "packet_limit" {
		t.Fatalf("open with a knob on the pure-Go client: %v, want UnsupportedKnobError(packet_limit)", err)
	}
	if f.inited {
		t.Error("a refused start marked the client started")
	}
}
