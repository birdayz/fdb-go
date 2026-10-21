"""Derived M2 timed-operation rates; lifecycle CPU is a separate scope.

Inputs must SHA-verify every load/text read. No files are opened here. The caller
supplies the frozen bench_analyze.paired_gm; this module has no bootstrap code.
"""
import csv
import io
import math
import re
import statistics


PHASES = {"M2rl": (12, 9), "M2skip": (4, 3)}
ARMS = ("final-ordinary", "final-remote")
BLOCKS = 6
REPEATS = 3
SEED = 492817
RESAMPLES = 20000
CSV_FIELDS = {"phase", "case", "block", "position", "arm", "repeat", "N",
              "ns/op", "B/op", "allocs/op", "rows/op", "prefix-B"}
CPU_SCOPE = "lifecycle GNU time user+system CPU / runner process_wall_ns; includes fixtures/calibration/audits/shutdown"


def need(condition, message):
    if not condition:
        raise ValueError(message)


def number(value, *, positive=False):
    need(not isinstance(value, bool), "boolean numeric input")
    try:
        value = float(value)
    except (TypeError, ValueError, OverflowError) as error:
        raise ValueError("invalid numeric input") from error
    need(math.isfinite(value) and (value > 0 if positive else value >= 0), "invalid numeric input")
    return value


def integer(value):
    need(isinstance(value, (str, int)) and not isinstance(value, bool)
         and str(value).isdecimal(), "invalid integer input")
    return int(value)


def base_row(kind, phase, case, **fields):
    row = dict(record_type=kind, phase=phase, case=case, arm="", block="", position="",
               metric="", throughput_ops_per_second="", cpu_util_cores="",
               lifecycle_cpu_util_cores="", numerator="", denominator="", ratio="",
               ci95_low="", ci95_high="", blocks="", status="clean",
               source_processes="", source_repeat_samples="")
    row.update(fields)
    return row


def time_receipt(text):
    def field(label):
        matches = re.findall(r"^\s*" + re.escape(label) + r":\s*(\S+)\s*$", text, re.MULTILINE)
        need(len(matches) == 1, "missing/duplicate GNU time field: " + label)
        return matches[0]

    user = number(field("User time (seconds)"))
    system = number(field("System time (seconds)"))
    pieces = field("Elapsed (wall clock) time (h:mm:ss or m:ss)").split(":")
    need(len(pieces) in (2, 3), "invalid GNU time elapsed duration")
    seconds = number(pieces[-1])
    need(seconds < 60 and all(piece.isdecimal() for piece in pieces[:-1]), "invalid GNU time elapsed duration")
    minutes = int(pieces[-2])
    need(len(pieces) == 2 or minutes < 60, "invalid GNU time elapsed duration")
    elapsed = seconds + 60 * minutes + (3600 * int(pieces[0]) if len(pieces) == 3 else 0)
    need(elapsed > 0, "nonpositive GNU time elapsed duration")
    percent = field("Percent of CPU this job got")
    need(re.fullmatch(r"\d+%", percent) is not None, "invalid GNU time CPU percentage")
    need(field("Exit status") == "0", "non-successful GNU time process")
    return user, system, elapsed, int(percent[:-1])


def sample_groups(text, phase, jobs):
    reader = csv.DictReader(io.StringIO(text))
    need(len(reader.fieldnames or []) == len(CSV_FIELDS) and set(reader.fieldnames or []) == CSV_FIELDS,
         "wrong M2 metrics CSV schema")
    groups, identities = {}, set()
    for row in reader:
        need(None not in row and all(value is not None for value in row.values()), "malformed M2 CSV row")
        block, position, repeat, count = (integer(row[key]) for key in ("block", "position", "repeat", "N"))
        key = (row["case"], block, row["arm"])
        need(row["phase"] == phase and key in jobs, "unknown M2 sample identity")
        need(position == jobs[key]["position"] and repeat in range(REPEATS) and count > 0,
             "M2 sample position/repeat/N differs")
        identity = (*key, repeat)
        need(identity not in identities, "duplicate M2 sample")
        identities.add(identity)
        value = dict(repeat=repeat, N=count)
        for metric in CSV_FIELDS - {"phase", "case", "block", "position", "arm", "repeat", "N"}:
            value[metric] = number(row[metric], positive=metric in ("ns/op", "rows/op", "prefix-B"))
        groups.setdefault(key, []).append(value)
    return groups


