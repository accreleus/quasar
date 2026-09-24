//! RH05 next-session policy catalog: group membership, catalog-typed value
//! validation, resolution against the deployment baseline, verified
//! composition onto [`RuntimeSettings`], and RFC 8785 canonical JSON for every
//! digest. Twin of `control-plane/internal/hostcfg/policy_catalog.go`; the
//! bounds must match `hostcfg.Catalog()`.

use serde_json::{Map, Value};
use sha2::{Digest, Sha256};

use crate::session::settings::RuntimeSettings;

/// Every next-session group, sorted. Outside `hardware`, a group is its key.
pub const NEXT_SESSION_GROUPS: &[&str] = &[
    "abr_cliff_guard_frac",
    "abr_deadband",
    "abr_down_dwell_ms",
    "abr_enabled",
    "abr_ewma_alpha",
    "abr_floor_kbps",
    "abr_floor_ratio",
    "abr_ladder",
    "abr_ladder_engage_dwell",
    "abr_ladder_floor_follows_rung",
    "abr_ladder_fps",
    "abr_ladder_max_bias",
    "abr_ladder_order",
    "abr_ladder_recover_dwell",
    "abr_ladder_res_engage_dwell",
    "abr_ladder_res_engage_frac",
    "abr_ladder_res_exponent",
    "abr_ladder_res_min_height",
    "abr_ladder_res_min_step_s",
    "abr_ladder_res_recover_dwell",
    "abr_ladder_res_recover_frac",
    "abr_ladder_resolution",
    "abr_max_down_step",
    "abr_max_up_step",
    "abr_min_interval_ms",
    "abr_mode",
    "app_boot_timeout_secs",
    "gop",
    "home_root",
    "idle_timeout_secs",
    "latency_probe",
    "nvidia_lib32_path",
    "queue_buffers",
    "slices",
    "target_usage",
    "zerocopy",
];

/// Minimum engage→recover band of the resolution rung (hostcfg `MinHysteresisBand`).
const MIN_HYSTERESIS_BAND: f64 = 0.05;

pub fn group_for_key(key: &str) -> &str {
    match key {
        "encoder" | "render_node" | "cuda_device" => "hardware",
        other => other,
    }
}

pub fn is_next_session_group(group: &str) -> bool {
    NEXT_SESSION_GROUPS.binary_search(&group).is_ok()
}

pub const HARDWARE_KEYS: &[&str] = &["cuda_device", "encoder", "render_node"];

pub fn is_restart_group(group: &str) -> bool {
    group == "hardware"
}

enum Kind {
    Bool,
    Int(f64, Option<f64>),
    Float(f64, Option<f64>),
    Enum(&'static [&'static str]),
    String,
    AbsPathOrEmpty,
}

fn kind_of(key: &str) -> Option<Kind> {
    use Kind::*;
    Some(match key {
        "abr_enabled"
        | "abr_ladder"
        | "abr_ladder_resolution"
        | "abr_ladder_fps"
        | "abr_ladder_floor_follows_rung"
        | "zerocopy"
        | "latency_probe" => Bool,
        "abr_floor_kbps" | "abr_min_interval_ms" | "gop" | "slices" | "queue_buffers" => {
            Int(1.0, None)
        }
        "abr_down_dwell_ms" | "idle_timeout_secs" | "app_boot_timeout_secs" => Int(0.0, None),
        "abr_ladder_max_bias" => Int(0.0, Some(255.0)),
        "abr_ladder_engage_dwell" | "abr_ladder_recover_dwell" => Int(1.0, Some(255.0)),
        "abr_ladder_res_engage_dwell" | "abr_ladder_res_recover_dwell" => Int(1.0, Some(60.0)),
        "abr_ladder_res_min_step_s" => Int(5.0, Some(120.0)),
        "abr_ladder_res_min_height" => Int(360.0, Some(2160.0)),
        "target_usage" => Int(1.0, Some(7.0)),
        "cuda_device" => Int(0.0, None),
        "abr_floor_ratio" => Float(0.0, Some(1.0)),
        "abr_ewma_alpha" => Float(0.000001, Some(1.0)),
        "abr_deadband" | "abr_max_down_step" | "abr_cliff_guard_frac" => {
            Float(0.000001, Some(0.999999))
        }
        "abr_max_up_step" => Float(0.000001, None),
        "abr_ladder_res_exponent" => Float(0.5, Some(1.0)),
        "abr_ladder_res_engage_frac" => Float(0.2, Some(0.95)),
        "abr_ladder_res_recover_frac" => Float(0.3, Some(1.0)),
        "abr_mode" => Enum(&["off", "protective", "smooth"]),
        "encoder" => Enum(&["openh264", "va", "nvenc", "vulkan"]),
        "abr_ladder_order" => Enum(&["res_first", "fps_first", "hybrid"]),
        "render_node" => String,
        "home_root" | "nvidia_lib32_path" => AbsPathOrEmpty,
        _ => return None,
    })
}

