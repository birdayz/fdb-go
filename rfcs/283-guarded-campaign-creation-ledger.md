# RFC-283: Guarded campaign creation with an exact ownership ledger

**Status:** Proposed design only. No implementation or paid-launch approval.
**Source anchor:** `2aaf8ce0054b503af08df438c03b13e5f793c1ba`.
**Scope:** The existing multinode provisioner, teardown guard and primary-IP
ownership check. No external cloud wrapper, generic orchestration framework,
workload, compiler, FoundationDB or CPU-source change.

## Problem and existing evidence

`tools/benchcampaign/multinode/teardown_guard.py:68–85` currently calls a numeric
ID owned when campaign label, name prefix and protection checks pass. Both
immediate and deadline paths use that classification (`124–147`). Neither reads
creation receipts. A matching stranger can consequently be deleted. Verification
only follows IDs in the initial discovery, so a concurrent late create can escape.

`provision.py:245–263` retains command output in memory until a final log write;
server descriptions appear after the invocation succeeds. `mn_common.py:153–156`
bounds a CLI command at120s, but a timeout can occur after the provider commits a
create. Process death can lose earlier returned IDs. A deadline value is checked,
not an armed guard (`provision.py:206–224`).

`primary_ip_check.py:39–78` records server/IP associations but discovers server
ownership from names and labels. The teardown guard never consumes those records.
The SSH transport's separate created-server-ID check is not deletion authority.

Historical teardown success and a final inventory census do not prove that an
unknown matching resource could not have been deleted. The paid barrier remains
closed until this design, implementation, negative controls and final review pass.

## Required invariants

1. Deletion requires **committed provenance for the exact `(kind, id)` AND current
   campaign label AND exact expected name/prefix AND no protection conflict**.
   Name/label discovery alone never adds ownership.
2. Before any provider mutation, an intent is durable and a pinned, live,
   supervised guard is armed against the same journal, baseline and fixed times.
3. A create is dispatched at most once per intent. A timeout, killed CLI, missing
   output or ambiguous response is not permission to retry or declare no create.
4. The guard closes creation admission atomically before cleanup. Immediate and
   deadline teardown use the same recovery, authorization and confirmation path.
5. Pending/ambiguous creates remain monitored. An empty list is not completion
   while an operation might still materialize. Late materialization cannot escape
   a one-pass snapshot because completion is not based on one-pass discovery.
6. Pre-existing resources are never adopted, relabelled or deleted. Implicit
   primary IPs are never directly deleted by these tools.
7. A fixed16h hard resource-lifetime ceiling is distinct from an earlier cleanup
   cutover and reserve. Reaching the hard deadline with unresolved resources is a
   failure/incident, not an extension or a successful teardown.

## Threat model and the ownership-proof boundary

Trusted: this controller's pinned code, private journal directory, OS process
identity and random-number generator; authenticated TLS provider responses; and
project writers who may operate unrelated resources but do not maliciously forge
this controller's private intents. Credentials remain on the controller; no token
is copied into a VM, receipt or command argument.

In scope: benign concurrent project activity, prefix/label collisions, stale files,
controller/CLI death, lost acknowledgements, filesystem failure, API timeouts,
eventual visibility, delayed create actions, metadata drift and deadline races.
The initial complete ten-kind baseline and each create's pre-call snapshot protect
both pre-existing resources and unrelated resources seen since that baseline.

**A nonce label is not a provider-authenticated creator identity.** An adversarial
project writer who can observe an issued nonce and deliberately copy its entire
binding can defeat label-based recovery. A compromised controller/OS can also
forge the journal. Under either stronger threat model, automatic unknown-outcome
recovery is **not authorized** without provider-supported request/action or
idempotency evidence, or isolation that removes the untrusted writer. Merely
calling the label cryptographic does not eliminate this boundary.

For the stated benign-writer model, a secret-before-dispatch, independently random
128-bit per-intent value makes an accidental matching create negligibly probable.
The proof is the conjunction of a pre-existing durable intent, one dispatch,
complete pre-call absence evidence, exact canonical fields and one unique new
candidate carrying that value—not an ordinary campaign selector. Provider
`created` is required well-formed metadata, not a freshness or clock-accuracy proof.
Once an ID is committed it is immutable: disappearance or identity drift never
transfers ownership to a replacement, even if the provider reuses its numeric ID.

The already accepted exclusive-owned-graph reservation covers external edits,
clones, replacements and replay of this campaign's issued identity bindings from
creation through teardown. Unrelated benign writers may create colliding names or
ordinary campaign labels, but do not copy/replay the private per-intent nonce into
another resource. The nonce becomes provider-visible on dispatch; secrecy after
that point is not claimed. Deliberate binding copying, including by an otherwise
benign cloning tool, violates this reservation and requires fail-closed owner
handling, not automatic adoption. This is the same conditional cooperative-writer
boundary, not a claim of provider-atomic isolation.

**Launch decision:** the owner must explicitly accept this threat model after
native design review. If the required ownership claim includes hostile project
writers, the provider-evidence alternative below is a blocking prerequisite. The
implementation must not choose the weaker model silently.

## Focused changes and interfaces

