//! Signaling wire types — a faithful, 1:1 encoding of `protocol/signaling.md`.
//! Frozen interface: the JSON shapes here must match the protocol doc byte-for-byte.
//!
//!   Host -> client:   {"type":"offer","sdp":"..."}
//!                     {"type":"ice","candidate":{"candidate":"...","sdpMid":"0","sdpMLineIndex":0}}
//!   Client -> host:   {"type":"answer","sdp":"..."}
//!                     {"type":"ice","candidate":{...}}
//!   Either direction: {"type":"error","message":"..."}
//!                     {"type":"bye"}
//!
//! The control plane relays this exact inner `msg` over the agent-API WebSocket
//! (agent-api.md §Signaling relay) — the shapes don't change, only the transport.
//!
//! #304 amendment: `offer`/`answer`/`ice` carry an optional `pc` field
//! (`"video"` or `"audio"`) identifying which PeerConnection the message belongs
//! to. Absent → `"video"` (backwards-compatible). The host creates two
//! webrtcbin instances when audio is enabled; the video PC carries video + the
//! `"input"` DataChannel, the audio PC carries only audio.

use std::fmt;

use serde::{Deserialize, Serialize};

/// Which PeerConnection a signaling message belongs to (#304). The video PC
/// carries video + the `"input"` DataChannel; the audio PC carries only audio.
/// Serialised as `"video"` / `"audio"` (lowercase, matching the protocol doc).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PcId {
    #[default]
    Video,
    Audio,
}

impl fmt::Display for PcId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            PcId::Video => f.write_str("video"),
            PcId::Audio => f.write_str("audio"),
        }
    }
}

/// One ICE candidate, in the browser `RTCIceCandidateInit` shape the protocol
/// mandates. `sdp_mid` / `sdp_m_line_index` serialize to the camelCase wire
/// names `sdpMid` / `sdpMLineIndex`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct IceCandidate {
    pub candidate: String,
    #[serde(rename = "sdpMid")]
    pub sdp_mid: Option<String>,
    #[serde(rename = "sdpMLineIndex")]
    pub sdp_m_line_index: Option<u32>,
}

/// A signaling message. `#[serde(tag = "type")]` produces/consumes the
/// `"type"`-discriminated objects the protocol defines, variant names lowercased
/// to match (`offer`, `answer`, `ice`, `error`, `bye`).
///
/// The `pc` field on `Offer`/`Answer`/`Ice` is **optional** (#304): it serialises
/// only when `Some` (`skip_serializing_if`), so a pre-#304 peer sees the same
/// shapes it always did; an absent `pc` deserialises to `PcId::Video`.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "lowercase")]
pub enum SignalMsg {
    /// Host -> client. SDP offer (host is the offerer).
    Offer {
        sdp: String,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        pc: Option<PcId>,
    },
    /// Client -> host. SDP answer.
    Answer {
        sdp: String,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        pc: Option<PcId>,
    },
    /// Either direction. Trickled ICE candidate.
    Ice {
        candidate: IceCandidate,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        pc: Option<PcId>,
    },
    /// Client -> host. Ask the offerer to generate an ICE-restart offer.
    #[serde(rename = "restart_ice")]
    RestartIce {
        #[serde(default, skip_serializing_if = "Option::is_none")]
        pc: Option<PcId>,
    },
    /// Either direction (diagnostics).
    Error { message: String },
    /// Either direction (diagnostics).
    Bye,
}

impl SignalMsg {
    /// Serialize to a compact JSON string for one WebSocket text frame.
    pub fn to_json(&self) -> serde_json::Result<String> {
        serde_json::to_string(self)
    }

    /// Parse one WebSocket text frame.
    pub fn from_json(s: &str) -> serde_json::Result<Self> {
        serde_json::from_str(s)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn offer_roundtrips_to_protocol_shape() {
        let m = SignalMsg::Offer {
            sdp: "v=0\r\n".to_string(),
            pc: None,
        };
        let json = m.to_json().unwrap();
        assert_eq!(json, r#"{"type":"offer","sdp":"v=0\r\n"}"#);
    }

    #[test]
    fn offer_with_pc_serializes_pc_field() {
        let m = SignalMsg::Offer {
            sdp: "v=0\r\n".to_string(),
            pc: Some(PcId::Audio),
        };
        let json = m.to_json().unwrap();
        assert_eq!(json, r#"{"type":"offer","sdp":"v=0\r\n","pc":"audio"}"#);
    }

    #[test]
    fn answer_with_pc_parses_back() {
        let json = r#"{"type":"answer","pc":"video","sdp":"v=0\r\n"}"#;
        match SignalMsg::from_json(json).unwrap() {
            SignalMsg::Answer { sdp, pc } => {
                assert_eq!(sdp, "v=0\r\n");
                assert_eq!(pc, Some(PcId::Video));
            }
            other => panic!("expected answer, got {other:?}"),
        }
    }

    #[test]
    fn answer_parses_from_protocol_shape() {
        // Pre-#304 shape: no pc field → defaults to None (treated as Video).
        let json = r#"{"type":"answer","sdp":"v=0\r\n"}"#;
        match SignalMsg::from_json(json).unwrap() {
            SignalMsg::Answer { sdp, pc } => {
                assert_eq!(sdp, "v=0\r\n");
                assert_eq!(pc, None);
            }
            other => panic!("expected answer, got {other:?}"),
        }
    }

    #[test]
    fn ice_uses_camelcase_wire_names() {
        let m = SignalMsg::Ice {
            candidate: IceCandidate {
                candidate: "candidate:1 1 UDP 2122252543 10.0.0.1 54321 typ host".to_string(),
                sdp_mid: Some("0".to_string()),
                sdp_m_line_index: Some(0),
            },
            pc: None,
        };
        let json = m.to_json().unwrap();
        assert!(json.contains(r#""sdpMid":"0""#), "got {json}");
        assert!(json.contains(r#""sdpMLineIndex":0"#), "got {json}");
        // And it parses back.
        let back = SignalMsg::from_json(&json).unwrap();
        matches!(back, SignalMsg::Ice { .. });
    }

    #[test]
    fn ice_with_pc_serializes_and_parses() {
        let m = SignalMsg::Ice {
            candidate: IceCandidate {
                candidate: "candidate:1 1 UDP 2122252543 10.0.0.1 54321 typ host".to_string(),
                sdp_mid: Some("0".to_string()),
                sdp_m_line_index: Some(0),
            },
            pc: Some(PcId::Audio),
        };
        let json = m.to_json().unwrap();
        assert!(json.contains(r#""pc":"audio""#), "got {json}");
        match SignalMsg::from_json(&json).unwrap() {
            SignalMsg::Ice { pc, .. } => assert_eq!(pc, Some(PcId::Audio)),
            other => panic!("expected ice, got {other:?}"),
        }
    }

    #[test]
    fn restart_ice_matches_protocol_shape() {
        let m = SignalMsg::RestartIce {
            pc: Some(PcId::Video),
        };
        assert_eq!(
            m.to_json().unwrap(),
            r#"{"type":"restart_ice","pc":"video"}"#
        );
    }

    #[test]
    fn bye_has_only_type() {
        assert_eq!(SignalMsg::Bye.to_json().unwrap(), r#"{"type":"bye"}"#);
    }
}