/// Validate one explicit catalog-typed value.
pub fn validate_value(key: &str, value: &Value) -> Result<(), &'static str> {
    let in_range = |n: f64, min: f64, max: Option<f64>| n >= min && max.is_none_or(|m| n <= m);
    let ok = match kind_of(key).ok_or("unsupported_group")? {
        Kind::Bool => value.is_boolean(),
        Kind::Int(min, max) => value
            .as_f64()
            .is_some_and(|n| n.fract() == 0.0 && in_range(n, min, max)),
        Kind::Float(min, max) => value.as_f64().is_some_and(|n| in_range(n, min, max)),
        Kind::Enum(allowed) => value.as_str().is_some_and(|s| allowed.contains(&s)),
        Kind::String => value.is_string(),
        Kind::AbsPathOrEmpty => value
            .as_str()
            .is_some_and(|s| s.is_empty() || s.starts_with('/')),
    };
    ok.then_some(()).ok_or("invalid_value")
}

/// Resolve a single-key next-session group's choice (`{"source":..}`) against
/// the pre-policy deployment baseline, as the offer's `resolved_settings`.
pub fn resolve_group(
    group: &str,
    choices: &Value,
    baseline: &RuntimeSettings,
) -> Result<Map<String, Value>, &'static str> {
    if !is_next_session_group(group) && !is_restart_group(group) {
        return Err("unsupported_group");
    }
    let choices = choices.as_object().ok_or("missing_setting")?;
    let keys: &[&str] = if is_restart_group(group) {
        HARDWARE_KEYS
    } else {
        std::slice::from_ref(&group)
    };
    if choices.len() != keys.len() || keys.iter().any(|key| !choices.contains_key(*key)) {
        return Err("missing_setting");
    }
    let mut resolved = Map::new();
    let mut deployment = baseline.deployment_map();
    for key in keys {
        let choice = &choices[*key];
        let value = match choice.get("source").and_then(Value::as_str) {
            Some("explicit") => {
                let value = choice.get("value").ok_or("invalid_value")?;
                validate_value(key, value)?;
                value.clone()
            }
            Some("deployment") if choice.get("value").is_none() => {
                deployment.remove(*key).ok_or("missing_setting")?
            }
            _ => return Err("unsupported_source"),
        };
        resolved.insert((*key).to_string(), value);
    }
    Ok(resolved)
}

/// The `deployment_baseline` fact ID for a group, or `None` when no choice in
/// it uses the deployment source.
pub fn deployment_fact(choices: &Value, resolved: &Map<String, Value>) -> Option<String> {
    let deployed: Map<String, Value> = choices
        .as_object()?
        .iter()
        .filter(|(_, choice)| choice.get("source").and_then(Value::as_str) == Some("deployment"))
        .filter_map(|(key, _)| resolved.get(key).map(|v| (key.clone(), v.clone())))
        .collect();
    (!deployed.is_empty()).then(|| digest(&Value::Object(deployed)))
}