Add one small private-journal helper under `tools/benchcampaign/multinode/`, used
by `provision.py`, `teardown_guard.py` and `primary_ip_check.py`. It provides typed
validation, atomic durable updates, one local lock and the shared authorization
predicate. It is not a service, plugin system or general cloud abstraction.

Proposed, **not currently runnable**, interfaces:

- `provision.py`: initialize a fresh journal offline; execute only with that
  journal and its guard-arming binding. Keep existing dry-run planning behavior.
- `teardown_guard.py --creation-ledger FILE`: arm against it, or request immediate
  teardown. `--execute` without a valid ledger refuses; there is no legacy
  broad-selector execution escape hatch. Historical runbooks remain history.
- `primary_ip_check.py`: require server IDs from this ledger before validating IP
  associations, then append subordinate IP evidence to it.

Keep output outside Git with0700 directories and0600 files. Refuse symlinks,
nonregular files, unexpected ownership/modes, malformed types, duplicate kinds/IDs,
unknown versions, missing required fields and protected-baseline conflicts. A
read-only legacy dry run may report discovered candidates but cannot authorize
mutation or create a ledger from those candidates.

### Journal header and typed records

The immutable header binds format/version, a random journal UUID, campaign,
expected prefix, controller boot ID, explicit hcloud context identity, source
hashes, baseline manifest/protect-file hashes, allowed resource plan, the one
registered provisioner PID/starttime and source/argv hashes, and immutable time
policy. Register that existing foreground process before arming/admission; a
restart cannot register a replacement creator or reopen provisioning. The plan allows only
four new `ccx43`/`fsn1` servers, one dedicated network, firewall and new SSH key;
no replacements, volumes, extra hosts or existing-resource adoption.

Separate records:

| Record | Minimum evidence |
|---|---|
| Create intent | Monotonic sequence; kind; exact expected name; random128-bit nonce; credential-free canonical request descriptor and exact wire-body SHA256; source/private-input hashes; complete pre-call ID census and hash; wall/monotonic observation brackets; producer PID/starttime; fixed command deadline. |
| Dispatch/send lease | Intent ID, one-shot state, registered existing producer PID/starttime, request hash and absolute whole-call deadline. Persist before its synchronous HTTP call; no mutation child is spawned. |
| Acknowledgement | Structured201 resource/action evidence and raw-body SHA256, with secret values removed before persistence; exact returned kind/ID; required well-formed provider `created` value retained literally; canonical response checks; known action IDs. |
| Recovered acknowledgement | Same exact ID and checks, plus the complete recovery census, matching-candidate proof and explicit recovery origin. Never overwrite the original missing acknowledgement. |
| Owned resource | Immutable kind/ID, owning intent, exact expected name/labels/attributes and evidence digests. |
| Primary-IP association | Exact IP ID, recorded-owned server ID, matching server reference/assignee, literal well-formed `created`, `auto_delete=true`, nonbaseline/nonprotected/pre-call-absent proof; no direct-delete authority. |
| Lifecycle transition | Previous generation/hash, next generation, state and reason; teardown/deletion confirmations retain their own evidence. |

Resource keys are `(kind, id)`, not bare numbers. Non-create mutations such as
adding a subnet, attaching a known server or powering it on must also have durable
intents and may target only already recorded-owned IDs. They do not allocate new
resources and cannot extend a deadline or reopen admission.

Canonical create fields include all materially requested fields, not just names:
server type/location/image, public-network settings (`--without-ipv6`), requested
SSH/firewall identities and user-data hash; network CIDR; firewall rule hash; SSH
public-key fingerprint; campaign/deadline/role labels and the intent nonce. Pin
input file bytes before dispatch. Private user-data bytes and Authorization
headers never enter the journal: retain their approved file/hash descriptor and
wire-body hash, construct the body in controller memory and recheck inputs before
sending. Strip any returned `root_password` or other credential value before
persisting response evidence; retain its presence/redaction fact, not its value.

Recovery compares every corresponding provider-observable field. A mismatch
refuses recovery. User-data is not returned by readback: its descriptor is evidence
of the pinned submitted request, **not** independent provider verification of that
field. Conditional nonce provenance ties a recovered ID to that one issued body
under the stated threat model. If independent readback of an unobservable field is
required for the ownership claim, recovery remains blocked; do not fabricate it.

### Durable filesystem protocol

Use one controller-local advisory lock on a separate, never-replaced lock file.
At initialization, durably create a0600 immutable anchor containing the journal
UUID, immutable-header hash and initial generation/hash. Pin the private directory's
owner/mode/device/inode and this anchor's identity/hash, not the replaceable
journal inode. A fresh256-bit local integrity key is stored separately with0600
permissions in that private directory; it never enters the journal, logs, argv or
VM. Both pinned processes load it on the controller. This is local integrity under
the trusted-controller model, not provider attribution or hostile-OS protection.

For each transition, retain an exclusively created0600 immutable generation file.
It contains the UUID/header hash, sequence, predecessor hash, complete next state
and HMAC-SHA256 over those fields using the local key. `fsync` that file and its
directory before publishing it. Write the complete same generation to a fresh
temporary head file, verify0600, `fsync`, atomically rename it over the journal
head, then `fsync` the directory before releasing the lock or dispatching anything.
A separate append-only evidence file may preserve raw responses; it must be durable
before a journal entry points to it. Never overwrite an earlier adverse receipt.

