//go:build bazelrunfiles

package conformance_test

// RYW and conflict-range conformance: each scenario script runs on the Go client and on libfdb_c
// (ryw_script_conformance.java); transcripts must match and contain the scenario's pinned lines.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type rywSel struct {
	K       string `json:"k"`
	OrEqual bool   `json:"orEqual"`
	Offset  int    `json:"offset"`
}

// rywStep is one script step; byte strings travel as hex. Steps run in order on
// the named transaction, created on first use.
type rywStep struct {
	Tx      string  `json:"tx"`
	Op      string  `json:"op"`
	K       string  `json:"k,omitempty"`
	E       string  `json:"e,omitempty"`
	V       string  `json:"v,omitempty"`
	B       *rywSel `json:"b,omitempty"`
	ES      *rywSel `json:"es,omitempty"`
	Code    int     `json:"code,omitempty"`
	Limit   int     `json:"limit,omitempty"`
	Exact   bool    `json:"exact,omitempty"`
	Reverse bool    `json:"reverse,omitempty"`
	Snap    bool    `json:"snap,omitempty"`
	ID      string  `json:"id,omitempty"`
	Opt     string  `json:"opt,omitempty"`
	RV      int64   `json:"rv,omitempty"`
	MS      int     `json:"ms,omitempty"` // sleep's duration in milliseconds
}

// Mutation type codes (fdb_c_options.g.h).
const (
	rywAdd             = 2
	rywBitAnd          = 6
	rywBitOr           = 7
	rywBitXor          = 8
	rywAppendIfFits    = 9
	rywMax             = 12
	rywMin             = 13
	rywByteMin         = 16
	rywByteMax         = 17
	rywCompareAndClear = 20
)

func hx(s string) string { return hex.EncodeToString([]byte(s)) }

func fge(k string) *rywSel { return &rywSel{K: hx(k), Offset: 1} }
func fgt(k string) *rywSel { return &rywSel{K: hx(k), OrEqual: true, Offset: 1} }
func lle(k string) *rywSel { return &rywSel{K: hx(k), OrEqual: true} }
func llt(k string) *rywSel { return &rywSel{K: hx(k)} }
func plus(s *rywSel, n int) *rywSel {
	c := *s
	c.Offset += n
	return &c
}

func sOpt(tx, opt string) rywStep { return rywStep{Tx: tx, Op: "option", Opt: opt} }
func sGRV(tx string) rywStep      { return rywStep{Tx: tx, Op: "grv"} }
func sSet(tx, k, v string) rywStep {
	return rywStep{Tx: tx, Op: "set", K: hx(k), V: hx(v)}
}
func sClear(tx, k string) rywStep { return rywStep{Tx: tx, Op: "clear", K: hx(k)} }
func sClearRange(tx, b, e string) rywStep {
	return rywStep{Tx: tx, Op: "clearRange", K: hx(b), E: hx(e)}
}

func sAtomic(tx string, code int, k, v string) rywStep {
	return rywStep{Tx: tx, Op: "atomic", Code: code, K: hx(k), V: hx(v)}
}

func sRCR(tx, b, e string) rywStep {
	return rywStep{Tx: tx, Op: "addReadConflictRange", K: hx(b), E: hx(e)}
}
func sRCK(tx, k string) rywStep { return rywStep{Tx: tx, Op: "addReadConflictKey", K: hx(k)} }
func sWCR(tx, b, e string) rywStep {
	return rywStep{Tx: tx, Op: "addWriteConflictRange", K: hx(b), E: hx(e)}
}
func sWCK(tx, k string) rywStep { return rywStep{Tx: tx, Op: "addWriteConflictKey", K: hx(k)} }
func sGet(tx, k string, snap bool, id string) rywStep {
	return rywStep{Tx: tx, Op: "get", K: hx(k), Snap: snap, ID: id}
}

func sGetKey(tx string, sel *rywSel, snap bool, id string) rywStep {
	return rywStep{Tx: tx, Op: "getKey", B: sel, Snap: snap, ID: id}
}

func sRange(tx string, b, e *rywSel, limit int, reverse, snap bool, id string) rywStep {
	return rywStep{Tx: tx, Op: "getRange", B: b, ES: e, Limit: limit, Reverse: reverse, Snap: snap, ID: id}
}

// EXACT with a bounded fixture avoids Java asList issuing later batches across writes
// (RangeQuery.java:79-96; fdb_c.cpp:1002-1011 maps WANT_ALL to byte-limited SERIAL).
func sExactRange(tx string, b, e *rywSel, reverse, snap bool, id string) rywStep {
	s := sRange(tx, b, e, 1000, reverse, snap, id)
	s.Exact = true
	return s
}

func sAwait(tx, id string) rywStep       { return rywStep{Tx: tx, Op: "await", ID: id} }
func sCommit(tx string) rywStep          { return rywStep{Tx: tx, Op: "commit"} }
func sConflicting(tx string) rywStep     { return rywStep{Tx: tx, Op: "conflicting"} }
func sReport(tx string) rywStep          { return sOpt(tx, "reportConflictingKeys") }
func sNoWriteConflict(tx string) rywStep { return sOpt(tx, "nextWriteNoWriteConflictRange") }
func sRV(tx string, v int64) rywStep     { return rywStep{Tx: tx, Op: "setReadVersion", RV: v} }
func sReset(tx string) rywStep           { return rywStep{Tx: tx, Op: "reset"} }
func sCancel(tx string) rywStep          { return rywStep{Tx: tx, Op: "cancel"} }
func sSleep(tx string, ms int) rywStep   { return rywStep{Tx: tx, Op: "sleep", MS: ms} }
func sOnError(tx string, code int) rywStep {
	return rywStep{Tx: tx, Op: "onError", Code: code}
}

// seedSteps commits key=value pairs in transaction "seed".
func seedSteps(kv ...string) []rywStep {
	var steps []rywStep
	for i := 0; i+1 < len(kv); i += 2 {
		steps = append(steps, sSet("seed", kv[i], kv[i+1]))
	}
	return append(steps, sCommit("seed"))
}

// rywValue is the transcript form of a value; it matches value() in the Java twin.
func rywValue(v []byte) string {
	if v == nil {
		return "null"
	}
	if len(v) <= 64 {
		return hex.EncodeToString(v)
	}
	sum := sha256.Sum256(v)
	return fmt.Sprintf("len=%d:sha256=%s", len(v), hex.EncodeToString(sum[:]))
}

