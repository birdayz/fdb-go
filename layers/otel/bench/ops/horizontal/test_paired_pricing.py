#!/usr/bin/env python3
"""Offline pricing normalizer regressions; no provider, clock, or process calls."""
from copy import deepcopy
from decimal import localcontext
import hashlib
import json
import unittest

import paired_cost as cost
import paired_pricing as pricing


FIRST = 10000


def response():
    return {"pricing": {
        "currency": "EUR", "vat_rate": "19.000000",
        "server_types": [{"name": "ccx43", "prices": [{"location": "hel1", "price_hourly": {
            "net": "0.3758", "gross": "0.447202"}}]}],
        "primary_ips": [{"type": "ipv4", "prices": [{"location": "hel1", "price_hourly": {
            "net": "0.0008", "gross": "0.000952"}}]}],
    }}


def normalize(raw=None, **changes):
    args = {"captured_epoch": FIRST, "traffic_upper_eur": "1.25",
            "other_upper_eur": "0.5", "bounds_conservative": True}
    args.update(changes)
    return pricing.normalize(json.dumps(response()) if raw is None else raw, **args)


def reseal(receipt):
    receipt[cost.SEAL] = cost.seal_receipt(receipt)
    return receipt


def replace(value, path, replacement):
    for key in path[:-1]:
        value = value[key]
    value[path[-1]] = replacement


