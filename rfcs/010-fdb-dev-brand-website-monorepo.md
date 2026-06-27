# RFC 010: `fdb.dev` — Brand, Website, and the Monorepo Re-framing

**Status:** Reviewed — D3 resolved 2026-06-27 (vanity `fdb.dev` imports kept). Remaining framing
decisions (roadmap placement, launch hero) pending author sign-off.
**Date:** 2026-06-27
**Author:** birdayz
**Reviewers:** Linus Torvalds, a random FAANG SWE, an OpenAI hiring manager, Sam Altman, Tyler Rockwood

---

## Summary

Re-frame the project from "a Go port of the Record Layer" to **"FoundationDB for Go"** — the
home for the whole FDB stack in Go — and give it a public face at **`fdb.dev`**.

Concretely:

1. **Brand.** Domain `fdb.dev` is the umbrella. Tagline: *"FoundationDB is all the database you
   need."* Component names stay descriptive (the client, the Record Layer, the SQL engine). No
   coined product name.
2. **Repo.** Rename `birdayz/fdb-record-layer-go` → **`birdayz/fdb-go`** (done — it's a monorepo,
   not just the record layer). GitHub keeps redirects from the old name.
3. **Go module path.** Move from `github.com/birdayz/fdb-record-layer-go` → **`fdb.dev`** (vanity
   import). Imports become `fdb.dev/pkg/fdbgo`, forward-compatible with a later `fdb.dev/client`,
   `fdb.dev/layers/recordlayer` restructure.
4. **Website.** A Hugo + hextra static site under `website/`: landing, docs, changelog,
   nightly-performance dashboard, and the Go vanity-import stubs. Deployed to GitHub Pages.
5. **Narrative.** The **layer model**: FDB does distributed strict-serializable ACID once; every
   data model is a layer. Shipping today: client, Record Layer, SQL. Roadmap: a lakehouse/OLAP
   layer (S3 for data, FDB for the transactional catalog), a vector index, more.

## Motivation

The pure-Go FDB client is a genuinely strong, underexposed result: it reimplements the FDB wire
protocol from scratch, runs with `CGO_ENABLED=0`, produces byte-identical reads/writes vs Apple's
C client, and is 2–4× faster on the read path. That deserves a real front door, not a README badge.
`fdb.dev` is available and exactly on-theme. The launch target is a Hacker News / FAANG-SWE
audience; the deliverable must be fast, dense, dark, and honest — that audience rewards substance
and punishes marketing.

## Decisions

### D1 — Brand: `fdb.dev` umbrella, descriptive names
Tagline *"FoundationDB is all the database you need."* The site, not a product name, is the brand.
Trademark note: "FDB for Go" / "for FoundationDB" is defensible community usage; the site must not
imply Apple endorsement.

### D2 — Repo name: `fdb-go`
`-go` suffix is the Go-ecosystem convention; "fdb" abbreviation keeps trademark distance vs
spelling out "foundationdb". Independent of the import path.

### D3 — Module path: `fdb.dev` (vanity import)
Single module rooted at the domain, so one `go-import` meta value covers every subpackage. Requires
(a) the code repo to be public (so `go get` can clone unauthenticated) and (b) a repo-wide rename
of ~1,354 files + `go.mod` + gazelle prefix + `buf.gen.yaml` go_package_prefix + proto
`go_package` + generated `gen/`. **Contested — see review.**

### D4 — Hero framing: the full stack, no roadmap
Lead with breadth (client + Record Layer + SQL), benchmark table as immediate proof.
**Resolved 2026-06-27:** the panel was unanimous and the author agreed — **all roadmap content is
removed from the site** (no dimmed cards, no `/roadmap` page). The site talks only about what ships
today. The "all the database you need" thesis and the layer-model framing stay (worldview, not a
feature promise).

### D5 — Hosting: GitHub Pages, private now → public at launch
Repo is currently PRIVATE. GH Pages (free tier) and `go get` both need a public repo. Plan: prep
everything on this branch (workflow, CNAME, vanity generator, module rename), keep private, preview
locally, flip public on launch day.

