// WebRTC session management — the answerer side of protocol/signaling.md (P1-D).
// Browser connects with a single-use session token; control plane validates,
// consumes, relays to the node agent. Media + input DataChannel flow
// peer-to-peer once ICE connects; signaling does not.
//
// #304: audio and video use SEPARATE RTCPeerConnections to eliminate browser A/V
// clock-coupling latency — the host tags offers/ICE with `pc` ("video"/"audio"),
// this client routes by it and plays audio off a separate element so the
// browser's A/V sync has no audio track to couple the video jitter buffer
// against. The "input" DataChannel arrives on the video PC.
//
// No React here so it can be instantiated in a useEffect and cleaned up on unmount.

import { applyPlayout, resolveInitialPlayoutMs } from "./playout";
import { RecoveryController, type RecoveryState } from "./recovery";

export type StatusHandler = (msg: string) => void;
export type TrackHandler = (stream: MediaStream) => void;
export type ChannelHandler = (ch: RTCDataChannel) => void;

/** WS close codes from signaling.md P1-D. */
const WS_CLOSE_REASONS: Record<number, string> = {
  4401: "token invalid / expired / already used — re-launch to retry",
  4404: "session not found or already ended",
  4409: "session not yet assigned to a host",
  4500: "relay to node agent unavailable (host offline)",
};

/**
 * #526 — a later attach took this session's signaling over
 * (control-plane/internal/signal/handler.go, `wsCloseTakenOver`). Must NOT reach
 * `recovery.terminal()`: that phase re-attaches with a new token, displacing the
 * tab that just displaced us, looping.
 */
export const WS_CLOSE_TAKEN_OVER = 4410;

/** Which PeerConnection a signaling message belongs to (#304). */
type PcId = "video" | "audio";

/** Structural minimum of an RTCRtpTransceiver needed to pick the mic slot. */
interface MidBearing {
  mid: string | null;
}

/**
 * Mic capture (spec §3.4): a granted session's `pc:"audio"` offer carries TWO
 * audio m-lines (host->client sendonly, and the recvonly mic slot). Right after
 * setRemoteDescription neither has negotiated, so `currentDirection`/
 * `receiver.track` can't tell them apart — only the offer SDP's direction
 * attribute can. Returns null when the offer has no recvonly m=audio section
 * (mic not granted, or a pre-amendment host).
 */
export function selectMicTransceiver<T extends MidBearing>(
  offerSdp: string,
  transceivers: readonly T[],
): T | null {
  const mid = micMidFromOffer(offerSdp);
  if (mid == null) return null;
  return transceivers.find((t) => t.mid === mid) ?? null;
}

/** The `a=mid:` of the offer's recvonly audio m-section, or null. Exported for tests. */
export function micMidFromOffer(offerSdp: string): string | null {
  const sections = offerSdp.split(/\r?\n(?=m=)/); // index 0 is the session-level block
  for (const section of sections) {
    if (!/^m=audio\s/.test(section)) continue;
    // sendrecv (the m-section's default direction when omitted) is not our slot.
    if (!/^a=recvonly\s*$/m.test(section)) continue;
    const mid = /^a=mid:(\S+)\s*$/m.exec(section);
    if (mid) return mid[1];
  }
  return null;
}

/**
 * Manages the WebSocket + two RTCPeerConnections for one signaling session (#304).
 *
 * The browser is always the **answerer** (host is the offerer) per signaling.md.
 * Callers create one instance per session and call close() on cleanup.
 */
export class QuasarSession {
  private ws: WebSocket;
  private pcVideo: RTCPeerConnection;
  private pcAudio: RTCPeerConnection | null = null;
  private closed = false;
  /** Set when the track arrives; exposed so the AS-05 {@link PlayoutController}
   * can re-target playout over the session's lifetime. */
  videoReceiver: RTCRtpReceiver | null = null;
  private readonly recovery: RecoveryController;

