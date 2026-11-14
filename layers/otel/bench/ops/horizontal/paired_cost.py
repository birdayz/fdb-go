#!/usr/bin/env python3
"""Offline EUR10 admission/continuation ledger for one bounded paired campaign.

All money is decimal text in the JSON interface. The controller must capture the
provider response, pin its receipt seal independently, and supply complete owned
inventory on every continuation. A SHA256 seal detects changed evidence; it is
not a provider signature or permission to trust a fabricated quote. Traffic and
other bounds must cover the *whole campaign*, including preparation and cleanup;
the caller must enforce those quotas. This module performs no I/O or cloud calls.
"""
from copy import deepcopy
from decimal import Decimal, InvalidOperation, localcontext
import hashlib
import json
import re


BUDGET_EUR = Decimal("10")
HOSTS = 6
LIFETIME_SECONDS = 7200
PROFILES = {2: 3600, 6: 7200}
MAX_PRICE_AGE_SECONDS = 3600
PRICE_SOURCE = "https://api.hetzner.cloud/v1/pricing"
SEAL = "seal_sha256"


class CostError(ValueError):
    """Admission/continuation refusal; report remains available for cleanup logs."""

    def __init__(self, message, report=None):
        super().__init__(message)
        self.report = report


def _require(condition, message):
    if not condition:
        raise CostError(message)


def _keys(value, keys, what):
    _require(type(value) is dict and set(value) == set(keys), "exact " + what + " fields required")


def _epoch(value, what):
    _require(type(value) is int and 0 < value < 10 ** 12, "invalid " + what)
    return value


def _host_id(value):
    _require(type(value) is int and 0 < value < 10 ** 18, "invalid host ID")
    return value


def _hex(value):
    return type(value) is str and re.fullmatch(r"[a-f0-9]{64}", value) is not None


def _decimal(value, what):
    # Bound both scale and significant digits, so context rounding cannot silently
    # erase a tiny over-budget amount. Floats (and bools) never enter arithmetic.
    _require(type(value) is str and re.fullmatch(r"(?:0|[1-9][0-9]{0,8})(?:\.[0-9]{1,18})?", value)
             is not None, "finite nonnegative decimal text required: " + what)
    try:
        result = Decimal(value)
    except InvalidOperation as exc:
        raise CostError("invalid decimal: " + what) from exc
    _require(result.is_finite() and result >= 0, "invalid decimal: " + what)
    return result


def seal_receipt(receipt):
    """Canonical integrity seal excluding seal_sha256 (also for deletion evidence)."""
    _require(type(receipt) is dict, "receipt object required")
    try:
        payload = json.dumps({k: v for k, v in receipt.items() if k != SEAL},
                             sort_keys=True, separators=(",", ":"), allow_nan=False)
    except (TypeError, ValueError) as exc:
        raise CostError("JSON-safe evidence required") from exc
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def _verify_seal(receipt):
    _require(_hex(receipt.get(SEAL)) and receipt[SEAL] == seal_receipt(receipt), "changed or missing evidence seal")


def validate_pricing(receipt, *, now_epoch, expected_seal):
    """Validate a fresh, independently pinned provider-derived pricing receipt.

    provenance.pricing_path identifies the ccx43/hel1 entry in the retained raw
    response; response_sha256 binds those bytes. Net/gross equality binds VAT.
    IPv4 is per held host-hour, never an active-host-only allowance.
    """
    _keys(receipt, ("schema", "provider", "source", "captured_epoch", "currency", "server_type", "location",
                    "server_hourly_net_eur", "server_hourly_gross_eur", "ipv4_hourly_net_eur",
                    "ipv4_hourly_gross_eur", "traffic_upper_eur", "other_upper_eur",
                    "bounds_conservative", "provenance", SEAL), "pricing receipt")
    _epoch(now_epoch, "current epoch")
    captured = _epoch(receipt["captured_epoch"], "pricing capture epoch")
    _require(0 <= now_epoch - captured <= MAX_PRICE_AGE_SECONDS, "stale or future pricing capture")
    _require(receipt["schema"] == "paired_pricing_v1" and receipt["provider"] == "hetzner"
             and receipt["source"] == PRICE_SOURCE, "unknown pricing source/schema")
    _require(receipt["currency"] == "EUR" and receipt["server_type"] == "ccx43"
             and receipt["location"] == "hel1", "unknown currency or host catalog choice")
    _require(receipt["bounds_conservative"] is True, "conservative whole-campaign extras bounds required")
    provenance = receipt["provenance"]
    _keys(provenance, ("response_sha256", "pricing_path", "vat_rate"), "pricing provenance")
    _require(_hex(provenance["response_sha256"]), "raw provider response hash required")
    _require(provenance["pricing_path"] == "pricing.server_types[ccx43].prices[hel1]",
             "exact ccx43 HEL1 pricing provenance required")
    vat = _decimal(provenance["vat_rate"], "VAT rate")
    _require(vat <= 1, "unknown VAT rate")
    with localcontext() as ctx:
        ctx.prec = 100
        for item in ("server", "ipv4"):
            net = _decimal(receipt[item + "_hourly_net_eur"], item + " net hourly")
            gross = _decimal(receipt[item + "_hourly_gross_eur"], item + " gross hourly")
            _require(net > 0 and gross == net * (1 + vat), "missing or inconsistent " + item + " gross/VAT price")
    for item in ("traffic_upper_eur", "other_upper_eur"):
        _decimal(receipt[item], item)
    _verify_seal(receipt)
    _require(_hex(expected_seal) and receipt[SEAL] == expected_seal, "untrusted pricing receipt seal")
    return deepcopy(receipt)