### D6 — Tech: Hugo + hextra, vendored theme
hextra vendored under `website/themes/hextra` (stripped of its `go.mod`/docs so it can't pollute
`go.work`/gazelle); `website/` added to `.bazelignore`. Precompiled CSS — no npm in the build.

### D7 — Deployment
GitHub Actions: on push to `master` (path-filtered to `website/**` + `CHANGELOG.md`), install Hugo
extended, copy `CHANGELOG.md` → `content/changelog.md` (single source of truth), `hugo --minify
--baseURL https://fdb.dev/`, `upload-pages-artifact` → `deploy-pages`. `website/static/CNAME` =
`fdb.dev`. DNS: four A records → `185.199.108–111.153`, four AAAA → `2606:50c0:8000–8003::153`,
`www` CNAME → `birdayz.github.io`.

### D8 — Nightly performance
Raw reports stay on Hetzner object storage (they grow nightly; GH Pages' ~1 GB/100 GB soft limits
make it the wrong home). `/performance` fetches the latest JSON and renders it.

## The layer model (narrative spine)

- **Foundation:** the pure-Go client. No cgo, faster reads, validated against libfdb_c 7.3.77.
- **Shipping layers:** Record Layer (wire-compatible with Java RL 4.12.11.0), SQL engine (Cascades).
- **Roadmap layers:**
  - **Lakehouse / OLAP** — columnar files in S3, transactional catalog in FDB (manifests,
    snapshot/MVCC versions, file stats, the commit protocol). Real multi-file ACID via FDB's
    strict-serializable transactions. *This is roughly the Snowflake architecture; Snowflake runs
    its metadata on FoundationDB.*
  - **Vector index** — ANN (SPFresh/SPANN) as a record-layer index; embeddings next to records.
  - **More** — queues, documents, time-series.

## Risks / open questions

- R1. Vanity import + module rename: churn and a new runtime dependency (DNS) for cosmetic gain.
- R2. "All the database you need" + roadmap cards risk reading as vaporware to the launch audience.
- R3. "Faster than the C client" must be scoped precisely or FDB-literate readers will pick it apart.
- R4. Protocol-version pinning: the FDB wire protocol is not a stable third-party contract; pinned
  to 7.3 and will need work for 8.0.
- R5. Bus factor / production-use / "who's behind it" — unanswered on the site today.

---

# Review Panel

> Each reviewer was asked to be critical and specific. Verdicts are theirs.

## Linus Torvalds — *code quality, BS detection*

The client is the only part of this RFC I actually respect, and it's buried under marketing. You
reimplemented a wire protocol, it's `CGO_ENABLED=0`, it's faster, and you *proved byte-identical
output with a differential suite*. That's real engineering. Lead with it. Everything else in here is
either fine or noise.

The noise: "FoundationDB is all the database you need" is a slogan, not a claim you can defend, and
the three dimmed "ROADMAP" cards are vaporware with a CSS opacity trick. Don't ship promises on a
landing page. A roadmap card for something you haven't written is a liability — it tells me you'd
rather advertise than build. Cut them from the landing or put them behind a `/roadmap` link where
nobody mistakes them for features. The OLAP pitch leaning on "Snowflake uses FDB" is borrowing
someone else's credibility; you haven't written a single columnar reader.

D3 is the one I'd actually NAK as written. You want to rewrite 1,354 files and take a permanent
runtime dependency on a DNS name resolving forever, so that imports read `fdb.dev` instead of
`github.com/birdayz/fdb-go`. The day that domain lapses or the meta tag breaks, *every build
everywhere* fails, and GitHub's own redirect already makes the repo rename free. Vanity imports buy
you a logo on your import line and cost you a single point of failure. If you do it anyway, fine, but
don't pretend it's free.

Naming: you now have `fdb-go` (repo), `fdb.dev` (import + site), and a heritage of "record layer."
Three strings. Survivable, others do it, but every one is a chance to confuse someone.

**Verdict: Conditional.** Land the client story honestly, kill or wall off the roadmap vaporware,
and justify D3 against the DNS-SPOF or drop it.