  constructor(
    /** ws(s)://host/v1/signal URL from the launch response; token appended here. */
    signalingUrl: string,
    token: string,
    onTrack: TrackHandler,
    onStatus: StatusHandler,
    onChannel: ChannelHandler,
    /** `?playout=` override, else tier playout₀, else default; AS-05 controller
     * adapts from here. See {@link resolveInitialPlayoutMs}. */
    private readonly initialPlayoutMs: number = resolveInitialPlayoutMs(),
    onRecoveryState?: (state: RecoveryState) => void,
    requestOfferOnOpen = false,
    /**
     * #509: STUN/TURN servers from `signaling.ice_servers` (control-api.md). Held
     * as a field because the lazily-created audio PC must match the video PC's
     * config exactly — the two connections gather candidates independently, so a
     * mismatch would connect video but not audio. Empty (LAN default) means
     * host candidates only.
     */
    private readonly iceServers: RTCIceServer[] = [],
  ) {
    const wsUrl = `${signalingUrl}?token=${encodeURIComponent(token)}`;

    this.pcVideo = new RTCPeerConnection({
      iceServers: this.iceServers,
    });
    this.recovery = new RecoveryController({
      // Host is the offerer, so it creates the ICE-restart offer; we keep the
      // existing signaling socket and all media/input/telemetry attachments.
      onRetry: () => this.wsSend({ type: "restart_ice", pc: "video" }),
      onState: (state) => {
        onStatus(state.message);
        onRecoveryState?.(state);
      },
    });

    this.pcVideo.ontrack = (e) => {
      if (e.track.kind === "video" && e.receiver) {
        this.videoReceiver = e.receiver;
        applyPlayout(e.receiver, this.initialPlayoutMs);
      }
      const stream = e.streams[0] ?? new MediaStream([e.track]);
      onTrack(stream);
    };

    this.pcVideo.ondatachannel = (e) => {
      if (e.channel.label === "input") onChannel(e.channel);
    };

    this.pcVideo.onicecandidate = (e) => {
      if (!e.candidate) return;
      this.wsSend({
        type: "ice",
        pc: "video",
        candidate: {
          candidate: e.candidate.candidate,
          sdpMid: e.candidate.sdpMid,
          sdpMLineIndex: e.candidate.sdpMLineIndex,
        },
      });
    };

    let prevIceState = "new";
    this.pcVideo.oniceconnectionstatechange = () => {
      const s = this.pcVideo.iceConnectionState;
      if (s === "connected" || s === "completed") {
        this.recovery.connected();
      } else if (s === "failed") {
        this.recovery.interrupted("ICE failed — checking whether the path can recover");
      } else if (s === "disconnected") {
        this.recovery.interrupted("Connection degraded — media path interrupted");
      } else {
        onStatus(`ICE: ${s}`);
      }
      this.onWebRtcStateChange?.("ice", prevIceState, s);
      prevIceState = s;
    };

    let prevConnectionState = "new";
    this.pcVideo.onconnectionstatechange = () => {
      const s = this.pcVideo.connectionState;
      this.onWebRtcStateChange?.("connection", prevConnectionState, s);
      prevConnectionState = s;
    };

    this.ws = new WebSocket(wsUrl);

    this.ws.onopen = () => {
      // The launch screen's first step. A signal, not a status string: the
      // strings are prose and change.
      this.onSignalingOpen?.();
      onStatus(requestOfferOnOpen ? "signaling restored — requesting media" : "ws open — waiting for offer");
      if (requestOfferOnOpen) {
        this.wsSend({ type: "restart_ice", pc: "video" });
        this.wsSend({ type: "restart_ice", pc: "audio" });
      }
    };

    this.ws.onclose = (e) => {
      // #526: terminal but not a fault, so it must not escalate. Close-code-driven,
      // not heuristic — an ordinary blip closes 1006/1000 and recovers as before.
      if (e.code === WS_CLOSE_TAKEN_OVER) {
        this.recovery.superseded("This session was opened in another tab or window");
        return;
      }
      const reason = WS_CLOSE_REASONS[e.code];
      this.recovery.terminal(reason ? `signaling: ${reason}` : `signaling closed (${e.code})`);
    };

    this.ws.onerror = () => this.recovery.terminal("Control-plane signaling connection failed");

    this.ws.onmessage = (ev: MessageEvent<string>) => {
      let msg: {
        type: string;
        sdp?: string;
        candidate?: RTCIceCandidateInit;
        message?: string;
        pc?: PcId;
      };
      try {
        msg = JSON.parse(ev.data) as typeof msg;
      } catch {
        return;
      }

      void (async () => {
        const pc = (msg.pc ?? "video") as PcId;
        if (msg.type === "offer" && msg.sdp) {
          try {
            const target = this.pcFor(pc);
            await target.setRemoteDescription({ type: "offer", sdp: msg.sdp });
            // Claim the mic transceiver as sendonly BEFORE createAnswer: left at
            // its post-SRD default it answers `inactive` and the slot is gone for
            // the session (host caps each PC at a single offer, no renegotiation).
            if (pc === "audio") this.claimMicTransceiver(target, msg.sdp);
            const answer = await target.createAnswer();
            await target.setLocalDescription(answer);
            this.wsSend({ type: "answer", pc, sdp: answer.sdp });
            if (pc === "video") {
              onStatus("answer sent — awaiting ICE");
            }
          } catch (e) {
            console.error(`[quasar] ${pc} PC offer/answer failed:`, e);
            onStatus(`${pc} PC negotiation failed: ${e}`);
          }
        } else if (msg.type === "ice" && msg.candidate) {
          const target = this.pcFor(pc);
          try {
            await target.addIceCandidate(msg.candidate);
          } catch {
            /* ignore addIceCandidate failures — non-fatal trickle ICE errors */
          }
        } else if (msg.type === "error") {
          onStatus(`host error: ${msg.message ?? ""}`);
        } else if (msg.type === "bye") {
          onStatus("host ended the session");
        }
      })();
    };
  }

