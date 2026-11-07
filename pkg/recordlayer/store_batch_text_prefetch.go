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
// Batch TEXT prefetch overlaps snapshot reads of token-map bunches before sequential maintenance,
// avoiding a per-record read round trip without changing write order.
//
// Prefetch only warms the RYW snapshot cache: maintenance still merges earlier writes and establishes conflicts.
// Truncation, errors or differing tokenization fall back to normal maintenance reads.

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
	index           *Index
	indexSubspace   subspace.Subspace
	secSubspace     subspace.Subspace
	versionSubspace subspace.Subspace
	tokenizer       TextTokenizer
	version         int
	store           *FDBRecordStore
}

func newTextIndexTarget(index *Index, indexSubspace, secSubspace subspace.Subspace) *textIndexTarget {
	tokenizer, err := getTextTokenizer(index)
	if err != nil {
		return nil
	}
	version, err := getTextTokenizerVersion(index)
	if err != nil {
		return nil
	}
	return &textIndexTarget{
		index:           index,
		indexSubspace:   indexSubspace,
		secSubspace:     secSubspace,
		versionSubspace: secSubspace.Sub(tokenizerVersionSubspaceTuple[0]),
		tokenizer:       tokenizer,
		version:         version,
	}
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
	key := []byte(t.versionSubspace.Pack(primaryKey))
	if _, seen := r.seenVersion[string(key)]; !seen {
		r.seenVersion[string(key)] = struct{}{}
		r.versionKeys = append(r.versionKeys, key)
	}
}

func (t *textIndexTarget) tokenizeToMap(text string) (map[string][]int, error) {
	if t.store != nil && t.store.batchTokenCache != nil {
		ck := batchTokenCacheKey{tokenizer: t.tokenizer.Name(), version: t.version, text: text}
		if cached, ok := t.store.batchTokenCache[ck]; ok {
			return cached, nil
		}
		positions, err := t.tokenizer.TokenizeToMap(text, t.version, TokenizerModeIndex)
		if err == nil {
			t.store.batchTokenCache[ck] = positions
		}
		return positions, err
	}
	return t.tokenizer.TokenizeToMap(text, t.version, TokenizerModeIndex)
}

// add best-effort records map keys using updateOneKey’s evaluation;
// maintenance reads anything it cannot evaluate.
func (r *textBunchReads) add(t *textIndexTarget, entryKeys []tuple.Tuple) {
	if r.byMap == nil {
		r.byMap = map[string]int{}
	}
	textPosition := textFieldPosition(t.index.RootExpression)
	var scratch []byte
	for _, entryKey := range entryKeys {
		if textPosition >= len(entryKey) {
			continue
		}
		text, ok := entryKey[textPosition].(string)
		if !ok || text == "" {
			continue
		}
		positions, err := t.tokenizeToMap(text)
		if err != nil {
			continue
		}
		grouping := entryKey[:textPosition]
		grouped := entryKey[textPosition+1:]
		var groupingPrefix []byte
		if len(grouping) == 0 {
			groupingPrefix = t.indexSubspace.Bytes()
		} else {
			groupingPrefix = t.indexSubspace.Pack(grouping)
		}
		packedGrouped := grouped.Pack()
		for token := range positions {
			scratch = append(scratch[:0], groupingPrefix...)
			scratch = appendPackedToken(scratch, token)
			i, ok := r.byMap[string(scratch)]
			var prefix []byte
			if !ok {
				prefix = bytes.Clone(scratch)
				i = len(r.maps)
				r.byMap[string(prefix)] = i
				r.maps = append(r.maps, textBunchMap{prefix: prefix})
			} else {
				prefix = r.maps[i].prefix
			}
			key := make([]byte, len(prefix)+len(packedGrouped))
			copy(key, prefix)
			copy(key[len(prefix):], packedGrouped)
			r.maps[i].keys = append(r.maps[i].keys, key)
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

// issue bounds each token map by predecessor/successor signposts plus tokenizer-version reads.
// The 2*keys+2 row cap limits distant-key overfetch; wait fills truncated coverage.
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

// wait drains initial reads, then missing bunches in a second wave;
// maintenance’s own reads report any dropped prefetch errors.
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

// prefetchBatchTextBunches warms maintained new/old TEXT keys and old tokenizer versions.
// olds[i]=nil means an insert or an old image supplied by an earlier record in this batch.
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
					t = &textIndexTarget{
						index:           index,
						indexSubspace:   tm.indexSubspace,
						secSubspace:     tm.secSubspace,
						versionSubspace: tm.versionSubspace,
						tokenizer:       tm.tokenizer,
						version:         tm.tokenizerVersion,
						store:           store,
					}
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
				// Updates read the tokenizer version and only remove/put differing entries (RemoveCommonEntries).
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