The common reader validates the anchor, immutable header and contiguous authenticated
generation chain before accepting a newer head. The guard always reopens the current
head; it never keeps watching an obsolete inode. Each process remembers its last
accepted generation/hash and refuses rollback, gaps, forks, altered predecessors,
wrong UUID/header/MAC or changed directory/anchor/key identity. At restart, reconcile
the full retained chain with the current head before any mutation: an unheaded next
generation or inconsistent crash residue requires fail-closed guard-led recovery,
not silent rollback or creator resumption. An initial receipt's generation/hash is
an authenticated ancestry anchor, not a permanent equality requirement.

A crash must expose either the previous complete head or the next complete head,
never a partially accepted record. No cloud mutation follows a failed write/fsync/
rename/directory-sync. If the response arrived but its durable commit failed, the
already durable dispatch remains ambiguous and enters recovery. Local deletion of
a temporary file is not evidence that a provider request failed.

## Guard arming and creation admission

Initialize the journal only after owner GO and fresh protection/price checks.
Choose activation time `T0` before arming and before the first create; using an
earlier origin is conservative. Fix `H=T0+16h` once. Proposed cutover is
`G=H−1h`; this deliberately retains more than a15min cleanup reserve. Resource
labels carry the same fixed cleanup deadline; the journal also records the
stricter hard deadline. Neither value may be extended or replaced by a restart.

A guard arms only after validating the journal and all referenced source/baseline
inputs. Its exclusive arming receipt binds the stable UUID, private-directory and
immutable-anchor/header identities, initial generation/hash, boot ID, both registered
producer and guard PID/starttime, source/argv hashes, fixed `T0/G/H`, and a fresh
local arming challenge. The supervisor must actually own those foreground processes;
a receipt alone is not liveness. Before each mutation the provisioner checks this
stable binding, authenticated current-head ancestry, live identities, pinned source,
recent monotonic guard heartbeat and open journal. The heartbeat advertises the
guard's last accepted generation/hash; it must be a valid ancestor of the current
head, not an unrelated or rolled-back journal. Legitimate authenticated advancement
does not invalidate arming, and requires no receipt equality with a replaced inode.
No stale receipt or PID reuse permits a create. Guard death or unresponsive
supervision halts admission and requires the same ledger-based immediate recovery/
cleanup, not a broad fallback.

All four hosts should be allocated during bounded setup, not near the deadline.
Proposed create-admission cutoff `C=T0+30min`, earlier than both `G` and `H`;
no new resource/replacement is admitted after `C`. This is a safety budget, not a
reason to reduce the44-case matrix. The owner may choose an even earlier cutoff
before launch; it may never move later after activation.

The one registered provisioner performs each provisioning mutation synchronously
in its existing main process: no mutation child, queued sender or unlocked handoff. One
critical section holds the journal lock across the final admission/identity/time
check, durable intent/send-lease commit, the entire single HTTP request/response,
and durable acknowledgement or `UNKNOWN` outcome commit. No second mutation or
registration may interleave. Before entering transport, persist `SENDING`; a crash
at or after that point is conservatively ambiguous, even if no bytes were sent.
In `CLOSING`, only the pinned guard process may take a teardown send lease, with
the same lock spanning fresh authorization/cascade checks, request and durable
outcome. An immediate command requests that armed guard, not a second sender.
A recovery guard may arm in `CLOSING` only after safely establishing the previous
guard is gone and validating the entire chain; it never reopens creation.

Every lease has a fixed whole-call deadline of at most20s, with a separately
reserved bounded2s termination/reap allowance. The applicable budget is explicitly
phase-specific, not a shared provisioning cutoff:

- In `OPEN`, a resource-create lease plus termination allowance must fit before
  `C`; later non-allocating provisioning mutations must fit before `G` and their
  sealed setup-stage deadline. Never start if that provisioning budget cannot
  cover the complete lease and termination allowance.
- In `CLOSING`, authorized cleanup/recovery requests use the remaining budget
  before `H`, not `C` or `G`. Thus cleanup beginning at `G` can use its reserved
  hour. Shorten each lease to leave the termination allowance before `H`; never
  restore provisioning admission merely to permit deletion.
- If the remaining `H` budget cannot cover a cleanup call and its termination,
  record reserve exhaustion and enter explicit incident recovery. Bounded
  authorized cleanup must continue, including after `H`, with fresh per-call
  supervision (at most20s plus2s termination), exact-ID/cascade checks and retained
  outcomes. Expired `C/G/H` never prohibit incident cleanup. They still prohibit
  provisioning/benchmark extension, and unresolved resources at `H` are a hard
  lifetime failure, never a changed deadline or successful ceiling receipt.

Cover DNS, connect, write and response handling, not just per-socket inactivity.
The owning supervisor enforces the outer deadline against the registered actor
through an owned process handle/Linux pidfd; a stale PID check followed by bare
`kill` is insufficient. It never launches another sender or retries the request.