func rywKVs(rows []gofdb.KeyValue, strip int) string {
	parts := make([]string, len(rows))
	for i, kv := range rows {
		parts[i] = hex.EncodeToString(kv.Key[strip:]) + "=" + rywValue(kv.Value)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

var (
	conflictingKeysBegin = []byte("\xff\xff/transaction/conflicting_keys/")
	conflictingKeysEnd   = []byte("\xff\xff/transaction/conflicting_keys/\xff\xff")
)

// rywErr renders an FDB error as the Java twin does; any other error fails the run.
func rywErr(err error) (string, error) {
	var fe gofdb.Error
	if errors.As(err, &fe) {
		return fmt.Sprintf("err=%d", fe.Code), nil
	}
	return "", err
}

func rywMutate(tr gofdb.Transaction, code int, k, v []byte) error {
	key := gofdb.Key(k)
	switch code {
	case rywAdd:
		tr.Add(key, v)
	case rywBitAnd:
		tr.BitAnd(key, v)
	case rywBitOr:
		tr.BitOr(key, v)
	case rywBitXor:
		tr.BitXor(key, v)
	case rywAppendIfFits:
		tr.AppendIfFits(key, v)
	case rywMax:
		tr.Max(key, v)
	case rywMin:
		tr.Min(key, v)
	case rywByteMin:
		tr.ByteMin(key, v)
	case rywByteMax:
		tr.ByteMax(key, v)
	case rywCompareAndClear:
		tr.CompareAndClear(key, v)
	default:
		return fmt.Errorf("unknown mutation type %d", code)
	}
	return nil
}

func rywSelector(s *rywSel) gofdb.KeySelector {
	k, _ := hex.DecodeString(s.K)
	return gofdb.KeySelector{Key: gofdb.Key(k), OrEqual: s.OrEqual, Offset: s.Offset}
}

// runGoRYWScript is the Go twin of RywScriptSteps.rywScript.
func runGoRYWScript(tenant gofdb.Tenant, steps []rywStep, progress *atomic.Int64) ([]string, error) {
	txs := map[string]gofdb.Transaction{}
	defer func() {
		for _, tr := range txs {
			tr.Cancel()
		}
	}()
	pending := map[string]func() (string, error){}
	var out []string
	for i, s := range steps {
		progress.Store(int64(i))
		tr, ok := txs[s.Tx]
		if !ok {
			var err error
			if tr, err = tenant.CreateTransaction(); err != nil {
				return nil, err
			}
			txs[s.Tx] = tr
		}
		k, _ := hex.DecodeString(s.K)
		e, _ := hex.DecodeString(s.E)
		v, _ := hex.DecodeString(s.V)
		var rt gofdb.ReadTransaction = tr
		if s.Snap {
			rt = tr.Snapshot()
		}
		var result string
		var err error
		switch s.Op {
		case "option":
			switch s.Opt {
			case "reportConflictingKeys":
				err = tr.Options().SetReportConflictingKeys()
			case "nextWriteNoWriteConflictRange":
				err = tr.Options().SetNextWriteNoWriteConflictRange()
			case "readYourWritesDisable":
				err = tr.Options().SetReadYourWritesDisable()
			default:
				return nil, fmt.Errorf("unknown option %q", s.Opt)
			}
		case "grv":
			_, err = tr.GetReadVersion().Get()
		case "setReadVersion":
			rv := s.RV
			if rv < 0 { // that far past a fresh read version, as in the Java twin
				fresh, ferr := tenant.CreateTransaction()
				if ferr != nil {
					return nil, ferr
				}
				v, ferr := fresh.GetReadVersion().Get()
				fresh.Cancel()
				if ferr != nil {
					return nil, ferr
				}
				rv = v - rv
			}
			tr.SetReadVersion(rv)
		case "reset":
			tr.Reset()
		case "cancel":
			tr.Cancel()
		case "sleep":
			time.Sleep(time.Duration(s.MS) * time.Millisecond)
		case "onError":
			if err = tr.OnError(gofdb.Error{Code: s.Code}).Get(); err == nil {
				result = "ok"
			}
		case "set":
			tr.Set(gofdb.Key(k), v)
		case "clear":
			tr.Clear(gofdb.Key(k))
		case "clearRange":
			tr.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(k), End: gofdb.Key(e)})
		case "atomic":
			if merr := rywMutate(tr, s.Code, k, v); merr != nil {
				return nil, merr
			}
		case "addReadConflictRange":
			err = tr.AddReadConflictRange(gofdb.KeyRange{Begin: gofdb.Key(k), End: gofdb.Key(e)})
		case "addReadConflictKey":
			err = tr.AddReadConflictKey(gofdb.Key(k))
		case "addWriteConflictRange":
			err = tr.AddWriteConflictRange(gofdb.KeyRange{Begin: gofdb.Key(k), End: gofdb.Key(e)})
		case "addWriteConflictKey":
			err = tr.AddWriteConflictKey(gofdb.Key(k))
		case "get", "getKey", "getRange":
			var wait func() (string, error)
			switch s.Op {
			case "get":
				f := rt.Get(gofdb.Key(k))
				wait = func() (string, error) {
					val, gerr := f.Get()
					if gerr != nil {
						return rywErr(gerr)
					}
					return rywValue(val), nil
				}
			case "getKey":
				f := rt.GetKey(rywSelector(s.B))
				wait = func() (string, error) {
					key, gerr := f.Get()
					if gerr != nil {
						return rywErr(gerr)
					}
					return hex.EncodeToString(key), nil
				}
			default:
				mode := gofdb.StreamingModeWantAll
				if s.Exact {
					mode = gofdb.StreamingModeExact
				}
				r := rt.GetRange(gofdb.SelectorRange{Begin: rywSelector(s.B), End: rywSelector(s.ES)},
					gofdb.RangeOptions{Limit: s.Limit, Reverse: s.Reverse, Mode: mode})
				wait = func() (string, error) {
					rows, gerr := r.GetSliceWithError()
					if gerr != nil {
						return rywErr(gerr)
					}
					return rywKVs(rows, 0), nil
				}
			}
			if s.ID != "" {
				pending[s.ID] = wait
				continue
			}
			if result, err = wait(); err != nil {
				return nil, err
			}
		case "await":
			wait, ok := pending[s.ID]
			if !ok {
				return nil, fmt.Errorf("step %d: no pending read %q", i, s.ID)
			}
			delete(pending, s.ID)
			if result, err = wait(); err != nil {
				return nil, err
			}
		case "commit":
			if err = tr.Commit().Get(); err == nil {
				result = "ok"
			}
		case "conflicting":
			var rows []gofdb.KeyValue
			rows, err = tr.GetRange(gofdb.KeyRange{Begin: gofdb.Key(conflictingKeysBegin), End: gofdb.Key(conflictingKeysEnd)},
				gofdb.RangeOptions{}).GetSliceWithError()
			if err == nil {
				result = rywKVs(rows, len(conflictingKeysBegin))
			}
		default:
			return nil, fmt.Errorf("unknown op %q", s.Op)
		}
		if err != nil {
			if result, err = rywErr(err); err != nil {
				return nil, fmt.Errorf("step %d %s: %w", i, s.Op, err)
			}
		}
		// Reads (issued ones at their await), commits and conflicting_keys always
		// report, even an empty key or value; other steps only an error.
		switch s.Op {
		case "get", "getKey", "getRange", "await", "commit", "conflicting", "onError":
		default:
			if result == "" {
				continue
			}
		}
		out = append(out, fmt.Sprintf("%d %s %s %s", i, s.Tx, s.Op, result))
	}
	return out, nil
}

