//! Go standard-library behaviour the Go updater's trust gates depend on.
//!
//! Outcome parity (a quirk that decides admit or refuse, signed or unsigned, direct or
//! proxied) is pinned by the golden vectors or unit tests, and must survive the Go
//! updater's deletion (RH06-15, #367):
//! - `json`: field names folded case-insensitively (with the Kelvin sign and long s),
//!   repeated keys merged into existing values and slice elements, `null` handling,
//!   the 10000-level depth cap, `]`/`}` allowed after the signature document, unknown
//!   fields refused in it and ignored in the manifest;
//! - `base64`: padding required, CR/LF ignored, non-zero trailing bits accepted;
//! - `url`: which base URLs and `Location`s parse, the scheme and host they yield,
//!   reference resolution and the request target;
//! - `proxy`: whether `HTTPS_PROXY`/`NO_PROXY` would route a request through a proxy;
//! - `text`: `strings.TrimSpace` and `strings.ToLower` where they select a keyword.
//!
//! Text-only parity (the wording of refusal and error messages: `strconv.Quote` in
//! `text::quote`, the `parse "...": ...` and `invalid ...` strings) may be relaxed once
//! the Go updater is gone; outcome parity may not.

pub(crate) mod base64;
pub(crate) mod json;
pub(crate) mod proxy;
pub(crate) mod text;
pub(crate) mod url;
