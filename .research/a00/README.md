# A00: WebTransport receive-throughput kit (throwaway)

Spike accreleus/quasar#1025. Research only: this branch never merges. Not product code.

## Pieces
- `sender/` Go (quic-go + webtransport-go) WebTransport server on UDP 4433, path `/wt`. Makes its own
  self-signed ECDSA P-256 cert (10 days, kept in `~/.local/state/a00-wt/` so the hash survives a restart),
  prints its SHA-256 hash (base64 and hex). Per run the client sends one JSON line on the first
  bidirectional stream: `{"rate":<Mbit/s>,"carriage":"datagram"|"stream","duration":<s>,"size":<<=1150>}`.
  - `datagram`: `size`-byte datagrams (byte 0 = 0xA0, bytes 1..4 = u32 LE sequence), paced.
  - `stream`: one unidirectional stream per 16 ms frame (12-byte header: seq, length), `rate*16ms` bytes each.
  - Reply line: packets/frames, bytes, achieved Mbit/s, pacing error (actual minus scheduled send time),
    process CPU seconds during the run.
- `measure.html` self-contained page (works from `file://`). Matrix {50,100,200,300,400} x {datagram,stream}, 10 s each.
  Receives in a Worker when `WebTransport` exists there (reported as `receiver`), else on the main thread.
  Table columns: received Mbit/s (first to last packet), loss % (vs sender's count), reorder, max inter-arrival gap,
  main-thread long-task ms, receiver busy ms (time inside the read loop body only; a lower bound), sender CPU.
  Results are also in `<pre id=result>` as JSON and `document.title` becomes `A00-DONE` at the end.
- `selftest.js` Playwright driver: `node selftest.js '<page url>' [timeoutSec] [shot.png]`.

## Run
Sender (long-lived, user systemd; needs `loginctl enable-linger`):

    cd sender && go build -o a00-wt-sender . && systemd-run --user --unit=q06-wt-sender --property=Restart=on-failure ./a00-wt-sender
    journalctl --user -u q06-wt-sender | head    # prints cert-hash-base64 / url lines
    ss -lunp | grep 4433

Page query: `?url=https://<sender-ip>:4433/wt&hash=<base64-or-hex>` plus optional `rates=50,100`, `carriages=datagram,stream`,
`secs=10`, `where=auto|worker|main|both`, `hwm=1024` (datagram incoming high-water mark), `size=1150`, `auto=1`.
URL-encode the base64 hash (`+` `/` `=`).

Headed Chrome via Playwright (Mac): see the comment on #1025 / the lead's hand-off.
Headless self-test from the dev host: `docker run --rm --network host -v $PWD:/work -w /work mcr.microsoft.com/playwright:v1.56.0-noble bash -c "npm i --silent playwright@1.56.0 && node selftest.js 'file:///work/measure.html?auto=1&url=...&hash=...&rates=50&secs=5' 90"`.

## Caveats
- quic-go congestion-controls datagrams too and its send queue is 32 datagrams, so a sender that cannot reach the target
  shows it as `sender achieved` below `target`. That is a property of this sender, not necessarily of Chrome.
- Go 1.26 toolchain (auto-downloaded). Kernel UDP buffer max on the dev host is 4 MiB (quic-go would like 7 MiB).