// rywTranscriptDiff lists the lines where the transcripts differ, by step: a
// step's line is "<index> <tx> <op> <result>", and a step without output has none.
func rywTranscriptDiff(goOut, javaOut []string) string {
	byStep := func(lines []string) (map[string]string, []string) {
		m := map[string]string{}
		var order []string
		for _, l := range lines {
			f := strings.SplitN(l, " ", 4)
			key := strings.Join(f[:3], " ")
			if _, ok := m[key]; !ok {
				order = append(order, key)
			}
			m[key] = l
		}
		return m, order
	}
	g, gOrder := byStep(goOut)
	j, jOrder := byStep(javaOut)
	var diff []string
	seen := map[string]bool{}
	for _, key := range append(jOrder, gOrder...) {
		if seen[key] {
			continue
		}
		seen[key] = true
		if g[key] != j[key] {
			diff = append(diff, fmt.Sprintf("  libfdb_c: %q\n  go:       %q", j[key], g[key]))
		}
	}
	return strings.Join(diff, "\n")
}

// runGoRYWScriptBounded fails a script that hangs, naming the step, instead of the suite.
func runGoRYWScriptBounded(tenant gofdb.Tenant, steps []rywStep, limit time.Duration) ([]string, error) {
	var progress atomic.Int64
	type result struct {
		out []string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runGoRYWScript(tenant, steps, &progress)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(limit):
		i := progress.Load()
		return nil, fmt.Errorf("hung for %v at step %d (%s %s)", limit, i, steps[i].Tx, steps[i].Op)
	}
}

var rywScriptJava struct {
	once    sync.Once
	invoker *JavaInvoker
	err     error
}

// rywScriptInvoker is a server at API version 7.1, the newest fdb-java 7.1.26
// selects, closest to the Go client's 7.3; the shared server keeps Java's 6.3.
func rywScriptInvoker() *JavaInvoker {
	rywScriptJava.once.Do(func() {
		flags := append(append([]string{}, reusedServerJVMFlags...), "-Dconformance.fdbApiVersion=710")
		rywScriptJava.invoker, rywScriptJava.err = startJavaServer(flags...)
	})
	Expect(rywScriptJava.err).NotTo(HaveOccurred())
	return rywScriptJava.invoker
}

type rywScenario struct {
	name  string
	steps []rywStep
	// want are transcript lines the scenario's point depends on; both sides must produce them.
	want []string
}

// line is the transcript line of steps[i] with the given result.
func (sc rywScenario) line(i int, result string) string {
	return fmt.Sprintf("%d %s %s %s", i, sc.steps[i].Tx, sc.steps[i].Op, result)
}

// at returns the index of the n-th (0-based) step with op on tx.
func (sc rywScenario) at(tx, op string, n int) int {
	for i, s := range sc.steps {
		if s.Tx == tx && s.Op == op {
			if n == 0 {
				return i
			}
			n--
		}
	}
	panic(fmt.Sprintf("scenario %s: no step %s %s #%d", sc.name, tx, op, n))
}

// expect pins the result of the n-th op on tx.
func (sc *rywScenario) expect(tx, op string, n int, result string) {
	sc.want = append(sc.want, sc.line(sc.at(tx, op, n), result))
}

func hkv(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, hx(pairs[i])+"="+pairs[i+1])
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// conflictingRanges renders conflicting_keys for begin/end pairs: "1" opens a range, "0" closes it.
func conflictingRanges(bounds ...string) string {
	var pairs []string
	for i := 0; i+1 < len(bounds); i += 2 {
		pairs = append(pairs, bounds[i], hx("1"), bounds[i+1], hx("0"))
	}
	return hkv(pairs...)
}

func digits(prefix string, n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s%d", prefix, i+1)
	}
	return keys
}

func seededKeys(prefix string, n int) []rywStep {
	var kv []string
	for _, k := range digits(prefix, n) {
		kv = append(kv, k, "v"+k)
	}
	return seedSteps(kv...)
}

