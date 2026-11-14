#!/usr/bin/env python3
"""Offline normalization of retained read-only Hetzner /pricing response text.

This module neither fetches prices nor authenticates provider evidence. The
controller retains the exact UTF-8 response, independently pins the receipt seal,
and validates freshness against its own observation time. Explicit traffic and
other bounds cover the whole campaign; catalog allowances, monthly caps and
proration are never used to infer them.
"""
from decimal import Decimal, InvalidOperation, localcontext
import hashlib
import json
import re

import paired_cost as cost


PRICING_PATH = "pricing.server_types[ccx43].prices[hel1]"
_DECIMAL_TEXT = re.compile(r"(?:0|[1-9][0-9]{0,8})(?:\.[0-9]{1,18})?")


def _require(condition, message):
    if not condition:
        raise cost.CostError(message)


def _decimal(value, what):
    _require(type(value) is str and _DECIMAL_TEXT.fullmatch(value) is not None,
             "finite nonnegative decimal text required: " + what)
    return Decimal(value)


def _loads(raw):
    _require(type(raw) is str, "raw pricing response text required")

    def pairs(items):
        result = {}
        for key, value in items:
            _require(key not in result, "duplicate JSON field: " + key)
            result[key] = value
        return result

    def constant(_):
        raise cost.CostError("nonfinite JSON value")

    def floating(text):
        value = Decimal(text)
        _require(value.is_finite(), "nonfinite JSON value")
        return value

    try:
        return json.loads(raw, object_pairs_hook=pairs, parse_constant=constant,
                          parse_float=floating)
    except (ValueError, InvalidOperation, RecursionError) as exc:
        raise cost.CostError("invalid pricing JSON: " + str(exc)) from exc


def _field(value, key):
    _require(type(value) is dict and key in value, "missing pricing field: " + key)
    return value[key]


def _select(items, key, identity):
    _require(type(items) is list, "pricing catalog array required: " + key)
    _require(all(type(item) is dict and type(item.get(key)) is str for item in items),
             "malformed pricing catalog entry: " + key)
    selected = [item for item in items if item[key] == identity]
    _require(len(selected) == 1, "exactly one pricing entry required: " + identity)
    return selected[0]


def _hourly(pricing, catalog, key, identity):
    item = _select(_field(pricing, catalog), key, identity)
    location = _select(_field(item, "prices"), "location", "hel1")
    hourly = _field(location, "price_hourly")
    # Validate raw strings before copying them, without rounding or deriving a
    # gross price that could conceal inconsistent provider evidence.
    net = _field(hourly, "net")
    gross = _field(hourly, "gross")
    _decimal(net, identity + " net hourly")
    _decimal(gross, identity + " gross hourly")
    return net, gross


def normalize(raw: str, *, captured_epoch: int, traffic_upper_eur: str,
              other_upper_eur: str, bounds_conservative: bool) -> dict:
    """Seal a paired_pricing_v1 quote for exact ccx43/HEL1 and IPv4/HEL1.

    The accepted tax basis is EUR with 19 percent VAT, expressed as a fraction
    only in the normalized receipt. No capture clock, quota, lease, or ledger is
    modified here. Call validate_pricing with the actual current epoch before
    using the result for admission or continuation.
    """
    pricing = _field(_loads(raw), "pricing")
    _require(_field(pricing, "currency") == "EUR", "EUR pricing required")
    percent = _decimal(_field(pricing, "vat_rate"), "provider VAT percent")
    _require(percent == Decimal("19"), "19 percent provider VAT required, not a fraction")
    with localcontext() as ctx:
        ctx.prec = 100
        vat = format(percent / 100, "f").rstrip("0").rstrip(".")
    server_net, server_gross = _hourly(pricing, "server_types", "name", "ccx43")
    ipv4_net, ipv4_gross = _hourly(pricing, "primary_ips", "type", "ipv4")
    try:
        response_sha256 = hashlib.sha256(raw.encode("utf-8")).hexdigest()
    except UnicodeEncodeError as exc:
        raise cost.CostError("UTF-8 pricing response required") from exc
    receipt = {
        "schema": "paired_pricing_v1", "provider": "hetzner", "source": cost.PRICE_SOURCE,
        "captured_epoch": captured_epoch, "currency": "EUR", "server_type": "ccx43", "location": "hel1",
        "server_hourly_net_eur": server_net, "server_hourly_gross_eur": server_gross,
        "ipv4_hourly_net_eur": ipv4_net, "ipv4_hourly_gross_eur": ipv4_gross,
        "traffic_upper_eur": traffic_upper_eur, "other_upper_eur": other_upper_eur,
        "bounds_conservative": bounds_conservative,
        "provenance": {"response_sha256": response_sha256, "pricing_path": PRICING_PATH, "vat_rate": vat},
    }
    receipt[cost.SEAL] = cost.seal_receipt(receipt)
    return cost.validate_pricing(receipt, now_epoch=captured_epoch, expected_seal=receipt[cost.SEAL])


def same_bounds(original: dict, fresh: dict) -> bool:
    """Compare sealed receipts exactly, excluding capture time and raw-body hash.

    Invalid evidence raises CostError; valid but changed bounds return False.
    Each receipt is structurally validated at its own capture epoch so stale
    original evidence can be compared. The caller must separately validate the
    fresh quote against the current epoch and an independently pinned seal.
    A recapture must not move backwards in time. This function never refreshes
    a ledger, renews a lease, resets counters, or lowers accrued reservations.
    """
    comparable = []
    epochs = []
    for receipt in (original, fresh):
        _require(type(receipt) is dict, "pricing receipt object required")
        checked = cost.validate_pricing(receipt, now_epoch=receipt.get("captured_epoch"),
                                        expected_seal=receipt.get(cost.SEAL))
        epochs.append(checked.pop("captured_epoch"))
        checked.pop(cost.SEAL)
        checked["provenance"].pop("response_sha256")
        comparable.append(checked)
    _require(epochs[1] >= epochs[0], "pricing recapture clock rollback")
    return comparable[0] == comparable[1]