For immediate/deadline cleanup, the guard requests admission stop out of band and
waits at most the existing lease's remaining budget for the sole registered
producer to quiesce. It can cancel/terminate that exact owned process without first
obtaining its journal lock. Once that process is reaped and cannot send, the guard
acquires the released lock and durably changes `OPEN` to `CLOSING`. A producer
restart/replacement is forbidden. A dead or timed-out producer's `SENDING` lease
remains `UNKNOWN` until provider evidence resolves it; local quiescence is not
provider cancellation. Thus no unregistered or admitted-but-unsent producer can
start a provisioning HTTP call after `CLOSING`. If bounded quiescence/lock acquisition fails,
refuse further work and emit an incident; do not claim cutover or cleanup success.
Guard heartbeats continue independently while draining/waiting for the lock.
Every read-only API/CLI wait uses the same phase-specific budget and bounded
per-call supervision; post-cutover/post-`H` evidence gathering is not refused
because provisioning admission expired. The provisioner rechecks before each
later command, not just at process entry.

## One-shot create and unknown-outcome recovery

### Mutation transport: the current CLI is not one-shot

Official hcloud CLI1.70.1, commit
`36a27dc22eabd9d22091a9d4072119902929b401`, emits create JSON only after its
create command returns. Server creation waits for actions and then describes the
server; firewall creation also waits for actions. Thus `-o json` does not durably
expose the initial ID before those waits. More importantly, its hcloud-go/v2
v2.52.0 client defaults to five retries and can replay POST after timeout/502/504
without an idempotency contract. One CLI invocation is not one provider dispatch.
The design MUST NOT use that path for create mutations or infer one-shot behavior
from a subprocess count. Pin the exact official source evidence in the provider
profile before implementation.

Propose a narrowly scoped, controller-only Python-standard-library HTTPS mutation
transport inside these tools, not a generic API client or external wrapper. Use a
single `http.client.HTTPSConnection` with normal certificate/hostname validation,
no redirect handler and no automatic request replay. It sends one POST to the
fixed official API endpoint, captures the initial201 resource/action/next-action
IDs immediately, removes any secret response values, and commits that evidence
before any action wait. The same one-request rule applies to later subnet,
attach, power-on and delete mutations: hidden SDK replay must not bypass a fresh
admission/ownership check. Read-only discovery may use the existing bounded CLI.
An explicit delete retry revalidates exact-ID ownership before its own single
request; it is never a transport-level replay. The same sealed auth profile must
be used by both paths. Expose only the fixed required endpoints/operations, not
an arbitrary API method/URL interface. Tests must count actual fake-provider POSTs
and inject a response-lost-after-commit failure; counting child invocations is
insufficient.

This changes credential handling and therefore needs explicit security review:
resolve the pinned local CLI context/environment precedence on the controller,
verify protected configuration-file ownership/modes and the unchanged nonsecret
profile binding, and retain the token only in controller process memory. Never
put it in argv, a journal/receipt, log, VM, rendered user-data or exception text.
Do not introduce a user-selectable API host or redirect secrets. No credential is
read for this RFC. If the owner rejects this narrow credential boundary, the
alternative is a reviewed/pinned CLI change that disables create retries and
exposes raw creation acknowledgements before waits. **The existing CLI is not an
acceptable fallback**, and implementation approval must choose a proven option.

For each of the seven explicitly allocated resources:

1. Under the same full-operation lock, validate `OPEN`, time, registered identity,
   guard liveness and baseline/source pins. Obtain and retain a fresh complete
   pre-call kind census (also complete IP/cascade-kind censuses for a server);
   require no expected name, nonce or protected-ID conflict. Generate the nonce once and durably commit the canonical intent and bounded
   one-shot send lease; repeat the final deadline/liveness check before `SENDING`.
2. While still holding that lock, synchronously dispatch exactly the pinned create
   request and receive its structured201 body. Persist the returned ID/response
   and outcome before releasing the lease/lock or another mutation. Verify it is
   absent from both baseline and pre-call IDs, has the expected kind, name, nonce/
   campaign/deadline labels, well-formed provider `created` and canonical fields.
   Do not compare `created` with controller or HTTP-Date clocks to authorize it.
3. A local failure provably before dispatch can be terminal `NOT_DISPATCHED`.
   A known authenticated provider rejection may be terminal `REJECTED` only when
   the pinned provider profile establishes that it cannot have committed a create.
   A generic nonzero CLI exit, timeout, invalid JSON or process death is `UNKNOWN`.
4. `UNKNOWN` closes further provisioning and starts immediate recovery/cleanup.
   It never causes a retry with either the same or a new nonce.
5. Recovery obtains a fresh complete census and independently describes each
   candidate. It may commit an ID only if exactly one new, nonprotected candidate
   matches the durable per-create nonce, exact name, kind and every required
   canonical field, with required well-formed provider `created` metadata. Record all
   candidates and refusals. Zero, multiple, pre-call/baseline, malformed or
   conflicting candidates remain unresolved; ordinary label/prefix matches cannot
   fill the gap.
6. A recovered ID uses the same immutable owned-resource record and deletion
   predicate as an acknowledged ID. A later response for the same intent must
   agree exactly; disagreement is an incident, never a second owned ID. Known
   resource identity and terminal provider operation are separate states. Retain
   known creation/action IDs from the initial response; after conditional ID
   recovery, a resource-specific action lookup may corroborate its creation
   action, but is not an authenticated client-request digest. If a required action
   cannot be identified or is still pending, retain that pending state. Neither
   positive ID recovery nor a transient404 alone closes it.

