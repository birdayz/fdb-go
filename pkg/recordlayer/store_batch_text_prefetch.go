package recordlayer

import (
	"bytes"
	"slices"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// TEXT bunch prefetch for batch saves, a Go batch-path extension (Java has no
// batch save).
//
// Saved one after another, each record's TEXT index update reads the
// BunchedMap bunches of its tokens (BunchedMap.put and remove,
// lastLessOrEqual(key) .. firstGreaterThan(key)+1 at snapshot isolation) only
// once the previous record is applied, so a batch costs a round trip per
// record. The batch path instead first issues, together, snapshot range reads
// that cover every one of those bunch reads, one per token map (index,
// grouping, token) over the batch's keys in it, and waits for them; then it
// applies the records in order through the unchanged maintenance code.
//
// The prefetch only fills the transaction's read-your-writes snapshot cache.
// Snapshot reads add no read conflict ranges and no mutations, and a read
// answered from that cache at the transaction's read version returns what the
// storage server would, merged with the transaction's own writes, exactly as
// a read sent to the server is. So every maintenance read returns what it
// returns when the records are saved one after another, including the writes
// of earlier records of the batch to the same bunch, and the maintenance makes
// the same conflict ranges and mutations; only its reads no longer leave the
// client. Anything the prefetch did not cover (a key past a truncated read,
// an error, a tokenization the maintenance does differently) is read by the
// maintenance as before.

// textBunchReads collects the reads a batch's TEXT index maintenance makes.
type textBunchReads struct {
	byMap map[string]int
	maps  []textBunchMap
	// versionKeys are records' tokenizer version keys, which an update or
	// delete reads (textIndexMaintainer.getRecordTokenizerVersion).
	versionKeys [][]byte
	seenVersion map[string]struct{}
}

// textBunchMap is one token map and the full keys (prefix + packed grouped
// key) the batch puts into or removes from it.
type textBunchMap struct {
	prefix []byte
	keys   [][]byte
}

// textIndexTarget is what the prefetch needs of a TEXT index.
type textIndexTarget struct {
	index         *Index
	indexSubspace subspace.Subspace
	secSubspace   subspace.Subspace
	tokenizer     TextTokenizer
	version       int
}

func newTextIndexTarget(index *Index, indexSubspace, secSubspace subspace.Subspace) (*textIndexTarget, bool) {
	tokenizer, err := getTextTokenizer(index)
	if err != nil {
		return nil, false
	}
	version, err := getTextTokenizerVersion(index)
	if err != nil {
		return nil, false
	}
	return &textIndexTarget{index: index, indexSubspace: indexSubspace, secSubspace: secSubspace, tokenizer: tokenizer, version: version}, true
}

// entryKeys are record's index entry keys, as updateOneKey builds them; nil
// when they cannot be evaluated.
func (t *textIndexTarget) entryKeys(record *FDBStoredRecord[proto.Message]) []tuple.Tuple {
	values, err := t.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil
	}
	keys := make([]tuple.Tuple, 0, len(values))
	for _, entry := range values {
		entryTuple := make(tuple.Tuple, len(entry))
		for i, v := range entry {
			entryTuple[i] = v
		}
		key, err := indexEntryKey(t.index, entryTuple, record.PrimaryKey)
		if err != nil {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

// addVersionKey records the tokenizer version key the maintenance reads for
// an update or delete of the record with primaryKey.
func (r *textBunchReads) addVersionKey(t *textIndexTarget, primaryKey tuple.Tuple) {
	if r.seenVersion == nil {
		r.seenVersion = map[string]struct{}{}
	}
	key := t.secSubspace.Sub(tokenizerVersionSubspaceTuple[0]).Sub(tupleToTupleElements(primaryKey)...).Bytes()
	if _, seen := r.seenVersion[string(key)]; !seen {
		r.seenVersion[string(key)] = struct{}{}
		r.versionKeys = append(r.versionKeys, key)
	}
}

// add records the map keys the index's maintenance puts or removes for the
// entry keys, computed as updateOneKey computes them. It is best effort: what
// it cannot evaluate, the maintenance reads itself.
func (r *textBunchReads) add(t *textIndexTarget, entryKeys []tuple.Tuple) {
	if r.byMap == nil {
		r.byMap = map[string]int{}
	}
	textPosition := textFieldPosition(t.index.RootExpression)
	for _, entryKey := range entryKeys {
		if textPosition >= len(entryKey) {
			continue
		}
		text, ok := entryKey[textPosition].(string)
		if !ok || text == "" {
			continue
		}
		positions, err := t.tokenizer.TokenizeToMap(text, t.version, TokenizerModeIndex)
		if err != nil {
			continue
		}
		grouping := entryKey[:textPosition]
		grouped := entryKey[textPosition+1:]
		for token := range positions {
			subspaceTuple := make(tuple.Tuple, 0, len(grouping)+1)
			subspaceTuple = append(subspaceTuple, grouping...)
			subspaceTuple = append(subspaceTuple, token)
			prefix := t.indexSubspace.Sub(tupleToTupleElements(subspaceTuple)...).Bytes()
			i, ok := r.byMap[string(prefix)]
			if !ok {
				i = len(r.maps)
				r.byMap[string(prefix)] = i
				r.maps = append(r.maps, textBunchMap{prefix: prefix})
			}
			r.maps[i].keys = append(r.maps[i].keys, grouped.PackWithPrefix(prefix))
		}
	}
}

// pendingTextBunchRead is an issued prefetch read of one token map.
type pendingTextBunchRead struct {
	m      *textBunchMap
	limit  int
	result fdb.RangeResult
}

// textBunchPrefetch is a prefetch in flight.
type textBunchPrefetch struct {
	tx       fdb.ReadTransaction
	reads    []pendingTextBunchRead
	versions []fdb.FutureByteSlice
}

// issue sends the first wave: per token map, one snapshot read from
// lastLessOrEqual(first key) to firstGreaterThan(last key)+1, which holds
// every bunch read of the map's keys (each reads the last signpost at or
// before its key and the two keys after it) as long as the read is not cut
// by its row limit; and the tokenizer version keys. The limit bounds a map
// whose keys lie far apart: 2*keys+2 rows hold the reads of keys that share
// bunches.
func (r *textBunchReads) issue(tx fdb.ReadTransaction) *textBunchPrefetch {
	p := &textBunchPrefetch{tx: tx}
	for i := range r.maps {
		m := &r.maps[i]
		slices.SortFunc(m.keys, bytes.Compare)
		m.keys = slices.CompactFunc(m.keys, bytes.Equal)
		limit := 2*len(m.keys) + 2
		end := fdb.FirstGreaterThan(fdb.Key(m.keys[len(m.keys)-1]))
		end.Offset++
		p.reads = append(p.reads, pendingTextBunchRead{m: m, limit: limit, result: tx.GetRange(fdb.SelectorRange{
			Begin: fdb.LastLessOrEqual(fdb.Key(m.keys[0])),
			End:   end,
		}, fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll})})
	}
	for _, key := range r.versionKeys {
		p.versions = append(p.versions, tx.Get(fdb.Key(key)))
	}
	return p
}

// wait drains the first wave, then reads, together, the bunches of the keys a
// truncated read did not reach, as BunchedMap.put reads them, and drains
// those. Errors are dropped: the maintenance's own reads report them.
func (p *textBunchPrefetch) wait() {
	var second []fdb.RangeResult
	for _, read := range p.reads {
		rows, err := read.result.GetSliceWithError()
		if err != nil || len(rows) < read.limit {
			continue
		}
		// A key is held when two of the rows lie after it.
		for _, key := range read.m.keys {
			after := len(rows) - sortSearchKeys(rows, key)
			if after >= 2 {
				continue
			}
			end := fdb.FirstGreaterThan(fdb.Key(key))
			end.Offset++
			second = append(second, p.tx.GetRange(fdb.SelectorRange{
				Begin: fdb.LastLessOrEqual(fdb.Key(key)),
				End:   end,
			}, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}))
		}
	}
	for _, f := range p.versions {
		_, _ = f.Get()
	}
	for _, result := range second {
		_, _ = result.GetSliceWithError()
	}
}

// sortSearchKeys is the index of the first row whose key is after key.
func sortSearchKeys(rows []fdb.KeyValue, key []byte) int {
	i, _ := slices.BinarySearchFunc(rows, key, func(row fdb.KeyValue, key []byte) int {
		if bytes.Compare(row.Key, key) <= 0 {
			return -1
		}
		return 1
	})
	return i
}

// prefetchBatchTextBunches is the prefetch for a batch save: the maintained
// TEXT indexes of each record's new and old type, the new records' keys, and
// the old images' keys and tokenizer versions. olds[i] is nil for an insert
// or for a record whose old image is an earlier record of the batch (its
// tokens are that record's).
func (store *FDBRecordStore) prefetchBatchTextBunches(news, olds []*FDBStoredRecord[proto.Message]) {
	var reads textBunchReads
	targets := map[string]*textIndexTarget{}
	targetsFor := func(rt *RecordType) []*textIndexTarget {
		var out []*textIndexTarget
		for _, indexes := range [][]*Index{store.metaData.GetIndexesForRecordType(rt.Name), store.metaData.GetUniversalIndexes()} {
			for _, index := range indexes {
				if canonicalIndexType(index.Type) != IndexTypeText {
					continue
				}
				if maintain, err := store.shouldMaintainIndex(index.Name); err != nil || !maintain {
					continue
				}
				t, ok := targets[index.Name]
				if !ok {
					m, err := store.getIndexMaintainer(index)
					if err != nil {
						continue
					}
					tm, isText := m.(*textIndexMaintainer)
					if !isText {
						continue
					}
					t = &textIndexTarget{index: index, indexSubspace: tm.indexSubspace, secSubspace: tm.secSubspace, tokenizer: tm.tokenizer, version: tm.tokenizerVersion}
					targets[index.Name] = t
				}
				out = append(out, t)
			}
		}
		return out
	}
	for i, record := range news {
		newTargets := targetsFor(record.RecordType)
		var oldTargets []*textIndexTarget
		if olds[i] != nil {
			oldTargets = targetsFor(olds[i].RecordType)
		}
		for _, t := range newTargets {
			newKeys := t.entryKeys(record)
			if olds[i] != nil && slices.Contains(oldTargets, t) {
				// An update: the version is read, and only the entries that
				// differ are removed and put (updateStandard's
				// RemoveCommonEntries).
				reads.addVersionKey(t, olds[i].PrimaryKey)
				oldKeys := t.entryKeys(olds[i])
				common := map[string]struct{}{}
				for _, k := range oldKeys {
					common[string(k.Pack())] = struct{}{}
				}
				var putKeys []tuple.Tuple
				kept := map[string]struct{}{}
				for _, k := range newKeys {
					if _, ok := common[string(k.Pack())]; ok {
						kept[string(k.Pack())] = struct{}{}
						continue
					}
					putKeys = append(putKeys, k)
				}
				var removeKeys []tuple.Tuple
				for _, k := range oldKeys {
					if _, ok := kept[string(k.Pack())]; !ok {
						removeKeys = append(removeKeys, k)
					}
				}
				reads.add(t, removeKeys)
				reads.add(t, putKeys)
				continue
			}
			reads.add(t, newKeys)
		}
		// Indexes of the old type only: a cross-type overwrite removes them.
		for _, t := range oldTargets {
			if !slices.Contains(newTargets, t) {
				reads.addVersionKey(t, olds[i].PrimaryKey)
				reads.add(t, t.entryKeys(olds[i]))
			}
		}
	}
	if len(reads.maps) == 0 && len(reads.versionKeys) == 0 {
		return
	}
	reads.issue(store.context.Transaction().Snapshot()).wait()
}