## A random FAANG SWE — *the HN comment section*

Saw "pure-Go FDB client, no cgo, faster than the C binding" and reflexively starred it. The cgo pain
is real — I've lost days to `libfdb_c` in CI containers and cross-compiles. If this just works with
`CGO_ENABLED=0`, that alone is worth a blog post.

But here's what I'd post in the thread: (1) *Who's behind this and is it used in prod anywhere?* One
maintainer, pre-1.0, reimplementing a wire protocol I'd be betting my on-call on — that's a hard
sell over Apple's client. (2) *Faster how?* Localhost microbenchmarks are noise; the C client does a
ton (network thread, multi-version client, TLS). The 10 ms-RTT number is the only one I believe —
lead with that. (3) The "all the database you need / full stack" framing makes me trust it *less*,
not more. It pattern-matches to every "we reinvented the database" post that flames out. A KV client
with a record layer is a great, focused thing. "OLAP roadmap" on the landing makes me think you'll
spread thin and the client will rot.

I'd upvote a post titled *"I reimplemented the FoundationDB client in pure Go and it's faster."* I'd
skip *"fdb.dev: the only database you'll ever need."*

**Verdict: Ship the client as its own story.** The breadth framing is working against you with my
crowd.

## OpenAI hiring manager — *engineering-signal lens*

As a signal of caliber, the core here is excellent: a from-scratch wire-protocol implementation, a
Cascades optimizer ported from Java, and — the part most people skip — *deterministic conformance
and differential testing against the reference*. That's staff-level systems work and exactly the
rigor I screen for. The bug-find stories from the differential harness would be the best thing on
your site; surface them.

My worry is dilution. Six things at 60% read worse than one thing at 100%. The roadmap (vector,
OLAP, queue) signals ambition outrunning evidence. For a credibility artifact, depth wins: the
client + the wire-compat proof + the testing methodology is a hire; the kitchen-sink vision is a
candidate who lists ten languages on their résumé. The vector-layer nod (SPFresh/SPANN, embeddings
beside records) is genuinely relevant to where infra is going — but only cite it when it runs.

**Verdict: Focus it.** Foreground the methodology, not the roadmap.

## Sam Altman — *strategy / altitude*

Decide what this is. "All the database you need" is a *company-sized* claim and what you have is a
*library*. Both are legitimate, but they imply different roadmaps, and right now you're hedging.

If it's a library: trim the vision, own the wedge — "the best way to use FoundationDB from Go" — and
win the Go-shops-adjacent-to-Java-FDB niche. Clean, defensible, done.

If it's a platform: then the layer model is the thesis and it's a good one — wire-compat creates a
real network effect (Go and Java services on one cluster), and the *why-now* is AI: embeddings,
agent memory, and transactional state living next to your vectors in one ACID store. That's the
story that raises money and pulls talent. But a platform isn't dim cards on a landing page; it's a
team and a funded multi-year build. A feature ("faster client") is not a moat — Apple could ship a
Go client and erase it. The moat is the *ecosystem of layers* plus the wire-compat lock-in.

Don't undersell. If you actually believe "all the database you need," write down what would have to
be true and go do that. If you don't, stop saying it.

**Verdict: Pick your altitude before you pick your hero copy.**

## Tyler Rockwood — *FDB domain truth-check*

I want this to exist, so let me try to break the claims before HN does.

**Wire compatibility.** "Wire-compatible" is a big claim. Which protocol version — and do you mean
the *data* format (keys, values, the Record Layer encoding) or the *transaction* protocol (GRV from
the proxies, commit through resolvers, read/write conflict *ranges*, versionstamps, the `\xff`
system keyspace, metadata-version key, `get_addresses_for_key`, tenants)? The FDB transaction
protocol is **not a stable third-party contract** — it moves between releases. You're pinned to
7.3.77; say so loudly, and say that 8.0 is future work. Otherwise someone points a 7.4 cluster at it
and files a "you lied" issue.

