package client

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// Exclusive: allocation counters and the shared timer pool are process-wide.
// Warming with GC disabled measures reuse, not the cost of cold pool misses.
func TestHedgeTimerAllocations(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const runs = 1000
	body := []byte("nonempty reply")
	ch := make(chan transport.Response, 1)
	makeRPC := func(addr string, ready bool) inFlightRPC {
		var reply <-chan transport.Response
		if ready {
			ch <- transport.Response{Body: body}
			reply = ch
		}
		return inFlightRPC{replyCh: reply, replyHandle: &transport.ReplyHandle{}, addr: addr, delta: 1}
	}
	qm := newQueueModel() // accrual on every lone reply must not allocate
	for _, shape := range []string{"single", "hedge", "race"} {
		t.Run(shape, func(t *testing.T) {
			measure := func(control bool) (float64, int) {
				calls := 0
				allocs := testing.AllocsPerRun(runs, func() {
					p := makeRPC("primary", true)
					var result hedgeResult
					if shape == "race" {
						s := makeRPC("secondary", false)
						if control {
							owner := replyGuard{handle: p.replyHandle}
							result, _ = readyReplyOwned(p, &owner)
							s.replyHandle.Cancel()
							s.replyHandle.Release()
							result.others = []rpcAccount{accountOf(s)}
						} else {
							result = raceReplies(context.Background(), p, s, time.Hour)
						}
					} else if control {
						owner := replyGuard{handle: p.replyHandle}
						result, _ = readyReplyOwned(p, &owner)
					} else {
						var secondary sendFunc
						if shape == "hedge" {
							secondary = func() inFlightRPC { panic("ready primary must win before an hour") }
						}
						result = sendFrameWithHedge(context.Background(), qm, time.Hour, func() inFlightRPC { return p }, secondary, time.Hour)
					}
					if result.err != nil || result.addr != "primary" || string(result.body) != string(body) || len(ch) != 0 {
						t.Fatalf("nonempty reply lost: %+v", result)
					}
					if shape == "race" && (len(result.others) != 1 || result.others[0].addr != "secondary") {
						t.Fatalf("loser accounting lost: %+v", result)
					}
					calls++
				})
				return allocs, calls
			}
			control, controls := measure(true)
			actual, calls := measure(false)
			wantControl := float64(1) // A fresh reply handle; no fake handle recycling.
			if shape == "race" {
				wantControl = 3 // Two handles and the retained loser-account slice.
			}
			t.Logf("HEDGE_TIMER_ALLOC %s actual=%g control=%g calls=%d/%d", shape, actual, control, calls, controls)
			if control != wantControl || calls != runs+1 || controls != runs+1 {
				t.Fatalf("invalid control/population: alloc=%g, calls=%d/%d", control, calls, controls)
			}
			if actual != control {
				t.Errorf("HEDGE_TIMER_ALLOC %s: %g extra allocations, want warmed timer reuse without extra allocations", shape, actual-control)
			}
		})
	}
	cold := testing.AllocsPerRun(runs, func() { timer := time.NewTimer(time.Hour); timer.Stop() })
	warm := testing.AllocsPerRun(runs, func() { timer := getTimer(time.Hour); putTimer(timer) })
	if cold != 3 || warm != 0 {
		t.Fatalf("timer controls: fresh=%g warmed=%g, want 3/0 (cold pool misses still allocate)", cold, warm)
	}
}

// Each bubble owns its timers. Never let a pooled timer cross fake-clock domains.
// Instrument only the existing pool constructor; production get/put is unchanged.
func hedgeTimerBubble(t *testing.T, f func(*testing.T, *[]*time.Timer)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		var created []*time.Timer
		timerPool = sync.Pool{New: func() any {
			timer := time.NewTimer(0)
			created = append(created, timer)
			return timer
		}}
		defer func() {
			for _, timer := range created {
				timer.Stop()
			}
			timerPool = sync.Pool{New: func() any { return time.NewTimer(0) }}
		}()
		f(t, &created)
	})
}