class NormalizationTests(unittest.TestCase):
    def test_exact_receipt_raw_digest_and_decimal_strings(self):
        raw = json.dumps(response()) + "\n"
        receipt = normalize(raw)
        expected = {
            "schema": "paired_pricing_v1", "provider": "hetzner", "source": cost.PRICE_SOURCE,
            "captured_epoch": FIRST, "currency": "EUR", "server_type": "ccx43", "location": "hel1",
            "server_hourly_net_eur": "0.3758", "server_hourly_gross_eur": "0.447202",
            "ipv4_hourly_net_eur": "0.0008", "ipv4_hourly_gross_eur": "0.000952",
            "traffic_upper_eur": "1.25", "other_upper_eur": "0.5", "bounds_conservative": True,
            "provenance": {"response_sha256": hashlib.sha256(raw.encode("utf-8")).hexdigest(),
                           "pricing_path": "pricing.server_types[ccx43].prices[hel1]", "vat_rate": "0.19"},
        }
        self.assertEqual(receipt, reseal(expected))
        self.assertEqual(cost.validate_pricing(receipt, now_epoch=FIRST, expected_seal=receipt[cost.SEAL]), receipt)
        self.assertEqual(json.loads(json.dumps(receipt)), receipt)
        for field in ("server_hourly_net_eur", "server_hourly_gross_eur", "ipv4_hourly_net_eur",
                      "ipv4_hourly_gross_eur", "traffic_upper_eur", "other_upper_eur"):
            self.assertIs(type(receipt[field]), str)

    def test_exact_catalog_selection_not_first_entry_or_allowance(self):
        body = response()["pricing"]
        for catalog, identity, other in (("server_types", "name", "ccx33"), ("primary_ips", "type", "ipv6")):
            entry = body[catalog][0]
            entry["prices"].insert(0, {"location": "fsn1", "price_hourly": {"net": "0", "gross": "0"}})
            entry.update(included_traffic="999999", price_monthly={"net": "0", "gross": "0"})
            body[catalog].insert(0, {identity: other, "prices": []})
        receipt = normalize(json.dumps({"pricing": body}), traffic_upper_eur="2.50", other_upper_eur="0.75")
        self.assertEqual(receipt["server_hourly_gross_eur"], "0.447202")
        self.assertEqual(receipt["ipv4_hourly_gross_eur"], "0.000952")
        self.assertEqual(receipt["traffic_upper_eur"], "2.50")
        self.assertEqual(receipt["other_upper_eur"], "0.75")
        self.assertEqual(normalize(traffic_upper_eur="0", other_upper_eur="0")["traffic_upper_eur"], "0")

    def test_explicit_conservative_bounds_required(self):
        raw = json.dumps(response())
        args = {"captured_epoch": FIRST, "traffic_upper_eur": "1", "other_upper_eur": "1",
                "bounds_conservative": True}
        for field in args:
            missing = dict(args)
            del missing[field]
            with self.subTest(missing=field), self.assertRaises(TypeError):
                pricing.normalize(raw, **missing)
        for value in (False, None, 1, "true"):
            with self.subTest(bounds=value), self.assertRaises(cost.CostError):
                normalize(bounds_conservative=value)
        for field in ("traffic_upper_eur", "other_upper_eur"):
            for value in (None, 1, True, 0.1, "-1", "NaN", "Infinity", "1e2", "", " 1", "1000000000"):
                with self.subTest(field=field, value=value), self.assertRaises(cost.CostError):
                    normalize(**{field: value})

    def test_malformed_json_duplicate_keys_and_nonfinite_anywhere(self):
        raw = json.dumps(response())
        cases = ("", "{", raw + "{}", "null", "[]", "true",
                 raw.replace('"currency": "EUR"', '"currency": "EUR", "currency": "EUR"'),
                 raw.replace('"net": "0.3758"', '"net": "0.3758", "net": "0.3758"'),
                 raw[:-1] + ', "unused": {"x": 1, "x": 2}}',
                 raw[:-1] + ', "unused": NaN}', raw[:-1] + ', "unused": Infinity}',
                 raw[:-1] + ', "unused": -Infinity}', raw[:-1] + ', "unused": "\ud800"}')
        for text in cases:
            with self.subTest(raw=ascii(text)), self.assertRaises(cost.CostError):
                normalize(text)
        with self.assertRaises(cost.CostError):
            normalize(raw.encode())

    def test_missing_malformed_or_duplicate_catalog_entries(self):
        cases = [(("pricing",), None), (("pricing", "server_types"), []),
                 (("pricing", "primary_ips"), []), (("pricing", "server_types"), {}),
                 (("pricing", "primary_ips"), [None]),
                 (("pricing", "server_types", 0), {}),
                 (("pricing", "server_types", 0, "name"), "CCX43"),
                 (("pricing", "primary_ips", 0, "type"), "ipv6")]
        for catalog in ("server_types", "primary_ips"):
            entry = response()["pricing"][catalog][0]
            cases.extend([(("pricing", catalog), [entry, deepcopy(entry)]),
                          (("pricing", catalog, 0, "prices"), []),
                          (("pricing", catalog, 0, "prices"), "hel1"),
                          (("pricing", catalog, 0, "prices"), [None]),
                          (("pricing", catalog, 0, "prices"), [entry["prices"][0], deepcopy(entry["prices"][0])]),
                          (("pricing", catalog, 0, "prices", 0, "location"), "HEL1"),
                          (("pricing", catalog, 0, "prices", 0, "price_hourly"), {}),
                          (("pricing", catalog, 0, "prices", 0, "price_hourly"), None)])
        for path, value in cases:
            body = response()
            replace(body, path, value)
            with self.subTest(path=path, value=value), self.assertRaises(cost.CostError):
                normalize(json.dumps(body))
        for path in (("pricing",), ("pricing", "currency"), ("pricing", "vat_rate"),
                     ("pricing", "server_types"), ("pricing", "primary_ips"),
                     ("pricing", "server_types", 0, "prices"),
                     ("pricing", "primary_ips", 0, "prices", 0, "price_hourly", "gross")):
            body = response()
            parent = body
            for key in path[:-1]:
                parent = parent[key]
            del parent[path[-1]]
            with self.subTest(missing=path), self.assertRaises(cost.CostError):
                normalize(json.dumps(body))

    def test_currency_vat_percent_fraction_and_raw_money_rejected(self):
        for currency in ("USD", "eur", None, 1):
            body = response()
            body["pricing"]["currency"] = currency
            with self.subTest(currency=currency), self.assertRaises(cost.CostError):
                normalize(json.dumps(body))
        for vat in ("0.19", "0.190000", "20.000000", "119", "-19", "NaN", "Infinity", "19e0", 19, None):
            body = response()
            body["pricing"]["vat_rate"] = vat
            with self.subTest(vat=vat), self.assertRaises(cost.CostError):
                normalize(json.dumps(body))
        for catalog in ("server_types", "primary_ips"):
            for field in ("net", "gross"):
                for value in (None, 1, True, 0.1, "0", "-1", "NaN", "Infinity", "1e-3", "0.1234567890123456789"):
                    body = response()
                    body["pricing"][catalog][0]["prices"][0]["price_hourly"][field] = value
                    with self.subTest(catalog=catalog, field=field, value=value), self.assertRaises(cost.CostError):
                        normalize(json.dumps(body))

    def test_decimal_context_cannot_round_raw_gross_consistency(self):
        with localcontext() as ctx:
            ctx.prec = 2
            self.assertEqual(normalize()["provenance"]["vat_rate"], "0.19")
            for catalog in ("server_types", "primary_ips"):
                body = response()
                hourly = body["pricing"][catalog][0]["prices"][0]["price_hourly"]
                hourly["gross"] += "000000000001"
                with self.subTest(catalog=catalog), self.assertRaises(cost.CostError):
                    normalize(json.dumps(body))

    def test_stale_future_and_invalid_capture_via_cost_validator(self):
        receipt = normalize()
        for now in (FIRST - 1, FIRST + cost.MAX_PRICE_AGE_SECONDS + 1):
            with self.subTest(now=now), self.assertRaisesRegex(cost.CostError, "stale or future"):
                cost.validate_pricing(receipt, now_epoch=now, expected_seal=receipt[cost.SEAL])
        self.assertEqual(cost.validate_pricing(receipt, now_epoch=FIRST + cost.MAX_PRICE_AGE_SECONDS,
                                             expected_seal=receipt[cost.SEAL]), receipt)
        for epoch in (True, 0, -1, "10000", 10000.0, 10 ** 12):
            with self.subTest(epoch=epoch), self.assertRaises(cost.CostError):
                normalize(captured_epoch=epoch)