  /** Audio PC is created lazily on the first `pc: "audio"` offer/ICE. */
  private pcFor(pc: PcId): RTCPeerConnection {
    if (pc === "video") return this.pcVideo;
    if (this.pcAudio) return this.pcAudio;
    this.pcAudio = new RTCPeerConnection({ iceServers: this.iceServers }); // #509: matches video PC
    this.pcAudio.onicecandidate = (e) => {
      if (!e.candidate) return;
      this.wsSend({
        type: "ice",
        pc: "audio",
        candidate: {
          candidate: e.candidate.candidate,
          sdpMid: e.candidate.sdpMid,
          sdpMLineIndex: e.candidate.sdpMLineIndex,
        },
      });
    };
    // Surfaced via onAudioTrack (not the caller's onTrack) so the browser never
    // couples this stream with the video PC's jitter buffer.
    this.pcAudio.ontrack = (e) => {
      const stream = e.streams[0] ?? new MediaStream([e.track]);
      this.onAudioTrack?.(stream);
    };
    return this.pcAudio;
  }

  /** Optional callback for the audio track (arrives on the audio PC). */
  onAudioTrack: TrackHandler | null = null;

  // Mic (spec §3.4) rides a reverse m-line on the SAME audio PC, negotiated
  // up-front. Enable/disable is purely local (replaceTrack) — mute/unmute never
  // renegotiates or sends a signaling message.

  /** The transceiver answering the host's mic m-line, once claimed. */
  private micTransceiver: RTCRtpTransceiver | null = null;

  /** Claim the offer's recvonly audio m-line as our send slot. */
  private claimMicTransceiver(pc: RTCPeerConnection, offerSdp: string): void {
    if (this.micTransceiver) return;
    const t = selectMicTransceiver(offerSdp, pc.getTransceivers());
    if (!t) return;
    try {
      t.direction = "sendonly";
      this.micTransceiver = t;
    } catch (e) {
      // No mic slot; rest of the session unaffected.
      console.warn("[quasar] mic transceiver claim failed:", e);
    }
  }

  /** True when this session negotiated a microphone m-line. */
  hasMicSlot(): boolean {
    return this.micTransceiver != null;
  }

  /**
   * Send the given capture track to the host. No renegotiation: replaceTrack on
   * an already-negotiated sendonly transceiver.
   */
  async attachMicTrack(track: MediaStreamTrack): Promise<void> {
    if (!this.micTransceiver) return;
    await this.micTransceiver.sender.replaceTrack(track);
  }

  /**
   * Stop sending microphone audio. The transceiver stays negotiated (so the mic
   * can be turned back on later); the caller separately stops the track to
   * release the device.
   */
  async detachMicTrack(): Promise<void> {
    if (!this.micTransceiver) return;
    await this.micTransceiver.sender.replaceTrack(null);
  }

  onWebRtcStateChange: ((kind: "ice" | "connection", from: string, to: string) => void) | null = null;

  /** Signalling socket opened. Assigned after construction, like the handlers
   *  above; `ws.onopen` cannot fire before the constructor returns. */
  onSignalingOpen: (() => void) | null = null;

  private wsSend(obj: unknown): void {
    if (this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(obj));
    }
  }

  /** Forward RTCPeerConnection stats for telemetry (P4-04). Uses the video PC. */
  getStats(): Promise<RTCStatsReport> {
    return this.pcVideo.getStats();
  }

  /** Query receiver parameters after negotiation for abs-capture-time evidence. */
  hasAbsCaptureTimeExtension(uri: string): boolean {
    return this.pcVideo
      .getReceivers()
      .find((receiver) => receiver.track?.kind === "video")
      ?.getParameters()
      .headerExtensions
      .some((extension) => extension.uri === uri) ?? false;
  }

  /** Stop bounded in-place recovery without tearing down the page first. */
  cancelRecovery(): void {
    this.recovery.cancel();
  }

  /** Trigger bounded ICE recovery when media telemetry stalls before the
   * browser's slower consent timer changes iceConnectionState. */
  recoverMediaPath(): void {
    this.recovery.interrupted("Media stalled — refreshing the network path");
  }

  /** Telemetry-level recovery signal for outages shorter than the browser's
   * ICE state transition threshold. */
  mediaPathFlowing(): void {
    this.recovery.connected();
  }

  /** Close the WS and both peer connections. Safe to call multiple times. */
  close(notifyPeer = true): void {
    if (this.closed) return;
    this.closed = true;
    this.recovery.close();
    if (notifyPeer) this.wsSend({ type: "bye" });
    this.ws.close();
    this.pcVideo.close();
    this.pcAudio?.close();
  }
}
