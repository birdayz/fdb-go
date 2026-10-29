#!/usr/bin/env python3
"""No cloud/SSH/builds: typed policy, source, readiness and barrier boundaries."""
import copy
from decimal import Decimal
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import driver as d
import policy as p


def plan():
    value = dict(schema="otel_horizontal_v1", approved=True, campaign="horizontal-test", prefix="horizontal-test",
                 location="hel1", server_type="ccx43", replication="single", topologies=[1, 2, 3], measured_topologies=[3, 2, 1],
                 count=100000, measured_count=1000000, batch=32, workers=4, gomaxprocs=4,
                 source_revision="a" * 40, first_epoch=1000, deadline_epoch=8200,
                 controller_cidr="192.0.2.1/32", hcloud="/private/hcloud", controller_public_key="/private/key.pub",
                 controller_private_key="/private/key", source_root="/private/source",
                 storage_options=dict(record_compression="brotli-4", posting_value_format="tuple",
                                      bulk_index_conflicts=True, hash_bytes=8))
    for key in p.ASSETS:
        value[key] = "/private/" + key
        value["input_manifest_sha256" if key == "source_manifest" else key + "_sha256"] = "b" * 64
    return value


def evidence(mode="ingest", count=100000):
    config = dict(run="prof20k9000", backend="fdb", count=count, tenants=1, attributes=8, seed=283,
                  match_every=100, batch=32, save_mode="batch", workers=4, diagnostics=True, tenant=0, k=100,
                  max_candidates=2000000, max_bytes=268435456, order="time", **plan()["storage_options"])
    value = dict(tool="otelbench-v2", backend="fdb", revision="a" * 40, failures=0, mode=mode, config=config,
                 stats=dict(rows=count, attributes=count * 8))
    if mode == "audit":
        value["stats"]["structures"] = {k: dict(keys=v) for k, v in
            dict(records=100000, eq_secondary=800000, span_id_v2=100000, span_time_v2=100000,
                 owned_total=1100002, eq_primary=0).items()}
    if mode == "query":
        value.update(suite="all", samples=[dict(iteration=i, **{"class": p.CLASSES[i % 16]}, ns=1,
                                               result_digest="c" * 64) for i in range(1600)])
        config["iterations"] = 1600
    return value


# Literal FoundationDB 7.3.77 storage-role excerpts, status/125.json SHA-256
# 9217e508d014db1266a8f16659842d1f6eeb3ace6e62001cfe950df709c5166a.
# Deliberately no shards field: these receipts did not report one.
FDB_7377_STORAGE_ROLE_EXCERPTS = {
    "node1": {"role": "storage", "id": "39b89ae0e65db0ec",
              "input_bytes": {"counter": 508945128, "hz": 101654000, "roughness": 244084000},
              "durable_bytes": {"counter": 96044, "hz": 0, "roughness": -1},
              "mutation_bytes": {"counter": 98425653, "hz": 19660800, "roughness": 47208400},
              "stored_bytes": 91995677},
    "node2": {"role": "storage", "id": "75a15d807045b9c0",
              "input_bytes": {"counter": 3508, "hz": 480.394, "roughness": 2401},
              "durable_bytes": {"counter": 1106, "hz": 0, "roughness": -1},
              "mutation_bytes": {"counter": 23321970, "hz": 4664320, "roughness": 22417400},
              "stored_bytes": 6812500},
    "node3": {"role": "storage", "id": "9d5b479f7838c7e4",
              "input_bytes": {"counter": 4710, "hz": 720.797, "roughness": 3603},
              "durable_bytes": {"counter": 1106, "hz": 0, "roughness": -1},
              "mutation_bytes": {"counter": 17397423, "hz": 3479460, "roughness": 16653900},
              "stored_bytes": 11439230},
}


def status_fixture():
    spec = dict(node="node1", address="10.93.0.2:4510", process_class="storage", required_roles=["storage"],
                machineid="node1", zoneid="node1", memory_limit_bytes=4 * p.GIB)
    process = dict(address=spec["address"], class_type="storage", locality=dict(machineid="node1", zoneid="node1"),
                   memory=dict(rss_bytes=100, used_bytes=50, limit_bytes=4 * p.GIB), messages=[],
                   roles=[dict(role="storage", id="storage-one", input_bytes=dict(counter=10), stored_bytes=20, shards=1)])
    c = dict(messages=[], database_available=True, data=dict(state=dict(healthy=True)),
             recovery_state=dict(name="fully_recovered"), degraded_processes=0,
             configuration=dict(storage_engine="ssd-redwood-1", log_engine="ssd-2", redundancy_mode="single",
                                logs=1, commit_proxies=1, grv_proxies=1, resolvers=1),
             latency_probe={k: 0.01 for k in ("transaction_start_seconds", "immediate_priority_transaction_start_seconds",
                                             "read_seconds", "commit_seconds")},
             qos=dict(worst_queue_bytes_storage_server=0, worst_durability_lag_storage_server=dict(seconds=0),
                      performance_limited_by=dict(name="workload")), processes={"one": process})
    return dict(cluster=c, client=dict(database_status=dict(healthy=True, available=True),
            coordinators=dict(quorum_reachable=True), cluster_file=dict(up_to_date=True), messages=[])), \
        dict(processes=[spec], storage_hosts=["node1"])