### Conditional provenance proof; no provider-clock premise

Under the stated accepted model, the provenance argument is:

1. Before dispatch, a complete baseline and fresh complete pre-call census exclude
   every observed pre-existing typed ID. The independently random128-bit nonce is
   generated once, bound to the exact request and durably recorded while still
   private. A matching name or ordinary campaign label is insufficient.
2. Only the one registered producer can dispatch that intent, through the pinned
   one-shot transport under the send lease. No retry, replacement intent or
   external copy/replay of its identity binding is permitted. The official single
   create operation is the sole admitted source of that new nonce-bearing object;
   transport-level replay is tested separately, not assumed away.
3. The authenticated acknowledgement, or exactly one independently described new
   recovery candidate, must match the issued nonce, exact kind/name, every required
   observable canonical field and nonbaseline/nonprotected/pre-call-absent ID.
   Eventual invisibility of an unrelated older object cannot give it this private
   intent binding. A concurrent unrelated benign create likewise lacks that nonce.
   Accidental random collision is negligible, not logically impossible. Zero or
   multiple candidates and unresolved actions remain pending; no census count
   establishes terminal absence or bounds late materialization.
4. Ownership is committed to that exact `(kind, id)` and literal identity record.
   Before every DELETE, independently revalidate its entire recorded name, labels,
   nonce, canonical identity, literal `created` and complete protected association
   graph. If disappearance is observed, retain that absence/retirement fact without
   clearing any unresolved action; a later resource at the same numeric ID is a replacement, never inherited
   ownership. Unobserved reuse with a different binding also fails fresh identity
   checks. Exact reproduction of the issued binding by another writer is outside
   the accepted reservation, even when its numeric ID was reused.

No never-reused-ID guarantee follows from the API or is required by this
conditional argument. A hostile writer, broken reservation, provider behavior
that reproduces the complete identity without the issued operation, or a stronger
provider-atomic attribution requirement invalidates the argument; automatic
adoption/deletion is then blocked. A nonce is not authenticated creator identity.

Provider `created` must be a nonempty, parseable timezone-qualified timestamp in
its documented API shape. Retain its exact text as opaque provider metadata and
corroboration, and require literal continuity after an ACK/recovered ACK. Missing,
malformed or later changed values refuse authorization, but a well-formed value
far before or after controller time is not itself proof of a pre-existing/new
resource. No clock-accuracy, wall-clock ordering or HTTP-Date bound is claimed or
required. There is no `provider-clock-bound` launch flag or qualification gate.

Lifetime is independent of that field: choose fixed local `T0` before the first
possible dispatch, enforce `C/G/H` using the conservative maximum of local wall
elapsed time and monotonic elapsed time anchored at that `T0`, and never reset
it on restart. Thus even inaccurate provider metadata cannot extend admission or
`H=T0+16h`. Retain provider billing timestamps as separate evidence; they cannot
override the local ceiling or turn unresolved cleanup into a lifetime success.

### Pending operations and the provider-capability gate

A zero-candidate result is **not** authoritative proof of no create. Requests can
have committed or still be pending after the local timeout. Keep the intent
pending and keep monitoring it; a candidate that materializes after an earlier
empty list still passes through the same exact binding before any deletion.
Do not mark the campaign empty/complete while such an intent exists.

Before implementation approval, pin official API/CLI capabilities for each kind:
structured output shape, typed ID and `created` fields, name/field semantics,
observable action/request correlation, terminal rejection/absence evidence,
idempotency support and cancellation if any. Current official OpenAPI evidence
contains no idempotency/request-ID or create-cancel contract, and arbitrary global
action listing was removed in2025; known action IDs are required. Server/firewall/
SSH-key names are documented unique; network name uniqueness is not documented.
The recovery algorithm therefore requires an exact unique candidate regardless
of name semantics, and cannot use an invented cancellation/absence API.
Do not assume that a CLI timeout
cancels provider work, that two empty reads prove absence, or that a provider
publishes a finite late-materialization bound.

If nonce recovery is inadmissible for the accepted threat model, or if a required
canonical binding or required well-formed metadata cannot be obtained, automatic
recovery remains blocked unless provider-authenticated action/idempotency evidence supplies the missing
binding. Surface the unresolved exact-ID owner decision; do not claim the
kill/lost-ack safety gate passed. Design review can approve an offline
implementation with this gate explicit; it cannot waive it for paid launch.

## Immediate and deadline teardown

Both entry points call the same state machine:

1. Stop and boundedly drain/cancel the sole registered producer as specified above,
   using its owned process identity, then under the lock durably change `OPEN` to
   `CLOSING`; no later provisioning mutation can obtain admission. There is no
   mutation child to appear after this barrier. Killing the producer does not
   cancel its provider request: an issued/uncertain lease becomes `UNKNOWN`, not
   absent. After controller restart, an unavailable trustworthy process handle
   requires fresh boot/PID/starttime verification and safe pidfd acquisition;
   ambiguity refuses signalling and reports an incident, never a bare-PID kill.
2. Resolve pending intents as above. Known IDs are never inferred from the broad
   selector, and unknown matching strangers are recorded/refused, not adopted.