// rywScenarios builds the corpus. Writer "b" commits between reader "a"'s read version
// and its commit, so a's shipped read conflicts decide what conflicts.
func rywScenarios() []rywScenario {
	var scs []rywScenario
	add := func(sc rywScenario, pins func(sc *rywScenario)) {
		if pins != nil {
			pins(&sc)
		}
		scs = append(scs, sc)
	}
	cat := func(parts ...[]rywStep) []rywStep {
		var out []rywStep
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}

	// (a) Explicit read conflicts: overlapping and adjacent ranges coalesce, empty ones
	// add nothing (RYW readConflicts is a CoalescedKeyRefRangeMap, ReadYourWrites.h:237).
	add(rywScenario{name: "explicit read conflicts coalesce", steps: cat(seededKeys("k", 9), []rywStep{
		sReport("a"), sGRV("a"),
		sRCR("a", "k2", "k4"), sRCR("a", "k4", "k6"), sRCR("a", "k3", "k5"), sRCK("a", "k8"),
		sRCR("a", "k7", "k7"), sRCR("a", "k9", "k8"),
		sSet("a", "zz", "a"),
		sSet("b", "k5", "w"), sSet("b", "k8", "w"), sSet("b", "k1", "w"), sCommit("b"),
		sCommit("a"), sConflicting("a"),
	})}, func(sc *rywScenario) {
		sc.expect("a", "addReadConflictRange", 4, "err=2005")
		sc.expect("a", "commit", 0, "err=1020")
		sc.expect("a", "conflicting", 0, conflictingRanges("k2", "k6", "k8", "k8\x00"))
	})

	// Without RYW the native transaction keeps the ranges as added; the reported
	// conflicting keys are still a coalesced map (NativeAPI.actor.cpp:6711-6716).
	add(rywScenario{name: "read conflicts without RYW", steps: cat(seededKeys("k", 9), []rywStep{
		sOpt("a", "readYourWritesDisable"), sReport("a"), sGRV("a"),
		sRCR("a", "k2", "k4"), sRCR("a", "k3", "k6"), sRCK("a", "k8"), sRCR("a", "k6", "k6"),
		sGet("a", "k9", false, ""), sSet("a", "k9", "x"), sGet("a", "k9", false, ""),
		sRange("a", fge("k7"), fge("k8"), 0, false, false, ""),
		sRange("a", lle("k1"), plus(fgt("k1"), 1), 0, false, true, ""),
		sSet("b", "k5", "w"), sSet("b", "k8", "w"), sSet("b", "k9", "w"), sSet("b", "k1", "w"), sCommit("b"),
		sCommit("a"), sConflicting("a"),
	})}, func(sc *rywScenario) {
		sc.expect("a", "get", 0, hx("vk9"))
		sc.expect("a", "get", 1, hx("vk9")) // no RYW: the local set is not read back
		sc.expect("a", "commit", 0, "err=1020")
		// Only the shipped ranges that conflict, uncoalesced: [k3,k6) holds k5, [k2,k4) nothing.
		// With RYW the same adds coalesce first ("explicit read conflicts coalesce").
		sc.expect("a", "conflicting", 0, conflictingRanges("k3", "k6", "k8", "k8\x00", "k9", "k9\x00"))
	})

	// AddReadConflictRange/Key skip what local independent writes satisfy
	// (updateConflictMap, ReadYourWrites.actor.cpp:335-351).
	for _, writer := range [][]string{{"k3"}, {"k3", "k4"}, {"k2", "k5"}} {
		steps := cat(seededKeys("k", 9), []rywStep{
			sReport("a"), sGRV("a"),
			sSet("a", "k3", "local"), sClear("a", "k5"), sAtomic("a", rywAdd, "k2", "\x01"),
			sRCR("a", "k1", "k6"), sRCK("a", "k3"), sRCK("a", "k5"),
			sSet("a", "zz", "a"),
		})
		for _, k := range writer {
			steps = append(steps, sSet("b", k, "w"))
		}
		steps = append(steps, sCommit("b"), sCommit("a"), sConflicting("a"))
		w := writer
		add(rywScenario{name: "read conflicts filtered by local writes/b=" + strings.Join(writer, ","), steps: steps},
			func(sc *rywScenario) {
				switch w[len(w)-1] {
				case "k3":
					sc.expect("a", "commit", 0, "ok")
				case "k4":
					sc.expect("a", "commit", 0, "err=1020")
					sc.expect("a", "conflicting", 0, conflictingRanges("k3\x00", "k5"))
				case "k5":
					// k2 holds a dependent atomic: its read conflict stays.
					sc.expect("a", "commit", 0, "err=1020")
					sc.expect("a", "conflicting", 0, conflictingRanges("k1", "k3"))
				}
			})
	}

	// Gets under local writes: a set or cleared key is served locally and adds no
	// conflict; a key with a pending atomic is read and conflicts (RYW:322-332).
	for _, writer := range []string{"k2,k3,k7", "k4", "k5,k6", "k2,k4,k6"} {
		steps := cat(seededKeys("k", 9), []rywStep{
			sReport("a"), sGRV("a"),
			sSet("a", "k2", "l"), sClear("a", "k3"), sAtomic("a", rywAdd, "k4", "\x01"),
			sAtomic("a", rywAppendIfFits, "k5", "x"),
			sGet("a", "k2", false, ""), sGet("a", "k3", false, ""), sGet("a", "k4", false, ""),
			sGet("a", "k5", false, ""), sGet("a", "k6", false, ""), sGet("a", "k7", true, ""),
			sSet("a", "zz", "a"),
		})
		for _, k := range strings.Split(writer, ",") {
			steps = append(steps, sSet("b", k, "w"))
		}
		steps = append(steps, sCommit("b"), sCommit("a"), sConflicting("a"))
		w := writer
		add(rywScenario{name: "gets under local writes/b=" + writer, steps: steps}, func(sc *rywScenario) {
			sc.expect("a", "get", 0, hx("l"))
			sc.expect("a", "get", 1, "null")
			sc.expect("a", "get", 2, hx("w")) // ADD keeps the operand's length: "v" + 1
			sc.expect("a", "get", 3, hx("vk5x"))
			sc.expect("a", "get", 4, hx("vk6"))
			sc.expect("a", "get", 5, hx("vk7"))
			switch w {
			case "k2,k3,k7":
				sc.expect("a", "commit", 0, "ok")
			case "k4":
				sc.expect("a", "conflicting", 0, conflictingRanges("k4", "k4\x00"))
			case "k5,k6":
				sc.expect("a", "conflicting", 0, conflictingRanges("k5", "k5\x00", "k6", "k6\x00"))
			case "k2,k4,k6":
				// k2 was served by the local set and adds no conflict.
				sc.expect("a", "conflicting", 0, conflictingRanges("k4", "k4\x00", "k6", "k6\x00"))
			}
		})
	}

	// Selector reads lastLessOrEqual(k)..firstGreaterThan(k)+1, snapshot and
	// serializable, with and without local writes in the range.
	for _, writer := range []string{"k5", "k4", "k3x", "k7"} {
		steps := cat(seededKeys("k", 9), []rywStep{
			sReport("a"), sGRV("a"),
			sRange("a", lle("k5"), plus(fgt("k5"), 1), 0, false, true, ""),
			sRange("a", lle("k3"), plus(fgt("k3"), 1), 0, false, false, ""),
			sSet("a", "k6x", "local"),
			sRange("a", lle("k6"), plus(fgt("k6"), 1), 0, false, false, ""),
			sRange("a", fge("k2"), fge("k9"), 2, true, false, ""),
			sSet("a", "zz", "a"),
			sSet("b", writer, "w"), sCommit("b"), sCommit("a"), sConflicting("a"),
		})
		w := writer
		add(rywScenario{name: "selector reads/b=" + writer, steps: steps}, func(sc *rywScenario) {
			sc.expect("a", "getRange", 0, hkv("k5", hx("vk5"), "k6", hx("vk6")))
			sc.expect("a", "getRange", 1, hkv("k3", hx("vk3"), "k4", hx("vk4")))
			sc.expect("a", "getRange", 2, hkv("k6", hx("vk6"), "k6x", hx("local")))
			sc.expect("a", "getRange", 3, hkv("k8", hx("vk8"), "k7", hx("vk7")))
			switch w {
			case "k5":
				sc.expect("a", "commit", 0, "ok")
			case "k4", "k3x":
				// The serializable read's range ends after its last row, not at its end selector.
				sc.expect("a", "commit", 0, "err=1020")
				sc.expect("a", "conflicting", 0, conflictingRanges("k3", "k4\x00"))
			case "k7":
				sc.expect("a", "conflicting", 0, conflictingRanges("k7", "k9"))
			}
		})
	}

	// (b) RYW read results over pending sets, clears, clear ranges and atomics.
	letters := strings.Split("a b c d e f g h i j", " ")
	var seedKV []string
	for _, l := range letters {
		seedKV = append(seedKV, l, l)
	}
	pendingWrites := []rywStep{
		sSet("a", "c", "C"), sClear("a", "e"), sClearRange("a", "g", "i"),
		sAtomic("a", rywAdd, "b", "\x01"), sAtomic("a", rywAppendIfFits, "d", "+"),
		sAtomic("a", rywByteMax, "f", "zz"), sAtomic("a", rywCompareAndClear, "j", "j"),
		sAtomic("a", rywBitOr, "h", "\x01"), sSet("a", "c5", "new"), sAtomic("a", rywMin, "a", "\x00"),
		sAtomic("a", rywAppendIfFits, "d", "-"), sSet("a", "i", "I"), sClear("a", "i"),
	}
	for _, snap := range []bool{true, false} {
		steps := cat(seedSteps(seedKV...), pendingWrites)
		for _, k := range []string{"", "a", "b", "c", "c5", "d", "e", "f", "g", "h", "hh", "i", "j", "z"} {
			steps = append(steps, sRange("a", lle(k), plus(fgt(k), 1), 0, false, snap, ""))
		}
		for _, sel := range []*rywSel{
			lle("e"), fge("e"), fgt("e"), llt("g"), plus(fge("g"), 1), plus(lle("h"), -1),
			plus(fge("a"), 3), plus(lle("j"), -2), fge("\xff"), llt(""), plus(fgt("c"), 2),
		} {
			steps = append(steps, sGetKey("a", sel, snap, ""))
		}
		for _, r := range []struct {
			b, e    *rywSel
			limit   int
			reverse bool
		}{
			{fge(""), fge("\xff"), 0, false},
			{fge(""), fge("\xff"), 0, true},
			{fge("b"), fge("i"), 3, false},
			{fge("b"), fge("i"), 3, true},
			{fgt("c"), plus(fge("g"), 1), 0, false},
			{lle("e"), fgt("h"), 2, true},
		} {
			steps = append(steps, sRange("a", r.b, r.e, r.limit, r.reverse, snap, ""))
		}
		for _, k := range letters {
			steps = append(steps, sGet("a", k, snap, ""))
		}
		steps = append(steps, sCommit("a"))
		for _, l := range append([]string{"c5"}, letters...) {
			steps = append(steps, sGet("check", l, false, ""))
		}
		add(rywScenario{name: fmt.Sprintf("ryw reads over pending writes/snapshot=%t", snap), steps: steps},
			func(sc *rywScenario) {
				// [lastLessOrEqual(d), firstGreaterThan(d)+1): d, appended twice, then f past
				// the cleared e, holding ByteMax("f", "zz").
				sc.expect("a", "getRange", 5, hkv("d", hx("d+-"), "f", hx("zz")))
				// From the start of the tenant to the first key: the tenant's smoke-test key.
				sc.expect("a", "getRange", 0, hkv("_init", hx("1")))
				sc.expect("a", "getRange", 1, hkv("a", hx("\x00"), "b", hx("c")))
				sc.expect("a", "get", 9, "null") // j: compareAndClear on an equal value
				sc.expect("a", "commit", 0, "ok")
			})
	}

	// Reads issued before later writes see the write map as at issue: the read's RYWIterator
	// is built before its first wait (ReadYourWrites.actor.cpp:382; WriteMap.h:158).
	mKeys := digits("m", 10)
	issueSeed := append([]string(nil), seedKV...)
	for _, m := range mKeys {
		issueSeed = append(issueSeed, m, m)
	}
	for _, snap := range []bool{true, false} {
		steps := cat(seedSteps(issueSeed...), []rywStep{
			sAtomic("a", rywAppendIfFits, "d", "+"),
			sGet("a", "d", snap, "r1"),
			sGet("a", "e", snap, "r2"),
			sExactRange("a", lle("c"), plus(fgt("c"), 1), false, snap, "r3"),
			sExactRange("a", fge("a"), fge("z"), false, snap, "r4"),
			sGetKey("a", fgt("b"), snap, "r5"),
			sExactRange("a", fge("f"), fge("j"), true, snap, "r6"),
			sAtomic("a", rywAppendIfFits, "d", "++"), sSet("a", "e", "late"), sSet("a", "c", "C2"),
			sSet("a", "b2", "new"), sClear("a", "c"), sClearRange("a", "a", "b"), sClearRange("a", "g", "i"),
			sAtomic("a", rywAdd, "f", "\x01"),
			sAwait("a", "r1"), sAwait("a", "r2"), sAwait("a", "r3"), sAwait("a", "r4"), sAwait("a", "r5"), sAwait("a", "r6"),
			sGet("a", "d", snap, ""), sGet("a", "e", snap, ""),
			sRange("a", lle("c"), plus(fgt("c"), 1), 0, false, snap, ""),
			sRange("a", fge("a"), fge("z"), 0, false, snap, ""),
			sGetKey("a", fgt("b"), snap, ""),
		})
		// Gets of keys this transaction has not touched, each followed by a write to its key.
		for _, m := range mKeys {
			steps = append(steps, sGet("a", m, snap, "g"+m))
		}
		for _, m := range mKeys {
			steps = append(steps, sSet("a", m, "after-"+m))
		}
		for _, m := range mKeys {
			steps = append(steps, sAwait("a", "g"+m))
		}
		add(rywScenario{name: fmt.Sprintf("reads issued before later writes/snapshot=%t", snap), steps: steps},
			func(sc *rywScenario) {
				sc.expect("a", "await", 0, hx("d+"))
				sc.expect("a", "await", 1, hx("e"))
				sc.expect("a", "await", 2, hkv("c", hx("c"), "d", hx("d+")))
				oldValues := make(map[string]string)
				var oldKeys []string
				for i := 0; i < len(issueSeed); i += 2 {
					oldKeys = append(oldKeys, issueSeed[i])
					oldValues[issueSeed[i]] = issueSeed[i+1]
				}
				oldValues["d"] = "d+"
				sort.Strings(oldKeys)
				var oldRange []string
				for _, k := range oldKeys {
					oldRange = append(oldRange, k, hx(oldValues[k]))
				}
				sc.expect("a", "await", 3, hkv(oldRange...))
				sc.expect("a", "await", 4, hx("c"))
				sc.expect("a", "await", 5, hkv("i", hx("i"), "h", hx("h"), "g", hx("g"), "f", hx("f")))
				sc.expect("a", "get", 2, hx("d+++"))
				sc.expect("a", "get", 3, hx("late"))
				for i, m := range mKeys {
					sc.expect("a", "await", 6+i, hx(m))
				}
			})
	}

	// (c) Write conflicts: explicit AddWriteConflictKey/Range interleaved with writes and
	// clears, seen through a reader's point reads (each its own read-conflict range).
	readerSteps := func() []rywStep {
		steps := []rywStep{sReport("r"), sGRV("r")}
		for _, k := range digits("k", 9) {
			steps = append(steps, sGet("r", k, false, ""))
		}
		return append(steps, sGet("r", "k3a", false, ""), sSet("r", "zz", "r"))
	}
	add(rywScenario{name: "write conflicts interleaved with writes", steps: cat(seededKeys("k", 9), readerSteps(), []rywStep{
		sWCK("w", "k2"), sSet("w", "k5", "w"), sClear("w", "k7"), sWCR("w", "k8", "k9"),
		sNoWriteConflict("w"), sSet("w", "k4", "x"),
		sNoWriteConflict("w"), sClear("w", "k3"),
		sWCR("w", "k6", "k6"), sWCR("w", "k9x", "k9"),
		sNoWriteConflict("w"), sClearRange("w", "k1", "k1\x00"),
		sCommit("w"), sCommit("r"), sConflicting("r"),
	})}, func(sc *rywScenario) {
		sc.expect("w", "addWriteConflictRange", 2, "err=2005")
		sc.expect("w", "commit", 0, "ok")
		sc.expect("r", "commit", 0, "err=1020")
		sc.expect("r", "conflicting", 0, conflictingRanges("k2", "k2\x00", "k5", "k5\x00", "k7", "k7\x00", "k8", "k8\x00"))
	})
	add(rywScenario{name: "write conflicts coalesce over adjacent writes", steps: cat(seededKeys("k", 9), readerSteps(), []rywStep{
		sWCR("w", "k1", "k3"), sSet("w", "k3", "x"), sWCR("w", "k3\x00", "k3a"), sAtomic("w", rywAdd, "k3a", "\x01"),
		sClearRange("w", "k6", "k7"), sWCK("w", "k7"), sNoWriteConflict("w"), sWCK("w", "k9"),
		sCommit("w"), sCommit("r"), sConflicting("r"),
	})}, func(sc *rywScenario) {
		sc.expect("r", "commit", 0, "err=1020")
		sc.expect("r", "conflicting", 0, conflictingRanges("k1", "k1\x00", "k2", "k2\x00", "k3", "k3\x00",
			"k3a", "k3a\x00", "k6", "k6\x00", "k7", "k7\x00", "k9", "k9\x00"))
	})
	// Disabling RYW after any write is 2000 (ReadYourWrites.actor.cpp:2536-2540), deferred by set_option
	// (ThreadSafeTransaction.cpp:730-743); earlier read conflicts are not shipped (RYW:1361-1399).
	add(rywScenario{name: "disable RYW after conflict-only write", steps: cat(seededKeys("k", 3), []rywStep{
		sWCK("a", "k1"), sOpt("a", "readYourWritesDisable"), sGet("a", "k2", false, ""),
		sRCR("c", "k1", "k2"), sOpt("c", "readYourWritesDisable"), sGRV("c"), sSet("c", "zz", "c"),
		sSet("b", "k1", "w"), sCommit("b"), sCommit("c"), sCommit("a"),
	})}, func(sc *rywScenario) {
		sc.expect("a", "get", 0, "err=2000")
		sc.expect("a", "commit", 0, "err=2000")
		sc.expect("c", "commit", 0, "ok")
	})

	// (d) AppendIfFits over VALUE_SIZE_LIMIT keeps the value (Atomic.h:114-125); pending appends coalesce into
	// one operand (WriteMap.cpp:385-391); an operand over the limit is a deferred 2103 (RYW:2240-2241).
	big := func(c byte, n int) string { return string(bytes.Repeat([]byte{c}, n)) }
	add(rywScenario{name: "append if fits sequences", steps: cat(seedSteps("p1", big('a', 60000)), []rywStep{
		sAtomic("a", rywAppendIfFits, "p1", big('b', 30000)),
		sAtomic("a", rywAppendIfFits, "p1", big('c', 20000)),
		sGet("a", "p1", false, ""),
		sAtomic("a", rywAppendIfFits, "p1", big('d', 10000)),
		sGet("a", "p1", true, ""),
		sAtomic("a", rywAppendIfFits, "p2", big('e', 100000)),
		sGet("a", "p2", false, ""),
		sAtomic("a", rywAppendIfFits, "p2", "f"),
		sAtomic("a", rywAppendIfFits, "p3", "x"), sAtomic("a", rywAppendIfFits, "p3", "y"), sAtomic("a", rywAppendIfFits, "p3", "z"),
		sSet("a", "p4", "s"), sAtomic("a", rywAppendIfFits, "p4", "t"), sClear("a", "p4"), sAtomic("a", rywAppendIfFits, "p4", "u"),
		sRange("a", fge("p"), fge("q"), 0, false, false, ""),
		sCommit("a"),
		sGet("check", "p1", false, ""), sGet("check", "p2", false, ""), sGet("check", "p3", false, ""), sGet("check", "p4", false, ""),
		sAtomic("o", rywAppendIfFits, "q", big('g', 100001)), sGet("o", "q", false, ""), sCommit("o"),
		sAtomic("o2", rywAppendIfFits, "p1", ""), sGet("o2", "p1", false, ""), sCommit("o2"),
	})}, func(sc *rywScenario) {
		sc.expect("a", "get", 0, rywValue([]byte(big('a', 60000))))
		sc.expect("a", "get", 1, rywValue([]byte(big('a', 60000))))
		sc.expect("a", "get", 2, rywValue([]byte(big('e', 100000))))
		sc.expect("a", "commit", 0, "ok")
		sc.expect("check", "get", 0, rywValue([]byte(big('a', 60000))))
		sc.expect("o", "get", 0, "err=2103")
		sc.expect("o", "commit", 0, "err=2103")
		sc.expect("check", "get", 1, rywValue([]byte(big('e', 100000))))
		sc.expect("check", "get", 2, hx("xyz"))
		sc.expect("check", "get", 3, hx("u"))
	})
	// An un-awaited read is in RYW's `reading` (ReadYourWrites.actor.cpp:1690-1691): commit waits
	// for it (:1359-1360), its future still delivers, and its failure (rv=1: 1007) fails the commit.
	add(rywScenario{name: "commit waits for un-awaited reads", steps: cat(seededKeys("k", 3), []rywStep{
		sGet("a", "k1", true, "ra"), sSet("a", "zz", "a"), sCommit("a"), sAwait("a", "ra"),
		sAtomic("b", rywAdd, "k2", "\x01"), sGet("b", "k2", false, "rb"), sSet("b", "zz", "b"), sCommit("b"), sAwait("b", "rb"),
		// Read version 1 is long past the MVCC window: the read fails, and so must the read-only commit.
		sRV("c", 1), sGet("c", "k3", true, "rc"), sCommit("c"), sAwait("c", "rc"),
		sRV("d", 1), sAtomic("d", rywAdd, "k3", "\x01"), sGet("d", "k3", false, "rd"), sCommit("d"), sAwait("d", "rd"),
		// No future-version (1009) shape: this target raises FUTURE_VERSION_DELAY to hold reads open.
	})}, func(sc *rywScenario) {
		sc.expect("a", "commit", 0, "ok")
		sc.expect("a", "await", 0, hx("vk1"))
		sc.expect("b", "commit", 0, "ok")
		sc.expect("b", "await", 0, hx("w"))
		sc.expect("c", "commit", 0, "err=1007")
		sc.expect("d", "commit", 0, "err=1007")
	})
	// Reset, Cancel and OnError with un-awaited reads that storage holds open (a version far ahead,
	// FUTURE_VERSION_DELAY raised by this target's knob), then the transaction's next read.
	for _, end := range []string{"reset", "cancel", "onError"} {
		for _, atomicKey := range []bool{false, true} {
			tx := []rywStep{sRV("a", -1_000_000_000_000)}
			if atomicKey {
				tx = append(tx, sAtomic("a", rywAdd, "k1", "\x01"), sGet("a", "k1", false, "r1"))
			} else {
				tx = append(tx, sGet("a", "k1", true, "r1"))
			}
			tx = append(tx, sGet("a", "k2", false, "r2"), sSet("a", "zz", "a"))
			if !atomicKey { // a stall past the default 1 s FUTURE_VERSION_DELAY must not change the outcome
				tx = append(tx, sSleep("a", 1500))
			}
			switch end {
			case "reset":
				tx = append(tx, sReset("a"))
			case "cancel":
				tx = append(tx, sCancel("a"))
			case "onError":
				tx = append(tx, sOnError("a", 1020))
			}
			tx = append(tx, sAwait("a", "r1"), sAwait("a", "r2"), sGet("a", "k1", false, ""), sGet("a", "zz", false, ""), sCommit("a"))
			e := end
			add(rywScenario{name: fmt.Sprintf("un-awaited reads across %s/atomic=%t", end, atomicKey), steps: cat(seededKeys("k", 2), tx)},
				func(sc *rywScenario) {
					// Reset and cancel fail reads in flight with transaction_cancelled (ReadYourWrites.actor.cpp:2726-2733).
					sc.expect("a", "await", 0, "err=1025")
					sc.expect("a", "await", 1, "err=1025")
					if e == "cancel" {
						sc.expect("a", "commit", 0, "err=1025")
						return
					}
					sc.expect("a", "get", 2, hx("vk1"))
					sc.expect("a", "get", 3, "null")
					sc.expect("a", "commit", 0, "ok")
				})
		}
	}

	// Not compared yet: an option error such as SetAccessSystemKeys on a tenant transaction,
	// which libfdb_c defers (TODO.md "Option errors are deferred in libfdb_c").

	// Explicit AddWriteConflictKey/Range do not consume NEXT_WRITE_NO_WRITE_CONFLICT_RANGE: only
	// writes read and clear it (ReadYourWrites.actor.cpp:2086-2087, 2461-2500).
	add(rywScenario{name: "explicit write conflicts keep the next-write option armed", steps: cat(seededKeys("k", 9), readerSteps(), []rywStep{
		sNoWriteConflict("w"), sWCK("w", "k1"), sSet("w", "k2", "w"),
		sNoWriteConflict("w"), sWCR("w", "k3", "k3\x00"), sClear("w", "k4"),
		sCommit("w"), sCommit("r"), sConflicting("r"),
	})}, func(sc *rywScenario) {
		sc.expect("w", "commit", 0, "ok")
		sc.expect("r", "commit", 0, "err=1020")
		sc.expect("r", "conflicting", 0, conflictingRanges("k1", "k1\x00", "k3", "k3\x00"))
	})

	// Illegal writes are the deferred error for later reads and commit (ThreadSafeTransaction.cpp:626-640);
	// clear checks the legal range before its empty return (ReadYourWrites.actor.cpp:2370-2372, 2396).
	sysKey := "\xff\x01" + big('a', 35000)
	for _, ryw := range []bool{true, false} {
		var steps []rywStep
		var names []string
		shape := func(name string, setup []rywStep, write rywStep) {
			tx := fmt.Sprintf("t%d", len(names))
			names = append(names, name)
			if !ryw {
				steps = append(steps, sOpt(tx, "readYourWritesDisable"))
			}
			for _, s := range setup {
				s.Tx = tx
				steps = append(steps, s)
			}
			write.Tx = tx
			steps = append(steps, write, sGet(tx, "k1", false, ""), sRange(tx, fge("k"), fge("l"), 0, false, false, ""), sCommit(tx))
		}
		shape("empty illegal", nil, sClearRange("", "\xff\x01", "\xff\x01"))
		shape("illegal clamping to empty", nil, sClearRange("", sysKey+"a", sysKey+"b"))
		shape("inverted", nil, sClearRange("", "b", "a"))
		shape("empty", nil, sClearRange("", "a", "a"))
		shape("legal clamping to empty", nil, sClearRange("", "k"+big('a', 20000)+"1", "k"+big('a', 20000)+"2"))
		shape("clear past \\xff", nil, sClear("", "\xff\x05"))
		shape("clear range end past \\xff", nil, sClearRange("", "a", "\xff\x01"))
		shape("clear oversized key", nil, sClear("", "k"+big('a', 20000)))
		shape("set oversized key", nil, sSet("", "k"+big('a', 20000), "v"))
		shape("set past \\xff", nil, sSet("", "\xff\x05", "v"))
		shape("set metadataVersion", nil, sSet("", "\xff/metadataVersion", "v"))
		shape("set value too large", nil, sSet("", "k5", big('v', 100001)))
		shape("clear range oversized legal bounds", nil, sClearRange("", "k1x", "k"+big('z', 20000)))
		n := names
		add(rywScenario{name: fmt.Sprintf("illegal writes are deferred/ryw=%t", ryw), steps: cat(seededKeys("k", 2), steps)},
			func(sc *rywScenario) {
				pin := func(name, code string) {
					for i, x := range n {
						if x == name {
							tx := fmt.Sprintf("t%d", i)
							sc.expect(tx, "get", 0, code)
							sc.expect(tx, "commit", 0, code)
						}
					}
				}
				pin("empty illegal", "err=2004")
				pin("illegal clamping to empty", "err=2004")
				pin("inverted", "err=2005")
				pin("clear past \\xff", "err=2004")
				pin("clear range end past \\xff", "err=2004")
				pin("set oversized key", "err=2102")
				pin("set value too large", "err=2103")
			})
	}
	return scs
}

