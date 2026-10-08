package recordlayer

import (
	"regexp"
	"strconv"
	"strings"

	"fdb.dev/pkg/internal/fdbclient"
)

// FDBClientKnob is Java's FDBClientKnob (#4488): a FoundationDB client knob
// the factory names and types. Its knob name is the lower-cased constant name.
type FDBClientKnob string

// The typed client knobs, FDBClientKnob.java:88-211.
const (
	KnobPacketLimit                              FDBClientKnob = "packet_limit"
	KnobPacketWarning                            FDBClientKnob = "packet_warning"
	KnobTLSClientHandshakeThreads                FDBClientKnob = "tls_client_handshake_threads"
	KnobDisableMainthreadTLSHandshake            FDBClientKnob = "disable_mainthread_tls_handshake"
	KnobTLSClientConnectionThrottleAttempts      FDBClientKnob = "tls_client_connection_throttle_attempts"
	KnobTLSClientConnectionThrottleTimeout       FDBClientKnob = "tls_client_connection_throttle_timeout"
	KnobMaxCommitProxyConnections                FDBClientKnob = "max_commit_proxy_connections"
	KnobMaxGRVProxyConnections                   FDBClientKnob = "max_grv_proxy_connections"
	KnobLocationCacheFailedEndpointRetryInterval FDBClientKnob = "location_cache_failed_endpoint_retry_interval"
	KnobLogConnectionAttemptsEnabled             FDBClientKnob = "log_connection_attempts_enabled"
	KnobConnectionLogDirectory                   FDBClientKnob = "connection_log_directory"
	KnobMaxReconnectionTime                      FDBClientKnob = "max_reconnection_time"
	KnobReconnectionTimeGrowthRate               FDBClientKnob = "reconnection_time_growth_rate"
	KnobReconnectionResetTime                    FDBClientKnob = "reconnection_reset_time"
	KnobLocationCachePeerEvictorEnabled          FDBClientKnob = "location_cache_peer_evictor_enabled"
	KnobLocationCachePeerEvictorDelay            FDBClientKnob = "location_cache_peer_evictor_delay"
	KnobLocationCachePeerEvictorFailedThreshold  FDBClientKnob = "location_cache_peer_evictor_failed_threshold"
	KnobLocationCachePeerEvictorScanChunk        FDBClientKnob = "location_cache_peer_evictor_scan_chunk"
	KnobShrinkProxyListClearCacheBelowThreshold  FDBClientKnob = "shrink_proxy_list_clear_cache_below_threshold"
)

// KnobValueType is Java's FDBClientKnob.KnobValueType.
type KnobValueType string

const (
	KnobValueInt     KnobValueType = "INT"
	KnobValueLong    KnobValueType = "LONG"
	KnobValueDouble  KnobValueType = "DOUBLE"
	KnobValueBoolean KnobValueType = "BOOLEAN"
	KnobValueString  KnobValueType = "STRING"
)

var clientKnobTypes = map[FDBClientKnob]KnobValueType{
	KnobPacketLimit:                              KnobValueLong,
	KnobPacketWarning:                            KnobValueLong,
	KnobTLSClientHandshakeThreads:                KnobValueInt,
	KnobDisableMainthreadTLSHandshake:            KnobValueBoolean,
	KnobTLSClientConnectionThrottleAttempts:      KnobValueInt,
	KnobTLSClientConnectionThrottleTimeout:       KnobValueDouble,
	KnobMaxCommitProxyConnections:                KnobValueInt,
	KnobMaxGRVProxyConnections:                   KnobValueInt,
	KnobLocationCacheFailedEndpointRetryInterval: KnobValueDouble,
	KnobLogConnectionAttemptsEnabled:             KnobValueBoolean,
	KnobConnectionLogDirectory:                   KnobValueString,
	KnobMaxReconnectionTime:                      KnobValueDouble,
	KnobReconnectionTimeGrowthRate:               KnobValueDouble,
	KnobReconnectionResetTime:                    KnobValueDouble,
	KnobLocationCachePeerEvictorEnabled:          KnobValueBoolean,
	KnobLocationCachePeerEvictorDelay:            KnobValueDouble,
	KnobLocationCachePeerEvictorFailedThreshold:  KnobValueInt,
	KnobLocationCachePeerEvictorScanChunk:        KnobValueInt,
	KnobShrinkProxyListClearCacheBelowThreshold:  KnobValueBoolean,
}

// KnobName is the knob's name as the client knows it.
func (k FDBClientKnob) KnobName() string { return string(k) }

// ValueType is the type the knob's value must parse as.
func (k FDBClientKnob) ValueType() KnobValueType { return clientKnobTypes[k] }

// FDBClientKnobFromName is Java's fromKnobName: the typed knob of that name,
// or false.
func FDBClientKnobFromName(name string) (FDBClientKnob, bool) {
	k := FDBClientKnob(name)
	_, ok := clientKnobTypes[k]
	return k, ok
}

