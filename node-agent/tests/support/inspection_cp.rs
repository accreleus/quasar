//! Bounded control-plane fixture for the real-Docker home-GC acceptance.
use quasar_node_agent::{
    cp_http::CpClient,
    enrollment::TransportPolicy,
    session::gc::{GcClient, LiveRefs},
};
use std::{
    io::{Read, Write},
    net::TcpListener,
    sync::{
        atomic::{AtomicBool, AtomicUsize, Ordering},
        Arc, Mutex,
    },
    thread,
    time::{Duration, Instant},
};

pub struct ControlPlane {
    base: String,
    confirms: Arc<AtomicUsize>,
    stop: Arc<AtomicBool>,
    task: Option<thread::JoinHandle<()>>,
}
impl ControlPlane {
    pub fn new(home: &std::path::Path) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let base = format!("http://{}", listener.local_addr().unwrap());
        let pending =
            serde_json::json!({"homes":[{"id":"fixture-home", "provider":"local", "ref":home}]})
                .to_string();
        let stop = Arc::new(AtomicBool::new(false));
        let confirms = Arc::new(AtomicUsize::new(0));
        let stopping = stop.clone();
        let confirmed = confirms.clone();
        let task = thread::spawn(move || {
            let end = Instant::now() + Duration::from_secs(90);
            while !stopping.load(Ordering::SeqCst) && Instant::now() < end {
                let (mut stream, _) = match listener.accept() {
                    Ok(pair) => pair,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(5));
                        continue;
                    }
                    Err(e) => panic!("fixture listener: {e}"),
                };
                stream
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                stream
                    .set_write_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                let mut head = Vec::new();
                while !head.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    stream.read_exact(&mut byte).unwrap();
                    head.push(byte[0]);
                    assert!(head.len() < 16 * 1024);
                }
                let head = String::from_utf8(head).unwrap();
                let length = head
                    .lines()
                    .filter_map(|line| line.split_once(':'))
                    .find(|(key, _)| key.eq_ignore_ascii_case("content-length"))
                    .map(|(_, value)| value.trim().parse::<usize>().unwrap())
                    .unwrap_or(0);
                assert!(length < 16 * 1024);
                stream.read_exact(&mut vec![0; length]).unwrap();
                let body = if head.starts_with("GET /v1/agent/storage/gc-pending ") {
                    pending.as_str()
                } else {
                    assert!(head.starts_with("POST /v1/agent/storage/gc-confirm "));
                    confirmed.fetch_add(1, Ordering::SeqCst);
                    r#"{"deleted":1}"#
                };
                write!(stream, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
            }
        });
        Self {
            base,
            confirms,
            stop,
            task: Some(task),
        }
    }
    pub fn client(&self) -> GcClient {
        self.client_with_live(Arc::new(Mutex::new(Default::default())))
    }
    pub fn client_with_live(&self, live: LiveRefs) -> GcClient {
        let cp = CpClient::new(
            &TransportPolicy::Plaintext,
            self.base.clone(),
            "fixture-node".into(),
            "fixture-secret".into(),
        )
        .unwrap();
        GcClient::new(cp, live)
    }
    pub fn confirmations(&self) -> usize {
        self.confirms.load(Ordering::SeqCst)
    }
}
impl Drop for ControlPlane {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(task) = self.task.take() {
            let _ = task.join();
        }
    }
}