**"Faster than the C client."** Be precise or it reads as benchmark-gaming. The Apple client carries
the multi-version-client shim, a dedicated network thread + futures, TLS, trace/metrics, and
load-aware routing. If you're faster, is it because Go's runtime beats the C client's thread-hop on
localhost, or because you've skipped MVC/TLS? Both are fine answers — but state it. And drop the
localhost row to a footnote: it's syscall/IPC-bound. The netem-RTT numbers are the honest, defensible
ones; those are your headline.

**OLAP on FDB + S3.** The architecture is sound — it's broadly how large FDB metadata deployments
plus blob storage work — but your one-paragraph version hand-waves the parts that are actually hard:
read-version latency on the catalog hot path; the 5 s transaction limit forcing chunked commits for
big compactions; **manifest hotspotting** (monotonic version keys land on one shard — you'll need to
randomize the keyspace); the 10 MB-tx / 100 KB-value limits forcing manifest sharding; mapping FDB's
read version to snapshot isolation over immutable S3 files. And the non-negotiable discipline: *data
never goes in FDB, only pointers + stats* — Snowflake doesn't, neither can you. The Iceberg/Delta
dunk ("no object-store CAS hacks") is fair on atomicity, but they ship multi-region and an open
ecosystem you won't have on day one. Cite Snowflake's FDB metadata usage (it's public) but don't
imply parity — that's a decade of hardening.

Get the scoping right and the FDB community — exactly the people you want — will champion this.
Overclaim and the same people will take it apart line by line.

**Verdict: Scope the wire-compat and perf claims precisely; list the OLAP constraints. Then I'm in.**

---

# Synthesis — what the panel converged on

1. **Roadmap vaporware (Torvalds, FAANG, hiring-mgr):** remove the dimmed roadmap cards from the
   landing or move them behind an explicit `/roadmap` page. Don't let unbuilt layers sit next to
   shipped ones.
2. **Lead with the client (Torvalds, FAANG):** for the *launch post* specifically, the headline is
   the pure-Go/no-cgo/faster client — even though the *site* keeps the full-stack story. The thesis
   stays; the launch wedge narrows.
3. **Scope the claims (Tyler):** state the pinned protocol version (7.3.77) and that 8.0 is future
   work; explain *why* it's faster (be explicit about MVC/TLS); make the netem-RTT numbers the
   headline and demote the localhost row to a footnote.
4. **Surface the methodology (hiring-mgr):** give the differential/conformance testing and its
   bug-find stories real estate — it's the strongest credibility signal we have.
5. **Re-examine D3 (Torvalds):** the vanity import + 1,354-file rename takes a permanent DNS
   dependency for cosmetic gain; the repo rename is already free via GitHub redirects. Decision
   needed: accept the SPOF, mitigate it, or drop vanity imports and import `github.com/birdayz/fdb-go`.
6. **Pick the altitude (Altman):** library vs platform — decide before finalizing hero copy.
7. **Answer the trust questions (FAANG):** add bus-factor / maturity / "who's behind it" to the docs.

## Proposed changes adopted (pending author sign-off)

- **D4 → resolved:** roadmap removed from the site entirely. Landing keeps the layer-model thesis
  and the three shipping layers only. Launch post leads with the client.
- **Perf copy → revised:** add protocol-version scoping and the "why faster" explanation; lead with
  netem-RTT, footnote localhost. (Already partly honest; tighten per Tyler.)
- **OLAP copy → revised:** keep the pitch, add the constraints (metadata-only discipline, tx limits,
  manifest hotspotting) so it reads as engineering, not marketing.
- **D3 → resolved:** vanity `fdb.dev` imports kept (proxy caching mitigates the DNS-SPOF). The
  module rename is unblocked.

## Next steps (unblocked regardless of D3)

1. Deploy workflow + `CNAME` + DNS doc (D7).
2. Docs skeleton: client, record-layer, SQL guides; add maturity/bus-factor page.
3. `/performance` wired to the reports bucket (D8).
4. Vanity stub generator + module rename to `fdb.dev` (D3 resolved — unblocked).