/// Compose typed group snapshots onto `settings` in group order, verifying each
/// group reads back its own values right after it is applied, then check the
/// cross-key rules on the final state. Later groups win where two keys write
/// one field (`abr_mode` after the deprecated `abr_enabled`, as legacy maps do).
pub fn compose_groups<'a>(
    settings: &mut RuntimeSettings,
    groups: impl IntoIterator<Item = (&'a str, &'a Value)>,
) -> Result<(), &'static str> {
    let mut composed = settings.clone();
    let mut ordered: Vec<_> = groups.into_iter().collect();
    ordered.sort_by(|a, b| a.0.cmp(b.0));
    for (group, resolved) in ordered {
        let values = resolved.as_object().ok_or("typed_snapshot_invalid")?;
        if values.is_empty() || values.keys().any(|key| group_for_key(key) != group) {
            return Err("typed_snapshot_unknown_key");
        }
        composed.apply_json(resolved);
        let readback = composed.deployment_map();
        if values
            .iter()
            .any(|(key, value)| !readback.get(key).is_some_and(|got| json_equal(got, value)))
        {
            return Err("typed_snapshot_invalid_value");
        }
    }
    check_cross_keys(&composed)?;
    *settings = composed;
    Ok(())
}

/// Cross-key rules over a whole effective configuration.
pub fn check_cross_keys(settings: &RuntimeSettings) -> Result<(), &'static str> {
    let map = settings.deployment_map();
    let frac = |key: &str| map.get(key).and_then(Value::as_f64);
    if let (Some(engage), Some(recover)) = (
        frac("abr_ladder_res_engage_frac"),
        frac("abr_ladder_res_recover_frac"),
    ) {
        // Same float comparison as hostcfg.ValidateResolved.
        if recover - engage < MIN_HYSTERESIS_BAND {
            return Err("cross_key_invalid");
        }
    }
    Ok(())
}

/// Effects a candidate must have on this host before durable acceptance:
/// a storage root inside the deployment mount (no implicit mount creation), and
/// an accessible 32-bit NVIDIA library directory when one is named.
pub fn check_host_effects(
    resolved: &Map<String, Value>,
    baseline: &RuntimeSettings,
    is_dir: &dyn Fn(&str) -> bool,
) -> Result<(), &'static str> {
    if let Some(root) = resolved.get("home_root").and_then(Value::as_str) {
        if !root.is_empty() && !path_within(root, &baseline.home_root) {
            return Err("home_root_outside_mount");
        }
    }
    if let Some(path) = resolved.get("nvidia_lib32_path").and_then(Value::as_str) {
        if !path.is_empty() && !is_dir(path) {
            return Err("path_inaccessible");
        }
    }
    Ok(())
}

fn path_within(candidate: &str, mount: &str) -> bool {
    let clean = |p: &str| {
        let mut parts = Vec::new();
        for part in p.split('/') {
            match part {
                "" | "." => {}
                ".." => {
                    parts.pop();
                }
                other => parts.push(other),
            }
        }
        format!("/{}", parts.join("/"))
    };
    let mount = mount.trim();
    if mount.is_empty() || !mount.starts_with('/') {
        return false;
    }
    let (candidate, mount) = (clean(candidate), clean(mount));
    candidate == mount || mount == "/" || candidate.starts_with(&format!("{mount}/"))
}

/// JSON equality that compares numbers by value, so `1` and `1.0` agree.
pub fn json_equal(a: &Value, b: &Value) -> bool {
    match (a, b) {
        (Value::Number(x), Value::Number(y)) => x.as_f64() == y.as_f64(),
        _ => a == b,
    }
}

/// Lowercase SHA-256 of the RFC 8785 serialization.
pub fn digest(value: &Value) -> String {
    Sha256::digest(canonical_json(value).as_bytes())
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

/// Digest of a group's active snapshot, `{group,resolved_settings}`.
pub fn snapshot_digest(group: &str, resolved: &Value) -> String {
    digest(&serde_json::json!({"group": group, "resolved_settings": resolved}))
}

/// RFC 8785 canonical JSON. serde_json's maps are sorted and its string
/// escaping already matches; numbers need the ECMAScript form.
pub fn canonical_json(value: &Value) -> String {
    let mut out = String::new();
    write_canonical(value, &mut out);
    out
}

fn write_canonical(value: &Value, out: &mut String) {
    match value {
        Value::Number(n) => out.push_str(&es6_number(n)),
        Value::Array(items) => {
            out.push('[');
            for (i, item) in items.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                write_canonical(item, out);
            }
            out.push(']');
        }
        Value::Object(map) => {
            out.push('{');
            for (i, (key, item)) in map.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                out.push_str(&Value::String(key.clone()).to_string());
                out.push(':');
                write_canonical(item, out);
            }
            out.push('}');
        }
        other => out.push_str(&other.to_string()),
    }
}

