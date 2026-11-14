#!/usr/bin/env python3
"""Private append-only row authorization for the named paired campaign family.

Issue before releasing a population; an unsuccessful/uncertain issue is never
reissued or reclaimed. The ledger belongs in private shared scratch, not a clone.
An inode xattr witnesses the committed head/length, detecting even whole-record
suffix truncation. An interrupted append/witness update fails closed; there is no
automatic repair. This is integrity against accidental damage, not an authenticated
log against an owner able to rewrite both the file and its xattr.
"""
import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import json
import os
import re
import stat
import sys


BUDGET_NAME = "otel_paired_go_rust_v1"
CEILING_ROWS = 15000000
PRIOR_ROWS = 0
SCHEMA = "otel_paired_rows_jsonl_v1"
ARMS = ("go_pure", "go_native", "rust_native")
STAGES = ("local", "pilot", "pairs1", "pairs2", "pairs3")
PHASES = ("local", "prerequisite", "measured")
STATUSES = ("passed", "failed", "aborted", "unknown")
HEAD_XATTR = "user.otel_paired_rows_v1_head"
MAX_BYTES = 64 * 1024 * 1024
MAX_LINE_BYTES = 16384
ZERO_HASH = "0" * 64
BASE_KEYS = {"schema", "budget_name", "sequence", "previous_sha256", "record_type", "record_sha256"}
TOKEN_KEYS = ("campaign", "stage", "arm", "repeat", "phase")


class RowBudgetError(ValueError):
    """No authorization was returned; do not release the requested population."""


def _require(condition, message):
    if not condition:
        raise RowBudgetError(message)


def _canonical(value):
    try:
        return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True,
                          allow_nan=False).encode("ascii")
    except (TypeError, ValueError) as exc:
        raise RowBudgetError("canonical JSON required") from exc


def _digest(value):
    return hashlib.sha256(_canonical(value)).hexdigest()


def _object(pairs):
    result = {}
    for key, value in pairs:
        _require(key not in result, "duplicate JSON key")
        result[key] = value
    return result


def _nonfinite(value):
    raise RowBudgetError("nonfinite JSON number")


def _parse(data):
    try:
        return json.loads(data.decode("utf-8"), object_pairs_hook=_object, parse_constant=_nonfinite)
    except (ValueError, UnicodeError, RecursionError) as exc:
        raise RowBudgetError("invalid or corrupt JSON evidence") from exc


def _integer(value, minimum, maximum, what):
    _require(type(value) is int and minimum <= value <= maximum, "invalid " + what)
    return value


def _token(campaign, stage, arm, repeat, phase):
    _require(type(campaign) is str and re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}", campaign)
             is not None, "invalid campaign identifier")
    _require(type(stage) is str and stage in STAGES, "unknown stage")
    _require(type(arm) is str and arm in ARMS, "unknown arm")
    _integer(repeat, 0, 1000000, "repeat")
    _require(type(phase) is str and phase in PHASES, "unknown phase")
    _require((stage == "local") == (phase == "local"), "local stage/phase must be paired")
    return dict(zip(TOKEN_KEYS, (campaign, stage, arm, repeat, phase)))


def _check_file(fd):
    metadata = os.fstat(fd)
    _require(stat.S_ISREG(metadata.st_mode) and metadata.st_uid == os.geteuid()
             and stat.S_IMODE(metadata.st_mode) == 0o600 and metadata.st_nlink == 1,
             "owner-only 0600 singly-linked regular ledger required")
    _require(metadata.st_size <= MAX_BYTES, "ledger size bound exceeded")
    return metadata


@contextmanager
def _locked(path, *, create=False, exclusive=True):
    """Walk using dirfds and O_NOFOLLOW; never follow an ancestor symlink."""
    try:
        raw = os.fspath(path)
    except TypeError as exc:
        raise RowBudgetError("absolute ledger path required") from exc
    _require(type(raw) is str and raw.startswith("/") and "\x00" not in raw, "absolute ledger path required")
    parts = raw.split("/")[1:]
    _require(parts and all(part not in ("", ".", "..") for part in parts), "canonical absolute ledger path required")
    parent_fd = fd = None
    try:
        parent_fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        for part in parts[:-1]:
            next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC,
                              dir_fd=parent_fd)
            os.close(parent_fd)
            parent_fd = next_fd
        parent = os.fstat(parent_fd)
        _require(parent.st_uid == os.geteuid() and stat.S_IMODE(parent.st_mode) == 0o700,
                 "ledger parent must be existing private owner-only 0700 directory")
        flags = os.O_RDWR | os.O_APPEND | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK
        if create:
            flags |= os.O_CREAT | os.O_EXCL
        fd = os.open(parts[-1], flags, 0o600, dir_fd=parent_fd)
        _check_file(fd)
        fcntl.flock(fd, fcntl.LOCK_EX if exclusive else fcntl.LOCK_SH)
        before = _check_file(fd)
        current = os.stat(parts[-1], dir_fd=parent_fd, follow_symlinks=False)
        _require((before.st_dev, before.st_ino) == (current.st_dev, current.st_ino), "ledger path replaced")
        yield fd, parent_fd
        current = os.stat(parts[-1], dir_fd=parent_fd, follow_symlinks=False)
        after = _check_file(fd)
        _require((after.st_dev, after.st_ino) == (current.st_dev, current.st_ino), "ledger path replaced")
    except OSError as exc:
        raise RowBudgetError("ledger filesystem operation failed: " + str(exc)) from exc
    finally:
        if fd is not None:
            os.close(fd)
        if parent_fd is not None:
            os.close(parent_fd)


