#!/usr/bin/env python3
"""Two preregistered native arms in one immutable, bounded two-host lease."""
import argparse
import copy
import json
from pathlib import Path

import diagnostic as n
import driver as d
import policy as p

CASES = ("capacity16g", "baseline")
SESSION_ROWS = 2200000
RECEIPT_BUDGET = 1024 ** 3


def case_plan(lease, profile):
    p.require(type(lease) is dict and lease.get("schema") == "otel_native_session_v1"
              and lease.get("cases") == list(CASES) and profile in CASES, "preregistered session case required")
    value = {key: copy.deepcopy(value) for key, value in lease.items() if key not in ("schema", "cases", "prior_rows_upper")}
    value.update(schema="otel_native_diagnostic_v1", storage_memory_profile=profile)
    return value


def validate_plan(plan, *, check_assets=True):
    p.require(type(plan) is dict and plan.get("schema") == "otel_native_session_v1"
              and plan.get("approved") is True, "explicit native session approval required")
    p.require(type(plan.get("cases")) is list and plan["cases"] == list(CASES), "exact ordered session cases required")
    p.require(type(plan.get("storage_process_count")) is int and plan["storage_process_count"] == 2
              and type(plan.get("storage_max_outstanding")) is int and plan["storage_max_outstanding"] == 64,
              "session requires two storage processes at default I/O concurrency")
    p.require("storage_memory_profile" not in plan, "session memory profiles belong only to its exact cases")
    prior = plan.get("prior_rows_upper")
    p.require(type(prior) is int and prior >= 0 and prior + SESSION_ROWS <= 10000000, "session cumulative 10M row ceiling")
    for profile in CASES:
        n.validate_plan(case_plan(plan, profile), check_assets=check_assets)
    return plan


def classify_arm_failure(error, document, layout, wave, *, fatal_guards_passed):
    """A termination decision, never a health waiver. Unknown shapes are fatal."""
    if (type(error) is not ValueError or str(error) != "strict client health" or fatal_guards_passed is not True
            or type(layout) is not dict or type(wave) is not dict or wave.get("release_bound") is not True
            or wave.get("observation_phase") != "load" or wave.get("error") != "strict client health"
            or type(wave.get("profile")) is not dict or wave["profile"].get("producer_process_bound") is not True):
        return False
    receipt = wave.get("producer_process_receipt")
    exits = wave.get("sampler_exits")
    errors = wave.get("collection_errors", [])
    roles = n.ROLES
    storage_population = layout.get("storage_process_count", 1)
    if layout.get("schema") == "otel_native_scaling_v1":
        hosts = layout.get("storage_hosts")
        if (type(hosts) is not list or hosts not in (["node1"], ["node1", "node2"], ["node1", "node2", "node3"])
                or type(storage_population) is not int or storage_population != 3):
            return False
        roles = {kind + str(i) for i in range(1, len(hosts) + 1) for kind in ("node", "client")}
        storage_population *= len(hosts)
        receipts = wave.get("producer_process_receipts")
        clients = {role for role in roles if role.startswith("client")}
        if (type(receipts) is not dict or set(receipts) != clients or receipt != receipts.get("client1")
                or any(type(value) is not dict or value.get("started") is not True
                       or not p.integer(value.get("pid"), 1) or not p.integer(value.get("starttime_ticks"), 1)
                       for value in receipts.values())):
            return False
    if (type(receipt) is not dict or receipt.get("started") is not True
            or not p.integer(receipt.get("pid"), 1) or not p.integer(receipt.get("starttime_ticks"), 1)
            or type(exits) is not dict or set(exits) != roles
            or not all(type(value) is int and value == 0 for value in exits.values())
            or type(errors) is not list or errors):
        return False
    try:
        client, cluster = document["client"], document["cluster"]
        if (client["database_status"] != {"available": True, "healthy": False}
                or type(client["database_status"]["available"]) is not bool
                or type(client["database_status"]["healthy"]) is not bool or client["messages"] != []
                or cluster["messages"] != [{"name": "batch_priority_transaction_start_probe_timeout",
                                            "description": "Unable to start batch priority transaction after 5 seconds."}]):
            return False
        if (client["cluster_file"]["path"] != layout["remote_cluster_file"]
                or client["cluster_file"]["up_to_date"] is not True
                or client["coordinators"]["quorum_reachable"] is not True):
            return False
        coordinators = client["coordinators"]["coordinators"]
        expected_coordinators = {x["address"] for x in layout["processes"] if x["port"] == 4500}
        if (type(coordinators) is not list or len(coordinators) != len(expected_coordinators)
                or {x["address"] for x in coordinators} != expected_coordinators
                or not all(x["reachable"] is True for x in coordinators)):
            return False
        if (cluster["database_available"] is not True or cluster["full_replication"] is not True
                or cluster["recovery_state"]["name"] != "fully_recovered"
                or type(cluster["degraded_processes"]) is not int or cluster["degraded_processes"] != 0
                or cluster["incompatible_connections"] != [] or cluster["data"]["state"]["healthy"] is not True
                or cluster["data"]["state"]["name"] not in ("healthy", "healthy_repartitioning")):
            return False
        trackers = cluster["data"]["team_trackers"]
        if (type(trackers) is not list or not trackers
                or not all(x["state"]["healthy"] is True and type(x["unhealthy_servers"]) is int
                           and x["unhealthy_servers"] == 0 for x in trackers)):
            return False
        processes = cluster["processes"]
        if (type(processes) is not dict or not all(x["excluded"] is False and x["version"] == "7.3.77"
                                                  and x["messages"] == [] for x in processes.values())):
            return False
        storage_by_address = {process["address"]: role["id"] for process in processes.values()
                              for role in process["roles"] if role["role"] == "storage"}
        storage_ids = list(storage_by_address.values())
        if (len(storage_ids) != storage_population
                or not all(type(x) is str and x for x in storage_ids) or len(set(storage_ids)) != len(storage_ids)):
            return False
        history = wave.get("native_activity")
        if type(history) is not list or not history:
            return False
        for sample in history:
            observed = sample["storage_processes"]
            if (type(observed) is not dict or set(observed) != set(storage_by_address)
                    or any(observed[address]["storage_id"] != storage_id for address, storage_id in storage_by_address.items())):
                return False
        for key in ("performance_limited_by", "batch_performance_limited_by"):
            reason = cluster["qos"][key]
            if (type(reason) is not dict or set(reason) != {"description", "name", "reason_id", "reason_server_id"}
                    or reason["name"] != "storage_server_write_queue_size" or type(reason["reason_id"]) is not int
                    or reason["reason_id"] != 1 or reason["reason_server_id"] not in storage_ids
                    or reason["description"] != "Storage server performance (storage queue)."):
                return False
        if set(cluster["latency_probe"]) != {"commit_seconds", "immediate_priority_transaction_start_seconds",
                                               "read_seconds", "transaction_start_seconds"}:
            return False
        # Validate every remaining original health/identity/resource rule. The
        # original failed document is retained unchanged, never used as a PASS.
        projection = copy.deepcopy(document)
        projection["client"]["database_status"]["healthy"] = True
        projection["cluster"]["messages"] = []
        p.strict_status(projection, layout)
        return True
    except (AttributeError, KeyError, TypeError, ValueError):
        return False


