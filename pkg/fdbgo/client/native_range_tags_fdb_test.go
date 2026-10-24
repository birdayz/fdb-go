package client

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire/types"
)

// These are the RFC-262 callers of the shared RPC sender, not the old exact
// range loop already covered by TestReadTagSamplingOneDrawPerRequest.
func TestNativeReadTagSampling(t *testing.T) {
	t.Parallel()
	// Seed before scheduling the parallel cases. Their deadlines start only
	// after each child's t.Parallel returns, not while it waits to run.
	seedCtx, cancelSeed := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancelSeed)
	prefix := t.Name() + "/"
	seed := newTestDatabase(t, seedCtx, sharedClusterFile, plainDial)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		setKey(t, seedCtx, seed, []byte(prefix+k), []byte("v"))
		setKey(t, seedCtx, seed, []byte(prefix+"large/"+k), bytes.Repeat([]byte("v"), 40000))
	}
	seed.Close()
	cancelSeed()

	setup := func(t *testing.T, ctx context.Context) (*Transaction, *sendRecorder, *simDialer, *readTagDraws, int64, *sync.Map) {
		t.Helper()
		sim, draws, aliases := newSimDialer(), &readTagDraws{}, &sync.Map{}
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			if real, ok := aliases.Load(addr); ok {
				addr = real.(string)
			}
			return sim.dial(ctx, network, addr)
		}
		db, rec := openReadTagDB(t, ctx, sharedClusterFile, dial, 0.5, draws.draw)
		tx := db.CreateTransaction()
		t.Cleanup(tx.Cancel)
		if err := tx.SetAutoThrottleTag("rt"); err != nil {
			t.Fatal(err)
		}
		rv, err := tx.GetReadVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.db.locCache.locateRange(tx.db, ctx, []byte(prefix), []byte(prefix+"\xff"), 100, false, NoTenantID, types.SpanContext{}); err != nil {
			t.Fatal(err)
		}
		return tx, rec, sim, draws, rv, aliases
	}
	tagged := taggedRequest{kind: "GetKeyValuesRequest", has: true, tags: string(encodeTagSet([]string{"rt"}))}
	for _, route := range []string{"native-range", "ryw-getKey"} {
		for _, fault := range []string{"hedge", "alternative", "reply-timeout", "maybe-delivered", "wrong-shard", "all-alternatives-failed"} {
			t.Run(route+"/"+fault, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				t.Cleanup(cancel)
				tx, rec, sim, draws, _, aliases := setup(t, ctx)
				tx.rpcTimeoutOverride = 300 * time.Millisecond
				var reply []byte
				var alternative ServerInfo
				wantDraws := 1
				second := tagged
				switch fault {
				case "hedge":
					duplicateServers(&Database{db: tx.db}, 2)
				case "alternative":
					loc, err := tx.db.locCache.locate(tx.db, ctx, []byte(prefix+"a"), NoTenantID, types.SpanContext{}, false)
					if err != nil || len(loc.Servers) == 0 {
						t.Fatalf("alternative precondition: location %+v, %v; want a real server", loc, err)
					}
					real := loc.Servers[0]
					alias := "rfc262-alternative.invalid:1"
					aliases.Store(alias, real.Address)
					alternative = deadEndpoint(alias)
					servers := []ServerInfo{alternative, real}
					setServers(&Database{db: tx.db}, servers)
					q := tx.db.queueModel
					delta := q.startRequest(real.Address)
					defer q.endRequest(real.Address, delta, 0, false)
					q.mu.Lock()
					q.getOrCreate(real.Address).smoothOutstanding.estimate = 1
					q.getOrCreate(alias).latency = time.Hour.Seconds() // no timer hedge within the test's lifetime
					q.mu.Unlock()
					if best, next := q.chooseTopTwo(servers); best != 0 || next != 1 {
						t.Fatalf("alternative selection = %d,%d; want dead primary then real", best, next)
					}
				case "maybe-delivered":
					reply = inlineErrorReply(uint16(ErrRequestMaybeDelivered), injectPenalty)
				case "wrong-shard":
					reply = inlineErrorReply(uint16(ErrWrongShardServer), injectPenalty)
					wantDraws, second = 2, taggedRequest{kind: "GetKeyValuesRequest"}
				case "all-alternatives-failed":
					reply = inlineErrorReply(uint16(ErrAllAlternativesFailed), injectPenalty)
					wantDraws, second = 2, taggedRequest{kind: "GetKeyValuesRequest"}
				}
				plan := &replyPlan{rec: rec}
				if fault != "alternative" {
					plan.entries = []plannedReply{replaceReply("GetKeyValuesRequest", reply)}
				}
				sim.setIntercept(plan.intercept)
				sim.armEverything()
				draws.script(drawHit, drawMiss)
				rec.start()
				if route == "native-range" {
					r, err := tx.nativeGetRange(ctx, firstGreaterOrEqual([]byte(prefix+"a")), firstGreaterOrEqual([]byte(prefix+"f")), newRangeLimits(0, -1), true, false)
					if err != nil || len(r.kvs) != 5 {
						t.Fatalf("native range = %d rows, %v; want 5", len(r.kvs), err)
					}
				} else {
					k, err := tx.StartGetKey(ctx, KeySelector{Key: []byte(prefix + "a"), Offset: 3}).Get()
					if err != nil || string(k) != prefix+"c" {
						t.Fatalf("walk getKey = %q, %v; want c", k, err)
					}
				}
				got := taggedRequests(t, rec.stop())
				plan.check(t)
				if alternative.Address != "" && !tx.db.failMon.endpointFailed(alternative.Address, getAdjustedEndpoint(alternative.Token, EndpointGetKeyValues)) {
					t.Fatal("alternative case did not observe ENDPOINT_NOT_FOUND")
				}
				want := []taggedRequest{tagged, second}
				if fmt.Sprint(got) != fmt.Sprint(want) || draws.count() != wantDraws {
					t.Fatalf("requests %v after %d draws, want %v after %d", got, draws.count(), want, wantDraws)
				}
			})
		}
	}

	for _, route := range []string{"native-progress", "walk-storage-progress"} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			t.Cleanup(cancel)
			tx, rec, _, draws, _, _ := setup(t, ctx)
			tx.db.hedgeEnabled.Store(false)
			draws.script(alternatingDraws(100)...)
			rec.start()
			if route == "native-progress" {
				r, err := tx.nativeGetRange(ctx, firstGreaterOrEqual([]byte(prefix+"large/")), firstGreaterOrEqual([]byte(prefix+"large/\xff")), newRangeLimits(0, -1), true, false)
				if err != nil || len(r.kvs) != 5 {
					t.Fatalf("native continuation = %d rows, %v; want 5", len(r.kvs), err)
				}
			} else {
				tx.Clear([]byte(prefix + "b")) // request 1 returns a cleared row; the walk needs storage again
				k, err := tx.StartGetKey(ctx, KeySelector{Key: []byte(prefix + "a"), Offset: 4}).Get()
				if err != nil || string(k) != prefix+"e" {
					t.Fatalf("walk continuation = %q, %v; want e", k, err)
				}
			}
			got := taggedRequests(t, rec.stop())
			for i := range got {
				if got[i].kind != "GetKeyValuesRequest" {
					t.Fatalf("unexpected progress request %v", got[i])
				}
				got[i].kind = ""
			}
			checkNewRequests(t, got, draws.count(), 2, taggedRequest{has: true, tags: tagged.tags})
		})
	}

	for _, hedge := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback-two-nontrivial-selectors/hedge=%t", hedge), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			t.Cleanup(cancel)
			tx, rec, sim, draws, rv, _ := setup(t, ctx)
			tx.db.hedgeEnabled.Store(hedge)
			plan := &replyPlan{rec: rec}
			if hedge {
				// One planned hedge for a selector and one for the exact read.
				duplicateServers(&Database{db: tx.db}, 2)
				plan.entries = []plannedReply{dropReply("GetKeyRequest"), dropReply("GetKeyValuesRequest")}
				sim.setIntercept(plan.intercept)
				sim.armEverything()
			}
			draws.script(drawHit, drawMiss, drawHit)
			rec.start()
			r, err := tx.nativeGetRangeFallback(ctx, KeySelector{Key: []byte(prefix + "a"), Offset: 2}, KeySelector{Key: []byte(prefix + "d"), Offset: 2}, newRangeLimits(0, -1), rv, false)
			if err != nil || len(r.kvs) != 3 || string(r.kvs[0].Key) != prefix+"b" || string(r.kvs[2].Key) != prefix+"d" {
				t.Fatalf("resolved fallback = %+v, %v; want b,c,d", r.kvs, err)
			}
			recorded := rec.stop()
			got := taggedRequests(t, recorded)
			plan.check(t)
			// A hedge resends its request with its tags (LoadBalance.actor.h:660-662). Go also
			// hedges a slow reply that C++'s second-request budget would not (TODO.md).
			sends := map[string]int{}
			for _, r := range got {
				sends[r.kind]++
			}
			selectors, exact := sends["GetKeyRequest"], sends["GetKeyValuesRequest"]
			// Each request goes to at most two replicas; the planned drops force one hedge
			// of the exact read and of a selector, and the other selector may hedge too.
			minSelectors, maxSelectors, wantExact := 2, 2, 1
			if hedge {
				minSelectors, maxSelectors, wantExact = 3, 4, 2
			}
			if draws.count() != 3 || len(got) != selectors+exact || selectors < minSelectors || selectors > maxSelectors || exact != wantExact {
				t.Fatalf("fallback requests %v: %d draws, %d selector and %d exact sends; want 3 draws, %d-%d selector and %d exact sends",
					got, draws.count(), selectors, exact, minSelectors, maxSelectors, wantExact)
			}
			for _, msg := range recorded.msgs {
				switch q := msg.(type) {
				case *types.GetKeyRequest:
					want := string(q.Sel.Key) == prefix+"a"
					if string(q.Sel.Key) != prefix+"a" && string(q.Sel.Key) != prefix+"d" {
						t.Fatalf("unexpected fallback selector %+v", q.Sel)
					}
					if q.HasTags != want || (want && string(q.Tags) != tagged.tags) || (!want && len(q.Tags) != 0) {
						t.Errorf("fallback selector %q HasTags=%t Tags=%q, want sampled=%t", q.Sel.Key, q.HasTags, q.Tags, want)
					}
				case *types.GetKeyValuesRequest:
					if !q.HasTags || string(q.Tags) != tagged.tags {
						t.Errorf("fallback exact request HasTags=%t Tags=%q, want new third sample", q.HasTags, q.Tags)
					}
				}
			}
		})
	}
}