def _record(kind, sequence, previous, **fields):
    value = {"schema": SCHEMA, "budget_name": BUDGET_NAME, "sequence": sequence,
             "previous_sha256": previous, "record_type": kind, **fields}
    value["record_sha256"] = _digest(value)
    return value


def _append(fd, record):
    payload = _canonical(record) + b"\n"
    _require(len(payload) <= MAX_LINE_BYTES, "record too large")
    _require(os.fstat(fd).st_size + len(payload) <= MAX_BYTES, "ledger size bound exceeded")
    _require(os.write(fd, payload) == len(payload), "incomplete append; ledger quarantined")
    # Both the issue bytes and independent committed-head witness precede release.
    os.fsync(fd)
    witness = {"bytes": os.fstat(fd).st_size, "head_sha256": record["record_sha256"]}
    os.setxattr(fd, HEAD_XATTR, _canonical(witness))
    os.fsync(fd)


def _read(fd):
    metadata = _check_file(fd)
    _require(metadata.st_size > 0, "empty/uninitialized or truncated ledger")
    try:
        witness = _parse(os.getxattr(fd, HEAD_XATTR))
    except OSError as exc:
        raise RowBudgetError("missing committed-head witness; ledger quarantined") from exc
    _require(type(witness) is dict and set(witness) == {"bytes", "head_sha256"}, "invalid committed-head witness")
    _require(type(witness["bytes"]) is int and witness["bytes"] == metadata.st_size, "ledger truncated or uncommitted append")
    os.lseek(fd, 0, os.SEEK_SET)
    records, issues, outcomes = [], {}, {}
    total, previous = 0, ZERO_HASH
    # dup shares the file position but leaves the locked descriptor open.
    with os.fdopen(os.dup(fd), "rb") as stream:
        while True:
            line = stream.readline(MAX_LINE_BYTES + 1)
            if not line:
                break
            _require(len(line) <= MAX_LINE_BYTES and line.endswith(b"\n"), "truncated or oversized JSONL record")
            record = _parse(line[:-1])
            _require(type(record) is dict and line == _canonical(record) + b"\n", "noncanonical or corrupt JSONL record")
            kind = record.get("record_type")
            extra = {"init": {"ceiling_rows", "prior_rows", "issued_rows_total"},
                     "issue": set(TOKEN_KEYS) | {"issue_id", "rows", "issued_rows_total"},
                     "outcome": {"issue_id", "status", "issued_rows_total"}}
            _require(type(kind) is str and kind in extra and set(record) == BASE_KEYS | extra[kind], "unknown record type/fields")
            _require(record["schema"] == SCHEMA and record["budget_name"] == BUDGET_NAME, "unknown row budget/schema")
            _require(type(record["sequence"]) is int and record["sequence"] == len(records), "duplicate/missing record sequence")
            _require(record["previous_sha256"] == previous, "broken record hash chain")
            expected = _digest({k: v for k, v in record.items() if k != "record_sha256"})
            _require(record["record_sha256"] == expected, "changed record hash")
            _require((len(records) == 0) == (kind == "init"), "exactly one initial genesis required")
            if kind == "init":
                _require(type(record["ceiling_rows"]) is int and record["ceiling_rows"] == CEILING_ROWS
                         and type(record["prior_rows"]) is int and record["prior_rows"] == PRIOR_ROWS,
                         "fixed named budget ceiling/prior required")
            elif kind == "issue":
                token = _token(*(record[key] for key in TOKEN_KEYS))
                identity = _digest(token)
                _require(record["issue_id"] == identity and identity not in issues, "duplicate or changed population token")
                total += _integer(record["rows"], 1, CEILING_ROWS, "issued rows")
                _require(total <= CEILING_ROWS, "named row ceiling exceeded")
                issues[identity] = record
            else:
                identity = record["issue_id"]
                _require(type(identity) is str and identity in issues and identity not in outcomes,
                         "unknown or duplicate issue outcome")
                _require(type(record["status"]) is str and record["status"] in STATUSES, "unknown outcome status")
                outcomes[identity] = record
            _require(type(record["issued_rows_total"]) is int and record["issued_rows_total"] == total,
                     "changed issued population total; rows are never reclaimed")
            records.append(record)
            previous = expected
    _require(records and previous == witness["head_sha256"], "committed head mismatch or suffix truncation")
    return records, issues, outcomes, total