class PolicyTests(unittest.TestCase):
    def test_exact_plan_and_row_population(self):
        value = plan()
        self.assertEqual(p.validate_plan(value, check_assets=False), value)
        self.assertEqual(p.authorized_rows(value), 6600000)
        value["measured_count"] = 100000
        self.assertEqual(p.authorized_rows(value), 1200000)

    def test_storage_max_outstanding_is_never_a_legacy_horizontal_option(self):
        for value in (64, 128, True, None, 128.0, {'max_outstanding': 128}):
            candidate = plan(); candidate['storage_max_outstanding'] = value
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, 'diagnostic-only'):
                p.validate_plan(candidate, check_assets=False)

    def test_storage_process_count_is_diagnostic_only(self):
        for value in (1, 2, True, None, 2.0):
            candidate = plan(); candidate['storage_process_count'] = value
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, 'diagnostic-only'):
                p.validate_plan(candidate, check_assets=False)

    def test_diagnostic_status_keeps_both_storage_ids_counters_and_unknowns_without_legacy_changes(self):
        document, layout = status_fixture()
        baseline = p.strict_status(document, layout)
        self.assertNotIn('storage_processes', baseline)
        layout['storage_process_count'] = 2
        first = layout['processes'][0]; first['memory_limit_bytes'] = 2*p.GIB
        second = copy.deepcopy(first); second['address'] = '10.93.0.2:4511'; layout['processes'].append(second)
        one = document['cluster']['processes']['one']; one['memory']['limit_bytes'] = 2*p.GIB
        one['roles'][0].update(id='ss-one', input_bytes=dict(counter=100), durable_bytes=dict(counter=60))
        two = copy.deepcopy(one); two['address'] = second['address']
        two['roles'][0].update(id='ss-two', input_bytes=dict(counter=200), durable_bytes=dict(counter=150))
        document['cluster']['processes'] = {'two': two, 'one': one}
        result = p.strict_status(document, layout)
        self.assertEqual(set(result['storage_processes']), {first['address'], second['address']})
        self.assertEqual(result['storage_processes'][first['address']]['storage_id'], 'ss-one')
        self.assertEqual(result['storage_processes'][second['address']]['input_bytes'], 200)
        self.assertEqual(result['storage']['node1']['input_bytes'], 300)
        self.assertEqual(result['storage']['node1']['durable_bytes'], 210)
        self.assertEqual(result['storage']['node1']['storage_ids'], ['ss-one', 'ss-two'])
        del two['roles'][0]['durable_bytes']
        self.assertIsNone(p.strict_status(document, layout)['storage']['node1']['durable_bytes'])
        two['roles'][0]['id'] = 'ss-one'
        self.assertIsNone(p.strict_status(document, layout)['storage']['node1']['input_bytes'])

    def test_selected_two_vs_three_study_binds_exact_population_without_mutating_plan(self):
        value = plan(); value["selected_topologies"] = [3, 2]; value["prerequisite_topologies"] = [3, 2, 1]
        before = copy.deepcopy(value)
        p.validate_plan(value, check_assets=False)
        self.assertEqual(value, before)
        self.assertEqual(value["topologies"], [1, 2, 3])
        self.assertEqual(value["measured_topologies"], [3, 2, 1])
        self.assertEqual(p.authorized_rows(value), 4400000)
        self.assertLess(p.authorized_rows(value), 10000000)
        value["measured_count"] = 100000
        self.assertEqual(p.authorized_rows(value), 800000)

    def test_measured_order_accepts_only_the_two_typed_preregistered_orders(self):
        for order in ([3, 2, 1], [2, 3, 1]):
            value = plan(); value["measured_topologies"] = order
            value["selected_topologies"] = [3, 2]
            before = copy.deepcopy(value)
            self.assertEqual(p.validate_plan(value, check_assets=False), value)
            self.assertEqual(value, before)
            self.assertEqual(p.authorized_rows(value), 4400000)
        for order in (None, True, [], [3, 2], [3, 3, 1], [1, 2, 3], [1, 3, 2], [2, 1, 3], [3, 1, 2],
                      [True, 3, 1], [2, 3, True], [2.0, 3, 1], [3, 2.0, 1], (2, 3, 1), "2,3,1"):
            with self.subTest(order=order):
                value = plan(); value["measured_topologies"] = order
                with self.assertRaises(ValueError):
                    p.validate_plan(value, check_assets=False)

    def test_selected_topologies_rejects_empty_arbitrary_duplicate_or_untyped_values(self):
        for selected in (None, True, [], [3], [2, 3], [3, 3], [2, 1], [3, 2, 1],
                         [True, 2], [3, False], [3.0, 2], (3, 2), "3,2"):
            with self.subTest(selected=selected):
                value = plan(); value["selected_topologies"] = selected
                with self.assertRaises(ValueError): p.validate_plan(value, check_assets=False)

    def test_plan_rejects_boolean_or_unapproved_values(self):
        for key, bad in (("approved", 1), ("measured_count", True), ("count", 1000000), ("batch", 128),
                         ("gomaxprocs", True), ("replication", "double"), ("deadline_epoch", 8201),
                         ("topologies", [True, 2, 3]), ("measured_topologies", [1, 2, 3]),
                         ("measured_topologies", [3, 2, True]), ("controller_cidr", "192.0.2.0/24")):
            with self.subTest(key=key):
                value = plan(); value[key] = bad
                with self.assertRaises(ValueError): p.validate_plan(value, check_assets=False)

    def test_optional_prerequisite_order_is_explicit_and_does_not_rewrite_approval(self):
        for order in (None, [1, 2, 3], [3, 2, 1]):
            value = plan()
            if order is not None:
                value["prerequisite_topologies"] = order
            before = copy.deepcopy(value)
            p.validate_plan(value, check_assets=False)
            self.assertEqual(value, before)
            self.assertEqual(value["topologies"], [1, 2, 3])
            self.assertEqual(value["measured_topologies"], [3, 2, 1])
            self.assertEqual("prerequisite_topologies" in value, order is not None)

    def test_optional_prerequisite_order_rejects_unknown_or_untyped_permutations(self):
        for order in (None, True, [], [1, 2], [1, 1, 3], [1, 3, 2], [2, 1, 3], [2, 3, 1],
                      [3, 1, 2], [True, 2, 3], [3, 2, True], [3.0, 2, 1], (3, 2, 1), "3,2,1"):
            with self.subTest(order=order):
                value = plan(); value["prerequisite_topologies"] = order
                with self.assertRaises(ValueError): p.validate_plan(value, check_assets=False)

    def test_storage_requires_all_typed_keys(self):
        for change in ({"bulk_index_conflicts": 1}, {"hash_bytes": True}, {"record_compression": "zstd"}):
            value = plan()["storage_options"]; value.update(change)
            with self.assertRaises(ValueError): p.storage(value)
        value = plan()["storage_options"]; del value["bulk_index_conflicts"]
        with self.assertRaises(ValueError): p.storage(value)

    def test_source_manifest_hashes_actual_contents(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); source = root / "source"; source.mkdir(); file = source / "a.go"; file.write_text("sealed")
            manifest = root / "manifest"; manifest.write_text(p.sha(file) + "  a.go\n")
            value = dict(source_root=str(source), source_manifest=str(manifest), input_manifest_sha256=p.sha(manifest))
            self.assertEqual(p.verify_source(value)["source_files"], 1)
            file.write_text("changed")
            with self.assertRaisesRegex(ValueError, "changed source file"): p.verify_source(value)

    def test_source_manifest_refuses_escape_duplicate_and_empty(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); file = root / "a"; file.write_text("a"); manifest = root / "manifest"
            for text in ("", "0" * 64 + "  ../escape\n", (p.sha(file) + "  a\n") * 2):
                manifest.write_text(text)
                value = dict(source_root=tmp, source_manifest=str(manifest), input_manifest_sha256=p.sha(manifest))
                with self.assertRaises(ValueError): p.verify_source(value)

    def test_promotion_needs_nonempty_this_topology_complete_pair(self):
        gate = dict(storage_hosts=2, count_per_producer=100000, healthy=True, both_censuses=True, both_oracles=True)
        p.require_promotion(gate, 2)
        for key, bad in (("storage_hosts", 1), ("count_per_producer", 1000000), ("healthy", False),
                         ("both_censuses", False), ("both_oracles", 1)):
            value = dict(gate); value[key] = bad
            with self.assertRaises(ValueError): p.require_promotion(value, 2)

    def test_common_wall_is_aggregate_not_sum_of_client_rates(self):
        self.assertEqual(p.common_throughput(2000000, 1000000000, 11000000000), 200000)
        for args in ((0, 1, 2), (1, 2, 2), (1, 3, 2), (True, 1, 2)):
            with self.assertRaises(ValueError): p.common_throughput(*args)

    def test_cost_separates_active_topology_and_held_bill(self):
        resources = dict(price_hourly_net="0.4423", price_hourly_gross="0.526337", currency="EUR",
                         pricing_receipt_sha256="d" * 64, first_create_epoch=1000)
        for hosts, active in ((1, 3), (2, 4), (3, 5)):
            result = p.topology_cost(resources, hosts, 20000)
            self.assertEqual(result["active_topology_nodes"], active)
            self.assertEqual(result["held_billable_nodes"], 5)
            self.assertEqual(Decimal(result["hourly_gross"]), Decimal("0.526337") * active)
            factor = Decimal(1000000) / Decimal(20000) / Decimal(3600)
            self.assertEqual(Decimal(result["per_million_ack_rows_net"]), Decimal("0.4423") * active * factor)
            self.assertEqual(Decimal(result["per_million_ack_rows_gross"]), Decimal("0.526337") * active * factor)
            self.assertEqual(result["scope"], "SERVER-ONLY")
            self.assertEqual(result["excludes"], ["IPv4", "traffic"])
            self.assertIn("server-only", result["boundary"].lower())
        bill = p.bill_bound(resources, dict(finished_epoch=4601))
        self.assertEqual(bill["rounded_hours_per_server_upper_bound"], 2)
        self.assertEqual(bill["gross_upper_bound"], "5.263370")

    def test_rounded_bill_uses_all_five_created_and_delete_receipts(self):
        resources = dict(price_hourly_net="0.4423", price_hourly_gross="0.526337", currency="EUR",
                         nodes={str(i): dict(id=i, created="2026-01-01T00:00:00Z") for i in range(1, 6)})
        cleanup = dict(actions=[dict(kind="server", id=i, returncode=0, finished_epoch=1767229201)
                                for i in range(1, 6)])
        bill = p.rounded_server_bill(resources, cleanup)
        self.assertTrue(bill["known"])
        self.assertEqual(bill["server_hours"], 10)
        self.assertEqual(bill["gross"], "5.263370")
        cleanup["actions"].pop()
        self.assertFalse(p.rounded_server_bill(resources, cleanup)["known"])

    def test_all_evidence_modes_have_positive_population(self):
        for mode in ("ingest", "audit", "query"):
            self.assertTrue(p.evidence(evidence(mode), plan(), "prof20k9000", 100000, mode))

    def test_evidence_never_substitutes_missing_or_false_profile(self):
        for key in plan()["storage_options"]:
            value = evidence(); del value["config"][key]
            with self.assertRaises(ValueError): p.evidence(value, plan(), "prof20k9000", 100000, "ingest")
        for error in (False, 0, None, "failed"):
            value = evidence(); value["error"] = error
            with self.assertRaises(ValueError): p.evidence(value, plan(), "prof20k9000", 100000, "ingest")

    def test_every_census_structure_is_required_and_exact(self):
        for name in evidence("audit")["stats"]["structures"]:
            value = evidence("audit"); value["stats"]["structures"][name]["keys"] += 1
            with self.assertRaises(ValueError): p.evidence(value, plan(), "prof20k9000", 100000, "audit")

    def test_oracle_requires_all_1600_ordered_class_samples(self):
        for mutation in (lambda x: x["samples"].pop(), lambda x: x["samples"][0].update(iteration=True),
                         lambda x: x["samples"][15].update({"class": "trace"}),
                         lambda x: x["samples"][0].update(error=False),
                         lambda x: x["samples"][0].update(result_digest="bad")):
            value = evidence("query"); mutation(value)
            with self.assertRaises(ValueError): p.evidence(value, plan(), "prof20k9000", 100000, "query")