var _ = Describe("RYW and conflict-range conformance against libfdb_c", Label("ryw-script"), func() {
	It("matches libfdb_c transcripts for every scenario", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		java := rywScriptInvoker()
		type population struct{ Scenarios, Lines, Pinned, Conflicts, ConflictingReports, Reads int }
		var pop population
		var failures []string
		for _, sc := range rywScenarios() {
			By(sc.name)
			script, err := json.Marshal(sc.steps)
			Expect(err).NotTo(HaveOccurred())
			goEnv, err := SetupJavaTenant(ctx, sharedContainer, "ryw_go_"+uuid.NewString())
			Expect(err).NotTo(HaveOccurred())
			javaEnv, err := SetupJavaTenant(ctx, sharedContainer, "ryw_java_"+uuid.NewString())
			Expect(err).NotTo(HaveOccurred())
			tenant, err := sharedDB.OpenTenant(gofdb.Key(goEnv.TenantName))
			Expect(err).NotTo(HaveOccurred())
			goOut, err := runGoRYWScriptBounded(tenant, sc.steps, time.Minute)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: Go: %v", sc.name, err))
			}
			var javaOut []string
			javaCtx, javaCancel := context.WithTimeout(ctx, 2*time.Minute)
			Expect(java.InvokeAs(javaCtx, "rywScript", map[string]any{
				"clusterFile": javaEnv.ClusterFile,
				"tenantName":  javaEnv.TenantName,
				"script":      string(script),
			}, &javaOut)).To(Succeed(), sc.name)
			javaCancel()
			Expect(goEnv.Cleanup(ctx)).To(Succeed())
			Expect(javaEnv.Cleanup(ctx)).To(Succeed())

			fmt.Fprintf(GinkgoWriter, "== %s (libfdb_c)\n%s\n", sc.name, strings.Join(javaOut, "\n"))
			if diff := rywTranscriptDiff(goOut, javaOut); diff != "" {
				failures = append(failures, fmt.Sprintf("%s: Go versus libfdb_c:\n%s", sc.name, diff))
			}
			have := map[string]bool{}
			for _, l := range javaOut {
				have[l] = true
			}
			for _, w := range sc.want {
				if !have[w] {
					failures = append(failures, fmt.Sprintf("%s: libfdb_c transcript lacks pinned line %q", sc.name, w))
				}
			}
			pop.Scenarios++
			pop.Lines += len(javaOut)
			pop.Pinned += len(sc.want)
			for _, l := range javaOut {
				f := strings.Fields(l)
				switch {
				case f[2] == "commit" && f[3] == "err=1020":
					pop.Conflicts++
				case f[2] == "conflicting" && f[3] != "[]":
					pop.ConflictingReports++
				case f[2] == "get" || f[2] == "getKey" || f[2] == "getRange" || f[2] == "await":
					pop.Reads++
				}
			}
		}
		fmt.Fprintf(GinkgoWriter, "ryw script conformance: %+v\n", pop)
		AddReportEntry("ryw-script-population", pop)
		Expect(failures).To(BeEmpty())
		Expect(pop.Scenarios).To(Equal(len(rywScenarios())))
		Expect(pop.Conflicts).To(BeNumerically(">=", 10))
		Expect(pop.ConflictingReports).To(BeNumerically(">=", 10))
		Expect(pop.Reads).To(BeNumerically(">=", 150))
	})
})
