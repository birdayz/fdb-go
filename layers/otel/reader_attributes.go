package otel

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// Attribute autocomplete (rfcs/grafana-datasource.md §8): no key catalog
// exists, so keys and values are read from the typed attribute indexes,
// keyed (scope, key, value, ...), by skip scans: the first entry after the
// previous distinct value, one single-row range read per value. Long strings
// are indexed by digest and never offered.

// The caps of AttributeKeys and AttributeValues: their default and largest
// limit.
const (
	MaxAttributeKeys   = 1000
	MaxAttributeValues = 100
)

const (
	// attributeScanTime bounds one skip scan; past it the answer is truncated.
	attributeScanTime = 2 * time.Second
	// attributeScanReads bounds one skip scan's reads, which a value prefix
	// on a non-string type filters.
	attributeScanReads = 10_000
)

// attributeTypes are the indexed types, in AttributeKeys' scan order.
var attributeTypes = []string{"string", "int", "double", "bool"}

// attributeIndexes are each signal's typed attribute indexes and scopes.
var attributeIndexes = map[string]struct {
	index  map[string]string
	scopes []string
}{
	"span": {map[string]string{"string": spansStrAttrIndex, "int": spansIntAttrIndex, "double": spansDoubleAttrIndex, "bool": spansBoolAttrIndex}, []string{"resource", "scope", "span"}},
	"log":  {map[string]string{"string": logsStrAttrIndex, "int": logsIntAttrIndex, "double": logsDoubleAttrIndex, "bool": logsBoolAttrIndex}, []string{"resource", "scope", LogScopeRecord}},
}

// attributeIndex resolves signal ("span" or "log"), scope and typ ("" for
// every type) to the indexes to scan, in attributeTypes order.
func attributeIndex(op, signal, scope, typ string) ([]string, error) {
	s, ok := attributeIndexes[signal]
	if !ok {
		return nil, failf("config", op, "signal must be span or log, not %q", signal)
	}
	if !slices.Contains(s.scopes, scope) {
		return nil, failf("config", op, "%s attribute scope must be one of %s, not %q", signal, strings.Join(s.scopes, ", "), scope)
	}
	if typ == "" {
		out := make([]string, len(attributeTypes))
		for i, t := range attributeTypes {
			out[i] = s.index[t]
		}
		return out, nil
	}
	index, ok := s.index[typ]
	if !ok {
		return nil, failf("config", op, "attribute type must be string, int, double or bool, not %q", typ)
	}
	return []string{index}, nil
}

func attributeLimit(op string, limit, max int) (int, error) {
	if limit < 0 || limit > max {
		return 0, failf("config", op, "limit must be 0..%d", max)
	}
	if limit == 0 {
		return max, nil
	}
	return limit, nil
}

