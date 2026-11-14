//! Test-only live native proof, run only by the Go integration test against its own
//! cluster. Built with `--test` so `native.rs`'s cfg(test) C-boundary hooks exist.
pub use fdb_otel_rust::{Error, ErrorKind, Result};
mod writer {
    pub use fdb_otel_rust::writer::*;
}
#[path = "../src/native.rs"]
mod native;

use fdb_otel_rust::storage::RecordCompression;
use std::{fs, path::PathBuf};
use writer::{Context, Layout, Writer};

const SCENARIO: &str = "applied_commit_unknown_result";
const BOUNDARY: &str = "applied commit reported as commit_unknown_result at the client boundary; fault-injected by a test-only C-boundary interceptor, not server- or transport-generated";

fn path(name: &str) -> PathBuf {
    PathBuf::from(
        std::env::var_os(name)
            .unwrap_or_else(|| panic!("{name} must be supplied by the Go native proof")),
    )
}

/// One write call of one input wrapper: its batch really commits, the interceptor then
/// reports 1021 once, real OnError(1021) runs on the same handle and the Writer replays.
#[test]
fn applied_commit_unknown_result() {
    let layout: Layout =
        serde_json::from_slice(&fs::read(path("OTEL_NATIVE_PROOF_LAYOUT")).unwrap()).unwrap();
    let wire = fs::read(path("OTEL_NATIVE_PROOF_INPUT")).unwrap();
    native::fault::expose_commit_unknown_once();
    let db = native::NativeDatabase::open_intercepted(
        &path("OTEL_NATIVE_PROOF_CLIENT"),
        &path("OTEL_NATIVE_PROOF_CLUSTER"),
    )
    .unwrap();
    let writer = Writer::new(db, &layout, 64, RecordCompression::None).unwrap();
    let (stats, result) = writer.write(&Context::default(), &[wire]);
    let c = native::fault::counters();
    let handle = c.created.first().copied();
    let same_handle = handle.is_some()
        && c.commits
            .iter()
            .chain(c.on_errors.iter().map(|(tx, _)| tx))
            .chain(c.options.iter().map(|(tx, _, _)| tx))
            .all(|tx| Some(*tx) == handle);
    let options: Vec<(i32, i64)> = c
        .options
        .iter()
        .map(|(_, option, value)| {
            let value = <[u8; 8]>::try_from(value.as_slice()).expect("8-byte option value");
            (*option, i64::from_le_bytes(value))
        })
        .collect();
    let report = serde_json::json!({
        "scenario": SCENARIO,
        "boundary": BOUNDARY,
        "error": result.as_ref().err().map(ToString::to_string),
        "transactions_created": c.created.len(),
        "same_transaction_handle": same_handle,
        "option_calls": options,
        "commit_calls": c.commits.len(),
        "real_commit_successes": c.real_commit_successes,
        "exposed_commit_unknown_result": c.exposed_1021,
        "on_error_codes": c.on_errors.iter().map(|(_, code)| *code).collect::<Vec<_>>(),
        "committed_spans": stats.committed_spans,
        "committed_batches": stats.committed_batches,
        "retries": stats.retries,
        "logical_otlp_bytes": stats.logical_otlp_bytes,
    });
    fs::write(
        path("OTEL_NATIVE_PROOF_REPORT"),
        serde_json::to_vec(&report).unwrap(),
    )
    .unwrap();
    result.unwrap();
    assert_eq!(c.created.len(), 1);
    assert!(same_handle, "commit, OnError and options used one handle");
    assert_eq!(options, [(500, 10_000), (501, -1)]);
    assert_eq!(c.commits.len(), 2);
    assert_eq!(c.real_commit_successes, 2);
    assert_eq!(c.exposed_1021, 1);
    assert_eq!(
        c.on_errors
            .iter()
            .map(|(_, code)| *code)
            .collect::<Vec<_>>(),
        [1021]
    );
    assert_eq!((stats.committed_batches, stats.retries), (1, 1));
    assert!(stats.committed_spans > 0);
}
