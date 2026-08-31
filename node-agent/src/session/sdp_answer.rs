//! Answer-SDP m-line reading: what the peer actually accepted.
//!
//! Why: the agent already reports a `set-remote-description` that webrtcbin **refuses**
//! (`webrtc.remote_description_failed`, #503), but reported nothing for the other shape of
//! the same problem — an answer that applies cleanly and *rejects an m-line inside it*
//! (headless Chrome cannot hardware-decode HEVC on Linux and answers an HEVC offer with the
//! video m-line at port 0; webrtcbin accepts it, the session goes `running`, no media
//! flows). A rejected m-line is a FACT the peer stated and must never be inferred.
//!
//! Deliberately a small line parser rather than the GStreamer SDP API: this runs on the
//! runner's supervision tick over a string it already holds, and being pure text-in /
//! value-out is what lets the rejection shapes be unit-tested without a pipeline.

/// One m-section of an answer, reduced to what a diagnosis needs.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MLine {
    /// `a=mid:` value, when the answer carries one (BUNDLE always does).
    pub mid: Option<String>,
    /// `"video"` | `"audio"` | `"application"` | the raw media type for anything else.
    pub kind: String,
    /// The `a=rtpmap` encoding name of the FIRST format in the m-line's format list —
    /// i.e. the codec the answer selected. `None` for a data channel, for a rejected
    /// m-line that lists no format, or when no rtpmap names the chosen payload type.
    pub codec: Option<String>,
    /// The m-line's port. **`0` is the SDP way of saying "I reject this media".**
    pub port: i32,
    /// `sendonly` | `recvonly` | `sendrecv` | `inactive`. Absent in the SDP ⇒ `sendrecv`
    /// (RFC 4566's default), which is what a reader must assume rather than "unknown".
    pub direction: String,
    /// The peer will not carry this media: port 0, or an explicit `inactive`.
    ///
    /// We are always the offerer and every media m-line we offer is `sendonly` (video,
    /// audio) or `recvonly` (the mic), so an `inactive` answer is a refusal in both
    /// directions — there is no case here where `inactive` is a legitimate agreement.
    pub rejected: bool,
}

/// What one answer said. `ice_ufrag` is the first one seen (session- or media-level), used
/// only to tell an ICE restart apart from a re-answer of the same negotiation.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Answer {
    pub m_lines: Vec<MLine>,
    pub ice_ufrag: Option<String>,
}

impl Answer {
    /// How many m-lines the peer refused.
    pub fn rejected_count(&self) -> usize {
        self.m_lines.iter().filter(|m| m.rejected).count()
    }

    /// The codecs of the refused m-lines, for the log line. Named because "the peer
    /// rejected an m-line" is a fact and "the peer cannot decode h265" is the answer.
    pub fn rejected_codecs(&self) -> Vec<String> {
        self.m_lines
            .iter()
            .filter(|m| m.rejected)
            .map(|m| match &m.codec {
                Some(c) => format!("{}/{}", m.kind, c),
                None => m.kind.clone(),
            })
            .collect()
    }
}

fn normalise_direction(raw: &str) -> Option<&'static str> {
    match raw {
        "sendonly" => Some("sendonly"),
        "recvonly" => Some("recvonly"),
        "sendrecv" => Some("sendrecv"),
        "inactive" => Some("inactive"),
        _ => None,
    }
}

