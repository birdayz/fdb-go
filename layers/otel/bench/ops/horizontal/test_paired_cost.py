#!/usr/bin/env python3
"""Fake-only price, inventory, time and cost guards; no provider calls or I/O."""
from copy import deepcopy
from decimal import Decimal, localcontext
import json
import unittest

import paired_cost as cost


FIRST = 10000


def seal(value):
    value[cost.SEAL] = cost.seal_receipt(value)
    return value


def pricing(**changes):
    receipt = {"schema": "paired_pricing_v1", "provider": "hetzner", "source": cost.PRICE_SOURCE,
               "captured_epoch": FIRST, "currency": "EUR", "server_type": "ccx43", "location": "hel1",
               "server_hourly_net_eur": "0.35", "server_hourly_gross_eur": "0.4165",
               "ipv4_hourly_net_eur": "0.0008", "ipv4_hourly_gross_eur": "0.000952",
               "traffic_upper_eur": "0", "other_upper_eur": "0.1", "bounds_conservative": True,
               "provenance": {"response_sha256": "a" * 64,
                              "pricing_path": "pricing.server_types[ccx43].prices[hel1]", "vat_rate": "0.19"}}
    receipt.update(changes)
    return seal(receipt)


def admit(receipt=None, hosts=6, lifetime=None):
    receipt = pricing() if receipt is None else receipt
    lifetime = (7200 if hosts == 6 else 3600) if lifetime is None else lifetime
    return cost.admit(receipt, expected_seal=receipt[cost.SEAL], first_epoch=FIRST,
                      deadline_epoch=FIRST + lifetime, now_epoch=FIRST, held_host_count=hosts)


def populated(hosts=6):
    ledger = admit(hosts=hosts)
    for identity in range(1, hosts + 1):
        ledger = cost.record_creation(ledger, host_id=identity, created_epoch=FIRST, now_epoch=FIRST)
    return ledger


def deletion(identity, epoch):
    return seal({"host_id": identity, "observed_epoch": epoch,
                 "source": "https://api.hetzner.cloud/v1/servers/" + str(identity), "status": "not_found"})