3. For each committed kind/ID, obtain fresh provider metadata by that exact ID and
   require current campaign/nonce labels, exact expected name/prefix, canonical
   identity, literal recorded `created`, unretired ownership and no protection
   conflict. Refuse type/ID reuse, replacement or metadata drift.
   For a server, additionally require the complete fresh subordinate-deletion
   graph below: `OWNED` alone never means `SAFE_TO_DELETE`.
4. Delete by exact numeric ID, in dependency order: servers, firewall, SSH key,
   network. Recheck authorization, including every server cascade association,
   immediately before each one-request action under its bounded send lease.
   Retry only deletion of a recorded-owned ID, with bounded calls/retries and
   retained errors. Missing/conflicting cascade proof refuses the server DELETE;
   no delete-first/post-hoc-inventory check substitutes for authorization.
5. Confirm actual absence with fresh exact-ID/all-kind observations and terminal
   state for all known create/delete actions. Continue recovery/cleanup while any
   known resource, unresolved operation or unknown-outcome intent remains. A
   resource/action still initializing or locked is not a successful deletion.
   Do not assume deleting a known resource cancels another pending create.
6. Confirm subordinate primary IP disappearance and exact ten-kind inventory
   restoration. Publish separate facts for owned-resource absence and baseline
   equality; unrelated concurrent inventory changes are not permission to delete
   those resources or replace the original baseline.

Completion requires no pending intent, no recorded-owned resource/IP survivor,
all deletion/absence evidence valid, and the ten-kind baseline equality check.
Unknown matching resources, protection conflicts, ambiguous API state or failed
lists make the receipt non-successful. Never return success from an empty
selector while a pending request could still materialize.

## Hard deadline, reserves and billing

`G` is teardown **start/cutover**, not proof that lifetime ended. All owned resources
must be confirmed absent by `H`. Stop admitting benchmark phases early enough for
all remaining primary phases, per-phase collection, final artifacts and cleanup;
no deterministic budget rule may silently drop w32 or reduce primary quotas.
Before timing, the full remaining matrix must pass its capacity/admission forecast.

Proposed16h partition, to freeze before activation:

| Budget | Maximum planning allocation |
|---|---:|
| Deploy four prebuilt hosts, F0/P0/diagnostics and capacity admission | 10,800s |
| All six unchanged primary phase caps | 18,000s |
| Six phase collections,600s each | 3,600s |
| Final artifact transfer/sealing/export | 3,600s |
| Exact-ID cleanup and inventory confirmation | 3,600s |
| Remaining contingency within57,600s | 18,000s |

The18,000s phase sum is3150+3150+2850+2850+4700+1300. Actual F0 telemetry bytes,
sampler overhead and transfer forecasts must fit the reserved collections; this
is not an empirical completion-time claim. Prebuilt offline binaries avoid paid
compilation. Failure to admit the full matrix is NO-GO/teardown, not permission to
extend the lifetime or substitute a smaller campaign.

At the refreshed fsn1 price, at most64 started server-hours cost EUR33.685568 gross;
four IPv4 addresses over the same allowance add EUR0.060928 at EUR0.000952/hour.
Hourly rounding means even a small overrun can exceed a16-hour allocation. The
one-hour early cutover is intentional. Retain provider billing timestamps and do
not report the ceiling met unless allocation/deletion evidence supports it.

**External failure boundary:** no controller can guarantee physical deletion by
`H` if the provider indefinitely withholds visibility or rejects deletion. No
finite pending-create tail or cancellable create is assumed here. At or before
reserve exhaustion, stop all workload/admission, emit an incident with exact known
IDs and unresolved intents, notify the owner, and continue authorized recovery
rather than report success or silently extend benchmark work. If the owner
requires an unconditional provider-side lifetime guarantee, paid launch remains
blocked until an actual provider TTL/cancellation guarantee is established.
A supervisor timeout is not proof resources vanished; never cancel the guard just
because the planned lifetime elapsed while cleanup is unresolved.

## Implicit IPs

Before the next server stage, validate only a recorded-owned server ID. Record its
exact IPv4 ID from structured server metadata and a fresh complete primary-IP
listing; require matching references/assignee, `auto_delete=true`, absence from
baseline/protected/pre-call IDs and provenance consistent with this server's
canonical creation evidence. Subordinate attribution has two explicit origins:

- With a valid saved server-create response, retain its exact IP reference and
  cross-check the complete fresh server/IP readbacks against it and typed
  baseline/protected/pre-call absence.
- If the entire create response was lost, first recover the exact immutable server
  identity by the conditional nonce/canonical/unique-new-candidate proof above.
  Its independent full description must expose exactly the plan's one newly
  allocated IPv4 reference and no IPv6. A fresh complete primary-IP census and
  exact-IP readback must agree on the same positive IP ID, IPv4 type, exact owned
  server assignee, `auto_delete=true`, well-formed `created`, and absence from all
  baseline/protected/pre-call IDs. The sealed request must have requested a new
  implicit IPv4, not supplied an existing IP identity. All other cascade fields
  and complete association listings must be present and allowed by the plan.
  Under the accepted exclusive-owned-graph reservation, no external actor may
  attach, swap, clone or replay another IP into that newly created graph. The
  sole admitted server-create is consequently the source of this new referenced
  subordinate; pre-call absence alone would not establish that attribution.
  Persist a distinct `recovered-server-association` receipt binding the owning
  intent, recovered server kind/ID/nonce/identity digest, exact IP identity and
  literal metadata, complete server/IP census/describe hashes and typed absence
  evidence. This is conditional recovery-origin evidence, never a fabricated
  original response or provider-authenticated request digest. Preserve the
  original ACK as missing and all unresolved creation actions as pending.