def collect(inputs, paired_gm):
    """Return process/absolute/paired/omitted CSV dictionaries and method metadata.

    All paths passed to inputs.load/text are relative to this campaign's root.
    Paired records contain either timed throughput or explicitly lifecycle CPU;
    cpu_util_cores (measured-window CPU) stays empty for every M2 record.
    """
    consumed = set()

    def load(path):
        consumed.add(path)
        return inputs.load(path)

    def text(path):
        consumed.add(path)
        return inputs.text(path)

    process_rows, omitted_rows, populations = [], [], []
    phase_counts = {}
    for phase, (planned_count, clean_count) in PHASES.items():
        raw = "results-raw/" + phase
        plan = load(raw + "/plan.json")
        report = load("analysis/measured-492817/" + phase + "/report.json")
        reconciliation = load("reconcile/" + phase + "/report.json")
        need(plan["phase"] == report["phase"] == reconciliation["phase"] == phase,
             "M2 phase differs")
        need(reconciliation["reconciliation_complete"] is True, "incomplete R4 reconciliation")
        cases = {case["key"]: case for case in plan["cases"]}
        r4 = {case["case"]: case for case in reconciliation["cases"]}
        need(len(cases) == len(plan["cases"]) == planned_count, "wrong/duplicate planned M2 cases")
        need(len(r4) == len(reconciliation["cases"]) and set(cases) == set(r4) == set(report["cases"]),
             "M2 case population differs")
        orders = plan["orders"]
        need(report["orders"] == orders and len(orders) == BLOCKS
             and all(len(order) == 2 and set(order) == set(ARMS) for order in orders)
             and sum(order[0] == ARMS[0] for order in orders) == 3, "wrong M2 balanced block orders")
        jobs, run_ids = {}, set()
        for job in plan["jobs"]:
            key = (job["case"], job["block"], job["arm"])
            need(job["case"] in cases and type(job["block"]) is int and job["block"] in range(BLOCKS)
                 and job["arm"] in ARMS and job["position"] == orders[job["block"]].index(job["arm"]),
                 "invalid planned M2 job")
            need(key not in jobs and job["id"] not in run_ids
                 and isinstance(job["id"], str) and re.fullmatch(r"[A-Za-z0-9_-]+", job["id"]) is not None,
                 "duplicate/invalid M2 run identity")
            jobs[key] = job
            run_ids.add(job["id"])
        need(set(jobs) == {(case, block, arm) for case in cases for block in range(BLOCKS) for arm in ARMS},
             "incomplete planned M2 process matrix")
        groups = sample_groups(text(raw + "/metrics.csv"), phase, jobs)
        clean = 0
        for case, spec in cases.items():
            result = report["cases"][case]
            clean_r4 = r4[case]["verdict"] == "clean"
            need((result["status"] == "ok") == clean_r4, "R4/current analysis eligibility differs")
            case_groups = {key: value for key, value in groups.items() if key[0] == case}
            if not clean_r4:
                omitted_rows.append(base_row("omitted", phase, case, status="N/A",
                                             r4_verdict=r4[case]["verdict"], analysis_status=result["status"],
                                             reason="whole case excluded by retained R4/current analysis; no partial-block salvage",
                                             source_processes=len(case_groups),
                                             source_repeat_samples=sum(len(value) for value in case_groups.values()),
                                             placement_label=spec["placement_label"], placement_fraction=spec["placement_fraction"]))
                continue
            clean += 1
            need(not r4[case]["contaminated_reasons"] and not r4[case]["unverifiable_reasons"],
                 "clean R4 case has adverse reasons")
            expected_ids = {jobs[case, block, arm]["id"] for block in range(BLOCKS) for arm in ARMS}
            actual_ids = [run["run_id"] for run in r4[case]["runs"]]
            need(len(actual_ids) == len(expected_ids) == 12 and set(actual_ids) == expected_ids,
                 "R4 clean process population differs")
            need(result["placement_label"] == spec["placement_label"]
                 and result["placement_fraction"] == spec["placement_fraction"], "M2 placement label differs")
            comparison = [item for item in report["comparisons"] if item["case"] == case
                          and item["metric"] == "ns/op" and item["numerator"] == ARMS[1]
                          and item["denominator"] == ARMS[0]]
            need(len(comparison) == 1 and comparison[0]["status"] == "ok"
                 and comparison[0]["blocks"] == BLOCKS and comparison[0]["raw_samples_per_arm"] == 18,
                 "missing exact M2 latency comparison population")
            process = {}
            for block in range(BLOCKS):
                for arm in ARMS:
                    job = jobs[case, block, arm]
                    repeats = sorted(groups.get((case, block, arm), []), key=lambda value: value["repeat"])
                    need(len(repeats) == REPEATS and [row["repeat"] for row in repeats] == [0, 1, 2],
                         "incomplete M2 three-repeat process")
                    latencies = [row["ns/op"] for row in repeats]
                    stored = comparison[0]["block_medians_numerator" if arm == ARMS[1] else "block_medians_denominator"]
                    need(len(stored) == BLOCKS and statistics.median(latencies) == stored[block],
                         "M2 raw CSV/stored process median differs")
                    expected_rows = result["absolutes"][arm]["rows/op"]["median_of_process_repeat_medians"]
                    need(all(row["rows/op"] == expected_rows for row in repeats), "M2 returned-row audit differs")
                    folder = raw + "/" + job["id"]
                    timing = load(folder + "/result.json")
                    need(timing["status"] == "ok" and timing["returncode"] == 0
                         and timing["id"] == timing["run_id"] == job["id"]
                         and all(timing[key] == job[key] for key in ("case", "arm", "block", "position")),
                         "M2 successful process receipt identity differs")
                    user = number(timing["process_user_cpu_seconds"])
                    system = number(timing["process_system_cpu_seconds"])
                    need(type(timing["process_wall_ns"]) is int, "noninteger M2 process wall")
                    wall = number(timing["process_wall_ns"], positive=True)
                    time_user, time_system, time_elapsed, time_percent = time_receipt(text(folder + "/process-time.txt"))
                    need(user == time_user and system == time_system and user + system > 0,
                         "GNU time/process CPU receipt differs or is zero")
                    need("includes fixtures/calibration/audits/shutdown" in timing["cpu_scope"],
                         "unrecognized M2 process CPU scope")
                    rates = [1e9 / value for value in latencies]
                    cores = (user + system) / (wall / 1e9)
                    row = base_row("process", phase, case, arm=arm, block=block, position=job["position"],
                                   throughput_ops_per_second=statistics.median(rates),
                                   lifecycle_cpu_util_cores=cores, blocks=1, source_processes=1,
                                   source_repeat_samples=3, source_run_id=job["id"],
                                   repeat_indices=[value["repeat"] for value in repeats],
                                   repeat_N=[value["N"] for value in repeats], repeat_ns_per_op=latencies,
                                   repeat_throughput_ops_per_second=rates,
                                   repeat_rows_per_op=[value["rows/op"] for value in repeats],
                                   process_user_cpu_seconds=user, process_system_cpu_seconds=system,
                                   process_wall_ns=timing["process_wall_ns"],
                                   gnu_time_elapsed_seconds=time_elapsed, gnu_time_percent_cpu=time_percent,
                                   placement_label=spec["placement_label"], placement_fraction=spec["placement_fraction"])
                    process_rows.append(row)
                    process[block, arm] = row
            populations.append((phase, case, spec, orders, process))
        need(clean == clean_count, "wrong eligible M2 case population")
        phase_counts[phase] = dict(planned_cases=planned_count, clean_cases=clean, omitted_cases=planned_count-clean)
    need(len(populations) == 12 and len(process_rows) == 144 and len(omitted_rows) == 4,
         "incomplete M2 whole-campaign population")

    # No callback is invoked until every private source/population check passes.
    absolute_rows, paired_rows = [], []
    for phase, case, spec, orders, process in populations:
        for arm in ARMS:
            arm_processes = [process[block, arm] for block in range(BLOCKS)]
            rates = [row["throughput_ops_per_second"] for row in arm_processes]
            raw_rates = [value for row in arm_processes for value in row["repeat_throughput_ops_per_second"]]
            cores = [row["lifecycle_cpu_util_cores"] for row in arm_processes]
            absolute_rows.append(base_row("absolute", phase, case, arm=arm,
                                         throughput_ops_per_second=statistics.median(rates),
                                         lifecycle_cpu_util_cores=statistics.median(cores), blocks=BLOCKS,
                                         source_processes=6, source_repeat_samples=18,
                                         process_throughput_ops_per_second=rates,
                                         process_lifecycle_cpu_util_cores=cores,
                                         raw_repeat_throughput_median=statistics.median(raw_rates),
                                         minimum_repeat_throughput=min(raw_rates), maximum_repeat_throughput=max(raw_rates),
                                         placement_label=spec["placement_label"], placement_fraction=spec["placement_fraction"]))
        for metric in ("throughput_ops_per_second", "lifecycle_cpu_util_cores"):
            numerator = [process[block, ARMS[1]][metric] for block in range(BLOCKS)]
            denominator = [process[block, ARMS[0]][metric] for block in range(BLOCKS)]
            ratios = [n / d for n, d in zip(numerator, denominator)]
            summary = paired_gm(ratios, resamples=RESAMPLES, seed=SEED)
            interval = summary["nominal_95_interval"]
            need(summary["blocks"] == BLOCKS and summary["resamples"] == RESAMPLES and summary["seed"] == SEED
                 and len(interval) == 2 and number(interval[0], positive=True) <= number(interval[1], positive=True),
                 "paired_gm callback contract differs")
            paired_rows.append(base_row("paired", phase, case, metric=metric, numerator=ARMS[1], denominator=ARMS[0],
                                       ratio=number(summary["gm"], positive=True), ci95_low=interval[0], ci95_high=interval[1],
                                       blocks=BLOCKS, source_processes=12, source_repeat_samples=36,
                                       block_values_numerator=numerator, block_values_denominator=denominator,
                                       block_ratios=ratios, resamples=RESAMPLES, seed=SEED,
                                       numerator_first_blocks=sum(order[0] == ARMS[1] for order in orders),
                                       denominator_first_blocks=sum(order[0] == ARMS[0] for order in orders),
                                       ratio_gt_1_means="numerator has more timed operations/s" if metric == "throughput_ops_per_second"
                                       else "numerator uses more lifecycle average CPU cores; not measured-window CPU",
                                       placement_label=spec["placement_label"], placement_fraction=spec["placement_fraction"]))
    metadata = dict(phases=phase_counts, eligible_cases=12, omitted_cases=4, process_rows=144,
                    absolute_rows=24, paired_rows=24, source_repeat_samples=432,
                    independent_paired_blocks=BLOCKS, repeats_clustered_per_process=REPEATS,
                    seed=SEED, resamples=RESAMPLES, paired_api="frozen bench_analyze.paired_gm",
                    throughput_definition="1e9 / printed testing.B ns/op; median of 3 repeat rates per process, then median of 6 process rates",
                    throughput_scope="timed benchmark scan/plan executions, excluding stopped-timer validation; not capacity",
                    measured_window_cpu_available=False, cpu_util_cores="unavailable; intentionally empty in every M2 row",
                    lifecycle_cpu_definition=CPU_SCOPE,
                    lifecycle_CPU_wall="runner process_wall_ns; GNU time elapsed is separately retained, not substituted",
                    historical_comparisons="none; current R4-clean/current-analysis-ok cohort only",
                    inference="nominal exploratory 95% paired-block intervals; no multiplicity, equivalence or capacity claim",
                    input_paths=sorted(consumed))
    return process_rows + absolute_rows + paired_rows + omitted_rows, metadata