class SameBoundsTests(unittest.TestCase):
    def test_same_bound_recapture_allows_new_epoch_and_exact_raw_body_hash(self):
        raw = json.dumps(response())
        original = normalize(raw)
        fresh = normalize(raw + " \n", captured_epoch=FIRST + 4000)
        before = deepcopy((original, fresh))
        self.assertNotEqual(original["provenance"]["response_sha256"], fresh["provenance"]["response_sha256"])
        self.assertNotEqual(original[cost.SEAL], fresh[cost.SEAL])
        self.assertTrue(pricing.same_bounds(original, fresh))
        self.assertTrue(pricing.same_bounds(original, original))
        self.assertEqual((original, fresh), before)
        # Comparing stale old evidence is allowed; using stale fresh evidence is not.
        with self.assertRaises(cost.CostError):
            cost.validate_pricing(fresh, now_epoch=FIRST + 8000, expected_seal=fresh[cost.SEAL])

    def test_changed_raw_rates_and_explicit_extras_are_not_same_bound(self):
        original = normalize()
        for catalog in ("server_types", "primary_ips"):
            for net, gross in (("0.5", "0.595"), ("0.0001", "0.000119")):
                body = response()
                body["pricing"][catalog][0]["prices"][0]["price_hourly"] = {"net": net, "gross": gross}
                fresh = normalize(json.dumps(body), captured_epoch=FIRST + 1)
                with self.subTest(catalog=catalog, net=net):
                    self.assertFalse(pricing.same_bounds(original, fresh))
        for field in ("traffic_upper_eur", "other_upper_eur"):
            for value in ("0", "2", "1.250"):
                with self.subTest(field=field, value=value):
                    self.assertFalse(pricing.same_bounds(original, normalize(**{field: value})))
        # Even equivalent decimal text is not an exact unchanged field.
        changed = deepcopy(original)
        changed["server_hourly_net_eur"] += "0"
        self.assertFalse(pricing.same_bounds(original, reseal(changed)))

    def test_changed_tax_basis_or_currency_and_provenance_refuse(self):
        original = normalize()
        changed = deepcopy(original)
        changed["provenance"]["vat_rate"] = "0.20"
        changed["server_hourly_gross_eur"] = "0.45096"
        changed["ipv4_hourly_gross_eur"] = "0.00096"
        self.assertFalse(pricing.same_bounds(original, reseal(changed)))
        for path, value in ((("currency",), "USD"), (("provider",), "other"),
                            (("server_type",), "ccx33"), (("location",), "fsn1"),
                            (("source",), "https://example.invalid/pricing"),
                            (("schema",), "paired_pricing_v2"), (("bounds_conservative",), False),
                            (("provenance", "vat_rate"), "19.000000"),
                            (("provenance", "pricing_path"), "pricing.primary_ips[ipv4]")):
            changed = deepcopy(original)
            replace(changed, path, value)
            reseal(changed)
            for left, right in ((original, changed), (changed, original)):
                with self.subTest(path=path, value=value), self.assertRaises(cost.CostError):
                    pricing.same_bounds(left, right)

    def test_tampered_raw_body_receipt_and_unpinned_seal_refuse(self):
        original = normalize()
        for path, value in ((("provenance", "response_sha256"), "b" * 64),
                            (("captured_epoch",), FIRST + 1), (("traffic_upper_eur",), "0"),
                            ((cost.SEAL,), "0" * 64)):
            changed = deepcopy(original)
            replace(changed, path, value)
            with self.subTest(path=path), self.assertRaises(cost.CostError):
                pricing.same_bounds(original, changed)
        fresh = normalize(json.dumps(response()) + " ", captured_epoch=FIRST + 1)
        with self.assertRaisesRegex(cost.CostError, "untrusted"):
            cost.validate_pricing(fresh, now_epoch=FIRST + 1, expected_seal=original[cost.SEAL])

    def test_rollback_missing_unknown_fields_and_malformed_receipts_refuse(self):
        original = normalize()
        with self.assertRaisesRegex(cost.CostError, "rollback"):
            pricing.same_bounds(original, normalize(captured_epoch=FIRST - 1))
        for value in (None, [], "quote"):
            with self.subTest(value=value), self.assertRaises(cost.CostError):
                pricing.same_bounds(original, value)
        for field in original:
            changed = deepcopy(original)
            del changed[field]
            with self.subTest(missing=field), self.assertRaises(cost.CostError):
                pricing.same_bounds(original, changed)
        for field in ("first_epoch", "deadline_epoch", "hosts", "accrued_upper_eur"):
            changed = deepcopy(original)
            changed[field] = "0"
            with self.subTest(extra=field), self.assertRaises(cost.CostError):
                pricing.same_bounds(original, reseal(changed))


if __name__ == "__main__":
    unittest.main()
