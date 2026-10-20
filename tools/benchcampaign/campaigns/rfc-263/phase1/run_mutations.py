import csv
import difflib
import json
from pathlib import Path
import subprocess
import sys

ROOT = Path('${FDB_GO_WORKTREES}/fdb-go-perf')
OUT = Path('/tmp/rfc263-phase1.owjNRR/mutations')
OUT.mkdir(exist_ok=True)
Q = 'pkg/fdbgo/client/timeout_queue.go'
R = 'pkg/fdbgo/client/read_incarnation.go'
T = 'pkg/fdbgo/client/transaction.go'
LEASE = '//pkg/fdbgo/client:lease_alloc_unit_test'
M = []
def add(name, test, assertion, changes, race=False):
    M.append(dict(name=name, test=test, assertion=assertion, changes=changes, race=race))
add('remove-slack','TestTimeoutQueueWake/slack','WAKE_POLICY',[(Q,'timeoutSlack = 100 * time.Microsecond','timeoutSlack = 0 * time.Microsecond')])
add('remove-interval','TestTimeoutQueueWake/interval','WAKE_POLICY',[(Q,'min(n.deadline+int64(timeoutSlack), now+int64(timeoutInterval))','n.deadline+int64(timeoutSlack)')])
add('deadline-less-equal','TestTimeoutQueueWake/interval','DEADLINE_DELIVERY',[(Q,'if now < n.deadline {','if now <= n.deadline {')])
add('relative-deadline','TestTimeoutQueueContentionPreservesDeadline','CONTENTION_BUDGET',[(Q,'n.deadline = deadline','n.deadline = now + deadline')])
add('old-sequence-on-requeue','TestTimeoutQueueFIFO','FIFO_INTERVAL_SEQUENCE',[(Q,'q.pushLocked(n, now) // the timebomb\'s loop: wait again (RYW:1568-1570)','oldSeq := n.seq\n\t\t\t\tq.pushLocked(n, now)\n\t\t\t\tn.seq = oldSeq')])
add('heap-order-not-batch-fifo','TestTimeoutQueueBatchOrder','BATCH_FIFO',[(Q,'slices.SortFunc(q.ready, func(a, b *timeoutNode) int { return cmp.Compare(a.seq, b.seq) })','slices.SortFunc(q.ready, func(a, b *timeoutNode) int { return cmp.Compare(a.wake, b.wake) })')])
add('ignore-batch-withdrawal','TestTimeoutQueueBatchWithdrawal','BATCH_WITHDRAWAL',[(Q,'if n.index != inReady {','if false && n.index != inReady {')])
add('omit-swap-index','TestTimeoutQueueHeapInvariant','HEAP_INDEX',[(Q,'h[i].index, h[j].index = i, j','// MUTANT: indices not updated')])
add('retain-removed-slot','TestTimeoutQueueHeapInvariant','HEAP_SLOT',[(Q,'q.heap[last] = nil','// MUTANT: removed slot retained')])
add('retain-ready-slots','TestTimeoutQueueBatchWithdrawal','READY_SLOT',[(Q,'clear(q.ready)','// MUTANT: ready slots retained')])
add('linear-heap-rebuild','TestTimeoutQueueSiftComparisons','PUSH_COMPLEXITY',[(Q,'q.up(n.index)','q.up(n.index)\n\tfor i := len(q.heap)/2 - 1; i >= 0; i-- { q.down(i) }')])
add('overlapping-drainers','TestTimeoutQueueSingleDrainer','SINGLE_DRAINER',[(Q,'if q.running {','if false && q.running {')])
exit_old='if len(q.heap) > 0 {\n\t\tq.requestLocked(q.heap[0].wake, q.now())\n\t}'
exit_new='if len(q.heap) > 0 {\n\t\tq.reqAt = q.heap[0].wake // MUTANT: no actual timer request\n\t}'
add('disable-comparison-counter','TestTimeoutQueueSiftComparisons','COMPARISON_POPULATION',[(Q,'if q.compare != nil {\n\t\tq.compare()\n\t}','// MUTANT: comparison instrumentation disabled')])
add('idle-head-not-advanced','TestTimeoutQueueEarlierHeadReplacesRequest','EARLIER_HEAD',[(Q,'if !q.running && (!q.requested || n.wake < q.reqAt) {','if !q.running && !q.requested {')])
add('stale-callback-loses-wake','TestTimeoutQueueStaleCallback','STALE_CALLBACK_WAKE',[(Q,exit_old,exit_new)])
add('schedule-requests-while-draining','TestTimeoutQueueScheduleDuringDelivery','DRAINING_REQUEST',[(Q,'if !q.running && (!q.requested || n.wake < q.reqAt) {','if !q.requested || n.wake < q.reqAt {')])
add('delivery-exit-loses-wake','TestTimeoutQueueScheduleDuringDelivery','DELIVERY_WAKE',[(Q,exit_old,exit_new)])
add('stale-pop-rearm','TestTimeoutPopThenRearm','STALE_REARM',[(R,'if !tx.timeoutArmed || tx.timeoutArm != arm {','if !tx.timeoutArmed {')])
add('stale-pop-mid-turnover','TestTimeoutPopThenReset/mid-turnover','STALE_MID_TURNOVER',[(R,'if !tx.timeoutArmed || tx.timeoutArm != arm {','if tx.timeoutArm != arm {')])
add('stale-pop-published','TestTimeoutPopThenReset/after-publication','STALE_PUBLISHED',[(R,'if !tx.timeoutArmed || tx.timeoutArm != arm {','if !tx.timeoutArmed {')])
sticky_old='if inc.cause != nil {\n\t\treturn\n\t}\n\tinc.cause = cause'
sticky_new='inc.cause = cause'
add('record-then-reset-overwrites','TestTimeoutRecordThenReset','want FDB code 1031',[(R,sticky_old,sticky_new)])
add('pop-then-cancel-overwrites','TestTimeoutPopThenCancel','CANCEL_FIRST_RECORDED',[(R,sticky_old,sticky_new)])
add('cancel-after-timeout-overwrites','TestCancelAfterTimeout','want FDB code 1031',[(R,sticky_old,sticky_new)])
add('synthetic-retired-guard','TestCauseOnCleanRetiredIncarnationGuardSynthetic','SYNTHETIC_RETIRED_GUARD',[(R,'if inc == tx.readLife {\n\t\ttx.disarmTimeoutLocked()\n\t}','tx.disarmTimeoutLocked()')])
add('strong-transaction-node','TestAbandonedTransactionTimeoutDoesNotRetainIt','TIMEOUT_RETAINS',[(Q,'tx    weak.Pointer[Transaction]','tx    weak.Pointer[Transaction]\n\tstrong *Transaction'),(Q,'tx: weak.Make(tx), queue: q, index: -1','tx: weak.Make(tx), strong: tx, queue: q, index: -1')])
add('strong-database-node','TestTimeoutQueueDoesNotRetainDatabase','QUEUE_RETAINS_DATABASE',[(Q,'tx    weak.Pointer[Transaction]','tx    weak.Pointer[Transaction]\n\tdatabase *database'),(Q,'tx: weak.Make(tx), queue: q, index: -1','tx: weak.Make(tx), database: tx.db, queue: q, index: -1')])
add('close-suppresses-timeout','TestTimeoutAfterDatabaseClose','want FDB code 1031',[(R,'func (tx *Transaction) fireTimeout(arm uint64) {','func (tx *Transaction) fireTimeout(arm uint64) {\n\tif tx.db != nil && tx.db.ctx != nil && tx.db.ctx.Err() != nil { return }')])
construct_old='tx.readLife = &readIncarnation{ctx: ctx, cancel: cancel, gen: tx.readGen}'
construct_new=construct_old+'\n\t\tif tx.timeoutNs.Load() > 0 && time.Until(tx.deadlineTime()) <= 0 { tx.recordCauseLocked(tx.readLife, &wire.FDBError{Code: ErrTransactionTimedOut}) }'
add('construction-delivers-pending-disable','TestPendingTimeoutDisabledBeforeDelivery','DISABLE_PENDING',[(R,construct_old,construct_new)])
add('construction-delivers-pending-extend','TestPendingTimeoutExtendedBeforeDelivery','EXTEND_PENDING',[(R,construct_old,construct_new)])
add('delivery-skips-idle-generation','TestDeliveredTimeoutIsSticky','IDLE_GENERATION_DELIVERY',[(R,'inc = tx.readIncarnationLocked()\n\t}\n\ttx.recordCauseLocked(inc,','tx.readErrMu.Unlock()\n\t\treturn\n\t}\n\ttx.recordCauseLocked(inc,')])
add('expired-arm-not-synchronous','TestExpiredArmIsSynchronous','EXPIRED_ARM_SYNC',[(R,'if delay <= 0 {','if false && delay <= 0 {')])
add('shared-mutable-error','TestTimeoutDeliveryOwnsError','PRIVATE_TIMEOUT_ERROR',[(R,'func (tx *Transaction) fireTimeout(arm uint64) {','var mutantTimeoutError = &wire.FDBError{Code: ErrTransactionTimedOut}\n\nfunc (tx *Transaction) fireTimeout(arm uint64) {'),(R,'tx.recordCauseLocked(inc, &wire.FDBError{Code: ErrTransactionTimedOut})\n\tpending :=','tx.recordCauseLocked(inc, mutantTimeoutError)\n\tpending :=')])
add('unlocked-schedule','TestTimeoutQueueConcurrent','WARNING: DATA RACE',[(Q,'q.mu.Lock()\n\tnow := q.now()\n\tif n.index >= 0 {','q.mu.Lock()\n\tnow := q.now()\n\tq.mu.Unlock()\n\tif n.index >= 0 {'),(Q,'q.requestLocked(n.wake, now)\n\t}\n\tq.mu.Unlock()','q.requestLocked(n.wake, now)\n\t}\n\t// MUTANT: heap work outside the queue lock')],race=True)