// SetKnob is Java's setKnob(FDBClientKnob, String): it checks value against
// the knob's type, records it, and sets it on the client at once when the
// client has started. A recorded knob is set before the client opens its
// first database. The pure-Go client honors no knob, so there a recorded knob
// fails the open (fdbclient.UnsupportedKnobError) rather than being ignored.
func (f *FDBDatabaseFactory) SetKnob(knob FDBClientKnob, value string) error {
	if err := validateKnobValue(knob, value); err != nil {
		return err
	}
	return f.setKnob(knob.KnobName(), value)
}

// SetKnobByName is Java's setKnob(String, String): a typed knob's name is
// checked as SetKnob does; any other name only has to be nonblank and free of
// '=', and is passed to the client as given.
func (f *FDBDatabaseFactory) SetKnobByName(name, value string) error {
	if knob, ok := FDBClientKnobFromName(name); ok {
		return f.SetKnob(knob, value)
	}
	if strings.TrimSpace(name) == "" || strings.Contains(name, "=") {
		return &RecordCoreArgumentError{Message: "invalid client knob name", ClientKnobName: name}
	}
	return f.setKnob(name, value)
}

func (f *FDBDatabaseFactory) setKnob(name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.knobs == nil {
		f.knobs = make(map[string]string)
	}
	if _, ok := f.knobs[name]; !ok {
		f.knobOrder = append(f.knobOrder, name)
	}
	f.knobs[name] = value
	if f.inited {
		return fdbclient.SetKnob(name + "=" + value)
	}
	return nil
}

// Knobs is Java's getKnobs: the knobs set so far, by name.
func (f *FDBDatabaseFactory) Knobs() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.knobs))
	for k, v := range f.knobs {
		out[k] = v
	}
	return out
}

// ClearKnobs is Java's clearKnobs: refused once the client has started.
func (f *FDBDatabaseFactory) ClearKnobs() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inited {
		return &RecordCoreError{Message: "client knobs cannot be cleared as the client has already started"}
	}
	f.knobs, f.knobOrder = nil, nil
	return nil
}

// startClientLocked sets the recorded knobs, in the order first set, before
// the factory's first open (Java's initFDB). f.mu is held.
func (f *FDBDatabaseFactory) startClientLocked() error {
	if f.inited {
		return nil
	}
	for _, name := range f.knobOrder {
		if err := fdbclient.SetKnob(name + "=" + f.knobs[name]); err != nil {
			return err
		}
	}
	f.inited = true
	return nil
}

// validateKnobValue is Java's validateKnobValue: a sanity check that value
// parses as the knob's type the way the native client parses it.
func validateKnobValue(knob FDBClientKnob, value string) error {
	var valid bool
	switch knob.ValueType() {
	case KnobValueInt:
		valid = isValidKnobInteger(value, 32)
	case KnobValueLong:
		valid = isValidKnobInteger(value, 64)
	case KnobValueDouble:
		valid = javaDoubleSyntax.MatchString(value)
	case KnobValueBoolean:
		valid = strings.EqualFold(value, "true") || strings.EqualFold(value, "false") || isValidKnobInteger(value, 32)
	case KnobValueString:
		valid = true
	}
	if !valid {
		return &RecordCoreArgumentError{
			Message:        "client knob value does not match the knob's expected type",
			ClientKnobName: knob.KnobName(), ClientKnobValue: value, Expected: string(knob.ValueType()),
		}
	}
	return nil
}

// isValidKnobInteger is Java's isValidInteger: Integer.decode / Long.decode
// (an optional sign, then 0x/0X hex, a leading-0 octal, or decimal) within the
// type's range, without decode's '#' hex form, which the native client does
// not read.
func isValidKnobInteger(value string, bits int) bool {
	if strings.Contains(value, "#") || value == "" {
		return false
	}
	sign, rest := "", value
	if rest[0] == '-' || rest[0] == '+' {
		sign, rest = rest[:1], rest[1:]
	}
	base := 10
	switch {
	case strings.HasPrefix(rest, "0x"), strings.HasPrefix(rest, "0X"):
		base, rest = 16, rest[2:]
	case len(rest) > 1 && rest[0] == '0':
		base, rest = 8, rest[1:]
	}
	if rest == "" || rest[0] == '-' || rest[0] == '+' {
		return false
	}
	if sign == "+" {
		sign = ""
	}
	_, err := strconv.ParseInt(sign+rest, base, bits)
	return err == nil
}

// javaDoubleSyntax is the grammar Java's Double.parseDouble accepts
// (Double.valueOf's documented regular expression): surrounding ASCII control
// and space characters, an optional sign, then NaN, Infinity, a decimal with
// an optional exponent, or a hexadecimal with a binary exponent, each finite
// form with an optional f/F/d/D suffix.
var javaDoubleSyntax = regexp.MustCompile(`^[\x00-\x20]*[+-]?(NaN|Infinity|` +
	`((([0-9]+\.?[0-9]*)|(\.[0-9]+))([eE][+-]?[0-9]+)?[fFdD]?)|` +
	`(0[xX](([0-9a-fA-F]+\.?)|([0-9a-fA-F]*\.[0-9a-fA-F]+))[pP][+-]?[0-9]+[fFdD]?))[\x00-\x20]*$`)