// AttributeKeys returns up to limit (0: MaxAttributeKeys) distinct attribute
// keys of signal ("span": scope resource, scope or span; "log": resource,
// scope or log) that start with prefix and have a value of typ (string, int,
// double or bool; "" any, the types scanned in parallel), in byte order.
// truncated reports that more keys may exist: past limit, or past the scan's
// time or read cap.
func (r *Reader) AttributeKeys(ctx context.Context, signal, scope, typ, prefix string, limit int) (keys []string, truncated bool, err error) {
	const op = "attribute keys"
	keys = []string{}
	ctx, err = r.begin(ctx, op)
	if err != nil {
		return keys, false, err
	}
	indexes, err := attributeIndex(op, signal, scope, typ)
	if err != nil {
		return keys, false, err
	}
	if limit, err = attributeLimit(op, limit, MaxAttributeKeys); err != nil {
		return keys, false, err
	}
	scans := make([]skipScan, len(indexes))
	var wg sync.WaitGroup
	for i, index := range indexes {
		scans[i] = skipScan{index: index, fixed: tuple.Tuple{scope}, prefix: prefix, text: stringText, limit: limit, maxReads: attributeScanReads, maxTime: attributeScanTime}
		wg.Go(func() { scans[i].run(ctx, r.store, op) })
	}
	wg.Wait()
	for _, s := range scans {
		if s.err != nil {
			return []string{}, false, s.err
		}
		keys = append(keys, s.out...)
		truncated = truncated || s.truncated
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	if len(keys) > limit {
		keys, truncated = keys[:limit], true
	}
	return keys, truncated, nil
}

// AttributeValues returns up to limit (0: MaxAttributeValues) distinct
// values of signal's attribute (a.Scope, a.Key) of type a.Type (a.Value is
// not read), in their canonical predicate text, that start with prefix, in
// index order. truncated is as AttributeKeys'.
func (r *Reader) AttributeValues(ctx context.Context, signal string, a Attribute, prefix string, limit int) (values []string, truncated bool, err error) {
	const op = "attribute values"
	values = []string{}
	ctx, err = r.begin(ctx, op)
	if err != nil {
		return values, false, err
	}
	if a.Type == "" {
		return values, false, fail("config", op, "attribute type must be string, int, double or bool")
	}
	indexes, err := attributeIndex(op, signal, a.Scope, a.Type)
	if err != nil {
		return values, false, err
	}
	if limit, err = attributeLimit(op, limit, MaxAttributeValues); err != nil {
		return values, false, err
	}
	if len(a.Key) > otelsql.MaxAttributeKeyBytes {
		// Never indexed (otelsql.MaxAttributeKeyBytes).
		return values, false, nil
	}
	s := skipScan{index: indexes[0], fixed: tuple.Tuple{a.Scope, a.Key}, limit: limit, maxReads: attributeScanReads, maxTime: attributeScanTime}
	switch a.Type {
	case "string":
		s.prefix, s.text = prefix, stringText
	case "int":
		s.text = intText
	case "double":
		s.text = doubleText
	case "bool":
		s.text = boolText
	}
	if a.Type != "string" && prefix != "" {
		s.keep = func(text string) bool { return strings.HasPrefix(text, prefix) }
	}
	s.run(ctx, r.store, op)
	if s.err != nil {
		return []string{}, false, s.err
	}
	return append(values, s.out...), s.truncated, nil
}

// skipScan reads the distinct values of the index column after fixed, in one
// read-only transaction, each the first entry after the previous value's
// entries. A string column's prefix bounds the key range; keep filters the
// values' text otherwise. Past limit values, maxReads reads or maxTime it
// stops, truncated.
type skipScan struct {
	index    string
	fixed    tuple.Tuple
	prefix   string
	text     func(tuple.TupleElement) (string, bool)
	keep     func(string) bool
	limit    int
	maxReads int
	maxTime  time.Duration

	out       []string
	truncated bool
	err       error
}

func (s *skipScan) run(ctx context.Context, ts *tenantStore, op string) {
	s.err = ts.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		s.out, s.truncated = nil, false
		idx, err := readableIndex(store, op, s.index)
		if err != nil {
			return err
		}
		space := store.IndexSubspace(idx)
		begin := space.Pack(s.fixed)
		end := append(slices.Clone(begin), 0xFF)
		if s.prefix != "" {
			// The packed string without its terminator: every string it begins.
			p := tuple.Tuple{s.prefix}.Pack()
			begin = append(begin, p[:len(p)-1]...)
			if end, err = fdb.Strinc(begin); err != nil {
				return err
			}
		}
		tx := store.Context().ReadTransaction(true)
		deadline := time.Now().Add(s.maxTime)
		for reads := 0; ; reads++ {
			if reads == s.maxReads || time.Now().After(deadline) {
				s.truncated = true
				return nil
			}
			kvs, err := tx.GetRange(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
			if err != nil {
				return err
			}
			if len(kvs) == 0 {
				return nil
			}
			key, err := space.Unpack(kvs[0].Key)
			if err != nil || len(key) <= len(s.fixed) {
				return failf("corruption", op, "%s entry is not (%d columns, value, ...)", s.index, len(s.fixed))
			}
			v := key[len(s.fixed)]
			text, ok := s.text(v)
			if !ok {
				return failf("corruption", op, "%s entry column %d is a %T", s.index, len(s.fixed), v)
			}
			// Past every entry of this value (no tuple element tag is 0xFF).
			begin = append(space.Pack(append(s.fixed[:len(s.fixed):len(s.fixed)], v)), 0xFF)
			if s.keep != nil && !s.keep(text) {
				continue
			}
			if len(s.out) == s.limit {
				s.truncated = true
				return nil
			}
			s.out = append(s.out, text)
		}
	})
}

func stringText(v tuple.TupleElement) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func intText(v tuple.TupleElement) (string, bool) {
	n, ok := v.(int64)
	return strconv.FormatInt(n, 10), ok
}

func doubleText(v tuple.TupleElement) (string, bool) {
	f, ok := v.(float64)
	return otelsql.FormatDouble(f), ok
}

func boolText(v tuple.TupleElement) (string, bool) {
	b, ok := v.(bool)
	return strconv.FormatBool(b), ok
}
