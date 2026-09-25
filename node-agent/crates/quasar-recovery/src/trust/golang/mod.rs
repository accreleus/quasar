//! Go standard-library behaviour the Go updater's trust gates depend on, reproduced
//! exactly where it decides an outcome: `strings`/`strconv` text rules, `encoding/base64`
//! `StdEncoding`, `encoding/json` v1 decoding and `net/url` parsing. The golden vectors
//! pin each quirk; nothing here is general-purpose.

pub(crate) mod base64;
pub(crate) mod json;
pub(crate) mod text;
pub(crate) mod url;
