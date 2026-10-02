//! The critical section a signal waits for (the seed's create-then-start). A signal in the
//! test process would exit it, so this drives the public pieces the signal thread runs:
//! `settle` waits for an open section to close, and gives up after its grace.

use std::time::{Duration, Instant};

use quasar_recovery::shutdown;

#[test]
fn a_stop_waits_for_an_open_critical_section_and_gives_up_after_its_grace() {
    assert!(!shutdown::requested());
    assert!(shutdown::settle(Duration::ZERO), "nothing is open");

    // The section closes while the stop is waiting: the stop goes ahead right after it.
    let guard = shutdown::critical();
    let closer = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(200));
        drop(guard);
    });
    let start = Instant::now();
    assert!(shutdown::settle(Duration::from_secs(5)));
    let waited = start.elapsed();
    assert!(
        waited >= Duration::from_millis(150),
        "did not wait: {waited:?}"
    );
    assert!(
        waited < Duration::from_secs(2),
        "waited too long: {waited:?}"
    );
    closer.join().unwrap();

    // A section that never closes does not hold the stop past the grace.
    let _stuck = shutdown::critical();
    let start = Instant::now();
    assert!(!shutdown::settle(Duration::from_millis(100)));
    assert!(start.elapsed() < Duration::from_secs(1));
}