Both origins must establish the same complete attributable nonprotected graph
before any server DELETE. Neither a mere server ID nor a label/association alone
is sufficient. Once an association identity is recorded, later checks compare its
exact reference, literal metadata and graph rather than rebasing that record onto
replacement IPs. A missing/ambiguous response-free proof, changed association,
failed readback or broken reservation remains `NOT_SAFE_TO_DELETE` and emits an
incident. This recovery path supplies no direct-IP delete or detach authority.

No provider-clock comparison is used. Require well-formed IP `created` metadata,
retain its literal value with the association, and reject later identity drift.
The unchanged plan disables IPv6, but teardown must
prove the actual complete IPv4/IPv6 and other provider-documented auto-delete
association graph, not assume the requested plan was followed.

Server provenance is retained if association validation fails, but that confers
**no server-delete authority**. Before every server DELETE, freshly describe it
and cross-check complete association listings; every object the provider would
automatically delete must be attributable to that owned creation, nonbaseline,
nonprotected and allowed by the sealed plan. A newly attached foreign IPv6/IP or
other auto-deleted object, missing association, conflicting reference/auto-delete
flag, failed listing, incomplete provider cascade description or metadata drift
makes the server `NOT_SAFE_TO_DELETE`. Retain it and emit an exact-ID owner
incident; never delete it merely because its own provenance is known. No direct
IP delete, auto-delete flag change, detach/reassignment or other workaround is
authorized, even if refusal threatens the hard deadline.

The accepted benign-writer policy must reserve this owned resource/association
graph from concurrent external writes from creation through teardown; unrelated
project activity remains allowed. Read-before-delete is not a provider-atomic conditional-delete
primitive. If that reservation cannot be established, or an atomic guarantee is
required, automatic server deletion remains blocked without provider support.
Fresh checks detect observed drift but do not prove absence of an uncooperative
writer's change after the check.

After an authorized server deletion, verify every recorded subordinate disappeared
and all ten inventory sets match baseline. That postcondition detects failures;
it never retroactively authorizes a deletion. Surviving/foreign/unattributable
objects require an exact-ID owner decision, not bulk cleanup.

## Offline regression and mutation plan

Tests use injected clocks, fake provider state, synthetic `/proc` process identity
and fault-injectable filesystem operations—no real cloud allocation. Positive
controls must actually authorize/delete the exact expected kind/ID population.
Every negative control must reach its intended guard with the other prerequisites
valid; assert exact refusals, affected IDs/actions and durable states, not merely
an exception or zero deletes.

| Area | Required positive/negative arms |
|---|---|
| Shared authorization | Immediate and deadline paths; matching stranger; wrong kind/ID; exact-ID replacement, including numeric-ID reuse with changed binding and reappearance after observed retirement; changed name/label/nonce/literal `created`; protected baseline/pre-call resource; malformed/empty/duplicate ledger. |
| Durability | Crash before intent, after intent fsync, before durable `SENDING`/HTTP (provable `NOT_DISPATCHED` only before `SENDING`), after `SENDING` but before HTTP (conservative `UNKNOWN`), after provider commit/before response, after response/before journal sync, during temp write/rename/directory fsync; failed sync forbids next mutation. |
| Lost acknowledgement | Zero candidates then late materialization; one valid candidate; multiple candidates; wrong nonce/canonical field, missing or malformed `created`; protected/pre-call candidate; inconsistent late acknowledgement; no automatic retry or hidden transport POST replay. Positive ACK and recovery with well-formed `created` far before/after local time must succeed through the real authorization path, without changing fixed local deadlines. Actual whole-server-response-lost-after-commit must recover the server, persist the distinct response-free IP-association proof with original ACK still missing, resolve the required action, perform the exact safe server DELETE and confirm subordinate disappearance/ten-kind restoration; no Journal/Campaign authorization stubs. Independently corrupt each recovered-association conjunct and prove zero server DELETEs. |
| Arming | Positive multiple legitimate head replacements with the same live guard; stable UUID/anchor/directory binding; authenticated advancement versus rollback/fork/gap/wrong MAC/key; crash orphan generation; missing/stale/fake receipt; dead/reused PID; changed boot/source/argv/deadline; stale heartbeat; guard dies between stages; no create without positive armed control. |
| Cutover/send lease | Pause before final check, after durable `SENDING`, during HTTP and before ack commit; assert the lock spans each point, one registered existing PID and no mutation child; concurrent close drains/cancels/reaps before `CLOSING`; no provisioning HTTP starts after it; parent death releases lock but leaves pending; provider commits after local cancellation; zero census never closes pending; both teardown paths use one closed journal; exhausted whole-call/termination budgets fail closed. |
| Deletion/confirmation | Dependency order; transient delete failure; provider locked/action pending; metadata drift; absent resource; failed list; late create; nonempty survivors; no false success with unresolved intent. |
| IP/inventory | Positive safe server cascade and exact subordinate disappearance; owned server with baseline/protected/foreign IPv4, unexpected IPv6 or other auto-delete object must reach cascade guard and produce zero server DELETEs; missing/false auto-delete, cross-list mismatch, incomplete/failed association evidence, graph drift and unavailable writer reservation refuse before DELETE; surviving IP/unrelated inventory prevents completion; never direct-IP delete, detach or flag mutation. |
| Lifetime | Provider `created` before/after local time leaves fixed local `T0/C/G/H` unchanged and never grants extra time; exactly admitted create boundary versus one-nanosecond late; provisioning refuses at expired `C/G` while valid cleanup DELETE/read calls still execute at `G` using `H` reserve; near-`H` calls shorten or enter incident recovery; post-`H` bounded cleanup/read calls remain authorized while lifetime receipt is failure and provisioning stays closed; immutable deadlines, exhausted reserve, hour-rounding overrun and no phase/resource extension. |

