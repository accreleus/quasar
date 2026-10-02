//! #285: tests inject env-style values through a lookup closure instead of mutating
//! `std::env` — the process environment is shared by every test thread, so a
//! `set_var`/`remove_var` in one test races an unrelated test reading the same var in
//! parallel. `lookup` builds the closure production passes `&|k| std::env::var(k).ok()`
//! for in its place, from literal pairs.

/// `lookup(&[])` is the empty case: every key resolves to `None`, matching an unset env.
pub(crate) fn lookup(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
    let owned: Vec<(String, String)> = pairs
        .iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect();
    move |key: &str| owned.iter().find(|(k, _)| k == key).map(|(_, v)| v.clone())
}