class StatusTests(unittest.TestCase):
    def test_storage_counter_normalizes_only_bounded_integers_and_safe_integral_floats(self):
        for name in ("input_bytes", "durable_bytes"):
            for raw, expected in ((0, 0), (1, 1), (10**18-1, 10**18-1),
                                  (2**53+1, 2**53+1), (0.0, 0), (1234.0, 1234),
                                  (float(2**53-1), 2**53-1)):
                with self.subTest(name=name, raw=raw):
                    result = p.storage_counter({name: dict(counter=raw)}, name)
                    self.assertIs(type(result), int)
                    self.assertEqual(result, expected)
            for raw in (None, True, False, "1", -1, -1.0, 0.5, 10**18,
                        float(2**53), float(2**53+2), float("nan"), float("inf"), float("-inf"), [], {}):
                with self.subTest(name=name, raw=raw):
                    self.assertIsNone(p.storage_counter({name: dict(counter=raw)}, name))
            for role in (None, [], True, "role", {}, {name: None}, {name: []},
                         {name: "counter"}, {name: {}}, {name: dict(hz=99)}):
                with self.subTest(name=name, role=role):
                    self.assertIsNone(p.storage_counter(role, name))

    def test_literal_fdb_7377_storage_role_excerpts_need_no_private_receipt(self):
        expected = {"node1": (508945128, 96044, 91995677, "39b89ae0e65db0ec"),
                    "node2": (3508, 1106, 6812500, "75a15d807045b9c0"),
                    "node3": (4710, 1106, 11439230, "9d5b479f7838c7e4")}
        for node, literal in FDB_7377_STORAGE_ROLE_EXCERPTS.items():
            with self.subTest(node=node):
                value, layout = status_fixture()
                value["cluster"]["processes"]["one"]["roles"] = [copy.deepcopy(literal)]
                parsed = p.strict_status(value, layout)
                storage = parsed["storage"]["node1"]
                self.assertTrue(parsed["healthy"])
                self.assertEqual((storage["input_bytes"], storage["durable_bytes"],
                                  storage["stored_bytes"], storage["storage_id"]), expected[node])
                self.assertIsNone(storage["shards"])
                self.assertNotIn("bytes_input", literal)
                self.assertNotIn("shards", literal)

    def test_status_uses_only_input_bytes_and_normalizes_optional_durable_counter(self):
        value, layout = status_fixture()
        role = value["cluster"]["processes"]["one"]["roles"][0]
        role.update(input_bytes=dict(counter=1234.0), durable_bytes=dict(counter=99.0),
                    bytes_input=dict(counter=999999))
        parsed = p.strict_status(value, layout)["storage"]["node1"]
        self.assertIs(type(parsed["input_bytes"]), int)
        self.assertEqual(parsed["input_bytes"], 1234)
        self.assertEqual(parsed["durable_bytes"], 99)
        del role["input_bytes"]
        self.assertIsNone(p.strict_status(value, layout)["storage"]["node1"]["input_bytes"])
        for name in ("input_bytes", "durable_bytes"):
            for raw in (True, "99", -1, 0.25, float("nan"), float("inf"), float(2**53), [], {}):
                role[name] = dict(counter=raw)
                with self.subTest(name=name, raw=raw):
                    self.assertIsNone(p.strict_status(value, layout)["storage"]["node1"][name])

    def test_stored_bytes_and_shards_remain_typed_integers_and_unknown_stays_none(self):
        value, layout = status_fixture()
        role = value["cluster"]["processes"]["one"]["roles"][0]
        for name in ("stored_bytes", "shards"):
            for raw in (0, 1, 10**18-1):
                role[name] = raw
                self.assertEqual(p.strict_status(value, layout)["storage"]["node1"][name], raw)
            for raw in (None, True, False, "1", -1, 1.0, [], {}, 10**18):
                role[name] = raw
                with self.subTest(name=name, raw=raw):
                    self.assertIsNone(p.strict_status(value, layout)["storage"]["node1"][name])
            del role[name]
            self.assertIsNone(p.strict_status(value, layout)["storage"]["node1"][name])

    def test_strict_status_positive_control(self):
        value, layout = status_fixture()
        result = p.strict_status(value, layout)
        self.assertTrue(result["healthy"])
        self.assertEqual(result["storage"]["node1"]["input_bytes"], 10)

    def test_every_probe_and_warning_remains_strict(self):
        base, layout = status_fixture()
        for name in base["cluster"]["latency_probe"]:
            for bad in (5, None, True, float("nan")):
                value = copy.deepcopy(base); value["cluster"]["latency_probe"][name] = bad
                with self.assertRaises(ValueError): p.strict_status(value, layout)
        for target in ("cluster", "client"):
            value = copy.deepcopy(base); value[target]["messages"] = [dict(name="warning")]
            with self.assertRaises(ValueError): p.strict_status(value, layout)
        value = copy.deepcopy(base); value["client"]["configuration"] = dict(messages=["warning"])
        with self.assertRaises(ValueError): p.strict_status(value, layout)

    def test_config_process_identity_and_rss_are_not_optional(self):
        base, layout = status_fixture()
        for name in base["cluster"]["configuration"]:
            value = copy.deepcopy(base); value["cluster"]["configuration"][name] = "wrong"
            with self.assertRaises(ValueError): p.strict_status(value, layout)
        for change in (dict(address="10.0.0.1:1"), dict(class_type="transaction"), dict(messages=["warning"]),
                       dict(locality=dict(machineid="node2", zoneid="node1")),
                       dict(memory=dict(rss_bytes=True, used_bytes=50, limit_bytes=4 * p.GIB)),
                       dict(roles=[dict(role="storage"), dict(role="log")])):
            value = copy.deepcopy(base); value["cluster"]["processes"]["one"].update(change)
            with self.assertRaises(ValueError): p.strict_status(value, layout)

    def test_distribution_known_stable_counters_only_not_placement_proof(self):
        def sample(counter, identity="a"):
            return dict(storage={"node1": dict(input_bytes=counter, stored_bytes=2 * 1024 ** 2, storage_id=identity)})
        before, during, after = sample(10), [sample(2 * 1024 ** 2)], [sample(3 * 1024 ** 2)]
        result = p.distributed(before, during, ["node1"], after)
        self.assertTrue(result["observed_all_hosts"])
        self.assertTrue(result["scaling_proof"].startswith("INCONCLUSIVE"))
        for bad_during, bad_after in (([], after), ([sample(None)], after), (during, [sample(0)]),
                                      (during, [sample(None)]), (during, [sample(3 * 1024 ** 2, "b")])):
            self.assertEqual(p.distributed(before, bad_during, ["node1"], bad_after)["verdict"], "INCONCLUSIVE")

    def test_distribution_optional_durable_counters_must_be_known_and_monotone_everywhere(self):
        def sample(counter, durable, identity="stable-storage"):
            return dict(storage={"node1": dict(input_bytes=counter, durable_bytes=durable,
                        stored_bytes=2*1024**2, storage_id=identity, shards=None)})
        sequence = [sample(10, 5), sample(2*1024**2, 6), sample(3*1024**2, 7),
                    sample(4*1024**2, 8), sample(5*1024**2, 9)]
        result = p.distributed(sequence[0], sequence[1:3], ["node1"], sequence[3:])
        self.assertEqual(result["verdict"], "OBSERVED_HOST_DATA_PROCESSING")
        self.assertEqual(result["input_byte_deltas"]["node1"], 3*1024**2-10)
        self.assertEqual(result["durable_byte_deltas"]["node1"], 2)
        self.assertTrue(result["scaling_proof"].startswith("INCONCLUSIVE"))
        for index in range(len(sequence)):
            for missing in (False, True):
                changed = copy.deepcopy(sequence)
                storage = changed[index]["storage"]["node1"]
                if missing:
                    del storage["durable_bytes"]
                else:
                    storage["durable_bytes"] = None
                with self.subTest(index=index, missing=missing):
                    self.assertEqual(p.distributed(changed[0], changed[1:3], ["node1"], changed[3:])["verdict"],
                                     "INCONCLUSIVE")
        for index, bad in ((0, True), (1, 4), (2, 5), (3, 6), (4, 7), (1, "6"), (1, -1), (1, 6.0)):
            changed = copy.deepcopy(sequence); changed[index]["storage"]["node1"]["durable_bytes"] = bad
            with self.subTest(index=index, bad=bad):
                self.assertEqual(p.distributed(changed[0], changed[1:3], ["node1"], changed[3:])["verdict"],
                                 "INCONCLUSIVE")
        legacy = copy.deepcopy(sequence)
        for sample_value in legacy:
            del sample_value["storage"]["node1"]["durable_bytes"]
        self.assertTrue(p.distributed(legacy[0], legacy[1:3], ["node1"], legacy[3:])["observed_all_hosts"])
        for sample_value in legacy:
            sample_value["storage"]["node1"]["durable_bytes"] = None
        self.assertTrue(p.distributed(legacy[0], legacy[1:3], ["node1"], legacy[3:])["observed_all_hosts"])

    def test_distribution_requires_input_and_stable_identity_through_before_during_and_post_drain(self):
        sequence = [dict(storage={"node1": dict(input_bytes=counter, stored_bytes=2*1024**2,
                    storage_id="stable-storage")}) for counter in (10, 2*1024**2, 3*1024**2, 4*1024**2)]
        self.assertTrue(p.distributed(sequence[0], sequence[1:3], ["node1"], sequence[3:])["observed_all_hosts"])
        for index in range(len(sequence)):
            for field, bad in (("input_bytes", None), ("input_bytes", True), ("input_bytes", "123"),
                               ("input_bytes", -1), ("storage_id", None), ("storage_id", ""),
                               ("storage_id", "different-storage")):
                changed = copy.deepcopy(sequence); changed[index]["storage"]["node1"][field] = bad
                with self.subTest(index=index, field=field, bad=bad):
                    self.assertEqual(p.distributed(changed[0], changed[1:3], ["node1"], changed[3:])["verdict"],
                                     "INCONCLUSIVE")
        for index in (1, 2, 3):
            changed = copy.deepcopy(sequence); changed[index]["storage"]["node1"]["input_bytes"] = 0
            with self.subTest(reset_index=index):
                self.assertEqual(p.distributed(changed[0], changed[1:3], ["node1"], changed[3:])["verdict"],
                                 "INCONCLUSIVE")