/// Parse an answer SDP into its m-sections. Tolerant by construction: a line it does not
/// recognise is skipped, and a malformed m-line yields no section rather than an error —
/// this is a diagnostic reader, and refusing to report anything because one line was odd
/// is the opposite of what it is for.
pub fn parse_answer(sdp: &str) -> Answer {
    let mut out = Answer::default();
    // Per-section accumulators; flushed when the next `m=` (or the end) arrives.
    let mut cur: Option<MLine> = None;
    // The payload type the current m-line selected (its first format), as a string.
    let mut want_pt: Option<String> = None;

    for raw in sdp.lines() {
        let line = raw.trim_end_matches('\r');
        if let Some(rest) = line.strip_prefix("m=") {
            if let Some(m) = cur.take() {
                out.m_lines.push(m);
            }
            want_pt = None;
            let mut parts = rest.split_whitespace();
            let (Some(kind), Some(port)) = (parts.next(), parts.next()) else {
                continue;
            };
            let Ok(port) = port.parse::<i32>() else {
                continue;
            };
            let _proto = parts.next();
            want_pt = parts.next().map(str::to_string);
            cur = Some(MLine {
                mid: None,
                kind: kind.to_string(),
                codec: None,
                port,
                // Filled in at flush time: the default is `sendrecv` and `rejected`
                // depends on the direction, so both are only final once the section ends.
                direction: "sendrecv".to_string(),
                rejected: port == 0,
            });
            continue;
        }
        let Some(attr) = line.strip_prefix("a=") else {
            continue;
        };
        if let Some(ufrag) = attr.strip_prefix("ice-ufrag:") {
            // First wins: a session-level ufrag precedes every media section, and a
            // BUNDLE answer repeats the same value per section.
            out.ice_ufrag
                .get_or_insert_with(|| ufrag.trim().to_string());
            continue;
        }
        let Some(m) = cur.as_mut() else { continue };
        if let Some(mid) = attr.strip_prefix("mid:") {
            m.mid = Some(mid.trim().to_string());
        } else if let Some(dir) = normalise_direction(attr) {
            m.direction = dir.to_string();
            m.rejected = m.port == 0 || dir == "inactive";
        } else if let Some(rtpmap) = attr.strip_prefix("rtpmap:") {
            let mut it = rtpmap.split_whitespace();
            let (Some(pt), Some(enc)) = (it.next(), it.next()) else {
                continue;
            };
            if want_pt.as_deref() == Some(pt) {
                m.codec = Some(enc.split('/').next().unwrap_or(enc).to_string());
            }
        }
    }
    if let Some(m) = cur.take() {
        out.m_lines.push(m);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The exact shape headless Chrome returns for an HEVC offer on Linux: the answer
    /// applies, and the video m-line is at port 0 — once misread as an encoder stall.
    const HEVC_REJECTED: &str = "\
v=0\r
o=- 1 2 IN IP4 127.0.0.1\r
s=-\r
t=0 0\r
a=ice-ufrag:abcd\r
m=video 0 UDP/TLS/RTP/SAVPF 96\r
c=IN IP4 0.0.0.0\r
a=mid:0\r
a=rtpmap:96 H265/90000\r
a=inactive\r
m=audio 9 UDP/TLS/RTP/SAVPF 111\r
c=IN IP4 0.0.0.0\r
a=mid:1\r
a=rtpmap:111 opus/48000/2\r
a=recvonly\r
";

    #[test]
    fn rejected_video_m_line_is_reported_with_its_codec() {
        let a = parse_answer(HEVC_REJECTED);
        assert_eq!(a.m_lines.len(), 2);
        assert_eq!(a.rejected_count(), 1);
        assert_eq!(a.rejected_codecs(), vec!["video/H265".to_string()]);
        let v = &a.m_lines[0];
        assert_eq!(v.kind, "video");
        assert_eq!(v.port, 0);
        assert_eq!(v.mid.as_deref(), Some("0"));
        assert_eq!(v.codec.as_deref(), Some("H265"));
        assert_eq!(v.direction, "inactive");
        assert!(v.rejected);
        assert!(!a.m_lines[1].rejected);
        assert_eq!(a.ice_ufrag.as_deref(), Some("abcd"));
    }

    #[test]
    fn a_healthy_answer_rejects_nothing() {
        let sdp = "\
v=0\r
m=video 9 UDP/TLS/RTP/SAVPF 96\r
a=mid:0\r
a=rtpmap:96 H264/90000\r
a=recvonly\r
m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r
a=mid:2\r
";
        let a = parse_answer(sdp);
        assert_eq!(a.rejected_count(), 0);
        assert_eq!(a.m_lines[0].codec.as_deref(), Some("H264"));
        assert_eq!(a.m_lines[1].kind, "application");
        // No rtpmap for a data channel, and no direction attribute ⇒ the RFC default.
        assert_eq!(a.m_lines[1].codec, None);
        assert_eq!(a.m_lines[1].direction, "sendrecv");
        assert!(!a.m_lines[1].rejected);
    }

    #[test]
    fn port_zero_alone_is_a_rejection_even_without_a_direction() {
        let a = parse_answer("m=video 0 UDP/TLS/RTP/SAVPF 96\r\na=rtpmap:96 AV1/90000\r\n");
        assert_eq!(a.rejected_count(), 1);
        assert_eq!(a.m_lines[0].direction, "sendrecv");
        assert_eq!(a.rejected_codecs(), vec!["video/AV1".to_string()]);
    }

    #[test]
    fn the_rtpmap_of_a_pt_the_answer_did_not_select_is_ignored() {
        // The answer chose 96; 98 is listed further down the section and must not be
        // reported as the negotiated codec.
        let a = parse_answer(
            "m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=rtpmap:98 VP9/90000\r\na=rtpmap:96 H264/90000\r\n",
        );
        assert_eq!(a.m_lines[0].codec.as_deref(), Some("H264"));
    }

    #[test]
    fn a_malformed_m_line_is_skipped_not_fatal() {
        let a = parse_answer("m=video notaport UDP/TLS/RTP/SAVPF 96\r\nm=audio 9 x 111\r\n");
        assert_eq!(a.m_lines.len(), 1);
        assert_eq!(a.m_lines[0].kind, "audio");
    }
}
