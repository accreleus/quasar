//! Application lifecycle specifications at the public Quasar runtime boundary.
use super::*;

#[test]
fn application_request_requires_an_owned_name() {
    let request = ApplicationRequest {
        operation: "session-1-generation-1".into(),
        name: "foreign-name".into(),
        image: "example/game:latest".into(),
        ..Default::default()
    };

    assert!(!request.is_valid());
}

/// #464: a mknod-only device is a card node, and the same request never also grants that
/// card openable, by its own path or through the whole `/dev/dri`.
#[test]
fn a_mknod_only_card_is_a_card_never_also_granted_openable() {
    let request = |devices: &[&str], cards: &[&str]| ApplicationRequest {
        operation: "session-1-generation-1".into(),
        name: "quasar-sess-1".into(),
        image: "example/game:latest".into(),
        devices: devices.iter().map(|d| d.to_string()).collect(),
        mknod_only_cards: cards.iter().map(|c| c.to_string()).collect(),
        ..Default::default()
    };
    assert!(request(
        &["/dev/dri/renderD128"],
        &["/dev/dri/card0", "/dev/dri/card1"]
    )
    .is_valid());
    let refused: [(&[&str], &[&str]); 7] = [
        (&["/dev/dri/renderD128"], &["/dev/dri/renderD128"]),
        (&[], &["/dev/sda"]),
        (&[], &["/dev/dri/card"]),
        (&[], &["/dev/dri/card0/../../sda"]),
        (&[], &["/dev/dri/by-path/pci-0000:03:00.0-card"]),
        (&["/dev/dri/card0"], &["/dev/dri/card0"]),
        (&["/dev/dri"], &["/dev/dri/card0"]),
    ];
    for (devices, cards) in refused {
        assert!(!request(devices, cards).is_valid(), "{devices:?} {cards:?}");
    }
}