class RuntimeTests(unittest.TestCase):
    def test_collect_retains_completed_ingest_payload_when_pair_has_failed(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); grant = dict(node_id=123, run="prof20k9000", work=tmp)
            d.write(root / "plan.json", dict(grant=True))
            payload = evidence("ingest")
            d.write(root / "ingest.json", payload)
            d.write(root / "ingest.exit.json", dict(exit=0))
            (root / "ingest.stderr").write_text("paired run stopped by strict health guard\n")
            with mock.patch.object(d, "client_plan", return_value=grant), mock.patch.object(d, "identity"), \
                 mock.patch("builtins.print") as output:
                d.main(["_collect", "--path", str(root / "plan.json"), "--node-id", "123"])
            collected = json.loads(output.call_args.args[0])
            self.assertEqual(collected["run"], "prof20k9000")
            self.assertEqual(json.loads(collected["artifacts"]["ingest.json"]), payload)
            self.assertEqual(json.loads(collected["artifacts"]["ingest.exit.json"]), dict(exit=0))
            self.assertEqual(collected["artifacts"]["ingest.stderr"], "paired run stopped by strict health guard\n")
            self.assertNotIn("audit.json", collected["artifacts"])
            self.assertNotIn("query.json", collected["artifacts"])

    def test_collect_payload_keeps_existing_symlink_and_size_guards(self):
        for oversized in (False, True):
            with self.subTest(oversized=oversized), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp); grant = dict(node_id=123, run="prof20k9000", work=tmp)
                d.write(root / "plan.json", dict(grant=True))
                path = root / "ingest.json"
                if oversized:
                    with path.open("wb") as stream: stream.truncate(16 * 1024 ** 2)
                else:
                    other = root / "other.json"; d.write(other, evidence("ingest")); path.symlink_to(other)
                with mock.patch.object(d, "client_plan", return_value=grant), mock.patch.object(d, "identity"):
                    with self.assertRaisesRegex(ValueError, "receipt size/type"):
                        d.main(["_collect", "--path", str(root / "plan.json"), "--node-id", "123"])

    def test_json_duplicates_nonfinite_and_immutable_write(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "a.json"
            for text in ('{"a":1,"a":2}', '{"a":NaN}', '{"a":1e999}'):
                path.write_text(text)
                with self.assertRaises(ValueError): d.read(path)
            path.unlink(); d.write(path, dict(a=1))
            with self.assertRaises(FileExistsError): d.write(path, dict(a=2))

    def test_explicit_false_argv_no_global_guard_relaxation(self):
        grant = dict(binary="/bin/otel", cluster_file="/cluster", run="prof20k9000", count=1000000, batch=32,
                     workers=4, source_revision="a" * 40, work="/work",
                     storage_options=dict(record_compression="none", posting_value_format="json",
                                          bulk_index_conflicts=False, hash_bytes=32))
        argv = d.app_argv(grant, "ingest")
        self.assertIn("--bulk-index-conflicts=false", argv)
        self.assertEqual(argv[argv.index("--timeout") + 1], "300s")
        self.assertEqual(argv[argv.index("--count") + 1], "1000000")
        self.assertNotIn("--skip-validation", argv)
        query = d.app_argv(grant, "query")
        self.assertEqual(query[query.index("--iterations") + 1], "1600")

    def test_controller_budget_is_absolute_not_per_command_reset(self):
        with mock.patch.object(d.time, "time", return_value=6999):
            self.assertEqual(d.remaining(plan(), 30), 1)
        with mock.patch.object(d.time, "time", return_value=7000):
            with self.assertRaises(ValueError): d.remaining(plan())

    def test_quiet_absolute_cadence_includes_sequential_ssh_overhead(self):
        with tempfile.TemporaryDirectory() as tmp:
            now, starts, waits = [0.0], [], []
            def sleep(seconds):
                self.assertGreaterEqual(seconds, 0)
                waits.append(seconds)
                now[0] += seconds
            campaign = d.Campaign(plan(), tmp, mock.Mock(), clock=lambda: int(now[0] * 10 ** 9), sleep=sleep)
            campaign.nodes = {name: {} for name in ("node1", "node2", "node3", "client1", "client2")}
            costs = [5.0, 5.6, 5.2, 5.7, 5.1, 5.3, 5.0]
            def sample(initial=False):
                self.assertTrue(initial)
                starts.append(now[0])
                cost = costs[len(starts)-1]
                values = {}
                for name in campaign.nodes:
                    now[0] += cost / 5
                    values[name] = dict(epoch=1000 + now[0], mem_available_bytes=32 * p.GIB,
                                        pressure=dict(cpu_some=0, memory_some=0, memory_full=0, io_full=0))
                return values
            campaign.host_samples = sample
            campaign.quiet()
            self.assertEqual(len(starts), 7)
            for i, start in enumerate(starts):
                self.assertAlmostEqual(start, i * 12)
            self.assertEqual(len(waits), 6)
            self.assertAlmostEqual(sum(waits), 72 - sum(costs[:6]))
            self.assertEqual(len(d.read(Path(tmp) / "quiet.json")["samples"]), 7)

    def test_quiet_controller_overrun_does_not_waive_interval_guard(self):
        with tempfile.TemporaryDirectory() as tmp:
            now, waits = [0], []
            campaign = d.Campaign(plan(), tmp, mock.Mock(), clock=lambda: now[0] * 10 ** 9, sleep=waits.append)
            campaign.nodes = {"node1": {}}
            def sample(initial=False):
                now[0] += 16
                return {"node1": dict(epoch=1000 + now[0], mem_available_bytes=32 * p.GIB,
                                      pressure=dict(cpu_some=0, memory_some=0, memory_full=0, io_full=0))}
            campaign.host_samples = sample
            with self.assertRaisesRegex(ValueError, "intervals outside 10–15"):
                campaign.quiet()
            self.assertFalse(waits)
            self.assertFalse((Path(tmp) / "quiet.json").exists())

    def test_quiet_host_timestamps_still_require_ten_to_fifteen_seconds(self):
        for interval in (9, 16):
            with self.subTest(interval=interval), tempfile.TemporaryDirectory() as tmp:
                now = [0]
                def sleep(seconds): now[0] += seconds
                campaign = d.Campaign(plan(), tmp, mock.Mock(), clock=lambda: int(now[0] * 10 ** 9), sleep=sleep)
                campaign.nodes = {"node1": {}}
                campaign.host_samples = mock.Mock(side_effect=[{"node1": dict(epoch=1000 + i * interval,
                    mem_available_bytes=32 * p.GIB, pressure=dict(cpu_some=0, memory_some=0, memory_full=0, io_full=0))}
                    for i in range(7)])
                with self.assertRaisesRegex(ValueError, "intervals outside 10–15"):
                    campaign.quiet()
                self.assertEqual(campaign.host_samples.call_count, 7)
                self.assertFalse((Path(tmp) / "quiet.json").exists())

    def test_startup_records_unhealthy_then_requires_three_strict_passes(self):
        with tempfile.TemporaryDirectory() as tmp:
            value = plan(); value["first_epoch"] = 10 ** 9
            campaign = d.Campaign(value, tmp, mock.Mock(), clock=iter(range(1, 100)).__next__, sleep=lambda _: None)
            campaign.host_samples = mock.Mock()
            good = dict(queue_bytes=0, lag_seconds=0)
            campaign.status = mock.Mock(side_effect=[ValueError("recovering"), good, good, good])
            with mock.patch.object(d, "remaining", return_value=30), mock.patch.object(d.time, "monotonic", return_value=0):
                self.assertEqual(campaign.stable({}, startup=True), good)
            self.assertEqual(campaign.status.call_count, 4)
            self.assertEqual(len(list((Path(tmp) / "startup").glob("*.json"))), 1)

    def test_persistent_startup_unhealthy_times_out_and_load_is_immediate_fail(self):
        with tempfile.TemporaryDirectory() as tmp:
            campaign = d.Campaign(plan(), tmp, mock.Mock(), clock=lambda: 1, sleep=lambda _: None)
            campaign.host_samples = mock.Mock(); campaign.status = mock.Mock(side_effect=ValueError("recovering"))
            with mock.patch.object(d, "remaining", return_value=2), mock.patch.object(d.time, "monotonic", side_effect=[0, 0, 3]):
                with self.assertRaisesRegex(ValueError, "readiness/drain"): campaign.stable({}, startup=True)
            with self.assertRaisesRegex(ValueError, "recovering"): campaign.monitor({})

    def test_wrong_release_never_executes_and_correct_release_is_bound(self):
        for valid in (False, True):
            with tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp); path = root / "plan.json"; d.write(path, dict(grant=True))
                grant = dict(node_id=123, work=tmp, binary="/sealed", binary_sha256="a" * 64,
                             token="b" * 64, run="prof20k9000")
                d.write(root / "release.json", dict(token=grant["token"] if valid else "wrong", grant_sha256="a" * 64))
                with mock.patch.object(d, "client_plan", return_value=grant), mock.patch.object(d, "identity"), \
                     mock.patch.object(p, "sha", return_value="a" * 64), mock.patch.object(d, "remaining", return_value=60), \
                     mock.patch.object(d, "execute_app") as execute:
                    if valid: d.producer(path)
                    else:
                        with self.assertRaisesRegex(ValueError, "barrier release"): d.producer(path)
                    self.assertEqual(execute.call_count, int(valid))
                    self.assertEqual(d.read(root / "ready.json")["node_id"], 123)

    def test_actual_wave_releases_only_after_both_ready_and_uses_one_common_window(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); cluster = root / "cluster"; cluster.write_text("fresh-cluster")
            provider = mock.Mock(); provider.copy_to_node.return_value = (0, "", "")
            provider.run_on_node.return_value = (0, "", "")
            provider.ssh_argv.side_effect = lambda plan, out, node, args: [str(node["id"]), *args]
            campaign = d.Campaign(plan(), root, provider, clock=iter([1000000000, 11000000000]).__next__, sleep=lambda _: None)
            campaign.remote_root = "/srv/otel-horizontal/horizontal-test"; campaign.remote_harness = campaign.remote_root + "/harness"
            campaign.nodes = {"client1": dict(id=1), "client2": dict(id=2)}
            campaign.resources = dict(price_hourly_net="0.4423", price_hourly_gross="0.526337", currency="EUR",
                                      pricing_receipt_sha256="d" * 64)
            sample = dict(queue_bytes=0, lag_seconds=0, storage={"node1": dict(input_bytes=1, stored_bytes=1, storage_id="a")})
            campaign.stable = mock.Mock(return_value=sample); campaign.monitor = mock.Mock(return_value=sample)
            campaign.collect = mock.Mock()
            grants, ready, released = {}, set(), []
            def copied(plan, out, node, local, remote):
                if str(local).endswith("-plan.json"): grants[node["id"]] = (d.read(local), Path(local))
                return 0, "", ""
            provider.copy_to_node.side_effect = copied
            def command(node, args, cap=30):
                if args[0] == "sha256sum": return p.sha(cluster) + "  cluster"
                if "_release" in args:
                    self.assertEqual(ready, {1, 2})
                    released.append(node["id"])
                return ""
            campaign.command = command
            def remote_read(node, path):
                grant, local = grants[node["id"]]
                if path.endswith("ready.json"):
                    ready.add(node["id"])
                    return dict(token=grant["token"], node_id=node["id"], run=grant["run"], grant_sha256=p.sha(local))
                mode = Path(path).stem
                value = evidence(mode); value["config"]["run"] = grant["run"]
                return value
            campaign.read_remote = remote_read
            class Child:
                def __init__(self, args, **kwargs): self.mode = "_produce" if "_produce" in args else "_verify"; self.polls = 0
                def poll(self):
                    self.polls += 1
                    if self.mode == "_produce":
                        self_outer.assertEqual(set(released), {1, 2})
                        return None if self.polls == 1 else 0
                    return 0
                def wait(self, timeout=None): return 0
            self_outer = self
            layout = dict(cluster_file=str(cluster), remote_cluster_file=campaign.remote_root + "/arms/a/fdb.cluster", storage_hosts=["node1"])
            with mock.patch.object(d, "remaining", return_value=300), mock.patch.object(d.subprocess, "Popen", Child):
                result = campaign.wave(layout, 1, 100000, 0)
            self.assertEqual(result["rows"], 200000)
            self.assertEqual(result["ack_rows_per_second"], 20000)
            self.assertEqual(result["controller_started_ns"], 1000000000)
            self.assertEqual(result["controller_ended_ns"], 11000000000)
            self.assertIn("observation overhead", result["timing_boundary"])
            self.assertEqual(set(released), {1, 2})
            self.assertEqual(campaign.monitor.call_count, 2)

    def test_host_caps_cpuset_oom_swap_and_process_identity_are_hard_stops(self):
        base = dict(total_bytes=360 * p.GIB, available_bytes=330 * p.GIB, filesystem="ext4",
                    mem_available_bytes=32 * p.GIB, allocated_bytes=p.GIB, pswpin=0, pswpout=0,
                    fdb_pids={"1": "100"}, slices={"horizontal-fdb.slice": {
                        "cpu.max": "400000 100000", "cpuset.cpus.effective": "0-3", "memory.max": "17179869184",
                        "memory.swap.max": "0", "memory.current": "1000", "memory.events": "oom 0\noom_kill 0"}})
        with tempfile.TemporaryDirectory() as tmp:
            for key, bad in (("cpuset.cpus.effective", "0-15"), ("memory.swap.max", "max"),
                             ("memory.max", "max"), ("memory.events", "oom 1\noom_kill 0")):
                campaign = d.Campaign(plan(), tmp, mock.Mock()); campaign.nodes = {"node1": dict(id=1)}
                campaign.remote_root = "/owned"; campaign.remote_harness = "/owned/harness"
                value = copy.deepcopy(base); value["slices"]["horizontal-fdb.slice"][key] = bad
                campaign.command = mock.Mock(return_value=json.dumps(value))
                with self.assertRaises(ValueError): campaign.host_samples()
            campaign = d.Campaign(plan(), tmp, mock.Mock()); campaign.nodes = {"node1": dict(id=1)}
            campaign.remote_root = "/owned"; campaign.remote_harness = "/owned/harness"
            campaign.command = mock.Mock(return_value=json.dumps(base))
            self.assertEqual(campaign.host_samples()["node1"]["fdb_pids"], {"1": "100"})
            swapped = copy.deepcopy(base); swapped["pswpout"] = 1
            campaign.command.return_value = json.dumps(swapped)
            with self.assertRaisesRegex(ValueError, "swap"): campaign.host_samples()
            campaign.pid_baseline = {"node1": {"1": "other-starttime"}}
            campaign.command.return_value = json.dumps(base)
            with self.assertRaisesRegex(ValueError, "PID/starttime"): campaign.host_samples()

    def test_hard_and_six_consecutive_backlog_stops(self):
        campaign = d.Campaign(plan(), "/unused", mock.Mock())
        for sample in (dict(queue_bytes=2 * p.GIB, lag_seconds=0), dict(queue_bytes=0, lag_seconds=90)):
            with self.assertRaises(ValueError): campaign.backlog(sample)
        for _ in range(5): campaign.backlog(dict(queue_bytes=512 * 1024 ** 2, lag_seconds=60))
        with self.assertRaisesRegex(ValueError, "six consecutive"): campaign.backlog(dict(queue_bytes=512 * 1024 ** 2, lag_seconds=60))

    def test_all_prerequisites_precede_preregistered_measured_order_and_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            nodes = {name: dict(id=i + 1) for i, name in enumerate(("node1", "node2", "node3", "client1", "client2"))}
            provider = mock.Mock()
            provider.prepare.return_value = dict(nodes=nodes)
            provider.teardown.return_value = dict(passed=True)
            provider.setup_cluster.side_effect = lambda *args: dict(storage_hosts=args[3])
            campaign = d.Campaign(plan(), Path(tmp) / "campaign", provider)
            campaign.install_helpers = mock.Mock(); campaign.quiet = mock.Mock(); campaign.stable = mock.Mock()
            campaign.host_samples = mock.Mock(return_value={n: dict(fdb_pids={}) for n in nodes})
            calls = []
            def wave(layout, hosts, count, number):
                calls.append((hosts, count))
                return dict(storage_hosts=hosts, count_per_producer=count, healthy=True, both_censuses=True, both_oracles=True)
            campaign.wave = wave
            with mock.patch.object(p, "validate_plan"), mock.patch.object(p, "verify_source", return_value=dict(files=1)), \
                 mock.patch.object(p, "bill_bound", return_value={}), mock.patch.object(p, "rounded_server_bill", return_value={}), \
                 mock.patch.object(d, "remaining", return_value=30):
                campaign.run()
            self.assertEqual(calls, [(1, 100000), (2, 100000), (3, 100000),
                                     (3, 1000000), (2, 1000000), (1, 1000000)])
            self.assertEqual(provider.teardown.call_count, 1)

    def test_descending_prerequisites_are_preregistered_and_small_failure_stays_fatal(self):
        for fail_small in (False, True):
            with self.subTest(fail_small=fail_small), tempfile.TemporaryDirectory() as tmp:
                value = plan(); value["prerequisite_topologies"] = [3, 2, 1]; value["measured_count"] = 100000
                before = copy.deepcopy(value)
                nodes = {name: dict(id=i + 1) for i, name in enumerate(("node1", "node2", "node3", "client1", "client2"))}
                provider = mock.Mock(); provider.prepare.return_value = dict(nodes=nodes)
                provider.teardown.return_value = dict(passed=True)
                provider.setup_cluster.side_effect = lambda *args: dict(storage_hosts=args[3])
                campaign = d.Campaign(value, Path(tmp) / "campaign", provider)
                campaign.install_helpers = mock.Mock(); campaign.quiet = mock.Mock(); campaign.stable = mock.Mock()
                campaign.host_samples = mock.Mock(return_value={n: dict(fdb_pids={}) for n in nodes})
                calls = []
                def wave(layout, hosts, count, number):
                    calls.append((hosts, count))
                    if fail_small and hosts == 1:
                        raise ValueError("strict small-layout health failure")
                    return dict(storage_hosts=hosts, count_per_producer=count, healthy=True,
                                both_censuses=True, both_oracles=True)
                campaign.wave = wave
                with mock.patch.object(p, "validate_plan"), mock.patch.object(p, "verify_source", return_value=dict(files=1)), \
                     mock.patch.object(p, "bill_bound", return_value={}), mock.patch.object(p, "rounded_server_bill", return_value={}), \
                     mock.patch.object(d, "remaining", return_value=30):
                    if fail_small:
                        with self.assertRaisesRegex(ValueError, "strict small-layout health failure"): campaign.run()
                    else:
                        campaign.run()
                expected = [(3, 100000), (2, 100000), (1, 100000)]
                self.assertEqual(calls, expected if fail_small else expected * 2)
                arms = [call.args[4] for call in provider.setup_cluster.call_args_list]
                self.assertEqual(arms[:3], ["hosts3-prerequisite", "hosts2-prerequisite", "hosts1-prerequisite"])
                self.assertEqual(len(arms), 3 if fail_small else 6)
                self.assertEqual(provider.teardown.call_count, 1)
                self.assertEqual(d.read(Path(tmp) / "campaign" / "approved-plan.json"), before)
                self.assertEqual(value, before)

    def test_selected_study_requires_both_gates_before_measurement_and_never_runs_one_host(self):
        for invalid_gate in (None, "missing", "health", "census", "oracle"):
            with self.subTest(invalid_gate=invalid_gate), tempfile.TemporaryDirectory() as tmp:
                value = plan(); value["selected_topologies"] = [3, 2]; value["prerequisite_topologies"] = [3, 2, 1]
                before = copy.deepcopy(value)
                nodes = {name: dict(id=i + 1) for i, name in enumerate(("node1", "node2", "node3", "client1", "client2"))}
                provider = mock.Mock(); provider.prepare.return_value = dict(nodes=nodes)
                provider.teardown.return_value = dict(passed=True)
                provider.setup_cluster.side_effect = lambda *args: dict(storage_hosts=args[3])
                campaign = d.Campaign(value, Path(tmp) / "campaign", provider)
                campaign.install_helpers = mock.Mock(); campaign.quiet = mock.Mock(); campaign.stable = mock.Mock()
                campaign.host_samples = mock.Mock(return_value={n: dict(fdb_pids={}) for n in nodes})
                calls = []
                def wave(layout, hosts, count, number):
                    self.assertIn(hosts, [3, 2])
                    calls.append((hosts, count))
                    gate = dict(storage_hosts=hosts, count_per_producer=count, healthy=True,
                                both_censuses=True, both_oracles=True)
                    if hosts == 2 and count == 100000 and invalid_gate is not None:
                        if invalid_gate == "missing": return None
                        field = {"health": "healthy", "census": "both_censuses", "oracle": "both_oracles"}[invalid_gate]
                        gate[field] = False
                    return gate
                campaign.wave = wave
                with mock.patch.object(p, "validate_plan"), mock.patch.object(p, "verify_source", return_value=dict(files=1)), \
                     mock.patch.object(p, "bill_bound", return_value={}), mock.patch.object(p, "rounded_server_bill", return_value={}), \
                     mock.patch.object(d, "remaining", return_value=30):
                    if invalid_gate is None:
                        campaign.run()
                    else:
                        with self.assertRaisesRegex(ValueError, "promotion requires"): campaign.run()
                expected = [(3, 100000), (2, 100000)]
                self.assertEqual(calls, expected + [(3, 1000000), (2, 1000000)] if invalid_gate is None else expected)
                arms = [call.args[4] for call in provider.setup_cluster.call_args_list]
                self.assertEqual(arms[:2], ["hosts3-prerequisite", "hosts2-prerequisite"])
                self.assertFalse(any("hosts1" in arm for arm in arms))
                self.assertEqual(provider.teardown.call_count, 1)
                self.assertEqual(d.read(Path(tmp) / "campaign" / "approved-plan.json"), before)
                self.assertEqual(value, before)
                if invalid_gate is None:
                    self.assertEqual(d.read(Path(tmp) / "campaign" / "measured.json")["inserted_rows"], 4400000)

    def test_selected_two_then_three_measured_order_requires_both_fresh_gates_and_failure_is_fatal(self):
        for failure in (None, "hosts3-prerequisite", "hosts2-prerequisite", "hosts2-measured"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                value = plan(); value["selected_topologies"] = [3, 2]
                value["prerequisite_topologies"] = [3, 2, 1]; value["measured_topologies"] = [2, 3, 1]
                before = copy.deepcopy(value)
                nodes = {name: dict(id=i+1) for i, name in enumerate(("node1", "node2", "node3", "client1", "client2"))}
                provider = mock.Mock(); provider.prepare.return_value = dict(nodes=nodes)
                provider.teardown.return_value = dict(passed=True)
                fresh = []

                def setup(*args):
                    layout = dict(storage_hosts=args[3], armid=args[4], fresh_instance=len(fresh)+1)
                    fresh.append(layout)
                    return layout

                provider.setup_cluster.side_effect = setup
                campaign = d.Campaign(value, Path(tmp)/"campaign", provider)
                campaign.install_helpers = mock.Mock(); campaign.quiet = mock.Mock(); campaign.stable = mock.Mock()
                campaign.host_samples = mock.Mock(return_value={name: dict(fdb_pids={}) for name in nodes})
                calls = []

                def wave(layout, hosts, count, number):
                    if count == 1000000:
                        self.assertEqual(calls[:2], [(3, 100000), (2, 100000)])
                        self.assertIn(layout["armid"], ("hosts2-measured", "hosts3-measured"))
                        self.assertGreater(layout["fresh_instance"], 2)
                    calls.append((hosts, count))
                    if layout["armid"] == failure:
                        raise ValueError("strict study failure: "+failure)
                    return dict(storage_hosts=hosts, count_per_producer=count, healthy=True,
                                both_censuses=True, both_oracles=True)

                campaign.wave = wave
                with mock.patch.object(p, "validate_plan"), mock.patch.object(p, "verify_source", return_value=dict(files=1)), \
                     mock.patch.object(p, "bill_bound", return_value={}), mock.patch.object(p, "rounded_server_bill", return_value={}), \
                     mock.patch.object(d, "remaining", return_value=30):
                    if failure:
                        with self.assertRaisesRegex(ValueError, "strict study failure"):
                            campaign.run()
                    else:
                        campaign.run()
                full = [(3, 100000), (2, 100000), (2, 1000000), (3, 1000000)]
                length = {None: 4, "hosts3-prerequisite": 1, "hosts2-prerequisite": 2, "hosts2-measured": 3}[failure]
                self.assertEqual(calls, full[:length])
                self.assertEqual([layout["armid"] for layout in fresh],
                                 ["hosts3-prerequisite", "hosts2-prerequisite", "hosts2-measured", "hosts3-measured"][:length])
                self.assertEqual(len({layout["fresh_instance"] for layout in fresh}), length)
                self.assertEqual(provider.teardown.call_count, 1)
                self.assertEqual(d.read(Path(tmp)/"campaign"/"approved-plan.json"), before)
                self.assertEqual(value, before)
                if failure:
                    self.assertFalse((Path(tmp)/"campaign"/"measured.json").exists())

    def test_failed_prerequisite_never_promotes_and_still_cleans_up(self):
        with tempfile.TemporaryDirectory() as tmp:
            nodes = {name: dict(id=i + 1) for i, name in enumerate(("node1", "node2", "node3", "client1", "client2"))}
            provider = mock.Mock(); provider.prepare.return_value = dict(nodes=nodes)
            provider.teardown.return_value = dict(passed=True)
            campaign = d.Campaign(plan(), Path(tmp) / "campaign", provider)
            campaign.install_helpers = mock.Mock(); campaign.quiet = mock.Mock(); campaign.stable = mock.Mock()
            campaign.host_samples = mock.Mock(return_value={n: dict(fdb_pids={}) for n in nodes})
            campaign.wave = mock.Mock(side_effect=ValueError("strict health failure"))
            with mock.patch.object(p, "validate_plan"), mock.patch.object(p, "verify_source", return_value=dict(files=1)), \
                 mock.patch.object(p, "bill_bound", return_value={}), mock.patch.object(p, "rounded_server_bill", return_value={}), \
                 mock.patch.object(d, "remaining", return_value=30):
                with self.assertRaisesRegex(ValueError, "strict health failure"): campaign.run()
            self.assertEqual(campaign.wave.call_count, 1)
            self.assertEqual(provider.teardown.call_count, 1)

    def test_receipt_cap_is_fail_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "large"; path.touch()
            with path.open("r+b") as stream: stream.truncate(1024 ** 3)
            with self.assertRaisesRegex(ValueError, "receipt allocation"): d.Campaign(plan(), tmp, mock.Mock()).receipt_budget()


if __name__ == "__main__":
    unittest.main()
