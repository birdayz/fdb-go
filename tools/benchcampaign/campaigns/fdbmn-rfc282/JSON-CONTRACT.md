# Public results v2: observed cgroup accounting

The exporter emits `format: fdbmn-rfc282-public-results/v2`. The unchanged M1,
M2, retained comparisons, coverage and provenance sections retain their existing
contracts. The old `server_process_cpu` section is absent; its replacement is
`server_cgroup_accounting`. There is no v1 bounds compatibility mode.

- [Machine-readable section schema](accounting-contract.schema.json)
- [Synthetic section example](accounting-example.json)

The example is a section fixture, **not a complete 44-case campaign result**.
Schema validation covers this section; the exporter additionally enforces full
campaign geometry, eligibility, source pins and semantic population/arithmetic
checks. No actual benchmark result is supplied here.

## Metric and limits

`usage_usec` is a published cumulative scheduler-accounting counter. A difference
between reads is an **observed accounted-counter delta**, not CPU execution
attributable to the requested window or even wholly originating inside the read
bracket. Endpoint scheduler-accounting debt is unquantified. Clock and read
brackets do not bound that debt. Actual execution-window server CPU and its
bounds are unavailable for every row, including an observed zero delta.

There are no server CPU/op, busy-core, rate, lower/upper execution-bound or server
CPU ratio/confidence-interval fields. No interpolation or background subtraction
is applied. Client CPU fields and client paired comparisons elsewhere in the
result are unchanged and must retain their separate measured/lifecycle labels.

## Observed row

An R4-clean case publishes all expected arm/block process observations or none.
Each successful row has exactly these fields:

| Field | Type and meaning |
|---|---|
| `phase`, `case`, `arm`, `block` | Existing process identity; blocks are 0–5. |
| `status` | Literal `observed`; **not** qualification of actual execution CPU. |
| `actual_window_cpu_available` | Literal `false`. |
| `window_kind` | `measured-client-UTC-fences` for M1; `process-lifecycle-UTC` for M2. |
| `requested_window_started_unix_ns`, `requested_window_ended_unix_ns` | Exact integer UTC timestamps used to select observations. |
| `observed_accounted_cpu_usec` | Integer sum of the three node deltas. Authoritative display unit is µs. |
| `observed_accounted_cpu_seconds` | The fleet integer delta divided by 1,000,000. |
| `nodes` | Exactly `node1`, `node2`, `node3`, with the node fields below. |
| `scope` | Fixed disclosure from the schema/example: three node-specific read brackets; debt unquantified. |

Each node has exactly these fields:

| Field | Type and meaning |
|---|---|
| `observed_accounted_cpu_usec`, `observed_accounted_cpu_seconds` | Integer published-counter delta and its seconds conversion. |
| `sample_bracket_started_unix_ns`, `sample_bracket_ended_unix_ns` | First read's before timestamp and last read's after timestamp, as integer UTC ns. |
| `sample_bracket_wall_ns` | Positive integer end minus start. This is elapsed bracket wall, **not** runtime of FDB work. |
| `sample_sequence` | Two integer endpoint sample sequence numbers. |
| `inner_intervals`, `observed_intervals` | Counts for temporal selection only. |
| `temporal_selection_allowance_ns` | Clock/timestamp allowance used in sample selection; **not** an accounting-debt bound. |
| `phase_correlation_range_ns` | Observed realtime-minus-monotonic range in the phase. |
| `processes` | 7 for node1, 5 each for node2/node3. |
| `observed_accounted_user_cpu_seconds`, `observed_accounted_system_cpu_seconds` | Cgroup component deltas if both components are present; otherwise both `null`. Never proc-counter fallback or fabricated zero. |

Fleet values sum integer node deltas **within each process observation**. Nodes
can have different read-bracket endpoints and lengths: no common fleet bracket
or fleet wall/rate is published. A chart may show the median of process-level
fleet deltas and separately show node medians; it must never construct a fleet
median by summing node medians. Keep window-kind and bracket-duration context.
Display integer µs or enough decimal seconds to preserve a nonzero delta (up to
six fractional digits); an observed zero is not a claim of zero actual CPU.

## Unavailable whole case

A missing, malformed or otherwise inadmissible observation for any arm/block
replaces **all** CPU observations of that case with one row containing only:
`phase`, `case`, `status: CPU-unavailable`,
`actual_window_cpu_available: false`, and a public-safe `reason`.
No arm/block or numeric estimate is retained. Original R4 eligibility and the
otherwise eligible latency/throughput/client results are unchanged. Cases
excluded by original R4 remain in global coverage, not in accounting rows.

## Method and protocol

`method.server_cpu` is the fixed object shown in the schema/example: metric
`observed-cgroup-accounting-delta`, source `exclusive-cgroup-v2-usage-usec`, false
actual-window availability, unquantified scheduler debt, sum over node-specific
brackets and unavailable execution attribution.

The sealed protocol must additionally require:

```json
{
  "server_cpu_metric": "observed-cgroup-accounting-delta",
  "server_cpu_actual_window_available": false
}
```

Existing source, 100Hz interval, 17 exclusive units, clock checks, ten inner
intervals and 1.2 maximum sample-span ratio remain required. The latter two are
**temporal-selection checks only**, not proof of execution-window CPU accuracy.
Original R3/R4 and the full 44-case primary matrix remain unchanged. No kernel
profile, perf source or proc-stat accounting-forcing mechanism is introduced.
