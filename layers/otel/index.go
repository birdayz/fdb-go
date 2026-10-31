package otel

import (
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

const indexType = "otel_typed_scoped_eq_v1"

func init() { recordlayer.RegisterIndexMaintainerFactory(postingFactory{}) }

type postingFactory struct{}

func (postingFactory) IndexTypes() []string                 { return []string{indexType} }
func (postingFactory) IsIdempotent(*recordlayer.Index) bool { return true }
func (postingFactory) ValidateIndexOptions(i *recordlayer.Index) error {
	_, err := hashWidth(i)
	if err != nil {
		return err
	}
	if i.Options["order"] != "trace" && i.Options["order"] != "time" {
		return fail("config", "index options", "order must be trace or time")
	}
	_, err = normalizePostingValueFormat(i.Options["value_format"])
	return err
}

func (postingFactory) ValidateChangedOptions(_, _ *recordlayer.Index, changed map[string]bool) error {
	if len(changed) > 0 {
		return fail("config", "index evolution", "prototype layout options are immutable; use a fresh run")
	}
	return nil
}

func (f postingFactory) NewIndexMaintainer(s recordlayer.IndexMaintainerState) (recordlayer.IndexMaintainer, error) {
	if err := f.ValidateIndexOptions(s.Index); err != nil {
		return nil, err
	}
	width, err := hashWidth(s.Index)
	if err != nil {
		return nil, err
	}
	format, err := normalizePostingValueFormat(s.Index.Options["value_format"])
	if err != nil {
		return nil, err
	}
	return &postingMaintainer{StandardIndexMaintainer: *recordlayer.NewStandardIndexMaintainer(s), secondary: s.Store.IndexSecondarySubspace(s.Index), width: width, order: s.Index.Options["order"], valueFormat: format}, nil
}

func hashWidth(i *recordlayer.Index) (int, error) {
	w, err := strconv.Atoi(i.Options["hash_bytes"])
	if err != nil || w < 1 || w > 32 {
		return 0, fail("config", "index options", "hash_bytes must be 1..32")
	}
	return w, nil
}

type postingMaintainer struct {
	recordlayer.StandardIndexMaintainer
	secondary   subspace.Subspace
	width       int
	order       string
	valueFormat string
}
type coveringValue struct {
	Attribute Attribute `json:"a"`
	Start     int64     `json:"t"`
}
type posting struct {
	Key   fdb.Key
	Value []byte
}

func digest(s string, width int) []byte { h := sha256.Sum256([]byte(s)); return h[:width] }
func termPrefix(ss subspace.Subspace, tenant int, a Attribute, width int) subspace.Subspace {
	return ss.Sub("eq_v1", int64(tenant), a.Scope, digest(a.Key, width), a.Type, digest(a.Value, width))
}

func reverseTrace(s string) []byte {
	out := []byte(s)
	for i := range out {
		out[i] = ^out[i]
	}
	return out
}

func restoreTrace(b []byte) string {
	out := append([]byte(nil), b...)
	for i := range out {
		out[i] = ^out[i]
	}
	return string(out)
}

func postingSuffix(s Span, order string) tuple.Tuple {
	if order == "time" {
		return tuple.Tuple{-s.Start, reverseTrace(s.Trace), s.ID}
	}
	return tuple.Tuple{s.Trace, s.ID}
}

func normalizePostingValueFormat(format string) (string, error) {
	switch format {
	case "", "json":
		return "json", nil
	case "tuple":
		return format, nil
	default:
		return "", fail("config", "index options", "value_format must be json or tuple")
	}
}

func encodeCoveringValue(value coveringValue, format string) ([]byte, error) {
	switch format {
	case "", "json":
		return json.Marshal(value)
	case "tuple":
		a := value.Attribute
		return tuple.Tuple{a.Scope, a.Key, a.Type, a.Value, value.Start}.Pack(), nil
	default:
		return nil, fail("config", "covering posting", "value_format must be json or tuple")
	}
}

func decodeCoveringValue(value []byte, format string) (coveringValue, error) {
	var out coveringValue
	switch format {
	case "", "json":
		if err := json.Unmarshal(value, &out); err != nil {
			return out, wrap("corruption", "covering posting", err)
		}
		return out, nil
	case "tuple":
		fields, err := tuple.Unpack(value)
		if err != nil {
			return out, wrap("corruption", "covering posting", err)
		}
		if len(fields) != 5 {
			return out, fail("corruption", "covering posting", "expected five tuple fields")
		}
		strings := [4]*string{&out.Attribute.Scope, &out.Attribute.Key, &out.Attribute.Type, &out.Attribute.Value}
		for i, field := range strings {
			text, ok := fields[i].(string)
			if !ok || !utf8.ValidString(text) {
				return coveringValue{}, fail("corruption", "covering posting", "expected UTF-8 string tuple field")
			}
			*field = text
		}
		start, ok := fields[4].(int64)
		if !ok {
			return coveringValue{}, fail("corruption", "covering posting", "expected int64 tuple timestamp")
		}
		if len(out.Attribute.Key) > 1024 || len(out.Attribute.Value) > 64000 {
			return coveringValue{}, fail("corruption", "covering posting", "projection exceeds prototype coverage cap")
		}
		out.Start = start
		return out, nil
	default:
		return out, fail("config", "covering posting", "value_format must be json or tuple")
	}
}

func postings(ss subspace.Subspace, width int, order string, record *recordlayer.FDBStoredRecord[proto.Message]) ([]posting, error) {
	return postingsWithFormat(ss, width, order, "json", record)
}

func postingsWithFormat(ss subspace.Subspace, width int, order, format string, record *recordlayer.FDBStoredRecord[proto.Message]) ([]posting, error) {
	if _, err := normalizePostingValueFormat(format); err != nil {
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	s, err := decodeRecord(record.Record)
	if err != nil {
		return nil, err
	}
	out := make([]posting, 0, len(s.Attributes))
	for ordinal, a := range s.Attributes {
		if len(a.Key) > 1024 || len(a.Value) > 64000 {
			return nil, fail("admission", "covering projection", "projection exceeds prototype coverage cap")
		}
		v, err := encodeCoveringValue(coveringValue{a, s.Start}, format)
		if err != nil {
			return nil, err
		}
		out = append(out, posting{fdb.Key(termPrefix(ss, s.Tenant, a, width).Pack(append(postingSuffix(s, order), int64(ordinal)))), v})
	}
	return out, nil
}

// diffPostings retains only changed key/value pairs, including multiplicity.
// Matching keys alone would lose covering-value changes and digest collisions.
func diffPostings(before, after []posting) (removed, added []posting) {
	if len(before) == 0 {
		return nil, after
	}
	if len(after) == 0 {
		return before, nil
	}
	type identity struct{ key, value string }
	available := make(map[identity]int, len(before))
	matched := make(map[identity]int, len(before))
	for _, p := range before {
		available[identity{string(p.Key), string(p.Value)}]++
	}
	for _, p := range after {
		id := identity{string(p.Key), string(p.Value)}
		if matched[id] < available[id] {
			matched[id]++
		} else {
			added = append(added, p)
		}
	}
	for _, p := range before {
		id := identity{string(p.Key), string(p.Value)}
		if matched[id] > 0 {
			matched[id]--
		} else {
			removed = append(removed, p)
		}
	}
	return removed, added
}

func (m *postingMaintainer) Update(old, new *recordlayer.FDBStoredRecord[proto.Message]) error {
	return m.update(old, new, true)
}

func (m *postingMaintainer) update(old, new *recordlayer.FDBStoredRecord[proto.Message], skipUnchanged bool) error {
	before, err := postingsWithFormat(m.secondary, m.width, m.order, m.valueFormat, old)
	if err != nil {
		return err
	}
	after, err := postingsWithFormat(m.secondary, m.width, m.order, m.valueFormat, new)
	if err != nil {
		return err
	}
	removed, added := before, after
	if skipUnchanged {
		removed, added = diffPostings(before, after)
	}
	for _, p := range removed {
		m.Transaction().Clear(p.Key)
	}
	for _, p := range added {
		m.Transaction().Set(p.Key, p.Value)
	}
	return nil
}

func (m *postingMaintainer) UpdateWhileWriteOnly(old, new *recordlayer.FDBStoredRecord[proto.Message]) error {
	// The build may not have materialized the old record's entries yet.
	// Preserve full maintenance rather than assuming unchanged entries exist.
	return m.update(old, new, false)
}

func (m *postingMaintainer) Scan(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) recordlayer.RecordCursor[*recordlayer.IndexEntry] {
	return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](fail("config", "scan", "use the prototype typed equality scanner; not a VALUE index"))
}

func (m *postingMaintainer) CanDeleteWhere(prefix tuple.Tuple) error {
	if len(prefix) != 0 {
		return fail("config", "delete where", "prototype postings cannot range-delete a record prefix")
	}
	return nil
}

func (m *postingMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	if err := m.CanDeleteWhere(prefix); err != nil {
		return err
	}
	for _, ss := range []subspace.Subspace{m.IndexSubspace(), m.secondary} {
		r, err := fdb.PrefixRange(ss.Bytes())
		if err != nil {
			return err
		}
		m.Transaction().ClearRange(r)
	}
	return nil
}

// Query verifies covering values; time order stops at K verified distinct traces.
// Trace order must exhaust candidates to establish the same exact top-K.
func (m *postingMaintainer) Query(c Config, a Attribute) (QueryResult, error) {
	out := QueryResult{Hits: []Hit{}}
	latest := map[string]int64{}
	ss := termPrefix(m.secondary, c.Tenant, a, m.width)
	it := m.ReadTransaction(recordlayer.ForwardScan()).GetRange(ss, fdb.RangeOptions{Limit: c.MaxCandidates + 1, Mode: fdb.StreamingModeIterator}).Iterator()
	stoppedAtFrontier := false
	for it.Advance() {
		kv, err := it.Get()
		if err != nil {
			return out, err
		}
		out.Candidates++
		out.Bytes += int64(len(kv.Key) + len(kv.Value))
		if out.Candidates > c.MaxCandidates || out.Bytes > c.MaxBytes {
			return out, failf("budget", "equality", "candidates=%d logical_bytes=%d", out.Candidates, out.Bytes)
		}
		v, err := decodeCoveringValue(kv.Value, m.valueFormat)
		if err != nil {
			return out, err
		}
		if v.Attribute != a {
			continue
		}
		pk, err := ss.Unpack(kv.Key)
		if err != nil {
			return out, wrap("corruption", "posting key", err)
		}
		tr := ""
		if m.order == "time" {
			if len(pk) != 4 {
				return out, fail("corruption", "posting", "malformed time posting")
			}
			timestamp, ok := pk[0].(int64)
			if !ok || timestamp != -v.Start {
				return out, fail("corruption", "posting", "invalid covering time")
			}
			encoded, ok := pk[1].([]byte)
			if !ok {
				return out, fail("corruption", "posting", "invalid trace key")
			}
			tr = restoreTrace(encoded)
		} else {
			if len(pk) != 3 {
				return out, fail("corruption", "posting", "malformed trace posting")
			}
			var ok bool
			tr, ok = pk[0].(string)
			if !ok {
				return out, fail("corruption", "posting", "malformed trace id")
			}
		}
		if prev, exists := latest[tr]; !exists || v.Start > prev {
			latest[tr] = v.Start
		}
		if m.order == "time" && len(latest) == c.K {
			stoppedAtFrontier = true
			break
		}
	}
	if !stoppedAtFrontier {
		if _, err := it.Get(); err != nil {
			return out, err
		}
	}
	out.Hits = sortedHits(latest, c.K)
	return out, nil
}