func TestReplyTimerReuseStates(t *testing.T) {
	for _, state := range []string{"active", "fired-unread", "fired-consumed", "stopped"} {
		t.Run(state, func(t *testing.T) {
			hedgeTimerBubble(t, func(t *testing.T, created *[]*time.Timer) {
				timer := getTimer(5 * time.Second)
				if len(*created) != 1 {
					t.Fatal("cold checkout did not construct a timer")
				}
				switch state {
				case "fired-unread":
					time.Sleep(5 * time.Second) // Fake time; leave the expiry unread.
				case "fired-consumed":
					<-timer.C
				case "stopped":
					timer.Stop()
				}
				putTimer(timer)
				next := getTimer(7 * time.Second)
				time.Sleep(7*time.Second - time.Nanosecond)
				synctest.Wait()
				select {
				case <-next.C:
					t.Fatal("reused timer delivered its old expiry or an early reset expiry")
				default:
				}
				time.Sleep(time.Nanosecond)
				select {
				case <-next.C:
				default:
					t.Fatal("reset timer did not expire at the requested duration")
				}
				putTimer(next)
			})
		})
	}
}

func TestHedgeTimerStagesAndAccounting(t *testing.T) {
	replyFailure := &wire.FDBError{Code: 1037} // Canonical process_behind: try secondary.
	terminalFailure := &wire.FDBError{Code: 1007}
	sendFailure := errors.New("send failed")
	// Lone replies earn 0.05; a paid hedge earns nothing (LoadBalance.actor.h:694-716).
	// Pin the model alongside timer stages and balanced request accounting.
	for _, tc := range []struct {
		name                          string
		mode                          string
		cancelOnSecondary             bool
		pReady, sReady, cancel        bool
		pSendErr, sSendErr, pReplyErr error
		pDelay, sDelay, hedgeDelay    time.Duration
		pReplyAfter                   time.Duration // a primary reply this long after its send
		budget                        float64
		wantAddr                      string
		wantErr                       error
		wantElapsed, wantSecondaryAt  time.Duration
		wantStarted, accrued          int
		spent                         bool
	}{
		{name: "single-ready", mode: "single", pReady: true, wantAddr: "p", wantStarted: 1, accrued: 1},
		{name: "single-transport-failure", mode: "single", pReady: true, pReplyErr: sendFailure, wantAddr: "p", wantErr: sendFailure, wantStarted: 1, accrued: 1},
		{name: "cancelled-single-send-failure", mode: "single", cancel: true, pSendErr: context.Canceled, wantErr: context.Canceled},
		{name: "single-timeout-after-send", mode: "single", pDelay: 5 * time.Second, wantAddr: "p", wantErr: errReplyTimeout, wantElapsed: 12 * time.Second, wantStarted: 1},
		{name: "single-cancel", mode: "single", cancel: true, wantAddr: "p", wantErr: context.Canceled, wantStarted: 1},
		{name: "single-ready-cancel", mode: "single", cancel: true, pReady: true, wantAddr: "p", wantStarted: 1, accrued: 1},
		{name: "single-ready-with-budget", mode: "single", pReady: true, budget: 99.98, wantAddr: "p", wantStarted: 1, accrued: 1},
		{name: "race-primary", mode: "race", pReady: true, wantAddr: "p", wantStarted: 2},
		{name: "race-secondary", mode: "race", sReady: true, wantAddr: "s", wantStarted: 2},
		{name: "race-timeout", mode: "race", wantErr: errReplyTimeout, wantElapsed: 7 * time.Second, wantStarted: 2},
		{name: "race-cancel", mode: "race", cancel: true, wantErr: context.Canceled, wantStarted: 2},
		{name: "race-ready-primary-cancel", mode: "race", cancel: true, pReady: true, wantAddr: "p", wantStarted: 2},
		{name: "race-ready-secondary-cancel", mode: "race", cancel: true, sReady: true, wantAddr: "s", wantStarted: 2},
		{name: "hedge-primary", mode: "hedge", pReady: true, hedgeDelay: time.Hour, wantAddr: "p", wantStarted: 1, accrued: 1},
		{name: "hedge-cancel", mode: "hedge", cancel: true, hedgeDelay: time.Hour, wantAddr: "p", wantErr: context.Canceled, wantStarted: 1},
		{name: "hedge-ready-cancel", mode: "hedge", cancel: true, pReady: true, hedgeDelay: time.Hour, wantAddr: "p", wantStarted: 1, accrued: 1},
		{name: "hedge-terminal-reply", mode: "hedge", pReady: true, pReplyErr: terminalFailure, hedgeDelay: time.Hour, wantAddr: "p", wantErr: terminalFailure, wantStarted: 1, accrued: 1},
		{name: "hedge-retryable-reply-fallback", mode: "hedge", pReady: true, pReplyErr: replyFailure, sReady: true, hedgeDelay: time.Hour, wantAddr: "s", wantStarted: 2, accrued: 2},
		{name: "hedge-retryable-reply-send-failure", mode: "hedge", pReady: true, pReplyErr: replyFailure, sSendErr: sendFailure, hedgeDelay: time.Hour, wantAddr: "p", wantErr: replyFailure, wantStarted: 1, accrued: 2},
		{name: "hedge-retryable-reply-wait-timeout", mode: "hedge", pReady: true, pReplyErr: replyFailure, pDelay: 5 * time.Second, sDelay: 4 * time.Second, hedgeDelay: time.Hour, wantAddr: "s", wantErr: errReplyTimeout, wantElapsed: 16 * time.Second, wantSecondaryAt: 5 * time.Second, wantStarted: 2, accrued: 1},
		{name: "primary-send-failure-fallback", mode: "hedge", pSendErr: sendFailure, sReady: true, wantAddr: "s", wantStarted: 1, accrued: 2},
		{name: "primary-send-failure-wait-timeout", mode: "hedge", pSendErr: sendFailure, pDelay: 5 * time.Second, sDelay: 4 * time.Second, wantAddr: "s", wantErr: errReplyTimeout, wantElapsed: 16 * time.Second, wantSecondaryAt: 5 * time.Second, wantStarted: 1, accrued: 1},
		{name: "both-send-fail", mode: "hedge", pSendErr: sendFailure, sSendErr: sendFailure, wantErr: sendFailure, accrued: 2},
		{name: "single-send-fail", mode: "single", pSendErr: sendFailure, wantErr: sendFailure, accrued: 1},
		{name: "hedge-secondary-after-both-sends", mode: "hedge", sReady: true, pDelay: 5 * time.Second, sDelay: 4 * time.Second, hedgeDelay: 3 * time.Second, budget: 1, wantAddr: "s", wantElapsed: 12 * time.Second, wantSecondaryAt: 8 * time.Second, wantStarted: 2, spent: true},
		{name: "hedge-race-timeout-after-both-sends", mode: "hedge", pDelay: 5 * time.Second, sDelay: 4 * time.Second, hedgeDelay: 3 * time.Second, budget: 1, wantErr: errReplyTimeout, wantElapsed: 19 * time.Second, wantSecondaryAt: 8 * time.Second, wantStarted: 2, spent: true},
		{name: "hedge-secondary-send-failure-wait-timeout", mode: "hedge", sSendErr: sendFailure, pDelay: 5 * time.Second, sDelay: 4 * time.Second, hedgeDelay: 3 * time.Second, budget: 1, wantAddr: "p", wantErr: errReplyTimeout, wantElapsed: 19 * time.Second, wantSecondaryAt: 8 * time.Second, wantStarted: 1, spent: true},
		// A paid hedge that cannot be sent leaves the primary waited for alone,
		// without accruing (the !stream branch, LoadBalance.actor.h:640-647).
		{name: "hedge-secondary-send-failure-primary-reply", mode: "hedge", sSendErr: sendFailure, pReplyAfter: 5 * time.Second, hedgeDelay: 3 * time.Second, budget: 1, wantAddr: "p", wantElapsed: 5 * time.Second, wantSecondaryAt: 3 * time.Second, wantStarted: 1, spent: true},
		// The delay fires without budget: no hedge, and the reply then accrues.
		{name: "hedge-no-budget-primary-after-delay", mode: "hedge", pReplyAfter: 5 * time.Second, hedgeDelay: 3 * time.Second, budget: 0.95, wantAddr: "p", wantElapsed: 5 * time.Second, wantStarted: 1, accrued: 1},
		{name: "hedge-no-budget-transport-failure", mode: "hedge", pReplyAfter: 5 * time.Second, pReplyErr: sendFailure, sReady: true, hedgeDelay: 3 * time.Second, budget: 0.95, wantAddr: "s", wantElapsed: 5 * time.Second, wantSecondaryAt: 5 * time.Second, wantStarted: 2, accrued: 2},
		{name: "hedge-no-budget-transport-failure-secondary-send-fails", mode: "hedge", pReplyAfter: 5 * time.Second, pReplyErr: sendFailure, sSendErr: sendFailure, hedgeDelay: 3 * time.Second, budget: 0.95, wantAddr: "p", wantErr: sendFailure, wantElapsed: 5 * time.Second, wantSecondaryAt: 5 * time.Second, wantStarted: 1, accrued: 2},
		{name: "hedge-no-budget-timeout", mode: "hedge", hedgeDelay: 3 * time.Second, budget: 0.95, wantAddr: "p", wantErr: errReplyTimeout, wantElapsed: 10 * time.Second, wantStarted: 1},
		{name: "hedge-no-budget-cancel", mode: "hedge", cancel: true, hedgeDelay: 0, budget: 0.95, wantAddr: "p", wantErr: context.Canceled, wantStarted: 1},
		// The delay fires with budget: the hedge is paid for, and race replies
		// earn nothing; a second unit of budget buys no second hedge.
		{name: "hedge-budget-primary-wins-race", mode: "hedge", pReplyAfter: 5 * time.Second, hedgeDelay: 3 * time.Second, budget: 1, wantAddr: "p", wantElapsed: 5 * time.Second, wantSecondaryAt: 3 * time.Second, wantStarted: 2, spent: true},
		{name: "hedge-budget-two-spends-once", mode: "hedge", hedgeDelay: 3 * time.Second, budget: 2, wantErr: errReplyTimeout, wantElapsed: 10 * time.Second, wantSecondaryAt: 3 * time.Second, wantStarted: 2, spent: true},
		{name: "hedge-funded-cancel-before-delay", mode: "hedge", cancel: true, hedgeDelay: time.Hour, budget: 1, wantAddr: "p", wantErr: context.Canceled, wantStarted: 1},
		{name: "hedge-funded-cancel-ready-delay", mode: "hedge", cancel: true, budget: 1, wantAddr: "p", wantErr: context.Canceled, wantStarted: 1},
		{name: "hedge-funded-cancel-after-secondary-send", mode: "hedge", cancelOnSecondary: true, hedgeDelay: 3 * time.Second, budget: 1, wantErr: context.Canceled, wantElapsed: 3 * time.Second, wantSecondaryAt: 3 * time.Second, wantStarted: 2, spent: true},
		{name: "hedge-funded-ready-secondary-cancel", mode: "hedge", cancelOnSecondary: true, sReady: true, hedgeDelay: 3 * time.Second, budget: 1, wantAddr: "s", wantElapsed: 3 * time.Second, wantSecondaryAt: 3 * time.Second, wantStarted: 2, spent: true},
		{name: "hedge-instant-no-budget", mode: "hedge", pReplyAfter: 5 * time.Second, budget: 0, wantAddr: "p", wantElapsed: 5 * time.Second, wantStarted: 1, accrued: 1},
		{name: "hedge-instant-budget", mode: "hedge", sReady: true, budget: 1, wantAddr: "s", wantStarted: 2, spent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hedgeTimerBubble(t, func(t *testing.T, created *[]*time.Timer) {
				probe := getTimer(time.Hour)
				putTimer(probe)
				for round := 0; round < 2; round++ {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if tc.cancel {
						cancel()
					}
					qm := budgetModel(tc.budget)
					started := make(map[string]rpcAccount)
					begin := time.Now()
					var secondaryAt time.Duration
					secondaryCalls := 0
					prepare := func(addr string, ready bool, sendErr, replyErr error, delay, replyAfter time.Duration) sendFunc {
						return func() inFlightRPC {
							if addr == "s" {
								secondaryAt = time.Since(begin)
								secondaryCalls++
							}
							time.Sleep(delay) // Fake send completion; no wall-clock races.
							if sendErr != nil {
								return inFlightRPC{err: sendErr}
							}
							ch := make(chan transport.Response, 1)
							if ready {
								ch <- transport.Response{Body: []byte(addr), Err: replyErr}
							} else if replyAfter > 0 {
								go func() {
									time.Sleep(replyAfter)
									ch <- transport.Response{Body: []byte(addr), Err: replyErr}
								}()
							}
							r := inFlightRPC{replyCh: ch, replyHandle: &transport.ReplyHandle{}, addr: addr, delta: qm.startRequest(addr), start: time.Now()}
							started[addr] = accountOf(r)
							if addr == "s" && tc.cancelOnSecondary {
								cancel()
							}
							return r
						}
					}
					primary := prepare("p", tc.pReady, tc.pSendErr, tc.pReplyErr, tc.pDelay, tc.pReplyAfter)
					secondary := prepare("s", tc.sReady, tc.sSendErr, nil, tc.sDelay, 0)
					var got hedgeResult
					switch tc.mode {
					case "single":
						got = sendFrameWithHedge(ctx, qm, tc.hedgeDelay, primary, nil, 7*time.Second)
					case "race":
						got = raceReplies(ctx, primary(), secondary(), 7*time.Second)
					case "hedge":
						got = sendFrameWithHedge(ctx, qm, tc.hedgeDelay, primary, secondary, 7*time.Second)
					}
					if got.err != tc.wantErr || got.addr != tc.wantAddr || time.Since(begin) != tc.wantElapsed {
						t.Fatalf("round %d: result=%+v elapsed=%v; want addr=%q err=%v elapsed=%v", round, got, time.Since(begin), tc.wantAddr, tc.wantErr, tc.wantElapsed)
					}
					checkSecondModel(t, qm, tc.budget, tc.accrued, tc.spent)
					if tc.mode == "hedge" && got.triedSecondary != (secondaryCalls > 0) {
						t.Fatalf("triedSecondary=%v after %d secondary calls", got.triedSecondary, secondaryCalls)
					}
					if got.err == nil && string(got.body) != tc.wantAddr {
						t.Fatalf("reply body lost: %+v", got)
					}
					if len(started) != tc.wantStarted || secondaryCalls > 1 || secondaryAt != tc.wantSecondaryAt {
						t.Fatalf("started=%v secondary calls=%d at=%v; want %d at=%v", started, secondaryCalls, secondaryAt, tc.wantStarted, tc.wantSecondaryAt)
					}
					accounts := append([]rpcAccount(nil), got.others...)
					if got.addr != "" {
						accounts = append(accounts, rpcAccount{addr: got.addr, delta: got.delta, start: got.start})
					}
					if len(accounts) != len(started) {
						t.Fatalf("lost or duplicate accounts: %+v, started=%+v", accounts, started)
					}
					for _, account := range accounts {
						want, ok := started[account.addr]
						if !ok || account.delta != want.delta || account.start != want.start {
							t.Fatalf("duplicate/changed account: %+v, started=%+v", account, started)
						}
						delete(started, account.addr)
						qm.endRequest(account.addr, account.delta, 0, false)
						if qm.getOrCreate(account.addr).smoothOutstanding.total != 0 {
							t.Fatalf("outstanding request leaked: %s", account.addr)
						}
					}
					for _, timer := range *created {
						if timer.Stop() {
							t.Fatal("returned RPC left a pooled timer armed")
						}
					}
					// Two simultaneous checkouts must not alias (including after a
					// nested return), and neither may deliver an earlier lease's signal.
					a, b := getTimer(37*time.Second), getTimer(41*time.Second)
					if a == b {
						t.Fatal("a timer was returned twice or shared by simultaneous leases")
					}
					leaseStart := time.Now()
					<-a.C
					if time.Since(leaseStart) != 37*time.Second {
						t.Fatal("first reused lease expired at an old deadline")
					}
					select {
					case <-b.C:
						t.Fatal("second live lease expired with the first")
					default:
					}
					<-b.C
					if time.Since(leaseStart) != 41*time.Second {
						t.Fatal("second reused lease expired at an old deadline")
					}
					putTimer(a)
					putTimer(b)
				}
			})
		})
	}
}