Use targeted mutations to remove each independent ownership, durability, pending
or timing conjunct. Prove each mutation applied, executed the intended path and
failed its intended regression; a different earlier refusal is not that proof.
Final implementation validation remains locked `just test`, clean current
`just test-full`, nonempty CI and fresh native posted Sol/Astra review. These are
future gates, not claims of tests executed for this design.

## Official provider evidence pinned for design

- Official Cloud OpenAPI, fetched2026-10-27:
  <https://docs.hetzner.cloud/cloud.spec.json>, SHA256
  `0f7f538a1d760d2e09a34a33ab411a9e29b52102721a2c4986aaf51cdb188c5d`.
  The inspected artifact has79,826 lines and152 paths. POST/create/created lines:
  server48715/48919, network34635/34911, firewall5641/5887, SSH-key62530/62621.
  Server48732, firewall5657 and SSH-key response62595 document project-unique
  names; network request/response does not supply that guarantee. All expose ID
  and created; no explicit never-reused-ID or creation-clock accuracy promise was
  found. Neither is a premise of the conditional provenance proof above. Update
  fields expose mutable names/labels, not authenticated nonce proof.
- CLI v1.70.1 shared JSON output is after command completion:
  [base/create.go30,53–74](https://github.com/hetznercloud/cli/blob/v1.70.1/internal/cmd/base/create.go#L30-L74).
  Server wait/GET precedes output at
  [server/create.go116–156](https://github.com/hetznercloud/cli/blob/v1.70.1/internal/cmd/server/create.go#L116-L156);
  network at[57–78](https://github.com/hetznercloud/cli/blob/v1.70.1/internal/cmd/network/create.go#L57-L78),
  firewall at[52–63](https://github.com/hetznercloud/cli/blob/v1.70.1/internal/cmd/firewall/create.go#L52-L63),
  SSH key at[59–66](https://github.com/hetznercloud/cli/blob/v1.70.1/internal/cmd/sshkey/create.go#L59-L66).
- CLI[go.mod15](https://github.com/hetznercloud/cli/blob/v1.70.1/go.mod#L15)
  pins SDK v2.52.0, commit`cea7071cc6c9564a2a51d39da6f0227c1b747a04`.
  [client.go275–285](https://github.com/hetznercloud/hcloud-go/blob/v2.52.0/hcloud/client.go#L275-L285)
  defaults to five retries; the
  [retry handler20–83](https://github.com/hetznercloud/hcloud-go/blob/v2.52.0/hcloud/client_handler_retry.go#L20-L83)
  does not restrict replay by HTTP method. CLI
  [state.go67–148](https://github.com/hetznercloud/cli/blob/v1.70.1/internal/state/state.go#L67-L148)
  does not disable that behavior.
- Raw server creation preserves action identities in
  [schema/server.go130–136](https://github.com/hetznercloud/hcloud-go/blob/v2.52.0/hcloud/schema/server.go#L130-L136).
  [schema/action.go5–20](https://github.com/hetznercloud/hcloud-go/blob/v2.52.0/hcloud/schema/action.go#L5-L20)
  has resources/status/times, not a client request-body digest. Global action
  lookup requires known IDs, not an arbitrary audit listing:
  [action.go144–159](https://github.com/hetznercloud/hcloud-go/blob/v2.52.0/hcloud/action.go#L144-L159),
  OpenAPI line138 (removal2025-01-30).

These sources do not document a finite late-materialization tail, terminal absence
from empty reads, cancellation rollback, timestamp accuracy or adversarial nonce
attestation. Those missing capabilities are explicit boundaries, not inferred
positive guarantees. No paid probe or provider API mutation was used as evidence.

## Review and implementation gate

Review this RFC through the installed native harness: replicas2, every lens/chunk
one `codex/gpt-6.1-sol high` and one `codex/gpt-6-astra high`, verifier Sol-high.
Do not launch ad-hoc reviewers or treat unavailable models as passes. Return the
native report, selected plan/prompts and unresolved provider/threat-model items to
the owner. Only after design ACK and explicit implementation GO should the narrow
tool/test changes begin. Any missing proof actually required by the accepted model remains a paid-launch
blocker; do not invent provider-clock or never-reused-ID requirements.