def _sealed(value):
    value[SEAL] = seal_receipt(value)
    return value


def admit(receipt, *, expected_seal, first_epoch, deadline_epoch, now_epoch, held_host_count=HOSTS):
    """Reserve Stage1 (two hosts, <=1h) or Stage2 (six hosts, exactly 2h).

    first_epoch is the original admission/first-creation lower bound, never a
    renewable start time. Later provider creations do not move that bound.
    Before creation, all unfilled slots remain fully reserved. Persist this
    ledger durably; never re-admit the same campaign to reset owned-ID history.
    """
    _epoch(first_epoch, "original first epoch")
    _epoch(deadline_epoch, "hard deadline")
    _require(type(held_host_count) is int and held_host_count in PROFILES, "unknown held-host profile")
    lifetime = deadline_epoch - first_epoch
    _require((held_host_count == 2 and 0 < lifetime <= PROFILES[2])
             or (held_host_count == 6 and lifetime == PROFILES[6]), "original bounded profile deadline required")
    _epoch(now_epoch, "current epoch")
    _require(first_epoch <= now_epoch < deadline_epoch - 1200, "original active cutoff exhausted before admission")
    pricing = validate_pricing(receipt, now_epoch=now_epoch, expected_seal=expected_seal)
    ledger = _sealed({"schema": "paired_cost_v1", "first_epoch": first_epoch,
                      "deadline_epoch": deadline_epoch, "budget_eur": "10", "held_hosts": held_host_count,
                      "pricing": pricing, "pricing_seal": expected_seal,
                      "hosts": [], "last_epoch": now_epoch})
    assess(ledger, now_epoch=now_epoch, owned_ids=[])
    return ledger


def _validate_ledger(ledger):
    _keys(ledger, ("schema", "first_epoch", "deadline_epoch", "budget_eur", "held_hosts", "pricing",
                   "pricing_seal", "hosts", "last_epoch", SEAL), "cost ledger")
    _verify_seal(ledger)
    _require(ledger["schema"] == "paired_cost_v1" and ledger["budget_eur"] == "10"
             and type(ledger["held_hosts"]) is int and ledger["held_hosts"] in PROFILES, "changed cost scope")
    first = _epoch(ledger["first_epoch"], "original first epoch")
    _epoch(ledger["deadline_epoch"], "hard deadline")
    lifetime = ledger["deadline_epoch"] - first
    _require((ledger["held_hosts"] == 2 and 0 < lifetime <= PROFILES[2])
             or (ledger["held_hosts"] == 6 and lifetime == PROFILES[6]), "deadline renewal forbidden")
    _require(_epoch(ledger["last_epoch"], "last observation epoch") >= first, "invalid last observation epoch")
    # Cleanup evidence remains recordable even when pricing has become stale.
    captured = _epoch(ledger["pricing"].get("captured_epoch") if type(ledger["pricing"]) is dict else None,
                      "pricing capture epoch")
    _require(captured <= ledger["last_epoch"], "future pricing capture")
    validate_pricing(ledger["pricing"], now_epoch=captured, expected_seal=ledger["pricing_seal"])
    hosts = ledger["hosts"]
    _require(type(hosts) is list and len(hosts) <= ledger["held_hosts"], "unexpected host count")
    seen = set()
    for host in hosts:
        _keys(host, ("host_id", "created_epoch", "last_live_epoch", "deletion"), "host ledger")
        identity = _host_id(host["host_id"])
        _require(identity not in seen, "duplicate owned host ID")
        seen.add(identity)
        created = _epoch(host["created_epoch"], "creation epoch")
        _require(first <= created < ledger["deadline_epoch"] and created <= ledger["last_epoch"], "creation outside original lease")
        _require(created <= _epoch(host["last_live_epoch"], "last live epoch") <= ledger["last_epoch"], "invalid last live epoch")
        if host["deletion"] is not None:
            _validate_deletion(host["deletion"], now_epoch=ledger["last_epoch"])
            _require(host["deletion"]["host_id"] == identity and host["deletion"]["observed_epoch"] >= host["last_live_epoch"],
                     "deletion identity/time mismatch")