func TestHedgeTimerNestedLease(t *testing.T) {
	hedgeTimerBubble(t, func(t *testing.T, _ *[]*time.Timer) {
		ch := make(chan transport.Response, 1)
		var loan *time.Timer
		primary := func() inFlightRPC {
			return inFlightRPC{replyCh: ch, replyHandle: &transport.ReplyHandle{}, addr: "p"}
		}
		secondary := func() inFlightRPC {
			// Borrow from the same pool while the outer hedge timer is still
			// owned, then keep this loan live across the nested reply race.
			loan = getTimer(20 * time.Second)
			go func() {
				time.Sleep(2 * time.Second)
				ch <- transport.Response{Body: []byte("primary after hedge")}
			}()
			return inFlightRPC{replyCh: make(chan transport.Response), replyHandle: &transport.ReplyHandle{}, addr: "s"}
		}
		begin := time.Now()
		got := sendFrameWithHedge(context.Background(), budgetModel(1), 3*time.Second, primary, secondary, 7*time.Second)
		if got.err != nil || got.addr != "p" || string(got.body) != "primary after hedge" || len(got.others) != 1 || got.others[0].addr != "s" || time.Since(begin) != 5*time.Second {
			t.Fatalf("nested race result=%+v elapsed=%v", got, time.Since(begin))
		}
		// If nested/outer cleanup stopped or reset the borrowed timer, this
		// either deadlocks the bubble or reports a different fake deadline.
		<-loan.C
		if time.Since(begin) != 23*time.Second {
			t.Fatal("nested timer scopes stole another live lease")
		}
		putTimer(loan)
	})
}
