//! Span-only, in-process OTEL_V1 writer. This is not an upstream Collector or
//! an OTLP network receiver. Persisted rows use the repository's full descriptors.
pub mod collector;
pub mod native;
pub mod row;
pub mod storage;
pub mod tuple;
mod wire;
pub mod writer;

use std::fmt;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrorKind {
    Config,
    Admission,
    Ownership,
    Corruption,
    Budget,
    Canceled,
    Transient,
    Operation,
}

#[derive(Debug, Clone)]
pub struct Error {
    pub kind: ErrorKind,
    pub message: String,
    pub fdb_code: Option<i32>,
}

impl Error {
    pub fn new(kind: ErrorKind, message: impl Into<String>) -> Self {
        Self {
            kind,
            message: message.into(),
            fdb_code: None,
        }
    }

    pub fn permanent(&self) -> bool {
        matches!(
            self.kind,
            ErrorKind::Config
                | ErrorKind::Admission
                | ErrorKind::Ownership
                | ErrorKind::Corruption
                | ErrorKind::Operation
        )
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{:?}: {}", self.kind, self.message)
    }
}

impl std::error::Error for Error {}
pub type Result<T> = std::result::Result<T, Error>;