fn es6_number(n: &serde_json::Number) -> String {
    if let Some(u) = n.as_u64() {
        return u.to_string();
    }
    if let Some(i) = n.as_i64() {
        return i.to_string();
    }
    let f = n.as_f64().unwrap_or(0.0);
    if f == 0.0 {
        return "0".into();
    }
    if f < 0.0 {
        return format!(
            "-{}",
            es6_number(&serde_json::Number::from_f64(-f).unwrap())
        );
    }
    // `{:e}` gives the shortest round-trip digits; lay them out per ECMA-262
    // Number::toString.
    let sci = format!("{f:e}");
    let (mantissa, exp) = sci.split_once('e').unwrap_or((&sci, "0"));
    let exp: i32 = exp.parse().unwrap_or(0);
    let digits: String = mantissa.chars().filter(|c| *c != '.').collect();
    let k = digits.len() as i32;
    let n = exp + 1;
    if k <= n && n <= 21 {
        format!("{digits}{}", "0".repeat((n - k) as usize))
    } else if 0 < n && n <= 21 {
        format!("{}.{}", &digits[..n as usize], &digits[n as usize..])
    } else if -6 < n && n <= 0 {
        format!("0.{}{digits}", "0".repeat((-n) as usize))
    } else {
        let e = n - 1;
        let sign = if e < 0 { '-' } else { '+' };
        if k == 1 {
            format!("{digits}e{sign}{}", e.abs())
        } else {
            format!("{}.{}e{sign}{}", &digits[..1], &digits[1..], e.abs())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    /// One valid and one invalid explicit value per next-session key, the same
    /// samples as `hostcfg/policy_catalog_test.go explicitSamples`.
    fn samples() -> Vec<(&'static str, Value, Value)> {
        vec![
            ("abr_enabled", json!(false), json!("false")),
            ("abr_floor_kbps", json!(1), json!(0)),
            ("abr_floor_ratio", json!(1), json!(1.01)),
            ("abr_mode", json!("protective"), json!("fast")),
            ("abr_ewma_alpha", json!(0.000001), json!(0)),
            ("abr_deadband", json!(0.999999), json!(1)),
            ("abr_max_up_step", json!(0.000001), json!(0)),
            ("abr_min_interval_ms", json!(1), json!(1.5)),
            ("abr_max_down_step", json!(0.5), json!(1)),
            ("abr_down_dwell_ms", json!(0), json!(-1)),
            ("abr_cliff_guard_frac", json!(0.5), json!(1)),
            ("abr_ladder", json!(true), json!(1)),
            ("abr_ladder_max_bias", json!(255), json!(256)),
            ("abr_ladder_engage_dwell", json!(1), json!(0)),
            ("abr_ladder_recover_dwell", json!(255), json!(256)),
            ("abr_ladder_resolution", json!(true), json!("yes")),
            ("abr_ladder_res_exponent", json!(0.5), json!(0.49)),
            ("abr_ladder_res_engage_frac", json!(0.2), json!(0.19)),
            ("abr_ladder_res_recover_frac", json!(1), json!(1.1)),
            ("abr_ladder_res_engage_dwell", json!(60), json!(61)),
            ("abr_ladder_res_recover_dwell", json!(1), json!(0)),
            ("abr_ladder_res_min_step_s", json!(5), json!(4)),
            ("abr_ladder_res_min_height", json!(2160), json!(2161)),
            ("abr_ladder_fps", json!(true), json!(null)),
            ("abr_ladder_floor_follows_rung", json!(false), json!(0)),
            ("abr_ladder_order", json!("fps_first"), json!("fastest")),
            ("gop", json!(1), json!(0)),
            ("slices", json!(8), json!(2.5)),
            ("target_usage", json!(7), json!(8)),
            ("queue_buffers", json!(1), json!(0)),
            ("zerocopy", json!(true), json!("true")),
            ("latency_probe", json!(false), json!(0)),
            ("idle_timeout_secs", json!(0), json!(-1)),
            ("app_boot_timeout_secs", json!(0), json!(0.5)),
            (
                "home_root",
                json!("/srv/homes/users"),
                json!("relative/path"),
            ),
            ("nvidia_lib32_path", json!(""), json!("lib32")),
        ]
    }

    fn baseline() -> RuntimeSettings {
        let mut settings = RuntimeSettings::baseline_with(&|_| None);
        settings.home_root = "/srv/homes".into();
        settings
    }

    #[test]
    fn groups_are_sorted_unique_and_cover_every_sample() {
        assert!(NEXT_SESSION_GROUPS.windows(2).all(|w| w[0] < w[1]));
        assert_eq!(NEXT_SESSION_GROUPS.len(), 36);
        let keys: Vec<_> = samples().iter().map(|s| s.0).collect();
        for group in NEXT_SESSION_GROUPS {
            assert!(keys.contains(group), "{group} has no sample");
        }
        assert_eq!(group_for_key("render_node"), "hardware");
        assert!(!is_next_session_group("hardware"));
    }

    /// Per key: a valid explicit value resolves and takes effect on the next
    /// session's settings; an invalid one is rejected before any effect; a
    /// deployment choice resolves to the baseline value.
    #[test]
    fn every_next_session_key_resolves_validates_and_takes_effect() {
        let base = baseline();
        for (key, valid, invalid) in samples() {
            let explicit = json!({key: {"source": "explicit", "value": valid}});
            let resolved = resolve_group(key, &explicit, &base).unwrap();
            let mut next = base.clone();
            let snapshot = Value::Object(resolved.clone());
            compose_groups(&mut next, [(key, &snapshot)])
                .unwrap_or_else(|e| panic!("{key}={valid}: {e}"));
            assert!(
                json_equal(&next.deployment_map()[key], &valid),
                "{key}: next-session value {} != {valid}",
                next.deployment_map()[key]
            );
            let bad = json!({key: {"source": "explicit", "value": invalid}});
            assert_eq!(
                resolve_group(key, &bad, &base),
                Err("invalid_value"),
                "{key}"
            );

            let deployment = json!({key: {"source": "deployment"}});
            let resolved = resolve_group(key, &deployment, &base).unwrap();
            assert_eq!(resolved[key], base.deployment_map()[key], "{key}");
            assert!(deployment_fact(&deployment, &resolved).is_some());
            assert!(deployment_fact(&explicit, &resolved).is_none());
            assert_eq!(
                resolve_group(key, &json!({key: {"source": "automatic"}}), &base),
                Err("unsupported_source"),
                "{key}: no Automatic on next-session keys"
            );
        }
    }

    #[test]
    fn a_value_the_agent_would_ignore_is_not_claimed_applied() {
        // The catalog admits abr_floor_ratio=0, but the agent ignores a
        // non-positive ratio; readback must refuse rather than claim it.
        let mut next = baseline();
        let snapshot = json!({"abr_floor_ratio": 0});
        assert_eq!(
            compose_groups(&mut next, [("abr_floor_ratio", &snapshot)]),
            Err("typed_snapshot_invalid_value")
        );
        assert_eq!(
            next.abr_floor_ratio,
            baseline().abr_floor_ratio,
            "no partial effect"
        );
    }

    #[test]
    fn resolution_hysteresis_is_checked_on_the_composed_pair() {
        let mut next = baseline();
        let engage = json!({"abr_ladder_res_engage_frac": 0.9});
        assert_eq!(
            compose_groups(&mut next, [("abr_ladder_res_engage_frac", &engage)]),
            Err("cross_key_invalid"),
            "0.9 against the baseline recover 0.8"
        );
        let recover = json!({"abr_ladder_res_recover_frac": 1});
        compose_groups(
            &mut next,
            [
                ("abr_ladder_res_engage_frac", &engage),
                ("abr_ladder_res_recover_frac", &recover),
            ],
        )
        .unwrap();
        assert_eq!(
            next.deployment_map()["abr_ladder_res_engage_frac"],
            json!(0.9)
        );
    }

    #[test]
    fn abr_mode_wins_over_the_deprecated_switch_as_legacy_maps_do() {
        let mut next = baseline();
        let off = json!({"abr_enabled": false});
        let smooth = json!({"abr_mode": "smooth"});
        compose_groups(&mut next, [("abr_mode", &smooth), ("abr_enabled", &off)]).unwrap();
        let mut legacy = baseline();
        legacy.apply_json(&json!({"abr_enabled": false, "abr_mode": "smooth"}));
        assert_eq!(next.abr_mode, legacy.abr_mode);
    }

    #[test]
    fn storage_effects_stay_inside_the_mount_and_need_an_accessible_path() {
        let base = baseline();
        let never = |_: &str| false;
        let root = |v: &str| Map::from_iter([("home_root".to_string(), json!(v))]);
        assert!(check_host_effects(&root("/srv/homes/a"), &base, &never).is_ok());
        assert!(check_host_effects(&root(""), &base, &never).is_ok());
        for outside in ["/srv/homes-evil", "/srv/homes/../etc", "/tmp"] {
            assert_eq!(
                check_host_effects(&root(outside), &base, &never),
                Err("home_root_outside_mount"),
                "{outside}"
            );
        }
        let mut unmounted = base.clone();
        unmounted.home_root.clear();
        assert_eq!(
            check_host_effects(&root("/srv/homes"), &unmounted, &never),
            Err("home_root_outside_mount"),
            "no implicit mount creation"
        );
        let lib = Map::from_iter([("nvidia_lib32_path".to_string(), json!("/usr/lib32"))]);
        assert_eq!(
            check_host_effects(&lib, &base, &never),
            Err("path_inaccessible")
        );
        assert!(check_host_effects(&lib, &base, &|p| p == "/usr/lib32").is_ok());
    }

    /// The cross-language vector, byte-for-byte the one in
    /// `hostcfg/policy_candidate_test.go canonicalVector`: RFC 8785 emits
    /// U+2028/U+2029 literally and escapes only `"`, `\\` and C0 controls.
    #[test]
    fn canonical_json_cross_language_vector() {
        let value = json!({"group": "home_root", "resolved_settings": {
            "abr_floor_ratio": 0.3,
            "home_root": "/srv/a\u{2028}b\u{2029}c&<>\"\\u2028\\\n\u{1}\u{7f}é😀",
        }});
        assert_eq!(
            canonical_json(&value),
            "{\"group\":\"home_root\",\"resolved_settings\":{\"abr_floor_ratio\":0.3,\
             \"home_root\":\"/srv/a\u{2028}b\u{2029}c&<>\\\"\\\\u2028\\\\\\n\\u0001\u{7f}é😀\"}}"
        );
        assert_eq!(
            digest(&value),
            "d1522a0da765589b9e42f0c8062e42bfcbc2d3bd6475d6910ec0a182a65ed86b"
        );
    }

    #[test]
    fn canonical_json_matches_the_control_plane_serializer() {
        // Byte-for-byte the output hostcfg.canonicalJSON produces
        // (TestCanonicalJSONDoesNotEscapeHTML).
        let value = json!({"z": 1e21, "home_root": "/srv/a&b<c>", "a": 0.000001, "m": 1e-7});
        assert_eq!(
            canonical_json(&value),
            r#"{"a":0.000001,"home_root":"/srv/a&b<c>","m":1e-7,"z":1e+21}"#
        );
        for (f, want) in [
            (0.3, "0.3"),
            (1.0, "1"),
            (123.456, "123.456"),
            (1e20, "100000000000000000000"),
            (1.5e-7, "1.5e-7"),
            (-2.5, "-2.5"),
        ] {
            assert_eq!(canonical_json(&json!(f)), want, "{f}");
        }
        // The #335 idle digest input is unchanged by canonicalisation.
        let idle =
            json!({"group":"idle_timeout_secs","resolved_settings":{"idle_timeout_secs":120}});
        assert_eq!(canonical_json(&idle), serde_json::to_string(&idle).unwrap());
    }
}