class PairedCostTests(unittest.TestCase):
    def test_admission_reserves_all_six_and_extras_through_cleanup_margin(self):
        ledger = admit()
        report = cost.assess(ledger, now_epoch=FIRST, owned_ids=[])
        self.assertEqual(report["held_hosts"], 6)
        self.assertEqual(report["rounded_reserved_host_hours"], 18)
        self.assertEqual(Decimal(report["server_reserved_eur"]), Decimal("7.497"))
        self.assertEqual(Decimal(report["ipv4_reserved_eur"]), Decimal("0.017136"))
        self.assertEqual(Decimal(report["extras_reserved_eur"]), Decimal("0.1"))
        self.assertEqual(Decimal(report["total_upper_eur"]), Decimal("7.614136"))
        self.assertFalse(report["all_deleted"])
        self.assertEqual(json.loads(json.dumps(ledger)), ledger)

    def test_delayed_admission_and_batched_provider_receipts_keep_original_bound(self):
        receipt = pricing()
        ledger = cost.admit(receipt, expected_seal=receipt[cost.SEAL], first_epoch=FIRST,
                            deadline_epoch=FIRST + 3600, now_epoch=FIRST + 10, held_host_count=2)
        for identity in (1, 2):
            ledger = cost.record_creation(ledger, host_id=identity, created_epoch=FIRST + 20 + identity,
                                          now_epoch=FIRST + 30)
        report = cost.assess(ledger, now_epoch=FIRST + 30, owned_ids=[1, 2])
        self.assertEqual(report["first_epoch"], FIRST)
        self.assertEqual(report["deadline_epoch"], FIRST + 3600)
        self.assertEqual(report["rounded_reserved_host_hours"], 4)
        for identity in (1, 2):
            ledger = cost.record_deletion(ledger, verification=deletion(identity, FIRST + 40 + identity),
                                          now_epoch=FIRST + 50)
        self.assertTrue(cost.assess(ledger, now_epoch=FIRST + 50, owned_ids=[])["all_deleted"])
        with self.assertRaises(cost.CostError):
            cost.admit(receipt, expected_seal=receipt[cost.SEAL], first_epoch=FIRST,
                       deadline_epoch=FIRST + 3600, now_epoch=FIRST + 2400, held_host_count=2)

    def test_stage1_two_hosts_short_or_full_hour(self):
        for seconds in (1201, 1800, 3600):
            with self.subTest(seconds=seconds):
                ledger = admit(hosts=2, lifetime=seconds)
                report = cost.assess(ledger, now_epoch=FIRST, owned_ids=[])
                self.assertEqual(report["held_hosts"], 2)
                self.assertEqual(report["rounded_reserved_host_hours"], 4)
                self.assertEqual(Decimal(report["total_upper_eur"]), Decimal("1.769808"))
                self.assertEqual(report["deadline_epoch"], FIRST + seconds)

    def test_root_approved_pilot_reserves_cleanup_with_zero_traffic_and_ten_cent_margin(self):
        quote = pricing(server_hourly_net_eur="0.375800", server_hourly_gross_eur="0.447202",
                        ipv4_hourly_net_eur="0.000800", ipv4_hourly_gross_eur="0.000952",
                        traffic_upper_eur="0", other_upper_eur="0.10")
        report = cost.assess(admit(quote, hosts=2), now_epoch=FIRST, owned_ids=[])
        self.assertEqual(report["rounded_reserved_host_hours"], 4)
        self.assertEqual(Decimal(report["server_reserved_eur"]) + Decimal(report["ipv4_reserved_eur"]), Decimal("1.792616"))
        self.assertEqual(Decimal(report["total_upper_eur"]), Decimal("1.892616"))

    def test_unknown_count_and_cross_profile_deadlines_rejected(self):
        for hosts, seconds in ((True, 3600), (2.0, 3600), (1, 3600), (3, 7200), (5, 7200),
                               (7, 7200), (2, 0), (2, -1), (2, 3601), (2, 7200),
                               (6, 3600), (6, 7199), (6, 7201)):
            with self.subTest(hosts=hosts, seconds=seconds), self.assertRaises(cost.CostError):
                admit(hosts=hosts, lifetime=seconds)

    def test_exact_stage_budget_accepts_tiny_overflow_refuses(self):
        for hosts, traffic, limit in ((2, "0.330192", "2"), (6, "0.485864", "8")):
            with self.subTest(hosts=hosts):
                exact = pricing(traffic_upper_eur=traffic, other_upper_eur="0")
                report = cost.assess(admit(exact, hosts=hosts), now_epoch=FIRST, owned_ids=[])
                self.assertEqual(Decimal(report["total_upper_eur"]), Decimal(limit))
                overflow = traffic + "000000000001"
                with self.assertRaisesRegex(cost.CostError, "allocation") as rejected:
                    admit(pricing(traffic_upper_eur=overflow, other_upper_eur="0"), hosts=hosts)
                self.assertGreater(Decimal(rejected.exception.report["total_upper_eur"]), Decimal(limit))

    def test_global_budget_still_rejects_totals_over_ten(self):
        with self.assertRaisesRegex(cost.CostError, "budget") as rejected:
            admit(pricing(traffic_upper_eur="10", other_upper_eur="0"))
        self.assertGreater(Decimal(rejected.exception.report["total_upper_eur"]), Decimal("10"))

    def test_stage_budget_includes_cleanup_margin_not_just_active_lease(self):
        cases = ((2, "0.4423", "0.526337", "0.01", "0.0119", "2", 4),
                 (6, "0.3758", "0.447202", "0.0008", "0.000952", "8", 18))
        for hosts, server_net, server_gross, ipv4_net, ipv4_gross, limit, hours in cases:
            quote = pricing(server_hourly_net_eur=server_net, server_hourly_gross_eur=server_gross,
                            ipv4_hourly_net_eur=ipv4_net, ipv4_hourly_gross_eur=ipv4_gross,
                            traffic_upper_eur="0", other_upper_eur="0")
            lease_hours = 2 if hosts == 2 else 12
            self.assertLess(Decimal(lease_hours) * (Decimal(server_gross) + Decimal(ipv4_gross)), Decimal(limit))
            with self.subTest(hosts=hosts), self.assertRaisesRegex(cost.CostError, "allocation") as rejected:
                admit(quote, hosts=hosts)
            self.assertEqual(rejected.exception.report["rounded_reserved_host_hours"], hours)
            self.assertGreater(Decimal(rejected.exception.report["total_upper_eur"]), Decimal(limit))

    def test_decimal_context_does_not_hide_overflow(self):
        with localcontext() as ctx:
            ctx.prec = 2
            with self.assertRaisesRegex(cost.CostError, "allocation"):
                admit(pricing(traffic_upper_eur="0.485864000000000001", other_upper_eur="0"))

    def test_invalid_money_and_unknown_extras_rejected(self):
        for field in ("server_hourly_net_eur", "server_hourly_gross_eur", "ipv4_hourly_net_eur",
                      "ipv4_hourly_gross_eur", "traffic_upper_eur", "other_upper_eur"):
            for value in (None, True, False, 1, 0.1, float("nan"), float("inf"), "NaN", "Infinity", "-1",
                          "unknown", "", " 1", "+1", "1e100", "0.0000000000000000001", "1000000000"):
                with self.subTest(field=field, value=value), self.assertRaises(cost.CostError):
                    receipt = pricing()
                    receipt[field] = value
                    # Nonfinite non-JSON values must fail even before a seal exists.
                    receipt[cost.SEAL] = "0" * 64
                    cost.validate_pricing(receipt, now_epoch=FIRST, expected_seal=receipt[cost.SEAL])

    def test_zero_unknown_server_or_ipv4_price_rejected(self):
        for item in ("server", "ipv4"):
            with self.subTest(item=item), self.assertRaises(cost.CostError):
                receipt = pricing(**{item + "_hourly_net_eur": "0", item + "_hourly_gross_eur": "0"})
                admit(receipt)

    def test_receipt_requires_all_fields_and_rejects_unknown_fields(self):
        for field in pricing():
            with self.subTest(missing=field), self.assertRaises(cost.CostError):
                receipt = pricing()
                del receipt[field]
                cost.validate_pricing(receipt, now_epoch=FIRST, expected_seal="a" * 64)
        with self.assertRaises(cost.CostError):
            admit(pricing(unexpected="free"))

    def test_pricing_source_catalog_currency_and_conservative_provenance(self):
        cases = {"schema": "old", "provider": "unknown", "source": "https://example.invalid/pricing",
                 "currency": "USD", "server_type": "ccx33", "location": "fsn1", "bounds_conservative": 1}
        for field, value in cases.items():
            with self.subTest(field=field), self.assertRaises(cost.CostError):
                admit(pricing(**{field: value}))
        for field, value in (("response_sha256", "bad"), ("pricing_path", "unknown"),
                             ("vat_rate", None), ("vat_rate", True), ("vat_rate", "2"), ("vat_rate", "0.20")):
            with self.subTest(field=field, value=value), self.assertRaises(cost.CostError):
                receipt = pricing()
                receipt["provenance"][field] = value
                admit(seal(receipt))
        receipt = pricing()
        receipt["provenance"]["unknown"] = "x"
        with self.assertRaises(cost.CostError):
            admit(seal(receipt))

    def test_stale_future_and_untyped_capture_rejected(self):
        for epoch in (FIRST - 3601, FIRST + 1, None, True, 1.0, "10000", -1):
            with self.subTest(epoch=epoch), self.assertRaises(cost.CostError):
                admit(pricing(captured_epoch=epoch))
        cost.validate_pricing(pricing(captured_epoch=FIRST - 3600), now_epoch=FIRST,
                              expected_seal=pricing(captured_epoch=FIRST - 3600)[cost.SEAL])

    def test_capture_freshness_is_bound_to_first_epoch_not_delayed_admission(self):
        for hosts, lifetime in ((2, 3600), (6, 7200)):
            quote = pricing(captured_epoch=FIRST - 3600)
            ledger = cost.admit(quote, expected_seal=quote[cost.SEAL], first_epoch=FIRST,
                                deadline_epoch=FIRST + lifetime, now_epoch=FIRST + 30, held_host_count=hosts)
            self.assertEqual(ledger["pricing"], quote)
            self.assertEqual(ledger["first_epoch"], FIRST)
            self.assertEqual(ledger["deadline_epoch"], FIRST + lifetime)
            cost.assess(ledger, now_epoch=FIRST + 31, owned_ids=[])
            for captured in (FIRST - 3601, FIRST + 1, FIRST + 30):
                invalid = pricing(captured_epoch=captured)
                with self.subTest(hosts=hosts, captured=captured), self.assertRaises(cost.CostError):
                    cost.admit(invalid, expected_seal=invalid[cost.SEAL], first_epoch=FIRST,
                               deadline_epoch=FIRST + lifetime, now_epoch=FIRST + 30, held_host_count=hosts)

    def test_resealed_ledger_keeps_original_admission_capture_requirement(self):
        original = populated()
        for captured in (FIRST - 3601, FIRST + 1):
            changed = deepcopy(original)
            changed["pricing"] = pricing(captured_epoch=captured)
            changed["pricing_seal"] = changed["pricing"][cost.SEAL]
            changed["last_epoch"] = FIRST + 30
            with self.subTest(captured=captured), self.assertRaises(cost.CostError):
                cost.assess(seal(changed), now_epoch=FIRST + 31, owned_ids=list(range(1, 7)))

    def test_seal_and_external_pin_are_required(self):
        receipt = pricing()
        receipt["other_upper_eur"] = "0"
        with self.assertRaisesRegex(cost.CostError, "seal"):
            admit(receipt)
        receipt = pricing()
        with self.assertRaisesRegex(cost.CostError, "seal"):
            cost.admit(receipt, expected_seal="b" * 64, first_epoch=FIRST,
                       deadline_epoch=FIRST + 7200, now_epoch=FIRST)

    def test_first_and_deadline_not_reset(self):
        receipt = pricing()
        for first, deadline, now in ((True, FIRST + 7200, FIRST), (FIRST, True, FIRST),
                                     (FIRST, FIRST + 7200, True), (FIRST, FIRST + 7201, FIRST),
                                     (FIRST, FIRST + 7200, FIRST - 1),
                                     (FIRST, FIRST + 7200, FIRST + 6000)):
            with self.subTest(first=first, deadline=deadline, now=now), self.assertRaises(cost.CostError):
                cost.admit(receipt, expected_seal=receipt[cost.SEAL], first_epoch=first,
                           deadline_epoch=deadline, now_epoch=now)
        ledger = populated()
        with self.assertRaisesRegex(cost.CostError, "renewal"):
            cost.assess(ledger, now_epoch=FIRST + 1, owned_ids=list(range(1, 7)), deadline_epoch=FIRST + 7201)
        ledger["first_epoch"] += 100
        ledger["deadline_epoch"] += 100
        with self.assertRaisesRegex(cost.CostError, "seal"):
            cost.assess(ledger, now_epoch=FIRST + 100, owned_ids=list(range(1, 7)))

    def test_creation_is_functional_and_retains_exact_ids(self):
        ledger = admit()
        original = deepcopy(ledger)
        updated = cost.record_creation(ledger, host_id=1, created_epoch=FIRST, now_epoch=FIRST)
        self.assertEqual(ledger, original)
        self.assertEqual(updated["hosts"][0]["host_id"], 1)
        report = cost.assess(updated, now_epoch=FIRST + 1, owned_ids=[1])
        self.assertEqual(report["rounded_reserved_host_hours"], 18)
        self.assertEqual(report["owned_ids"], [1])

    def test_unexpected_duplicate_and_replacement_hosts_rejected(self):
        for identity in (True, False, 0, -1, "1", 1.0, None):
            with self.subTest(identity=identity), self.assertRaises(cost.CostError):
                cost.record_creation(admit(), host_id=identity, created_epoch=FIRST, now_epoch=FIRST)
        for count in (2, 6):
            ledger = populated(count)
            for identity in (1, 99):
                with self.subTest(count=count, identity=identity), self.assertRaises(cost.CostError):
                    cost.record_creation(ledger, host_id=identity, created_epoch=FIRST + 1, now_epoch=FIRST + 1)
            ledger = cost.record_deletion(ledger, verification=deletion(1, FIRST + 1), now_epoch=FIRST + 1)
            with self.assertRaises(cost.CostError):
                cost.record_creation(ledger, host_id=99, created_epoch=FIRST + 2, now_epoch=FIRST + 2)
        later = cost.record_creation(admit(), host_id=1, created_epoch=FIRST + 1, now_epoch=FIRST + 1)
        self.assertEqual(later["first_epoch"], FIRST)
        self.assertEqual(later["deadline_epoch"], FIRST + 7200)
        self.assertEqual(cost.assess(later, now_epoch=FIRST + 1, owned_ids=[1])["rounded_reserved_host_hours"], 18)

    def test_complete_owned_inventory_and_verified_absence_required(self):
        ledger = populated()
        for ids in (None, {}, [1, 2, 3], list(range(1, 8)), [1, 2, 3, 4, 5, True], [1, 2, 3, 4, 5, 5]):
            with self.subTest(ids=ids), self.assertRaises(cost.CostError):
                cost.assess(ledger, now_epoch=FIRST + 1, owned_ids=ids)
        self.assertEqual(cost.assess(ledger, now_epoch=FIRST + 1, owned_ids=set(range(1, 7)))["live_ids"], list(range(1, 7)))

    def test_only_verified_deletion_stops_accrual(self):
        ledger = populated()
        for identity in range(1, 7):
            ledger = cost.record_deletion(ledger, verification=deletion(identity, FIRST + 60), now_epoch=FIRST + 60)
        report = cost.assess(ledger, now_epoch=FIRST + 61, owned_ids=[])
        self.assertTrue(report["all_deleted"])
        self.assertEqual(report["rounded_reserved_host_hours"], 6)
        self.assertEqual(Decimal(report["total_upper_eur"]), Decimal("2.604712"))
        with self.assertRaises(cost.CostError):
            cost.assess(ledger, now_epoch=FIRST + 61, owned_ids=[1])

    def test_rounding_from_first_creation_not_per_stage_or_host_activation(self):
        ledger = admit()
        for identity in range(1, 7):
            epoch = FIRST if identity == 1 else FIRST + 3500
            ledger = cost.record_creation(ledger, host_id=identity, created_epoch=epoch, now_epoch=epoch)
        for identity in range(1, 7):
            ledger = cost.record_deletion(ledger, verification=deletion(identity, FIRST + 3601), now_epoch=FIRST + 3601)
        report = cost.assess(ledger, now_epoch=FIRST + 3601, owned_ids=[])
        self.assertEqual(report["rounded_reserved_host_hours"], 12)
        self.assertEqual(Decimal(report["total_upper_eur"]), Decimal("5.109424"))

    def test_delete_ack_unknown_id_backdate_bad_status_or_seal_refused(self):
        ledger = populated()
        for changes in ({"status": "delete_ack"}, {"status": "timeout"}, {"status": "forbidden"},
                        {"host_id": 99}, {"observed_epoch": FIRST + 2}, {"observed_epoch": True},
                        {"source": "local-inventory"}, {"extra": True}):
            evidence = deletion(1, FIRST + 1)
            evidence.update(changes)
            with self.subTest(changes=changes), self.assertRaises(cost.CostError):
                cost.record_deletion(ledger, verification=seal(evidence), now_epoch=FIRST + 1)
        bad = deletion(1, FIRST + 1)
        bad[cost.SEAL] = "b" * 64
        with self.assertRaises(cost.CostError):
            cost.record_deletion(ledger, verification=bad, now_epoch=FIRST + 1)
        ledger = cost.checkpoint(ledger, now_epoch=FIRST + 10, owned_ids=list(range(1, 7)))
        with self.assertRaisesRegex(cost.CostError, "backdated"):
            cost.record_deletion(ledger, verification=deletion(1, FIRST + 1), now_epoch=FIRST + 10)
        ledger = cost.record_deletion(ledger, verification=deletion(1, FIRST + 10), now_epoch=FIRST + 10)
        with self.assertRaises(cost.CostError):
            cost.record_deletion(ledger, verification=deletion(1, FIRST + 11), now_epoch=FIRST + 11)

    def test_missing_deletion_continues_accruing_past_deadline_and_cleanup_allowed(self):
        ledger = populated()
        with self.assertRaisesRegex(cost.CostError, "budget") as rejected:
            cost.assess(ledger, now_epoch=FIRST + 10801, owned_ids=list(range(1, 7)))
        self.assertEqual(rejected.exception.report["rounded_reserved_host_hours"], 24)
        self.assertFalse(rejected.exception.report["all_deleted"])
        for identity in range(1, 7):
            ledger = cost.record_deletion(ledger, verification=deletion(identity, FIRST + 10801), now_epoch=FIRST + 10801)
        self.assertTrue(all(h["deletion"] is not None for h in ledger["hosts"]))

    def test_exact_deadline_refuses_work_even_under_budget(self):
        ledger = populated()
        with self.assertRaisesRegex(cost.CostError, "hard deadline") as rejected:
            cost.assess(ledger, now_epoch=FIRST + 7200, owned_ids=list(range(1, 7)))
        self.assertEqual(rejected.exception.report["rounded_reserved_host_hours"], 18)
        with self.assertRaises(cost.CostError):
            cost.record_creation(admit(), host_id=1, created_epoch=FIRST + 7200, now_epoch=FIRST + 7200)

    def test_checkpoint_monotonicity_and_immutable_scope(self):
        ledger = populated(2)
        old = deepcopy(ledger)
        updated = cost.checkpoint(ledger, now_epoch=FIRST + 30, owned_ids=[1, 2])
        self.assertEqual(ledger, old)
        self.assertEqual(updated["first_epoch"], FIRST)
        self.assertEqual(updated["deadline_epoch"], FIRST + 3600)
        self.assertEqual(updated["last_epoch"], FIRST + 30)
        with self.assertRaisesRegex(cost.CostError, "rollback"):
            cost.assess(updated, now_epoch=FIRST + 29, owned_ids=[1, 2])
        for field, value in (("budget_eur", "11"), ("held_hosts", 5), ("schema", "unknown")):
            changed = deepcopy(ledger)
            changed[field] = value
            with self.subTest(field=field), self.assertRaises(cost.CostError):
                cost.assess(seal(changed), now_epoch=FIRST, owned_ids=[1, 2])

    def test_single_admission_quote_survives_last_active_minute_and_cleanup(self):
        for hosts, lifetime, active_cutoff in ((2, 3600, 2700), (6, 7200, 6000)):
            quote = pricing(captured_epoch=FIRST - 3600)
            ledger = admit(quote, hosts=hosts)
            for identity in range(1, hosts + 1):
                ledger = cost.record_creation(ledger, host_id=identity, created_epoch=FIRST, now_epoch=FIRST)
            original_quote = deepcopy(ledger["pricing"])
            ids = list(range(1, hosts + 1))
            for offset in (active_cutoff - 60, lifetime - 600, lifetime - 1):
                ledger = cost.checkpoint(ledger, now_epoch=FIRST + offset, owned_ids=ids)
                self.assertEqual(ledger["pricing"], original_quote)
                self.assertEqual(ledger["pricing_seal"], quote[cost.SEAL])
                self.assertEqual(ledger["first_epoch"], FIRST)
                self.assertEqual(ledger["deadline_epoch"], FIRST + lifetime)
            for identity in ids:
                ledger = cost.record_deletion(ledger, verification=deletion(identity, FIRST + lifetime - 1),
                                              now_epoch=FIRST + lifetime - 1)
            report = cost.assess(ledger, now_epoch=FIRST + lifetime - 1, owned_ids=[])
            self.assertTrue(report["all_deleted"])
            self.assertEqual(report["rounded_reserved_host_hours"], hosts * (lifetime // 3600))
            self.assertEqual(ledger["pricing"], original_quote)
            with self.assertRaisesRegex(cost.CostError, "hard deadline"):
                cost.assess(ledger, now_epoch=FIRST + lifetime, owned_ids=[])

    def test_refresh_is_disabled_even_for_identical_or_fresh_quotes(self):
        ledger = populated()
        original = deepcopy(ledger)
        for offset in (0, 1, 3601):
            for fields in ({}, {"traffic_upper_eur": "0.01"}, {"other_upper_eur": "0"},
                           {"server_hourly_net_eur": "0.1", "server_hourly_gross_eur": "0.119"},
                           {"ipv4_hourly_net_eur": "0.0001", "ipv4_hourly_gross_eur": "0.000119"},
                           {"other_upper_eur": "100"}):
                receipt = pricing(captured_epoch=FIRST + offset, **fields)
                with self.subTest(offset=offset, fields=fields), self.assertRaisesRegex(cost.CostError, "single admission capture"):
                    cost.refresh_pricing(ledger, receipt, expected_seal=receipt[cost.SEAL],
                                         now_epoch=FIRST + offset, owned_ids=list(range(1, 7)))
                self.assertEqual(ledger, original)


if __name__ == "__main__":
    unittest.main()
