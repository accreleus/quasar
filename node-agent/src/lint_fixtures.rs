//! One planted violation per lint added in `Cargo.toml` `[workspace.lints]` and `clippy.toml`.
//! If a lint stops firing here, clippy `--all-targets -D warnings` fails with
//! `unfulfilled_lint_expectations`: a `clippy.toml` entry was removed, or its path stopped
//! resolving. The two `[workspace.lints]` levels are not covered by this: `#[expect]`
//! overrides them.
#![allow(dead_code)]

use std::{future::ready, sync::Mutex};
use tokio::sync::watch;

#[expect(clippy::await_holding_lock)]
async fn lock_held_across_await(lock: &Mutex<()>) {
    let _guard = lock.lock().unwrap();
    ready(()).await;
}

#[expect(clippy::let_underscore_future)]
fn future_dropped_unpolled() {
    let _ = ready(());
}

#[expect(clippy::await_holding_invalid_type)]
async fn entered_held_across_await(span: &tracing::Span) {
    let _entered = span.enter();
    ready(()).await;
}

#[expect(clippy::await_holding_invalid_type)]
async fn entered_span_held_across_await(span: tracing::Span) {
    let _entered = span.entered();
    ready(()).await;
}

#[expect(clippy::disallowed_methods)]
fn watch_send(tx: &watch::Sender<bool>) {
    let _ = tx.send(true);
}