FULL = '//pkg/fdbgo/client:client_test'
add('pending-memo-poisoned-by-successor','TestTimeoutSuccessorPendingGet','old pipelined memo after successor timeout', [('pkg/fdbgo/client/pending_get.go','func (p *PendingGet) ResolveOrAbort(abort <-chan struct{}) ([]byte, error, bool) {','func (p *PendingGet) ResolveOrAbort(abort <-chan struct{}) ([]byte, error, bool) {\n\tp.tx.readErrMu.Lock()\n\tvar successorCause error\n\tif p.tx.readLife != nil { successorCause = p.tx.readLife.cause }\n\tp.tx.readErrMu.Unlock()\n\tif successorCause != nil { return nil, successorCause, false }')])
M[-1]['target']=FULL
add('old-stamp-poisoned-by-successor','TestTimeoutSuccessorVersionstampObservers','old committed stamp after successor timeout', [('pkg/fdbgo/client/versionstamp.go','func (p *PendingVersionstamp) Resolve() ([]byte, error) {','func (p *PendingVersionstamp) Resolve() ([]byte, error) {\n\tp.completion.tx.readErrMu.Lock()\n\tvar successorCause error\n\tif p.completion.tx.readLife != nil { successorCause = p.completion.tx.readLife.cause }\n\tp.completion.tx.readErrMu.Unlock()\n\tif successorCause != nil { return nil, successorCause }')])
M[-1]['target']=FULL
add('new-stamp-ignores-successor-cause','TestTimeoutSuccessorVersionstampObservers','new getter after successor timeout', [(T,'if inc.cause != nil {\n\t\ttx.readErrMu.Unlock()\n\t\treturn nil, inc.cause\n\t}\n\tcompletion := inc.versionstamp','completion := inc.versionstamp')])
M[-1]['target']=FULL
add('delivery-cancels-committed-watch','TestTimeoutSuccessorCommittedWatch','successor timeout canceled committed watch', [(R,'if tx.afterTimeoutRecord != nil {\n\t\ttx.afterTimeoutRecord()\n\t}\n\tinc.cancel(cause)','if tx.afterTimeoutRecord != nil {\n\t\ttx.afterTimeoutRecord()\n\t}\n\ttx.cancelWatches()\n\tinc.cancel(cause)')])
M[-1]['target']=FULL
add('early-request-and-missing-deadline-guard','TestTimeoutNoEarlierThanDeadline','before its original deadline', [(Q,'min(n.deadline+int64(timeoutSlack), now+int64(timeoutInterval))','min(n.deadline-int64(10*time.Millisecond), now+int64(timeoutInterval))'),(Q,'if now < n.deadline {','if false && now < n.deadline {')])
M[-1]['target']=FULL

