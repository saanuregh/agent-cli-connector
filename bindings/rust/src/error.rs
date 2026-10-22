use std::fmt;

use crate::protocol::{ErrorInfo, ErrorKind};

/// A failure from the library, the connection, or this binding. Match on
/// [`Error::kind`]; [`Error::info`] carries the library's details.
#[derive(Debug, Clone, PartialEq)]
pub struct Error {
    pub info: Box<ErrorInfo>,
    /// The JSON-RPC error code, or 0 when the error did not come from a
    /// JSON-RPC response.
    pub code: i64,
}

impl Error {
    pub fn new(kind: ErrorKind, message: impl Into<String>) -> Self {
        Self {
            info: Box::new(ErrorInfo {
                kind,
                message: message.into(),
                ..Default::default()
            }),
            code: 0,
        }
    }

    pub(crate) fn closed(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Closed, message)
    }

    pub(crate) fn internal(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Internal, message)
    }

    pub(crate) fn protocol(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Protocol, message)
    }

    pub fn kind(&self) -> ErrorKind {
        self.info.kind
    }

    pub fn message(&self) -> &str {
        &self.info.message
    }
}

impl From<ErrorInfo> for Error {
    fn from(info: ErrorInfo) -> Self {
        Self {
            info: Box::new(info),
            code: 0,
        }
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let kind = serde_json::to_value(self.info.kind).ok();
        let kind = kind.as_ref().and_then(|k| k.as_str()).unwrap_or("error");
        if self.info.message.is_empty() {
            write!(f, "{kind}")
        } else {
            write!(f, "{kind}: {}", self.info.message)
        }
    }
}

impl std::error::Error for Error {}

pub type Result<T, E = Error> = std::result::Result<T, E>;