def _summary(state):
    records, issues, outcomes, total = state
    return {"budget_name": BUDGET_NAME, "ceiling_rows": CEILING_ROWS, "prior_rows": PRIOR_ROWS,
            "issued_rows": total, "remaining_rows": CEILING_ROWS - total, "record_count": len(records),
            "head_sha256": records[-1]["record_sha256"], "issues": list(issues.values()),
            "outcomes": list(outcomes.values()), "pending_issue_ids": [key for key in issues if key not in outcomes]}


def init(path):
    """Exclusively create a new 0600 ledger in an existing 0700 directory."""
    with _locked(path, create=True) as (fd, directory_fd):
        genesis = _record("init", 0, ZERO_HASH, ceiling_rows=CEILING_ROWS, prior_rows=PRIOR_ROWS, issued_rows_total=0)
        _append(fd, genesis)
        os.fsync(directory_fd)
        return _summary(_read(fd))


def inspect(path):
    """Validate the complete persisted history and return authoritative totals."""
    with _locked(path, exclusive=False) as (fd, _):
        return _summary(_read(fd))


def issue(path, *, campaign, stage, arm, repeat, phase, rows):
    """Durably charge aggregate population rows before any producer release.

    The tuple campaign/stage/arm/repeat/phase is the unique population token.
    Multiple clients in the same population must be charged together (N*count).
    A returned receipt authorizes one release only; no retry/reclaim operation
    exists. A lost return after fsync remains charged and cannot be reissued.
    """
    token = _token(campaign, stage, arm, repeat, phase)
    _integer(rows, 1, CEILING_ROWS, "issued rows")
    identity = _digest(token)
    with _locked(path) as (fd, _):
        records, issues, _, total = _read(fd)
        _require(identity not in issues, "population already issued; rerelease forbidden")
        _require(total + rows <= CEILING_ROWS, "named row ceiling exceeded")
        receipt = _record("issue", len(records), records[-1]["record_sha256"],
                          **token, issue_id=identity, rows=rows, issued_rows_total=total + rows)
        _append(fd, receipt)
        return receipt


def outcome(path, *, issue_id, status):
    """Append one terminal observation; failed/aborted/unknown rows remain charged."""
    _require(type(issue_id) is str and re.fullmatch(r"[a-f0-9]{64}", issue_id) is not None, "invalid issue ID")
    _require(type(status) is str and status in STATUSES, "unknown outcome status")
    with _locked(path) as (fd, _):
        records, issues, outcomes, total = _read(fd)
        _require(issue_id in issues and issue_id not in outcomes, "unknown or already finalized issue")
        receipt = _record("outcome", len(records), records[-1]["record_sha256"],
                          issue_id=issue_id, status=status, issued_rows_total=total)
        _append(fd, receipt)
        return receipt


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for name in ("init", "show", "issue", "outcome"):
        command = commands.add_parser(name)
        command.add_argument("path", help="absolute ledger path in existing private 0700 scratch directory")
        if name == "issue":
            command.add_argument("--campaign", required=True)
            command.add_argument("--stage", required=True, choices=STAGES)
            command.add_argument("--arm", required=True, choices=ARMS)
            command.add_argument("--repeat", required=True, type=int)
            command.add_argument("--phase", required=True, choices=PHASES)
            command.add_argument("--rows", required=True, type=int)
        elif name == "outcome":
            command.add_argument("--issue-id", required=True)
            command.add_argument("--status", required=True, choices=STATUSES)
    args = vars(parser.parse_args(argv))
    command, path = args.pop("command"), args.pop("path")
    try:
        result = {"init": init, "show": inspect, "issue": issue, "outcome": outcome}[command](path, **args)
    except RowBudgetError as exc:
        print("row authorization refused: " + str(exc), file=sys.stderr)
        return 2
    print(_canonical(result).decode("ascii"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