def _validate_deletion(receipt, *, now_epoch):
    _keys(receipt, ("host_id", "observed_epoch", "source", "status", SEAL), "verified deletion receipt")
    identity = _host_id(receipt["host_id"])
    observed = _epoch(receipt["observed_epoch"], "deletion observation epoch")
    _require(observed <= now_epoch, "future deletion observation")
    _require(receipt["source"] == "https://api.hetzner.cloud/v1/servers/" + str(identity)
             and receipt["status"] == "not_found", "verified provider absence required, not a delete request/ACK")
    _verify_seal(receipt)


def _hours(first, end):
    return max(1, (end - first + 3599) // 3600)


def _totals(ledger, now_epoch):
    first, deadline = ledger["first_epoch"], ledger["deadline_epoch"]
    elapsed_hours = reserved_hours = 0
    for index in range(ledger["held_hosts"]):
        host = ledger["hosts"][index] if index < len(ledger["hosts"]) else None
        # Every held host is conservatively charged from FIRST creation, including
        # inactive hosts and later creations; only verified absence stops accrual.
        deleted = host["deletion"]["observed_epoch"] if host and host["deletion"] else None
        end = deleted if deleted is not None else now_epoch
        elapsed_hours += _hours(first, end)
        reserved_hours += _hours(first, deleted if deleted is not None else max(now_epoch, deadline))
    pricing = ledger["pricing"]
    with localcontext() as ctx:
        ctx.prec = 100
        server = _decimal(pricing["server_hourly_gross_eur"], "server hourly")
        ipv4 = _decimal(pricing["ipv4_hourly_gross_eur"], "IPv4 hourly")
        extras = _decimal(pricing["traffic_upper_eur"], "traffic bound") + _decimal(pricing["other_upper_eur"], "other bound")
        total = reserved_hours * (server + ipv4) + extras
        return {"currency": "EUR", "budget_eur": "10", "first_epoch": first, "deadline_epoch": deadline,
                "held_hosts": ledger["held_hosts"], "owned_ids": [h["host_id"] for h in ledger["hosts"]],
                "live_ids": [h["host_id"] for h in ledger["hosts"] if h["deletion"] is None],
                "rounded_accrued_host_hours": elapsed_hours, "rounded_reserved_host_hours": reserved_hours,
                "server_reserved_eur": str(reserved_hours * server), "ipv4_reserved_eur": str(reserved_hours * ipv4),
                "extras_reserved_eur": str(extras), "accrued_upper_eur": str(elapsed_hours * (server + ipv4) + extras),
                "total_upper_eur": str(total), "remaining_eur": str(BUDGET_EUR - total),
                "all_deleted": len(ledger["hosts"]) == ledger["held_hosts"] and all(h["deletion"] is not None for h in ledger["hosts"])}


def assess(ledger, *, now_epoch, owned_ids, deadline_epoch=None):
    """Fail closed before each paid action; owned_ids is complete *live* inventory.

    A missing host without a retained verified-absence receipt is not deleted.
    Throws CostError with a cost report at expiry/overflow, so cleanup can log
    continuing accrual. This guard never performs or forbids cleanup itself.
    """
    _validate_ledger(ledger)
    _epoch(now_epoch, "current epoch")
    _require(now_epoch >= ledger["last_epoch"], "clock rollback")
    if deadline_epoch is not None:
        _require(type(deadline_epoch) is int and deadline_epoch == ledger["deadline_epoch"], "deadline renewal forbidden")
    _require(type(owned_ids) in (list, tuple, set), "complete owned live IDs required")
    ids = [_host_id(identity) for identity in owned_ids]
    _require(len(ids) == len(set(ids)), "duplicate inventory ID")
    report = _totals(ledger, now_epoch)
    _require(set(ids) == set(report["live_ids"]), "missing or unexpected owned host ID; verified deletion required")
    if Decimal(report["total_upper_eur"]) > BUDGET_EUR:
        raise CostError("EUR10 all-in budget exceeded", report)
    if now_epoch >= ledger["deadline_epoch"]:
        raise CostError("original hard deadline reached; cleanup only", report)
    validate_pricing(ledger["pricing"], now_epoch=now_epoch, expected_seal=ledger["pricing_seal"])
    return report


def checkpoint(ledger, *, now_epoch, owned_ids, deadline_epoch=None):
    """Persist the last validated continuation epoch without resetting the lease."""
    assess(ledger, now_epoch=now_epoch, owned_ids=owned_ids, deadline_epoch=deadline_epoch)
    result = deepcopy(ledger)
    result["last_epoch"] = now_epoch
    for host in result["hosts"]:
        if host["deletion"] is None:
            host["last_live_epoch"] = now_epoch
    return _sealed(result)


def refresh_pricing(ledger, receipt, *, expected_seal, now_epoch, owned_ids):
    """Pin a fresh capture without lowering earlier conservative reservations.

    Stale old evidence must not prevent a new capture or cleanup. A lower new
    quote cannot retroactively reduce already accrued charges or extras bounds.
    """
    _validate_ledger(ledger)
    _epoch(now_epoch, "current epoch")
    _require(now_epoch >= ledger["last_epoch"], "clock rollback")
    pricing = validate_pricing(receipt, now_epoch=now_epoch, expected_seal=expected_seal)
    for field in ("server_hourly_gross_eur", "ipv4_hourly_gross_eur", "traffic_upper_eur", "other_upper_eur"):
        _require(_decimal(pricing[field], field) >= _decimal(ledger["pricing"][field], field),
                 "refresh cannot lower accrued/reserved cost bounds")
    result = deepcopy(ledger)
    result["pricing"], result["pricing_seal"], result["last_epoch"] = pricing, expected_seal, now_epoch
    _sealed(result)
    return checkpoint(result, now_epoch=now_epoch, owned_ids=owned_ids)


def record_creation(ledger, *, host_id, created_epoch, now_epoch):
    """Retain a newly owned ID exactly once; never replace a deleted host."""
    _validate_ledger(ledger)
    _host_id(host_id)
    _epoch(created_epoch, "creation epoch")
    _epoch(now_epoch, "current epoch")
    _require(ledger["last_epoch"] <= now_epoch < ledger["deadline_epoch"]
             and ledger["first_epoch"] <= created_epoch <= now_epoch, "creation outside original lease or clock rollback")
    _require(len(ledger["hosts"]) < ledger["held_hosts"] and host_id not in [h["host_id"] for h in ledger["hosts"]], "unexpected/duplicate/replacement host")
    result = deepcopy(ledger)
    result["hosts"].append({"host_id": host_id, "created_epoch": created_epoch,
                            "last_live_epoch": now_epoch, "deletion": None})
    result["last_epoch"] = now_epoch
    _sealed(result)
    assess(result, now_epoch=now_epoch, owned_ids=[h["host_id"] for h in result["hosts"] if h["deletion"] is None])
    return result


def record_deletion(ledger, *, verification, now_epoch):
    """Record exact provider-observed absence, even after expiry or stale pricing.

    A delete command/ACK or local inventory omission is insufficient. The capture
    adapter must only issue this evidence for an authenticated provider 404, not
    an authorization error, timeout, cached response or failed inventory call.
    """
    _validate_ledger(ledger)
    _epoch(now_epoch, "current epoch")
    _require(now_epoch >= ledger["last_epoch"], "clock rollback")
    _validate_deletion(verification, now_epoch=now_epoch)
    result = deepcopy(ledger)
    matches = [h for h in result["hosts"] if h["host_id"] == verification["host_id"]]
    _require(len(matches) == 1 and matches[0]["deletion"] is None, "unknown or already deleted owned ID")
    _require(verification["observed_epoch"] >= matches[0]["last_live_epoch"], "backdated deletion evidence")
    matches[0]["deletion"] = deepcopy(verification)
    result["last_epoch"] = now_epoch
    return _sealed(result)