class Session:
    def __init__(self, plan, out, provision, *, arm_factory=None):
        self.plan, self.out, self.provision = plan, Path(out), provision
        self.provision_out = self.out / "provision"
        self.arm_factory = arm_factory or n.Diagnostic
        self.resources = None
        self.trace_remaining = n.TRACE_BUDGET
        self.observation_history = {}

    def receipt_budget(self):
        p.require(sum(x.stat().st_size for x in self.out.rglob("*") if x.is_file()) < RECEIPT_BUDGET,
                  "session receipt allocation exceeds 1GiB")

    def run(self):
        validate_plan(self.plan)
        source = p.verify_source(self.plan)
        self.out.mkdir(mode=0o700, parents=True, exist_ok=False)
        d.write(self.out / "approved-plan.json", self.plan)
        d.write(self.out / "source-before.json", source)
        result = {"schema": "otel_native_session_result_v1", "diagnostic_only": True, "accepted_throughput": False,
                  "authorized_rows": SESSION_ROWS, "cumulative_rows_upper": self.plan["prior_rows_upper"] + SESSION_ROWS,
                  "first_epoch": self.plan["first_epoch"], "deadline_epoch": self.plan["deadline_epoch"],
                  "approved_plan_sha256": p.sha(self.out / "approved-plan.json"), "cases": [],
                  "all_cases_passed": False, "failed_cases": [], "status": "SESSION_FATAL"}
        try:
            self.receipt_budget()
            d.remaining(self.plan, reserve=2000)
            self.resources = self.provision.prepare(self.plan, self.provision_out)
            p.require(set(self.resources["nodes"]) == n.ROLES, "session exact two owned nodes")
            ids = [node["id"] for node in self.resources["nodes"].values()]
            owned = self.resources.get("ids", {}).get("server")
            protected = self.resources.get("protected_ids", {}).get("server")
            p.require(self.resources.get("prepared") is True and self.resources.get("campaign") == self.plan["campaign"]
                      and self.resources.get("prefix") == self.plan["prefix"] and all(type(x) is int and x > 0 for x in ids)
                      and len(set(ids)) == 2 and type(owned) is list and len(owned) == 2 and set(owned) == set(ids)
                      and type(protected) is list and not set(ids) & set(protected), "session exact new unprotected resource identities")
            for index, profile in enumerate(CASES):
                self.receipt_budget()
                arm_plan = case_plan(self.plan, profile)
                case_out = self.out / "cases" / profile
                record = {"name": profile, "status": "SESSION_FATAL", "diagnostic_result": str(case_out / "diagnostic-result.json")}
                result["cases"].append(record)
                arm = self.arm_factory(arm_plan, case_out, self.provision, provision_out=self.provision_out,
                                       receipt_root=self.out, arm_id="session-" + profile, run_base=9800 + index * 2)
                arm.trace_remaining = self.trace_remaining
                arm.observation_history = self.observation_history
                try:
                    try:
                        completed = arm.run_existing(self.resources, helpers_ready=index > 0)
                        p.require(type(completed) is dict and completed.get("diagnostic_only") is True
                                  and completed.get("accepted_throughput") is False, "session diagnostic result binding")
                        for phase, count in (("prerequisite", 100000), ("probe", 1000000)):
                            evidence = completed.get(phase)
                            p.require(type(evidence) is dict and type(evidence.get("count")) is int and evidence["count"] == count
                                      and type(evidence.get("producers")) is int and evidence["producers"] == 1
                                      and all(evidence.get(key) is True for key in ("healthy", "ingest_valid", "census_pass", "oracle_pass")),
                                      "session case requires its own complete gate and probe")
                        record["status"] = "PASS"
                    except BaseException as error:
                        # No stale pre-fault evidence may authorize reuse. A host,
                        # source, identity or deadline error here escapes fatally.
                        fresh_hosts = arm.host_samples()
                        fresh_source = p.verify_source(self.plan)
                        d.remaining(self.plan)
                        layout = arm.layouts[-1] if arm.layouts else {}
                        if not classify_arm_failure(error, arm.last_status_document, layout, arm.last_wave_result,
                                                    fatal_guards_passed=True):
                            raise
                        record.update(status="ARM_FAILURE", error=str(error))
                        d.write(case_out / "arm-failure-classification.json", {"status": "ARM_FAILURE", "error": str(error),
                                "raw_status": arm.last_status_document, "fresh_host_guards": fresh_hosts,
                                "fresh_source": fresh_source, "health_pass": False})
                    if record["status"] == "PASS":
                        arm.host_samples()
                        p.verify_source(self.plan)
                        d.remaining(self.plan)
                    p.require(arm.layouts, "session arm has no bound native layout")
                    stopped = self.provision.stop_owned_arm(arm_plan, self.provision_out, self.resources["nodes"],
                                                           arm.layouts[-1], scopes=arm.scope_bindings())
                    p.require(type(stopped) is dict and stopped.get("passed") is True and stopped.get("quiescent") is True,
                              "session owned-arm quiescence failed")
                    d.write(case_out / "quiescence.json", stopped)
                except BaseException as error:
                    record.update(status="SESSION_FATAL", error=str(error))
                    raise
                finally:
                    try:
                        remaining = arm.trace_remaining
                        p.require(type(remaining) is int and 0 <= remaining <= self.trace_remaining, "session trace budget reset or overflow")
                        p.require(arm.observation_history is self.observation_history, "session host observation history reset")
                        self.trace_remaining = remaining
                        record["trace_bytes_remaining"] = remaining
                    except BaseException as error:
                        record.update(status="SESSION_FATAL", error=str(error))
                        raise
                    finally:
                        d.write(case_out / "session-case.json", record)
                self.receipt_budget()
            result["all_cases_passed"] = all(x["status"] == "PASS" for x in result["cases"])
            result["status"] = "ALL_CASES_PASSED" if result["all_cases_passed"] else "COMPLETED_WITH_ARM_FAILURES"
        except BaseException as error:
            result.update(status="SESSION_FATAL", error=str(error), all_cases_passed=False)
            raise
        finally:
            try:
                if self.resources is not None:
                    cleanup = self.provision.teardown(self.plan, self.provision_out, self.resources)
                    d.write(self.out / "cleanup.json", cleanup)
                    result["cleanup_passed"] = cleanup.get("passed") is True
                    p.require(cleanup.get("passed") is True, "session exact ten-kind baseline cleanup did not pass")
            except BaseException as error:
                result.update(status="SESSION_FATAL", cleanup_passed=False, error=str(error), all_cases_passed=False)
                raise
            finally:
                try:
                    d.write(self.out / "source-after.json", p.verify_source(self.plan))
                except BaseException as error:
                    result.update(status="SESSION_FATAL", error=str(error), all_cases_passed=False)
                    raise
                finally:
                    result["trace_bytes_remaining"] = self.trace_remaining
                    result["failed_cases"] = [x["name"] for x in result["cases"] if x["status"] != "PASS"]
                    d.write(self.out / "session-result.json", result)
        return result


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("mode", choices=("run",))
    ap.add_argument("--plan", required=True)
    ap.add_argument("--output")
    ap.add_argument("--execute-approved", action="store_true")
    args = ap.parse_args(argv)
    plan = validate_plan(d.read(args.plan))
    if not args.execute_approved:
        print(json.dumps({"validated": True, "nodes": 2, "cases": 2, "rows": SESSION_ROWS,
                          "cumulative_rows": plan["prior_rows_upper"] + SESSION_ROWS, "diagnostic_only": True,
                          "accepted_throughput": False, "provisioned": False}))
        return
    p.require(args.output, "session output required")
    import provision  # Controller only; remote harness remains diagnostic/driver/policy.
    result = Session(plan, args.output, provision).run()
    if not result["all_cases_passed"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