selection = set(sys.argv[1:])
results=[]
prior = json.loads((OUT/'results.json').read_text()) if (OUT/'results.json').exists() else []
combined = {r['name']: r for r in prior}
for m in M:
    if selection and m['name'] not in selection: continue
    folder=OUT/m['name']; folder.mkdir(exist_ok=True)
    attempt = 1 + len(list(folder.glob('attempt-*-output.log')))
    if (folder/'output.log').exists() and attempt == 1:
        (folder/'pre-format-attempt-output.log').write_text((folder/'output.log').read_text())
    originals={p:(ROOT/p).read_text() for p,_,_ in m['changes']}
    changed=dict(originals)
    for p,old,new in m['changes']:
        assert changed[p].count(old)==1, (m['name'],p,repr(old),changed[p].count(old))
        changed[p]=changed[p].replace(old,new)
    patch=''.join(''.join(difflib.unified_diff(originals[p].splitlines(True),changed[p].splitlines(True),fromfile='a/'+p,tofile='b/'+p)) for p in originals)
    assert patch
    (folder/'mutant.patch').write_text(patch)
    (folder/'spec.json').write_text(json.dumps(m,indent=2))
    cmd=['bazelisk','test',m.get('target',LEASE),'--test_arg=-test.run=^'+m['test']+'$','--test_arg=-test.timeout=90s','--nocache_test_results','--test_output=all']
    if m['race']: cmd+=['--@rules_go//go/config:race']
    (folder/'command.json').write_text(json.dumps(cmd))
    try:
        for p in changed: (ROOT/p).write_text(changed[p])
        subprocess.run(['gofumpt','-w',*changed],cwd=ROOT,check=True)
        changed = {p:(ROOT/p).read_text() for p in originals}
        patch=''.join(''.join(difflib.unified_diff(originals[p].splitlines(True),changed[p].splitlines(True),fromfile='a/'+p,tofile='b/'+p)) for p in originals)
        (folder/'mutant.patch').write_text(patch)
        (folder/f'attempt-{attempt}-mutant.patch').write_text(patch)
        proc=subprocess.run(cmd,cwd=ROOT,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,timeout=600)
        text=proc.stdout
        (folder/'output.log').write_text(text)
        (folder/f'attempt-{attempt}-output.log').write_text(text)
        population=('=== RUN   '+m['test'].split('/')[0]) in text
        proof=proc.returncode!=0 and population and m['assertion'] in text and ('--- FAIL:' in text or m['race'])
        result=dict(name=m['name'],test=m['test'],returncode=proc.returncode,population=population,expected=m['assertion'],proof=proof)
        results.append(result)
        (folder/f'attempt-{attempt}-result.json').write_text(json.dumps(result,indent=2))
        combined[m['name']] = result
        with (OUT/'results.json').open('w') as f: json.dump(list(combined.values()),f,indent=2)
        print(json.dumps(result),flush=True)
        if not proof: raise RuntimeError('Mutation did not compile and fail expected assertion: '+m['name'])
    finally:
        for p in originals: (ROOT/p).write_text(originals[p])
print('All selected mutations applied, compiled and failed the named assertion.',flush=True)
