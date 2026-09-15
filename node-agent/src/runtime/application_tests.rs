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
